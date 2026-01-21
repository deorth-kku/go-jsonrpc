package jsonrpc

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"

	"github.com/deorth-kku/go-common"
)

// Object is my solution for named params in golang.
// Use an Object[T] type as the only param (except for context) in method, then its corresponded json object will be for "params" field in rpc call.
// T must be a Go struct, jsontext.Value, map[~string]T, or an unnamed pointer to such types.
// You can use Object[jsontext.Value] to get the usage similar to the old RawParams type.
type Object[T any] struct {
	Value T `json:",inline"`
}

func (Object[T]) isObject() {}

func GetObject[T any](v T) Object[T] {
	return Object[T]{
		Value: v,
	}
}

type isObject interface {
	isObject()
}

var (
	_            isObject = (Object[struct{}]{})
	isObjectType          = reflect.TypeFor[isObject]()
)

// methodHandler is a handler for a single method
type methodHandler struct {
	paramReceivers []reflect.Type
	nParams        int

	receiver    reflect.Value
	handlerFunc reflect.Value

	hasCtx          int
	hasObjectParams bool
	isVariadic      bool

	errOut int
	valOut int
}

// Request / response

type request struct {
	Jsonrpc string            `json:"jsonrpc"`
	ID      any               `json:"id,omitempty"`
	Method  string            `json:"method"`
	Params  params            `json:"params"`
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

	for i := range val.NumMethod() {
		method := val.Type().Method(i)

		funcType := method.Func.Type()
		hasCtx := 0
		if funcType.NumIn() >= 2 && funcType.In(1) == contextType {
			hasCtx = 1
		}

		hasObjectParams := false
		ins := funcType.NumIn() - 1 - hasCtx
		recvs := make([]reflect.Type, ins)
		for i := range ins {
			if hasObjectParams && i > 0 {
				panic("object params must be the last parameter")
			}
			if funcType.In(i + 1 + hasCtx).Implements(isObjectType) {
				hasObjectParams = true
			}
			recvs[i] = method.Type.In(i + 1 + hasCtx)
		}

		valOut, errOut, _ := processFuncOut(funcType)

		s.methods[s.methodNameFormatter(namespace, method.Name)] = methodHandler{
			paramReceivers: recvs,
			nParams:        ins,

			handlerFunc: method.Func,
			receiver:    val,

			hasCtx:          hasCtx,
			hasObjectParams: hasObjectParams,
			isVariadic:      funcType.IsVariadic(),

			errOut: errOut,
			valOut: valOut,
		}
	}
}

// Handle

type rpcErrFunc = func(w func(arg any) error, req *request, code ErrorCode, err error)
type chanOut = func(reflect.Value, any) error

func (s *handler) handleReader(ctx context.Context, r io.Reader, w io.Writer, rpcError rpcErrFunc) {
	if debugTrace {
		tv := NewTeeLogValue(r)
		r = tv.r
		defer s.logger.Debug("debugTrace request string", "json", tv)
	}
	jopts := WithContext(s.jsonOptions, ctx)
	dec := jsontext.NewDecoder(LimitReader(r, s.maxRequestSize), jopts)
	enc := jsontext.NewEncoder(w, jopts)
	wf := func(arg any) error {
		return json.MarshalEncode(enc, arg)
	}
	if dec.PeekKind() == '[' {
		dec.ReadToken()
		enc.WriteToken(jsontext.BeginArray)
		defer enc.WriteToken(jsontext.EndArray)
		for dec.PeekKind() != ']' {
			if s.do1req(ctx, dec, wf, rpcError) {
				break
			}
		}
		dec.ReadToken()
	} else {
		s.do1req(ctx, dec, wf, rpcError)
	}
}

func (s *handler) do1req(ctx context.Context, dec *jsontext.Decoder, wf func(any) error, rpcError rpcErrFunc) bool {
	var req request
	req.Params.getMethodHandler = func() (methodHandler, bool) { return s.getMethodHandler(req.Method) }
	err := json.UnmarshalDecode(dec, &req)
	if err != nil {
		var perr paramError
		if errors.As(err, &perr) {
			rpcError(wf, &req, rpcInvalidParams, perr)
			// since we are using stream reading, we no longer have the full bytes represent of the params
			// use dec.UnreadBuffer as it may help the Tracer to decide what to do
			safecall6(s.tracer.OnInvalidParams, req.Jsonrpc, req.ID, req.Method, dec.UnreadBuffer(), req.Meta, error(perr))
			return true
		} else {
			rpcError(wf, &req, rpcParseError, err)
			// same reason as above
			safecall2(s.tracer.OnParseError, dec.UnreadBuffer(), err)
			return true
		}
	}
	s.handle(ctx, req, wf, rpcError, func(bool) {}, nil)
	return false
}

func (s *handler) doCall(methodName string, f reflect.Value, params []reflect.Value) (out []reflect.Value, err error) {
	defer func() {
		if i := recover(); i != nil {
			err = fmt.Errorf("panic in rpc method '%s': %s", methodName, i)
			s.logger.Error("panic in rpc method", "method_name", methodName, "stack", stackstring{}, "err", err)
		}
	}()
	meth, _ := s.getMethodHandler(methodName)
	if meth.isVariadic {
		out = f.CallSlice(params)
	} else {
		out = f.Call(params)
	}
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

	if m, ok := err.(RPCErrorCodec); ok {
		o, err := m.ToJSONRPCError()
		if err != nil {
			s.logger.Error("Failed to convert error to JSONRPCError", "err", err)
		} else {
			out = &o
		}
	} else {
		meta, marshalErr := json.Marshal(err, s.jsonOptions)
		if marshalErr == nil {
			out.Meta = meta
		} else {
			s.logger.Error("Failed to marshal error metadata", "err", marshalErr)
		}
	}
	return out
}

func (s *handler) getMethodHandler(name string) (methodHandler, bool) {
	handler, ok := s.methods[name]
	if !ok {
		aliasTo, ok := s.aliasedMethods[name]
		if ok {
			handler, ok = s.methods[aliasTo]
			return handler, ok
		}
	}
	return handler, ok
}

func (s *handler) handle(ctx context.Context, req request, w func(any) error, rpcError rpcErrFunc, done func(keepCtx bool), chOut chanOut) {
	// Not sure if we need to sanitize the incoming req.Method or not.
	handler, ok := s.getMethodHandler(req.Method)
	if !ok {
		err := fmt.Errorf("method '%s' not found", req.Method)
		rpcError(w, &req, rpcMethodNotFound, err)
		safecall6(s.tracer.OnInvalidMethod, req.Jsonrpc, req.ID, req.Method, common.Drop1(req.Params.getdeferredData()).data, req.Meta, err)
		done(false)
		return
	}
	data, err := req.Params.deferredUnmarshal(handler)
	if err != nil {
		var perr paramError
		if errors.As(err, &perr) {
			rpcError(w, &req, rpcInvalidParams, err)
			safecall6(s.tracer.OnInvalidParams, req.Jsonrpc, req.ID, req.Method, data, req.Meta, err)
		} else {
			rpcError(w, &req, rpcParseError, err)
			safecall2(s.tracer.OnParseError, data, err)
		}
		return
	}

	outCh := handler.valOut != -1 && handler.handlerFunc.Type().Out(handler.valOut).Kind() == reflect.Chan
	defer done(outCh)

	if chOut == nil && outCh {
		err := fmt.Errorf("method '%s' not supported in this mode (no out channel support)", req.Method)
		rpcError(w, &req, rpcMethodNotFound, err)
		// I could just marshal the params to get the bytes represent but that will cost us some performance
		safecall6(s.tracer.OnInvalidMethod, req.Jsonrpc, req.ID, req.Method, nil, req.Meta, err)
		return
	}

	callParams := make([]reflect.Value, 1+handler.hasCtx)
	callParams[0] = handler.receiver
	if handler.hasCtx == 1 {
		callParams[1] = reflect.ValueOf(ctx)
	}
	callParams = append(callParams, req.Params.values...)

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

	if err = w(resp); err != nil {
		s.logger.Error("failed when writing resp", "err", err)
		safecall4(s.tracer.OnResponseError, req.Method, callParams, callResult, err)
		return
	}
}
