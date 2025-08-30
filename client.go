package jsonrpc

import (
	"container/list"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"runtime/pprof"
	"slices"
	"sync/atomic"
	"time"

	"github.com/deorth-kku/go-common"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	methodMinRetryDelay = 100 * time.Millisecond
	methodMaxRetryDelay = 10 * time.Minute
)

var (
	errorType   = reflect.TypeFor[error]()
	contextType = reflect.TypeFor[context.Context]()

	_defaultHTTPClient = &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
				DualStack: true,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
)

// ErrClient is an error which occurred on the client side the library
type ErrClient struct {
	err error
}

func (e *ErrClient) Error() string {
	return fmt.Sprintf("RPC client error: %s", e.err)
}

// Unwrap unwraps the actual error
func (e *ErrClient) Unwrap() error {
	return e.err
}

type resultValue struct {
	functy func() reflect.Type
	value  reflect.Value
}

var (
	_ json.UnmarshalerFrom = (*resultValue)(nil)
	_ json.MarshalerTo     = (*resultValue)(nil)

	reflectUint64Type = reflect.TypeFor[uint64]()
	reflectyAnyType   = reflect.TypeFor[any]()
)

func (rv *resultValue) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	if rv.functy == nil {
		return errors.New("cannot get value type")
	}
	ty := rv.functy()
	if ty == nil {
		if dec.PeekKind() == 'n' {
			rv.value = reflect.Value{}
			return dec.SkipValue()
		}
		// defer unmarshal if we don't know its type atm.
		// this should only happen for websocket clients.
		data, err := dec.ReadValue()
		if err != nil {
			return err
		}
		data = slices.Clone(data)
		rv.value = reflect.ValueOf(deferredData{data, dec.Options()})
		return nil
	}
	temprv := reflect.New(ty)
	rv.value = temprv.Elem()
	return json.UnmarshalDecode(dec, temprv.Interface())
}

type deferredData struct {
	data jsontext.Value
	opt  json.Options
}

// run the deffered unmarshal. if the data was already unmarshaled, it does nothing and return nil.
func (rv *resultValue) deferredUnmarshal(ty reflect.Type) error {
	if !rv.value.IsValid() {
		return nil
	}
	data, ok := reflect.TypeAssert[deferredData](rv.value)
	if !ok {
		return nil
	}
	temprv := reflect.New(ty)
	rv.value = temprv.Elem()
	return json.Unmarshal(data.data, temprv.Interface(), data.opt)
}

func (rv resultValue) MarshalJSONTo(enc *jsontext.Encoder) error {
	return json.MarshalEncode(enc, rv.value.Interface())
}

type clientResponse struct {
	Jsonrpc string        `json:"jsonrpc,omitzero"`
	Result  resultValue   `json:"result,omitzero"`
	ID      any           `json:"id,omitzero"`
	Error   *JSONRPCError `json:"error,omitzero"`
}

func (f *clientResponse) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type t0 clientResponse
	err := json.UnmarshalDecode(dec, (*t0)(f))
	if err != nil {
		return err
	}
	f.ID, err = normalizeID(f.ID)
	return err
}

type makeChanSink func() (context.Context, func([]byte, bool))

type clientRequest struct {
	req   request
	ready chan clientResponse

	// retCh provides a context and sink for handling incoming channel messages
	retCh makeChanSink
	// respType provides the type for [clientResponse.Result]
	respType reflect.Type
}

// ClientCloser is used to close Client from further use
type ClientCloser func()

// NewClient creates new jsonrpc 2.0 client
//
// handler must be pointer to a struct with function fields
// Returned value closes the client connection
// TODO: Example
func NewClient(ctx context.Context, addr string, namespace string, handler any, requestHeader http.Header, opts ...Option) (ClientCloser, error) {
	return NewMergeClient(ctx, addr, namespace, []any{handler}, requestHeader, opts...)
}

type client struct {
	namespace string
	errors    *Errors

	doRequest func(context.Context, clientRequest) (clientResponse, error)
	exiting   <-chan struct{}
	idCtr     int64

	methodNameFormatter MethodNameFormatter
	logger              *slog.Logger
	jsonOption          json.Options
}

// NewMergeClient is like NewClient, but allows to specify multiple structs
// to be filled in the same namespace, using one connection
func NewMergeClient(ctx context.Context, addr string, namespace string, outs []any, requestHeader http.Header, opts ...Option) (ClientCloser, error) {
	config := defaultConfig()
	for _, o := range opts {
		o(&config)
	}

	u, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("parsing address: %w", err)
	}

	switch u.Scheme {
	case "ws", "wss":
		return websocketClient(ctx, addr, namespace, outs, requestHeader, config)
	case "http", "https":
		return httpClient(ctx, addr, namespace, outs, requestHeader, config)
	default:
		return nil, fmt.Errorf("unknown url scheme '%s'", u.Scheme)
	}

}

// NewCustomClient is like NewMergeClient in single-request (http) mode, except it allows for a custom doRequest function
func NewCustomClient(namespace string, outs []any, doRequest func(ctx context.Context, body []byte) (io.ReadCloser, error), opts ...Option) (ClientCloser, error) {
	config := defaultConfig()
	for _, o := range opts {
		o(&config)
	}

	c := config.getclient(namespace)

	stop := make(chan struct{})
	c.exiting = stop

	c.doRequest = func(ctx context.Context, cr clientRequest) (clientResponse, error) {
		b, err := json.Marshal(&cr.req, c.jsonOption)
		if err != nil {
			return clientResponse{}, fmt.Errorf("marshalling request: %w", err)
		}

		if ctx == nil {
			ctx = context.Background()
		}

		rawResp, err := doRequest(ctx, b)
		if err != nil {
			return clientResponse{}, fmt.Errorf("doRequest failed: %w", err)
		}

		defer rawResp.Close()

		var resp clientResponse
		resp.Result.functy = func() reflect.Type { return cr.respType }
		if cr.req.ID != nil { // non-notification
			if err := json.UnmarshalRead(rawResp, &resp, c.jsonOption); err != nil {
				return clientResponse{}, fmt.Errorf("unmarshaling response: %w", err)
			}

			if resp.ID != cr.req.ID {
				return clientResponse{}, errors.New("request and response id didn't match")
			}
		}

		return resp, nil
	}

	if err := c.provide(outs); err != nil {
		return nil, err
	}

	return func() {
		close(stop)
	}, nil
}

func httpClient(ctx context.Context, addr string, namespace string, outs []any, requestHeader http.Header, config Config) (ClientCloser, error) {
	c := config.getclient(namespace)

	stop := make(chan struct{})
	c.exiting = stop

	if requestHeader == nil {
		requestHeader = http.Header{}
	}

	c.doRequest = func(ctx context.Context, cr clientRequest) (clientResponse, error) {
		hreq, err := http.NewRequest("POST", addr, NewJsonReader(cr.req, WithContext(c.jsonOption, ctx)))
		if err != nil {
			return clientResponse{}, &RPCConnectionError{err}
		}

		hreq.Header = requestHeader.Clone()

		if ctx != nil {
			hreq = hreq.WithContext(ctx)
		}

		hreq.Header.Set("Content-Type", "application/json")

		httpResp, err := config.httpClient.Do(hreq)
		if err != nil {
			return clientResponse{}, &RPCConnectionError{err}
		}

		// likely a failure outside of our control and ability to inspect; jsonrpc server only ever
		// returns json format errors with either a StatusBadRequest or a StatusInternalServerError
		if httpResp.StatusCode > http.StatusBadRequest && httpResp.StatusCode != http.StatusInternalServerError {
			return clientResponse{}, fmt.Errorf("request failed, http status %s", httpResp.Status)
		}

		defer httpResp.Body.Close()

		var resp clientResponse
		resp.Result.functy = func() reflect.Type { return cr.respType }
		if cr.req.ID != nil { // non-notification
			if err := json.UnmarshalRead(httpResp.Body, &resp, c.jsonOption); err != nil {
				return clientResponse{}, fmt.Errorf("http status %s unmarshaling response: %w", httpResp.Status, err)
			}

			if resp.ID != cr.req.ID {
				return clientResponse{}, errors.New("request and response id didn't match")
			}
		}

		return resp, nil
	}

	if err := c.provide(outs); err != nil {
		return nil, err
	}

	return func() {
		close(stop)
	}, nil
}

func websocketClient(ctx context.Context, addr string, namespace string, outs []any, requestHeader http.Header, config Config) (ClientCloser, error) {
	if config.wsDialer == nil {
		config.wsDialer = websocket.DefaultDialer
	}
	connFactory := func() (*websocket.Conn, error) {
		conn, _, err := config.wsDialer.Dial(addr, requestHeader)
		if err != nil {
			return nil, &RPCConnectionError{fmt.Errorf("cannot dial address %s for %w", addr, err)}
		}
		return conn, nil
	}

	if config.proxyConnFactory != nil {
		// used in tests
		connFactory = config.proxyConnFactory(connFactory)
	}

	conn, err := connFactory()
	if err != nil {
		return nil, err
	}

	if config.noReconnect {
		connFactory = nil
	}

	c := config.getclient(namespace)

	requests := c.setupRequestChan()

	stop := make(chan struct{})
	exiting := make(chan struct{})
	c.exiting = exiting

	var hnd requestHandler
	if len(config.reverseHandlers) > 0 {
		h := makeHandler(defaultServerConfig())
		h.aliasedMethods = config.aliasedHandlerMethods
		for _, reverseHandler := range config.reverseHandlers {
			h.register(reverseHandler.ns, reverseHandler.hnd)
		}
		hnd = h
	} else {
		hnd = &config
	}

	wconn := &wsConn{
		conn:             conn,
		connFactory:      connFactory,
		reconnectBackoff: config.reconnectBackoff,
		pingInterval:     config.pingInterval,
		timeout:          config.timeout,
		handler:          hnd,
		requests:         requests,
		stop:             stop,
		exiting:          exiting,
	}

	go func() {
		lbl := pprof.Labels("jrpc-mode", "wsclient", "jrpc-remote", addr, "jrpc-local", conn.LocalAddr().String(), "jrpc-uuid", uuid.New().String())
		pprof.Do(ctx, lbl, wconn.handleWsConn)
	}()

	if err := c.provide(outs); err != nil {
		return nil, err
	}

	return func() {
		close(stop)
		<-exiting
	}, nil
}

func (c *client) setupRequestChan() chan clientRequest {
	requests := make(chan clientRequest)

	c.doRequest = func(ctx context.Context, cr clientRequest) (clientResponse, error) {
		select {
		case requests <- cr:
		case <-c.exiting:
			return clientResponse{}, fmt.Errorf("websocket routine exiting")
		}

		var ctxDone <-chan struct{}
		var resp clientResponse

		if ctx != nil {
			ctxDone = ctx.Done()
		}

		// wait for response, handle context cancellation
	loop:
		for {
			select {
			case resp = <-cr.ready:
				break loop
			case <-ctxDone: // send cancel request
				ctxDone = nil
				cancelReq := clientRequest{
					req: request{
						Jsonrpc: "2.0",
						Method:  wsCancel,
						Params:  getParam(cr.req.ID),
					},
					ready: make(chan clientResponse, 1),
				}
				select {
				case requests <- cancelReq:
				case <-c.exiting:
					c.logger.Warn("failed to send request cancellation, websocket routing exited")
				}

			}
		}

		return resp, nil
	}

	return requests
}

func (c *client) provide(outs []any) error {
	for _, handler := range outs {
		htyp := reflect.TypeOf(handler)
		if htyp.Kind() != reflect.Pointer {
			return errors.New("expected handler to be a pointer")
		}
		typ := htyp.Elem()
		if typ.Kind() != reflect.Struct {
			return errors.New("handler should be a struct")
		}

		val := reflect.ValueOf(handler)

		for i := range typ.NumField() {
			fn, err := c.makeRpcFunc(typ.Field(i))
			if err != nil {
				return err
			}

			val.Elem().Field(i).Set(fn)
		}
	}

	return nil
}

func (c *client) makeOutChan(ctx context.Context, ftyp reflect.Type, valOut int) (func() reflect.Value, makeChanSink) {
	retVal := reflect.Zero(ftyp.Out(valOut))

	chCtor := func() (context.Context, func([]byte, bool)) {
		// unpack chan type to make sure it's reflect.BothDir
		ctyp := reflect.ChanOf(reflect.BothDir, ftyp.Out(valOut).Elem())
		ch := reflect.MakeChan(ctyp, 0) // todo: buffer?
		retVal = ch.Convert(ftyp.Out(valOut))

		incoming := make(chan reflect.Value, 32)

		// gorotuine to handle buffering of items
		go func() {
			buf := (&list.List{}).Init()

			for {
				front := buf.Front()

				cases := []reflect.SelectCase{
					{
						Dir:  reflect.SelectRecv,
						Chan: reflect.ValueOf(ctx.Done()),
					},
					{
						Dir:  reflect.SelectRecv,
						Chan: reflect.ValueOf(incoming),
					},
				}

				if front != nil {
					cases = append(cases, reflect.SelectCase{
						Dir:  reflect.SelectSend,
						Chan: ch,
						Send: front.Value.(reflect.Value).Elem(),
					})
				}

				chosen, val, ok := reflect.Select(cases)

				switch chosen {
				case 0:
					ch.Close()
					return
				case 1:
					if ok {
						vvval := common.MustOk(reflect.TypeAssert[reflect.Value](val))
						buf.PushBack(vvval)
						if buf.Len() > 1 {
							if buf.Len() > 10 {
								c.logger.Warn("rpc output message buffer", "n", buf.Len())
							} else {
								c.logger.Debug("rpc output message buffer", "n", buf.Len())
							}
						}
					} else {
						incoming = nil
					}

				case 2:
					buf.Remove(front)
				}

				if incoming == nil && buf.Len() == 0 {
					ch.Close()
					return
				}
			}
		}()

		return ctx, func(result []byte, ok bool) {
			if !ok {
				close(incoming)
				return
			}

			val := reflect.New(ftyp.Out(valOut).Elem())
			if err := json.Unmarshal(result, val.Interface(), c.jsonOption); err != nil {
				c.logger.Error("error unmarshaling chan response", "err", err)
				return
			}

			if ctx.Err() != nil {
				c.logger.Error("got rpc message with cancelled context", "err", ctx.Err())
				return
			}

			select {
			case incoming <- val:
			case <-ctx.Done():
			}
		}
	}

	return func() reflect.Value { return retVal }, chCtor
}

func (c *client) sendRequest(ctx context.Context, req request, chCtor makeChanSink, ty reflect.Type) (clientResponse, error) {
	creq := clientRequest{
		req:   req,
		ready: make(chan clientResponse, 1),

		retCh:    chCtor,
		respType: ty,
	}

	return c.doRequest(ctx, creq)
}

type rpcFunc struct {
	client *client

	ftyp reflect.Type
	name string

	nout   int
	valOut int
	errOut int

	// hasCtx is 1 if the function has a context.Context as its first argument.
	// Used as the number of the first non-context argument.
	hasCtx int

	hasObjectParams      bool
	returnValueIsChannel bool

	retry  bool
	notify bool
}

func (fn *rpcFunc) processResponse(resp clientResponse, rval reflect.Value) []reflect.Value {
	out := make([]reflect.Value, fn.nout)

	if fn.valOut != -1 {
		if rval.IsValid() {
			out[fn.valOut] = rval
		} else {
			out[fn.valOut] = reflect.Zero(fn.ftyp.Out(fn.valOut))
		}
	}
	if fn.errOut != -1 {
		out[fn.errOut] = reflect.New(errorType).Elem()
		if resp.Error != nil {
			out[fn.errOut].Set(resp.Error.val(fn.client.errors))
		}
	}

	return out
}

func (fn *rpcFunc) processError(err error) []reflect.Value {
	out := make([]reflect.Value, fn.nout)

	if fn.valOut != -1 {
		out[fn.valOut] = reflect.New(fn.ftyp.Out(fn.valOut)).Elem()
	}
	if fn.errOut != -1 {
		out[fn.errOut] = reflect.New(errorType).Elem()
		out[fn.errOut].Set(reflect.ValueOf(&ErrClient{err}))
	}

	return out
}

func (fn *rpcFunc) handleRpcCall(args []reflect.Value) []reflect.Value {
	var id any
	if !fn.notify {
		id = float64(atomic.AddInt64(&fn.client.idCtr, 1))
		// Prepare the ID to send on the wire.
		// We track int64 ids as float64 in the inflight map (because that's what
		// they'll be decoded to). encoding/json outputs numbers with their minimal
		// encoding, avoding the decimal point when possible, i.e. 3 will never get
		// converted to 3.0.
	}

	ctx := context.Background()
	if fn.hasCtx == 1 {
		ctx = common.MustOk(reflect.TypeAssert[context.Context](args[0]))
		args = args[1:]
	}

	retVal := func() reflect.Value { return reflect.Value{} }

	// if the function returns a channel, we need to provide a sink for the
	// messages
	var chCtor makeChanSink
	if fn.returnValueIsChannel {
		retVal, chCtor = fn.client.makeOutChan(ctx, fn.ftyp, fn.valOut)
	}

	req := request{
		Jsonrpc: "2.0",
		ID:      id,
		Method:  fn.name,
		Params:  params{values: args},
	}

	b := backoff{
		maxDelay: methodMaxRetryDelay,
		minDelay: methodMinRetryDelay,
	}

	var ty reflect.Type
	switch {
	case fn.returnValueIsChannel:
		ty = reflectUint64Type
	case fn.valOut == -1:
		ty = reflectyAnyType // this allow [resultValue] to discard any data when unmarsheling
	default:
		ty = fn.ftyp.Out(fn.valOut)
	}
	var err error
	var resp clientResponse
	// keep retrying if got a forced closed websocket conn and calling method
	// has retry annotation
	for attempt := 0; true; attempt++ {
		resp, err = fn.client.sendRequest(ctx, req, chCtor, ty)
		if err != nil {
			return fn.processError(fmt.Errorf("sendRequest failed: %w", err))
		}

		if !fn.notify && resp.ID != req.ID {
			return fn.processError(errors.New("request and response id didn't match"))
		}

		if fn.valOut != -1 && !fn.returnValueIsChannel {
			retVal = func() reflect.Value { return resp.Result.value }
		}
		retry := resp.Error != nil && resp.Error.Code == eTempWSError && fn.retry
		if !retry {
			break
		}

		time.Sleep(b.next(attempt))
	}

	return fn.processResponse(resp, retVal())
}

const (
	ProxyTagRetry     = "retry"
	ProxyTagNotify    = "notify"
	ProxyTagRPCMethod = "rpc_method"
)

func (c *client) makeRpcFunc(f reflect.StructField) (reflect.Value, error) {
	ftyp := f.Type
	if ftyp.Kind() != reflect.Func {
		return reflect.Value{}, errors.New("handler field not a func")
	}

	name := c.methodNameFormatter(c.namespace, f.Name)
	if tag, ok := f.Tag.Lookup(ProxyTagRPCMethod); ok {
		name = tag
	}

	fun := &rpcFunc{
		client: c,
		ftyp:   ftyp,
		name:   name,
		retry:  f.Tag.Get(ProxyTagRetry) == "true",
		notify: f.Tag.Get(ProxyTagNotify) == "true",
	}
	fun.valOut, fun.errOut, fun.nout = processFuncOut(ftyp)

	if fun.valOut != -1 && fun.notify {
		return reflect.Value{}, errors.New("notify methods cannot return values")
	}

	fun.returnValueIsChannel = fun.valOut != -1 && ftyp.Out(fun.valOut).Kind() == reflect.Chan

	if ftyp.NumIn() > 0 && ftyp.In(0) == contextType {
		fun.hasCtx = 1
	}
	// note: hasCtx is also the number of the first non-context argument
	if ftyp.NumIn() > fun.hasCtx && ftyp.In(fun.hasCtx).Implements(isObjectType) {
		if ftyp.NumIn() > fun.hasCtx+1 {
			return reflect.Value{}, errors.New("raw params can't be mixed with other arguments")
		}
		fun.hasObjectParams = true
	}

	return reflect.MakeFunc(ftyp, fun.handleRpcCall), nil
}
