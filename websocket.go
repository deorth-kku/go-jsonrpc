package jsonrpc

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/deorth-kku/go-common"
	"github.com/gorilla/websocket"
	"golang.org/x/xerrors"
)

const wsCancel = "xrpc.cancel"
const chValue = "xrpc.ch.val"
const chClose = "xrpc.ch.close"

var debugTrace = os.Getenv("JSONRPC_ENABLE_DEBUG_TRACE") == "1"

type frame struct {
	// common
	Jsonrpc string            `json:"jsonrpc"`
	ID      interface{}       `json:"id,omitzero"`
	Meta    map[string]string `json:"meta,omitzero"`

	// request
	Method string         `json:"method,omitzero"`
	Params jsontext.Value `json:"params,omitzero"`

	// response
	Result resultValue   `json:"result,omitzero"`
	Error  *JSONRPCError `json:"error,omitzero"`
}

var _ json.UnmarshalerFrom = (*frame)(nil)

func (f *frame) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type t0 frame
	err := json.UnmarshalDecode(dec, (*t0)(f))
	if err != nil {
		return err
	}
	f.ID, err = normalizeID(f.ID)
	return err
}

type outChanReg struct {
	reqID interface{}

	chID uint64
	ch   reflect.Value
}

type requestHandler interface {
	handle(ctx context.Context, req request, w func(func(io.Writer)), rpcError rpcErrFunc, done func(keepCtx bool), chOut chanOut)
	GetLogger() *slog.Logger
	GetJsonOptions() json.Options
}

type wsConn struct {
	// outside params
	conn             *websocket.Conn
	connFactory      func() (*websocket.Conn, error)
	reconnectBackoff backoff
	pingInterval     time.Duration
	timeout          time.Duration
	handler          requestHandler
	requests         <-chan clientRequest
	pongs            chan struct{}
	stopPings        func()
	stop             <-chan struct{}
	exiting          chan struct{}

	// incoming messages
	incoming    chan io.Reader
	incomingErr error
	errLk       sync.Mutex

	readError chan error

	frameExecQueue chan frame

	// outgoing messages
	writeLk sync.Mutex

	// ////
	// Client related

	// inflight are requests we've sent to the remote, value type: [clientRequest]
	inflight sync.Map

	// chanHandlers is a map of client-side channel handlers, value type [*chanHandler]
	chanHandlers sync.Map

	// ////
	// Server related

	// handling are the calls we handle, value type: [context.CancelFunc]
	handling sync.Map

	spawnOutChanHandlerOnce sync.Once

	// chanCtr is a counter used for identifying output channels on the server side
	chanCtr uint64

	registerCh chan outChanReg
}

func loadAssert[T any](loadf func(any) (any, bool), key any) (t T, ok bool) {
	v, ok := loadf(key)
	if !ok {
		return t, false
	}
	t, ok = v.(T)
	return
}

func rangeAssertValue[T any](rangef iter.Seq2[any, any]) iter.Seq2[any, T] {
	return func(yield func(any, T) bool) {
		rangef(func(k, v any) bool {
			return yield(k, v.(T))
		})
	}
}

type chanHandler struct {
	// take inside chanHandlersLk
	lk sync.Mutex

	cb func(m []byte, ok bool)
}

//                         //
// WebSocket Message utils //
//                         //

// nextMessage wait for one message and puts it to the incoming channel
func (c *wsConn) nextMessage() {
	c.resetReadDeadline()
	msgType, r, err := c.conn.NextReader()
	if err != nil {
		c.errLk.Lock()
		c.incomingErr = err
		c.errLk.Unlock()
		close(c.incoming)
		return
	}
	if msgType != websocket.BinaryMessage && msgType != websocket.TextMessage {
		c.errLk.Lock()
		c.incomingErr = errors.New("unsupported message type")
		c.errLk.Unlock()
		close(c.incoming)
		return
	}
	c.incoming <- r
}

func (c *wsConn) getlogger() *slog.Logger {
	return c.handler.GetLogger()
}

func (c *wsConn) jsonOptions() json.Options {
	return c.handler.GetJsonOptions()
}

// nextWriter waits for writeLk and invokes the cb callback with WS message
// writer when the lock is acquired
func (c *wsConn) nextWriter(cb func(io.Writer)) {
	c.writeLk.Lock()
	defer c.writeLk.Unlock()

	wcl, err := c.conn.NextWriter(websocket.TextMessage)
	if err != nil {
		c.getlogger().Error("handle me", "err", err)
		return
	}

	cb(wcl)

	if err := wcl.Close(); err != nil {
		c.getlogger().Error("handle me", "err", err)
		return
	}
}

func (c *wsConn) sendRequest(req request) error {
	c.writeLk.Lock()
	defer c.writeLk.Unlock()

	if debugTrace {
		c.getlogger().Debug("sendRequest", "req", req.Method, "id", req.ID)
	}

	if err := c.conn.WriteJSON(req); err != nil {
		return err
	}
	return nil
}

//                 //
// Output channels //
//                 //

// handleOutChans handles channel communication on the server side
// (forwards channel messages to client)
func (c *wsConn) handleOutChans() {
	regV := reflect.ValueOf(c.registerCh)
	exitV := reflect.ValueOf(c.exiting)

	cases := []reflect.SelectCase{
		{ // registration chan always 0
			Dir:  reflect.SelectRecv,
			Chan: regV,
		},
		{ // exit chan always 1
			Dir:  reflect.SelectRecv,
			Chan: exitV,
		},
	}
	internal := len(cases)
	var caseToID []uint64

	for {
		chosen, val, ok := reflect.Select(cases)

		switch chosen {
		case 0: // registration channel
			if !ok {
				// control channel closed - signals closed connection
				// This shouldn't happen, instead the exiting channel should get closed
				c.getlogger().Warn("control channel closed")
				return
			}

			registration := common.MustOk(reflect.TypeAssert[outChanReg](val))

			caseToID = append(caseToID, registration.chID)
			cases = append(cases, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: registration.ch,
			})

			c.nextWriter(func(w io.Writer) {
				resp := &response{
					Jsonrpc: "2.0",
					ID:      registration.reqID,
					Result:  registration.chID,
				}

				if err := json.MarshalWrite(w, resp, c.jsonOptions()); err != nil {
					c.getlogger().Error("failed when writing resp", "err", err)
					return
				}
			})

			continue
		case 1: // exiting channel
			if !ok {
				// exiting channel closed - signals closed connection
				//
				// We're not closing any channels as we're on receiving end.
				// Also, context cancellation below should take care of any running
				// requests
				return
			}
			c.getlogger().Warn("exiting channel received a message")
			continue
		}

		if !ok {
			// Output channel closed, cleanup, and tell remote that this happened

			id := caseToID[chosen-internal]

			n := len(cases) - 1
			if n > 0 {
				cases[chosen] = cases[n]
				caseToID[chosen-internal] = caseToID[n-internal]
			}

			cases = cases[:n]
			caseToID = caseToID[:n-internal]

			rp := common.Must(json.Marshal([]uint64{id}, c.jsonOptions()))

			if err := c.sendRequest(request{
				Jsonrpc: "2.0",
				ID:      nil, // notification
				Method:  chClose,
				Params:  rp,
			}); err != nil {
				c.getlogger().Warn("closed out channel sendRequest failed", "err", err)
			}
			continue
		}

		// forward message
		rp, err := json.Marshal([]any{caseToID[chosen-internal], val.Interface()}, c.jsonOptions())
		if err != nil {
			c.getlogger().Error("marshaling params for sendRequest failed", "err", err)
			continue
		}

		if err := c.sendRequest(request{
			Jsonrpc: "2.0",
			ID:      nil, // notification
			Method:  chValue,
			Params:  rp,
		}); err != nil {
			c.getlogger().Warn("sendRequest failed", "err", err)
			return
		}
	}
}

// handleChanOut registers output channel for forwarding to client
func (c *wsConn) handleChanOut(ch reflect.Value, req interface{}) error {
	c.spawnOutChanHandlerOnce.Do(func() {
		go c.handleOutChans()
	})
	id := atomic.AddUint64(&c.chanCtr, 1)

	select {
	case c.registerCh <- outChanReg{
		reqID: req,

		chID: id,
		ch:   ch,
	}:
		return nil
	case <-c.exiting:
		return xerrors.New("connection closing")
	}
}

//                          //
// Context.Done propagation //
//                          //

// handleCtxAsync handles context lifetimes for client
// TODO: this should be aware of events going through chanHandlers, and quit
//
//	when the related channel is closed.
//	This should also probably be a single goroutine,
//	Note that not doing this should be fine for now as long as we are using
//	contexts correctly (cancelling when async functions are no longer is use)
func (c *wsConn) handleCtxAsync(actx context.Context, id interface{}) {
	<-actx.Done()

	rp, err := json.Marshal([]any{id}, c.jsonOptions())
	if err != nil {
		c.getlogger().Error("marshaling params for sendRequest failed", "err", err)
		return
	}

	if err := c.sendRequest(request{
		Jsonrpc: "2.0",
		Method:  wsCancel,
		Params:  rp,
	}); err != nil {
		c.getlogger().Warn("failed to send request", "method", wsCancel, "id", id, "error", err.Error())
	}
}

// cancelCtx is a built-in rpc which handles context cancellation over rpc
func (c *wsConn) cancelCtx(req frame) {
	if req.ID != nil {
		c.getlogger().Warn(wsCancel + " call with ID set, won't respond")
	}

	var params []any
	if err := json.Unmarshal(req.Params, &params, c.jsonOptions()); err != nil {
		c.getlogger().Error("failed to unmarshal channel", "method", wsCancel, "error", err.Error())
		return
	}
	if len(params) == 0 {
		c.getlogger().Error("no params for "+wsCancel, "reqID", req.ID)
		return
	}

	cf, ok := loadAssert[context.CancelFunc](c.handling.Load, params[0])
	if ok {
		cf()
	} else {
		c.getlogger().Warn(wsCancel+" called with an invalid id", "reqID", req.ID, "params", params)
	}
}

//                     //
// Main Handling logic //
//                     //

func (c *wsConn) handleChanMessage(frame frame) {
	var params []param
	if err := json.Unmarshal(frame.Params, &params, c.jsonOptions()); err != nil {
		c.getlogger().Error("failed to unmarshal channel id in xrpc.ch.val", "reqID", frame.ID, "err", err)
		return
	}

	if len(params) < 2 {
		c.getlogger().Error("not enough params for xrpc.ch.val", "reqID", frame.ID, "params", params)
		return
	}

	var chid uint64
	if err := json.Unmarshal(params[0].data, &chid, c.jsonOptions()); err != nil {
		c.getlogger().Error("failed to unmarshal channel id in xrpc.ch.val", "reqID", frame.ID, "err", err)
		return
	}

	hnd, ok := loadAssert[*chanHandler](c.chanHandlers.Load, chid)
	if !ok {
		c.getlogger().Error("xrpc.ch.val: handler not found", "reqID", frame.ID, "chid", chid)
		return
	}

	hnd.lk.Lock()
	defer hnd.lk.Unlock()

	hnd.cb(params[1].data, true)
}

func (c *wsConn) handleChanClose(frame frame) {
	var params []param
	if err := json.Unmarshal(frame.Params, &params, c.jsonOptions()); err != nil {
		c.getlogger().Error("failed to unmarshal channel id in xrpc.ch.val", "reqID", frame.ID, "err", err)
		return
	}
	if len(params) == 0 {
		c.getlogger().Error("no params for xrpc.ch.val", "reqID", frame.ID)
	}

	var chid uint64
	if err := json.Unmarshal(params[0].data, &chid, c.jsonOptions()); err != nil {
		c.getlogger().Error("failed to unmarshal channel id in xrpc.ch.val", "reqID", frame.ID, "err", err)
		return
	}

	hnd, ok := loadAssert[*chanHandler](c.chanHandlers.LoadAndDelete, chid)
	if !ok {
		c.getlogger().Error("xrpc.ch.val: handler not found", "reqID", frame.ID, "chid", chid)
		return
	}

	hnd.lk.Lock()
	defer hnd.lk.Unlock()

	hnd.cb(nil, false)
}

func (c *wsConn) handleResponse(frame frame) {
	req, ok := loadAssert[clientRequest](c.inflight.Load, frame.ID)
	if !ok {
		c.getlogger().Error("client got unknown ID in response", "reqID", frame.ID)
		return
	}
	defer c.inflight.Delete(frame.ID)

	if err := frame.Result.deferredUnmarshal(req.respType); err != nil {
		req.ready <- clientResponse{
			Jsonrpc: frame.Jsonrpc,
			ID:      frame.ID,
			Error: &JSONRPCError{
				Code:    eTempWSError,
				Message: err.Error(),
			},
		}
	}

	if req.retCh != nil && frame.Result.value.IsValid() {
		// output is channel
		chid, ok := reflect.TypeAssert[uint64](frame.Result.value)
		if !ok {
			c.getlogger().Error("failed when unmarshal channel id response")
		}

		chanCtx, chHnd := req.retCh()

		c.chanHandlers.Store(chid, &chanHandler{cb: chHnd})

		go c.handleCtxAsync(chanCtx, frame.ID)
	}

	req.ready <- clientResponse{
		Jsonrpc: frame.Jsonrpc,
		Result:  frame.Result,
		ID:      frame.ID,
		Error:   frame.Error,
	}
}

func (c *wsConn) handleCall(ctx context.Context, frame frame) {
	req := request{
		Jsonrpc: frame.Jsonrpc,
		ID:      frame.ID,
		Meta:    frame.Meta,
		Method:  frame.Method,
		Params:  frame.Params,
	}

	ctx, cancel := context.WithCancel(ctx)

	nextWriter := func(cb func(io.Writer)) {
		cb(io.Discard)
	}
	done := func(keepCtx bool) {
		if !keepCtx {
			cancel()
		}
	}
	if frame.ID != nil {
		nextWriter = c.nextWriter

		c.handling.Store(frame.ID, cancel)

		done = func(keepctx bool) {
			if !keepctx {
				cancel()
				c.handling.Delete(frame.ID)
			}
		}
	}

	go c.handler.handle(ctx, req, nextWriter, rpcError(c.getlogger(), c.jsonOptions()), done, c.handleChanOut)
}

// handleFrame handles all incoming messages (calls and responses)
func (c *wsConn) handleFrame(ctx context.Context, frame frame) {
	// Get message type by method name:
	// "" - response
	// "xrpc.*" - builtin
	// anything else - incoming remote call
	switch frame.Method {
	case "": // Response to our call
		c.handleResponse(frame)
	case wsCancel:
		c.cancelCtx(frame)
	case chValue:
		c.handleChanMessage(frame)
	case chClose:
		c.handleChanClose(frame)
	default: // Remote call
		c.handleCall(ctx, frame)
	}
}

func (c *wsConn) closeInFlight() {
	for id, req := range rangeAssertValue[clientRequest](c.inflight.Range) {
		req.ready <- clientResponse{
			Jsonrpc: "2.0",
			ID:      id,
			Error: &JSONRPCError{
				Message: "handler: websocket connection closed",
				Code:    eTempWSError,
			},
		}
	}
	c.inflight = sync.Map{}
	for _, cancel := range rangeAssertValue[context.CancelFunc](c.handling.Range) {
		cancel()
	}
	c.handling = sync.Map{}
}

func (c *wsConn) closeChans() {
	for _, hnd := range rangeAssertValue[*chanHandler](c.chanHandlers.Range) {
		hnd.lk.Lock()
		hnd.cb(nil, false)
		hnd.lk.Unlock()
	}
	c.chanHandlers = sync.Map{}
}

func (c *wsConn) setupPings() func() {
	if c.pingInterval == 0 {
		return func() {}
	}

	c.conn.SetPongHandler(func(appData string) error {
		select {
		case c.pongs <- struct{}{}:
		default:
		}
		return nil
	})
	c.conn.SetPingHandler(func(appData string) error {
		// treat pings as pongs - this lets us register server activity even if it's too busy to respond to our pings
		select {
		case c.pongs <- struct{}{}:
		default:
		}
		return nil
	})

	stop := make(chan struct{})

	go func() {
		for {
			select {
			case <-time.After(c.pingInterval):
				c.writeLk.Lock()
				if err := c.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
					c.getlogger().Error("sending ping message", "err", err)
				}
				c.writeLk.Unlock()
			case <-stop:
				return
			}
		}
	}()

	var o sync.Once
	return func() {
		o.Do(func() {
			close(stop)
		})
	}
}

// returns true if reconnected
func (c *wsConn) tryReconnect(ctx context.Context) bool {
	if c.connFactory == nil { // server side
		return false
	}

	// connection dropped unexpectedly, do our best to recover it
	c.closeInFlight()
	c.closeChans()
	c.incoming = make(chan io.Reader) // listen again for responses
	go func() {
		c.stopPings()

		attempts := 0
		var conn *websocket.Conn
		for conn == nil {
			time.Sleep(c.reconnectBackoff.next(attempts))
			if ctx.Err() != nil {
				return
			}
			var err error
			if conn, err = c.connFactory(); err != nil {
				c.getlogger().Debug("websocket connection retry failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
			attempts++
		}

		c.writeLk.Lock()
		c.conn = conn
		c.errLk.Lock()
		c.incomingErr = nil
		c.errLk.Unlock()

		c.stopPings = c.setupPings()

		c.writeLk.Unlock()

		go c.nextMessage()
	}()

	return true
}

func (c *wsConn) readFrame(ctx context.Context, r io.Reader) {
	// debug util - dump all messages to stderr
	// r = io.TeeReader(r, os.Stderr)
	var frame frame
	frame.Result.functy = func() reflect.Type {
		req, ok := loadAssert[clientRequest](c.inflight.Load, frame.ID)
		if !ok {
			return nil
		}
		return req.respType
	}
	// use a autoResetReader in case the read takes a long time
	if err := json.UnmarshalRead(c.autoResetReader(r), &frame, c.jsonOptions()); err != nil {
		c.getlogger().Warn("failed to unmarshal frame", "error", err)
		if frame.ID != nil {
			frame.Error = &JSONRPCError{
				Code:    eTempWSError,
				Message: fmt.Sprintf("RPC client error: unmarshaling frame: %s", err)}
		} else {
			c.readError <- xerrors.Errorf("reading frame into a buffer: %w", err)
			return
		}
	}
	c.frameExecQueue <- frame
	if len(c.frameExecQueue) > 2*cap(c.frameExecQueue)/3 { // warn at 2/3 capacity
		c.getlogger().Warn("frame executor queue is backlogged", "queued", len(c.frameExecQueue), "cap", cap(c.frameExecQueue))
	}

	// got the whole frame, can start reading the next one in background
	go c.nextMessage()
}

func (c *wsConn) frameExecutor(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-c.frameExecQueue:
			c.handleFrame(ctx, frame)
		}
	}
}

var maxQueuedFrames = 256

func (c *wsConn) handleWsConn(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c.incoming = make(chan io.Reader)
	c.readError = make(chan error, 1)
	c.frameExecQueue = make(chan frame, maxQueuedFrames)
	c.pongs = make(chan struct{}, 1)

	c.registerCh = make(chan outChanReg)
	defer close(c.exiting)

	// ////

	// on close, make sure to return from all pending calls, and cancel context
	//  on all calls we handle
	defer c.closeInFlight()
	defer c.closeChans()

	// setup pings

	c.stopPings = c.setupPings()
	defer c.stopPings()

	var timeoutTimer *time.Timer
	if c.timeout != 0 {
		timeoutTimer = time.NewTimer(c.timeout)
		defer timeoutTimer.Stop()
	}

	// start frame executor
	go c.frameExecutor(ctx)

	// wait for the first message
	go c.nextMessage()
	for {
		var timeoutCh <-chan time.Time
		if timeoutTimer != nil {
			if !timeoutTimer.Stop() {
				select {
				case <-timeoutTimer.C:
				default:
				}
			}
			timeoutTimer.Reset(c.timeout)

			timeoutCh = timeoutTimer.C
		}

		start := time.Now()
		action := ""

		select {
		case r, ok := <-c.incoming:
			action = "incoming"
			c.errLk.Lock()
			err := c.incomingErr
			c.errLk.Unlock()

			if ok {
				go c.readFrame(ctx, r)
				break
			}

			if err == nil {
				return // remote closed
			}

			c.getlogger().Debug("websocket error", "error", err, "lastAction", action, "time", time.Since(start))
			// only client needs to reconnect
			if !c.tryReconnect(ctx) {
				return // failed to reconnect
			}
		case rerr := <-c.readError:
			action = "read-error"

			c.getlogger().Debug("websocket error", "error", rerr, "lastAction", action, "time", time.Since(start))
			if !c.tryReconnect(ctx) {
				return // failed to reconnect
			}
		case <-ctx.Done():
			c.getlogger().Debug("context cancelled", "error", ctx.Err(), "lastAction", action, "time", time.Since(start))
			return
		case req := <-c.requests:
			action = fmt.Sprintf("send-request(%s,%v)", req.req.Method, req.req.ID)

			c.writeLk.Lock()
			if req.req.ID != nil { // non-notification
				c.errLk.Lock()
				hasErr := c.incomingErr != nil
				c.errLk.Unlock()
				if hasErr { // No conn?, immediate fail
					req.ready <- clientResponse{
						Jsonrpc: "2.0",
						ID:      req.req.ID,
						Error: &JSONRPCError{
							Message: "handler: websocket connection closed",
							Code:    eTempWSError,
						},
					}
					c.writeLk.Unlock()
					break
				}
				c.inflight.Store(req.req.ID, req)
			}
			c.writeLk.Unlock()
			serr := c.sendRequest(req.req)
			if serr != nil {
				c.getlogger().Error("sendReqest failed (Handle me)", "err", serr)
			}
			if req.req.ID == nil { // notification, return immediately
				resp := clientResponse{
					Jsonrpc: "2.0",
				}
				if serr != nil {
					resp.Error = &JSONRPCError{
						Code:    eTempWSError,
						Message: fmt.Sprintf("sendRequest: %s", serr),
					}
				}
				req.ready <- resp
			}

		case <-c.pongs:
			action = "pong"

			c.resetReadDeadline()
		case <-timeoutCh:
			if c.pingInterval == 0 {
				// pings not running, this is perfectly normal
				continue
			}

			c.writeLk.Lock()
			if err := c.conn.Close(); err != nil {
				c.getlogger().Warn("timed-out websocket close error", "error", err)
			}
			c.writeLk.Unlock()
			c.getlogger().Error("Connection timeout", "remote", c.conn.RemoteAddr(), "lastAction", action)
			// The server side does not perform the reconnect operation, so need to exit
			if c.connFactory == nil {
				return
			}
			// The client performs the reconnect operation, and if it exits it cannot start a handleWsConn again, so it does not need to exit
			continue
		case <-c.stop:
			c.writeLk.Lock()
			cmsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
			if err := c.conn.WriteMessage(websocket.CloseMessage, cmsg); err != nil {
				c.getlogger().Warn("failed to write close message", "err", err)
			}
			if err := c.conn.Close(); err != nil {
				c.getlogger().Warn("websocket close error", "error", err)
			}
			c.writeLk.Unlock()
			return
		}

		if c.pingInterval > 0 && time.Since(start) > c.pingInterval*2 {
			c.getlogger().Warn("websocket long time no response", "lastAction", action, "time", time.Since(start))
		}
		if debugTrace {
			c.getlogger().Debug("websocket action", "lastAction", action, "time", time.Since(start))
		}
	}
}

var onReadDeadlineResetInterval = 5 * time.Second

// autoResetReader wraps a reader and resets the read deadline on if needed when doing large reads.
func (c *wsConn) autoResetReader(reader io.Reader) io.Reader {
	return &deadlineResetReader{
		r:         reader,
		reset:     c.resetReadDeadline,
		logger:    c.getlogger(),
		lastReset: time.Now(),
	}
}

type deadlineResetReader struct {
	r         io.Reader
	reset     func()
	logger    *slog.Logger
	lastReset time.Time
}

func (r *deadlineResetReader) Read(p []byte) (n int, err error) {
	n, err = r.r.Read(p)
	if time.Since(r.lastReset) > onReadDeadlineResetInterval {
		r.logger.Warn("slow/large read, resetting deadline while reading the frame", "since", time.Since(r.lastReset), "n", n, "err", err, "p", len(p))

		r.reset()
		r.lastReset = time.Now()
	}
	return
}

func (c *wsConn) resetReadDeadline() {
	if c.timeout > 0 {
		if err := c.conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
			c.getlogger().Error("setting read deadline", "err", err)
		}
	}
}

// Takes an ID as received on the wire, validates it, and translates it to a
// normalized ID appropriate for keying.
func normalizeID(id interface{}) (interface{}, error) {
	switch v := id.(type) {
	case string, float64, nil:
		return v, nil
	case int64: // clients sending int64 need to normalize to float64
		return float64(v), nil
	default:
		return nil, xerrors.Errorf("invalid id type: %T", id)
	}
}
