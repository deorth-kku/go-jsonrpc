package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"runtime"
)

type RawParams jsontext.Value

var rtRawParams = reflect.TypeFor[RawParams]()

// todo is there a better way to tell 'struct with any number of fields'?
func DecodeParams[T any](p RawParams, opts ...json.Options) (T, error) {
	var t T
	err := json.Unmarshal(p, &t, opts...)

	// todo also handle list-encoding automagically (json.Unmarshal doesn't do that, does it?)

	return t, err
}

// methodHandler is a handler for a single method
type methodHandler struct {
	paramReceivers []reflect.Type
	nParams        int

	receiver    reflect.Value
	handlerFunc reflect.Value

	hasCtx       int
	hasRawParams bool

	errOut int
	valOut int
}

// Request / response

type request struct {
	Jsonrpc string            `json:"jsonrpc"`
	ID      any               `json:"id,omitempty"`
	Method  string            `json:"method"`
	Params  jsontext.Value    `json:"params"`
	Meta    map[string]string `json:"meta,omitempty"`
}

var _ json.UnmarshalerFrom = (*request)(nil)

func (f *request) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type t0 request
	err := json.UnmarshalDecode(dec, (*t0)(f))
	if err != nil {
		return err
	}
	f.ID, err = normalizeID(f.ID)
	return err
}

// Limit request size. Ideally this limit should be specific for each field
// in the JSON request but as a simple defensive measure we just limit the
// entire HTTP body.
// Configured by WithMaxRequestSize.
const DEFAULT_MAX_REQUEST_SIZE = 100 << 20 // 100 MiB

type handler struct {
	methods map[string]methodHandler
	errors  *Errors

	maxRequestSize int64

	// aliasedMethods contains a map of alias:original method names.
	// These are used as fallbacks if a method is not found by the given method name.
	aliasedMethods map[string]string

	methodNameFormatter MethodNameFormatter
	logger              *slog.Logger
	jsonOptions         json.Options

	tracer Tracer
}

func (h *handler) GetLogger() *slog.Logger {
	return h.logger
}

func (h *handler) GetJsonOptions() json.Options {
	return h.jsonOptions
}

type (
	TracerBytesFunc   = func(data []byte, err error)
	TracerRequestFunc = func(jsonrpc string, id any, method string, params jsontext.Value, meta map[string]string, err error)
	TracerParamsFunc  = func(method string, params []reflect.Value, results []reflect.Value, err error)
)

type Tracer struct {
	OnParseError     TracerBytesFunc
	OnInvalidRequest TracerBytesFunc
	OnInvalidMethod  TracerRequestFunc
	OnInvalidParams  TracerRequestFunc
	OnRequestError   TracerParamsFunc
	OnResponseError  TracerParamsFunc
	OnNotification   TracerParamsFunc
	OnSuccess        TracerParamsFunc
}

func safecall2[Q, E any](f func(Q, E), arg0 Q, arg1 E) {
	if f == nil {
		return
	}
	f(arg0, arg1)
}

func safecall4[Q, W, E, R any](f func(Q, W, E, R), arg0 Q, arg1 W, arg2 E, arg3 R) {
	if f == nil {
		return
	}
	f(arg0, arg1, arg2, arg3)
}

func safecall6[Q, W, E, R, T, Y any](f func(Q, W, E, R, T, Y), arg0 Q, arg1 W, arg2 E, arg3 R, arg4 T, arg5 Y) {
	if f == nil {
		return
	}
	f(arg0, arg1, arg2, arg3, arg4, arg5)
}

func makeHandler(sc ServerConfig) *handler {
	return &handler{
		methods: make(map[string]methodHandler),
		errors:  sc.errors,

		aliasedMethods: map[string]string{},

		methodNameFormatter: sc.methodNameFormatter,
		logger:              sc.logger,
		jsonOptions:         sc.jsonOptions,

		maxRequestSize: sc.maxRequestSize,

		tracer: sc.tracer,
	}
}

// Register

func (s *handler) register(namespace string, r any) {
	val := reflect.ValueOf(r)
	// TODO: expect ptr

	for i := 0; i < val.NumMethod(); i++ {
		method := val.Type().Method(i)

		funcType := method.Func.Type()
		hasCtx := 0
		if funcType.NumIn() >= 2 && funcType.In(1) == contextType {
			hasCtx = 1
		}

		hasRawParams := false
		ins := funcType.NumIn() - 1 - hasCtx
		recvs := make([]reflect.Type, ins)
		for i := 0; i < ins; i++ {
			if hasRawParams && i > 0 {
				panic("raw params must be the last parameter")
			}
			if funcType.In(i+1+hasCtx) == rtRawParams {
				hasRawParams = true
			}
			recvs[i] = method.Type.In(i + 1 + hasCtx)
		}

		valOut, errOut, _ := processFuncOut(funcType)

		s.methods[s.methodNameFormatter(namespace, method.Name)] = methodHandler{
			paramReceivers: recvs,
			nParams:        ins,

			handlerFunc: method.Func,
			receiver:    val,

			hasCtx:       hasCtx,
			hasRawParams: hasRawParams,

			errOut: errOut,
			valOut: valOut,
		}
	}
}

// Handle

type rpcErrFunc = func(w func(func(io.Writer)), req *request, code ErrorCode, err error)
type chanOut = func(reflect.Value, any) error

func (s *handler) handleReader(ctx context.Context, r io.Reader, w io.Writer, rpcError rpcErrFunc) {
	wf := func(cb func(io.Writer)) {
		cb(w)
	}

	// We read the entire request upfront in a buffer to be able to tell if the
	// client sent more than maxRequestSize and report it back as an explicit error,
	// instead of just silently truncating it and reporting a more vague parsing
	// error.
	bufferedRequest := new(bytes.Buffer)
	// We use LimitReader to enforce maxRequestSize. Since it won't return an
	// EOF we can't actually know if the client sent more than the maximum or
	// not, so we read one byte more over the limit to explicitly query that.
	// FIXME: Maybe there's a cleaner way to do this.
	reqSize, err := bufferedRequest.ReadFrom(io.LimitReader(r, s.maxRequestSize+1))
	if err != nil {
		// ReadFrom will discard EOF so any error here is unexpected and should
		// be reported.
		err = fmt.Errorf("reading request: %w", err)
		rpcError(wf, nil, rpcParseError, err)
		safecall2(s.tracer.OnParseError, bufferedRequest.Bytes(), err)
		return
	}
	if reqSize > s.maxRequestSize {
		err = fmt.Errorf("request bigger than maximum %d allowed", s.maxRequestSize)
		rpcError(wf, nil, rpcParseError, err)
		// rpcParseError is the closest we have from the standard errors defined
		// in [jsonrpc spec](https://www.jsonrpc.org/specification#error_object)
		// to report the maximum limit.
		safecall2(s.tracer.OnParseError, bufferedRequest.Bytes(), err)
		return
	}

	// Trim spaces to avoid issues with batch request detection.
	bufferedRequest = bytes.NewBuffer(bytes.TrimSpace(bufferedRequest.Bytes()))
	reqSize = int64(bufferedRequest.Len())

	if reqSize == 0 {
		err = errors.New("invalid request")
		rpcError(wf, nil, rpcInvalidRequest, err)
		safecall2(s.tracer.OnInvalidRequest, bufferedRequest.Bytes(), err)
		return
	}

	if bufferedRequest.Bytes()[0] == '[' && bufferedRequest.Bytes()[reqSize-1] == ']' {
		var reqs []request

		if err := json.UnmarshalRead(bufferedRequest, &reqs, s.jsonOptions); err != nil {
			rpcError(wf, nil, rpcParseError, errors.New("parse error"))
			safecall2(s.tracer.OnParseError, bufferedRequest.Bytes(), err)
			return
		}

		if len(reqs) == 0 {
			err = errors.New("invalid request")
			rpcError(wf, nil, rpcInvalidRequest, err)
			safecall2(s.tracer.OnInvalidRequest, bufferedRequest.Bytes(), err)
			return
		}

		_, _ = w.Write([]byte("[")) // todo consider handling this error
		for idx, req := range reqs {
			s.handle(ctx, req, wf, rpcError, func(bool) {}, nil)

			if idx != len(reqs)-1 {
				_, _ = w.Write([]byte(",")) // todo consider handling this error
			}
		}
		_, _ = w.Write([]byte("]")) // todo consider handling this error
	} else {
		var req request
		if err := json.UnmarshalRead(bufferedRequest, &req, s.jsonOptions); err != nil {
			rpcError(wf, &req, rpcParseError, errors.New("parse error"))
			safecall2(s.tracer.OnParseError, bufferedRequest.Bytes(), err)
			return
		}
		s.handle(ctx, req, wf, rpcError, func(bool) {}, nil)
	}
}

type stackstring struct{}

func (stackstring) LogValue() slog.Value {
	const bufsize = 4096
	buf := make([]byte, bufsize)
	l := runtime.Stack(buf, false)
	buf = buf[:l]
	return slog.StringValue(string(buf))
}

var _ slog.LogValuer = stackstring{}

func (s *handler) doCall(methodName string, f reflect.Value, params []reflect.Value) (out []reflect.Value, err error) {
	defer func() {
		if i := recover(); i != nil {
			err = fmt.Errorf("panic in rpc method '%s': %s", methodName, i)
			s.logger.Error("panic in rpc method", "method_name", methodName, "stack", stackstring{}, "err", err)
		}
	}()

	out = f.Call(params)
	return
}

func (s *handler) createError(err error) *JSONRPCError {
	var code ErrorCode = 1
	if s.errors != nil {
		c, ok := s.errors.byType[reflect.TypeOf(err)]
		if ok {
			code = c
		}
	}

	out := &JSONRPCError{
		Code:    code,
		Message: err.Error(),
	}

	switch m := err.(type) {
	case RPCErrorCodec:
		o, err := m.ToJSONRPCError()
		if err != nil {
			s.logger.Error("Failed to convert error to JSONRPCError", "err", err)
		} else {
			out = &o
		}
	case marshalable:
		meta, marshalErr := m.MarshalJSON()
		if marshalErr == nil {
			out.Meta = meta
		} else {
			s.logger.Error("Failed to marshal error metadata", "err", marshalErr)
		}
	}

	return out
}

func (s *handler) handle(ctx context.Context, req request, w func(func(io.Writer)), rpcError rpcErrFunc, done func(keepCtx bool), chOut chanOut) {
	// Not sure if we need to sanitize the incoming req.Method or not.
	handler, ok := s.methods[req.Method]
	if !ok {
		aliasTo, ok := s.aliasedMethods[req.Method]
		if ok {
			handler, ok = s.methods[aliasTo]
		}
		if !ok {
			err := fmt.Errorf("method '%s' not found", req.Method)
			rpcError(w, &req, rpcMethodNotFound, err)
			safecall6(s.tracer.OnInvalidMethod, req.Jsonrpc, req.ID, req.Method, req.Params, req.Meta, err)
			done(false)
			return
		}
	}

	outCh := handler.valOut != -1 && handler.handlerFunc.Type().Out(handler.valOut).Kind() == reflect.Chan
	defer done(outCh)

	if chOut == nil && outCh {
		err := fmt.Errorf("method '%s' not supported in this mode (no out channel support)", req.Method)
		rpcError(w, &req, rpcMethodNotFound, err)
		safecall6(s.tracer.OnInvalidMethod, req.Jsonrpc, req.ID, req.Method, req.Params, req.Meta, err)
		return
	}

	callParams := make([]reflect.Value, 1+handler.hasCtx+handler.nParams)
	callParams[0] = handler.receiver
	if handler.hasCtx == 1 {
		callParams[1] = reflect.ValueOf(ctx)
	}

	if handler.hasRawParams {
		// When hasRawParams is true, there is only one parameter and it is a
		// json.RawMessage.

		callParams[1+handler.hasCtx] = reflect.ValueOf(RawParams(req.Params))
	} else {
		// "normal" param list; no good way to do named params in Golang

		var ps []param
		if len(req.Params) > 0 {
			err := json.Unmarshal(req.Params, &ps, s.jsonOptions)
			if err != nil {
				rpcError(w, &req, rpcParseError, fmt.Errorf("unmarshaling param array: %w", err))
				safecall6(s.tracer.OnInvalidParams, req.Jsonrpc, req.ID, req.Method, req.Params, req.Meta, err)
				return
			}
		}

		if len(ps) != handler.nParams {
			err := fmt.Errorf("wrong param count (method '%s'): %d != %d", req.Method, len(ps), handler.nParams)
			rpcError(w, &req, rpcInvalidParams, err)
			safecall6(s.tracer.OnInvalidParams, req.Jsonrpc, req.ID, req.Method, req.Params, req.Meta, err)
			done(false)
			return
		}

		for i := 0; i < handler.nParams; i++ {
			var rp reflect.Value

			typ := handler.paramReceivers[i]
			rp = reflect.New(typ)
			if err := json.Unmarshal(ps[i].data, rp.Interface(), s.jsonOptions); err != nil {
				rpcError(w, &req, rpcParseError, fmt.Errorf("unmarshaling params for '%s' (param: %T): %w", req.Method, rp.Interface(), err))
				safecall6(s.tracer.OnInvalidParams, req.Jsonrpc, req.ID, req.Method, req.Params, req.Meta, err)
				return
			}
			callParams[i+1+handler.hasCtx] = rp.Elem()
		}
	}

	// /////////////////

	callResult, err := s.doCall(req.Method, handler.handlerFunc, callParams)
	if err != nil {
		rpcError(w, &req, 0, fmt.Errorf("fatal error calling '%s': %w", req.Method, err))
		safecall4(s.tracer.OnRequestError, req.Method, callParams, callResult, err)
		return
	}
	if req.ID == nil {
		safecall4(s.tracer.OnNotification, req.Method, callParams, callResult, err)
		return // notification
	}

	safecall4(s.tracer.OnSuccess, req.Method, callParams, callResult, err)
	// /////////////////

	resp := response{
		Jsonrpc: "2.0",
		ID:      req.ID,
	}

	if handler.errOut != -1 {
		err, ok := reflect.TypeAssert[error](callResult[handler.errOut])
		if ok {
			s.logger.Warn("error in RPC call", "method", req.Method, "err", err)
			safecall4(s.tracer.OnRequestError, req.Method, callParams, callResult, err)
			resp.Error = s.createError(err)
		}
	}

	var kind reflect.Kind
	var res any
	var nonZero bool
	if handler.valOut != -1 {
		res = callResult[handler.valOut].Interface()
		kind = callResult[handler.valOut].Kind()
		nonZero = !callResult[handler.valOut].IsZero()
	}

	// check error as JSON-RPC spec prohibits error and value at the same time
	if resp.Error == nil {
		if res != nil && kind == reflect.Chan {
			// Channel responses are sent from channel control goroutine.
			// Sending responses here could cause deadlocks on writeLk, or allow
			// sending channel messages before this rpc call returns

			//noinspection GoNilness // already checked above
			err = chOut(callResult[handler.valOut], req.ID)
			if err == nil {
				return // channel goroutine handles responding
			}

			s.logger.Warn("failed to setup channel in RPC call", "method", req.Method, "err", err)
			safecall4(s.tracer.OnRequestError, req.Method, callParams, callResult, err)

			resp.Error = &JSONRPCError{
				Code:    1,
				Message: err.Error(),
			}
		} else {
			resp.Result = res
		}
	}
	if resp.Error != nil && nonZero {
		s.logger.Error("error and res returned", "request", req, "r.err", resp.Error, "res", res)
	}

	withLazyWriter(w, func(w io.Writer) {
		if err := json.MarshalWrite(w, resp, s.jsonOptions); err != nil {
			s.logger.Error("failed when writing resp", "err", err)
			safecall4(s.tracer.OnResponseError, req.Method, callParams, callResult, err)
			return
		}
	})
}

// withLazyWriter makes it possible to defer acquiring a writer until the first write.
// This is useful because json.Encode needs to marshal the response fully before writing, which may be
// a problem for very large responses.
func withLazyWriter(withWriterFunc func(func(io.Writer)), cb func(io.Writer)) {
	lw := &lazyWriter{
		withWriterFunc: withWriterFunc,

		done: make(chan struct{}),
	}

	defer close(lw.done)
	cb(lw)
}

type lazyWriter struct {
	withWriterFunc func(func(io.Writer))

	w    io.Writer
	done chan struct{}
}

func (lw *lazyWriter) Write(p []byte) (n int, err error) {
	if lw.w == nil {
		acquired := make(chan struct{})
		go func() {
			lw.withWriterFunc(func(w io.Writer) {
				lw.w = w
				close(acquired)
				<-lw.done
			})
		}()
		<-acquired
	}

	return lw.w.Write(p)
}
