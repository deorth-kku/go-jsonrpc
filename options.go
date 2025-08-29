package jsonrpc

import (
	"context"
	v1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/gorilla/websocket"
)

type clientHandler struct {
	ns  string
	hnd any
}

type Config struct {
	reconnectBackoff backoff
	pingInterval     time.Duration
	timeout          time.Duration

	errors *Errors

	reverseHandlers       []clientHandler
	aliasedHandlerMethods map[string]string

	httpClient *http.Client
	wsDialer   *websocket.Dialer

	noReconnect      bool
	proxyConnFactory func(func() (*websocket.Conn, error)) func() (*websocket.Conn, error) // for testing

	methodNamer MethodNameFormatter
	logger      *slog.Logger
	jsonOptions json.Options
}

func (c Config) getclient(namespace string) client {
	return client{
		namespace:           namespace,
		errors:              c.errors,
		methodNameFormatter: c.methodNamer,
		logger:              c.logger,
		jsonOption:          c.jsonOptions,
	}
}

func (c *Config) handle(context.Context, request, func(func(io.Writer)), rpcErrFunc, func(keepCtx bool), chanOut) {
	c.logger.Error("handleCall on client with no reverse handler")
}

func (c *Config) getMethodHandler(name string) (methodHandler, bool) {
	c.logger.Debug("getMethodHandler on client with no reverse handler")
	return methodHandler{}, false
}

func (c *Config) GetTimeout() time.Duration {
	return c.timeout
}

func (c *Config) GetLogger() *slog.Logger {
	return c.logger
}

func (c *Config) GetJsonOptions() json.Options {
	return c.jsonOptions
}

func (c *Config) GetHTTPClient() *http.Client {
	return c.httpClient
}

func jsonDefault() json.Options {
	// workaround for https://github.com/golang/go/issues/75149
	return json.JoinOptions(
		v1.CallMethodsWithLegacySemantics(true),
		v1.FormatByteArrayAsArray(true),
		v1.FormatBytesWithLegacySemantics(true),
		v1.FormatDurationAsNano(true),
		v1.MatchCaseSensitiveDelimiter(true),
		v1.MergeWithLegacySemantics(true),
		v1.OmitEmptyWithLegacySemantics(true),
		v1.ParseBytesWithLooseRFC4648(true),
		v1.ParseTimeWithLooseRFC3339(true),
		v1.ReportErrorsWithLegacySemantics(true),
		v1.StringifyWithLegacySemantics(true),
		v1.UnmarshalArrayFromAnyLength(true),
		json.Deterministic(true),
		json.FormatNilMapAsNull(true),
		json.FormatNilSliceAsNull(true),
		json.MatchCaseInsensitiveNames(true),
		jsontext.AllowDuplicateNames(true),
		jsontext.AllowInvalidUTF8(true),
		jsontext.EscapeForHTML(true),
		jsontext.EscapeForJS(true),
		jsontext.PreserveRawStrings(true),
	)
}

func defaultConfig() Config {
	return Config{
		reconnectBackoff: backoff{
			minDelay: 100 * time.Millisecond,
			maxDelay: 5 * time.Second,
		},
		pingInterval: 5 * time.Second,
		timeout:      30 * time.Second,

		aliasedHandlerMethods: map[string]string{},

		httpClient: _defaultHTTPClient,

		methodNamer: DefaultMethodNameFormatter,
		logger:      slog.Default(),
		jsonOptions: jsonDefault(),
	}
}

type Option func(c *Config)

func WithReconnectBackoff(minDelay, maxDelay time.Duration) func(c *Config) {
	return func(c *Config) {
		c.reconnectBackoff = backoff{
			minDelay: minDelay,
			maxDelay: maxDelay,
		}
	}
}

// Must be < Timeout/2
func WithPingInterval(d time.Duration) func(c *Config) {
	return func(c *Config) {
		c.pingInterval = d
	}
}

func WithTimeout(d time.Duration) func(c *Config) {
	return func(c *Config) {
		c.timeout = d
	}
}

func WithNoReconnect() func(c *Config) {
	return func(c *Config) {
		c.noReconnect = true
	}
}

type (
	UnmarshalerFunc[T any] = func(*jsontext.Decoder, T) error
	MarshalerFunc[T any]   = func(*jsontext.Encoder, T) error
)

func updateUnmarshalers[T any](opts *json.Options, fn UnmarshalerFunc[T]) {
	unmarshalers, ok := json.GetOption(*opts, json.WithUnmarshalers)
	if ok {
		unmarshalers = json.JoinUnmarshalers(unmarshalers, json.UnmarshalFromFunc(fn))
	} else {
		unmarshalers = json.UnmarshalFromFunc(fn)
	}
	*opts = json.JoinOptions(*opts, json.WithUnmarshalers(unmarshalers))
}

func updateMarshalers[T any](opts *json.Options, fn MarshalerFunc[T]) {
	marshalers, ok := json.GetOption(*opts, json.WithMarshalers)
	if ok {
		marshalers = json.JoinMarshalers(marshalers, json.MarshalToFunc(fn))
	} else {
		marshalers = json.MarshalToFunc(fn)
	}
	*opts = json.JoinOptions(*opts, json.WithMarshalers(marshalers))
}

func WithResultUnmarshaler[T any](fn UnmarshalerFunc[*T]) func(c *Config) {
	return func(c *Config) {
		updateUnmarshalers(&c.jsonOptions, fn)
	}
}

func WithParamMarshaler[T any](fn MarshalerFunc[T]) func(c *Config) {
	return func(c *Config) {
		updateMarshalers(&c.jsonOptions, fn)
	}
}

func WithLogger(logger *slog.Logger) func(c *Config) {
	return func(c *Config) {
		c.logger = logger
	}
}

func updateJsonOptions(opt *json.Options, new []json.Options) {
	if new == nil {
		*opt = nil
	} else {
		new = slices.Insert(new, 0, *opt)
		*opt = json.JoinOptions(new...)
	}
}

func WithJsonOptions(opts ...json.Options) func(c *Config) {
	return func(c *Config) {
		updateJsonOptions(&c.jsonOptions, opts)
	}
}

func WithErrors(es Errors) func(c *Config) {
	return func(c *Config) {
		c.errors = &es
	}
}

func WithClientHandler(ns string, hnd any) func(c *Config) {
	return func(c *Config) {
		c.reverseHandlers = append(c.reverseHandlers, clientHandler{ns, hnd})
	}
}

// WithClientHandlerAlias creates an alias for a client HANDLER method - for handlers created
// with WithClientHandler
func WithClientHandlerAlias(alias, original string) func(c *Config) {
	return func(c *Config) {
		c.aliasedHandlerMethods[alias] = original
	}
}

func WithHTTPClient(h *http.Client) func(c *Config) {
	return func(c *Config) {
		c.httpClient = h
	}
}

func WithWebsocketDialer(d *websocket.Dialer) func(c *Config) {
	return func(c *Config) {
		c.wsDialer = d
	}
}

func WithMethodNameFormatter(namer MethodNameFormatter) func(c *Config) {
	return func(c *Config) {
		c.methodNamer = namer
	}
}

type optionsStore[T any] struct {
	value T
}

const isStored = paramError("stored")

func OptionsStore[T any](opts json.Options, value T) json.Options {
	updateUnmarshalers(&opts, func(e *jsontext.Decoder, cs *optionsStore[T]) error {
		*cs = optionsStore[T]{value}
		e.SkipValue()
		return isStored
	})
	return opts
}

func OptionsLoad[T any](opts json.Options) (T, bool) {
	var store optionsStore[T]
	err := json.Unmarshal([]byte("{}"), &store, opts)
	return store.value, err == isStored
}

// this is sort of black magic, but it allows us to pass context into json unmarshaling
func WithContext(opts json.Options, ctx context.Context) json.Options {
	return OptionsStore(opts, ctx)
}

func ContextFrom(opts json.Options) context.Context {
	ctx, ok := OptionsLoad[context.Context](opts)
	if !ok {
		return context.Background()
	}
	return ctx
}
