package jsonrpc

import (
	"context"
	v1 "encoding/json"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"reflect"
	"time"
)

// note: we embed reflect.Type because proxy-structs are not comparable
type jsonrpcReverseClient struct{ reflect.Type }

type ParamDecoder func(ctx context.Context, json []byte) (reflect.Value, error)

type ServerConfig struct {
	maxRequestSize int64
	pingInterval   time.Duration

	errors *Errors

	reverseClientBuilder func(context.Context, *wsConn) (context.Context, error)
	tracer               Tracer
	methodNameFormatter  MethodNameFormatter
	logger               *slog.Logger
	jsonOptions          json.Options
}

type ServerOption func(c *ServerConfig)

func defaultServerConfig() ServerConfig {
	return ServerConfig{
		maxRequestSize: DEFAULT_MAX_REQUEST_SIZE,

		pingInterval:        5 * time.Second,
		methodNameFormatter: DefaultMethodNameFormatter,
		logger:              slog.Default(),
		jsonOptions:         v1.DefaultOptionsV1(),
	}
}

func (c *ServerConfig) GetLogger() *slog.Logger {
	return c.logger
}

func (c *ServerConfig) GetJsonOptions() json.Options {
	return c.jsonOptions
}

func WithResultMarshaler[T any](fn MarshalerFunc[T]) func(c *ServerConfig) {
	return func(c *ServerConfig) {
		updateMarshalers(&c.jsonOptions, fn)
	}
}

func WithParamUnmarshaler[T any](fn UnmarshalerFunc[*T]) func(c *ServerConfig) {
	return func(c *ServerConfig) {
		updateUnmarshalers(&c.jsonOptions, fn)
	}
}

func WithServerLogger(logger *slog.Logger) ServerOption {
	return func(c *ServerConfig) {
		c.logger = logger
	}
}

func WithServerJsonOptions(opts ...json.Options) ServerOption {
	return func(c *ServerConfig) {
		updateJsonOptions(&c.jsonOptions, opts)
	}
}

func WithMaxRequestSize(max int64) ServerOption {
	return func(c *ServerConfig) {
		c.maxRequestSize = max
	}
}

func WithServerErrors(es Errors) ServerOption {
	return func(c *ServerConfig) {
		c.errors = &es
	}
}

func WithServerPingInterval(d time.Duration) ServerOption {
	return func(c *ServerConfig) {
		c.pingInterval = d
	}
}

func WithServerMethodNameFormatter(formatter MethodNameFormatter) ServerOption {
	return func(c *ServerConfig) {
		c.methodNameFormatter = formatter
	}
}

// WithTracer allows the instantiator to trace the method calls and results.
// This is useful for debugging a client-server interaction.
func WithTracer(l Tracer) ServerOption {
	return func(c *ServerConfig) {
		c.tracer = l
	}
}

// WithReverseClient will allow extracting reverse client on **WEBSOCKET** calls.
// RP is a proxy-struct type, much like the one passed to NewClient.
func WithReverseClient[RP any](namespace string) ServerOption {
	return func(c *ServerConfig) {
		c.reverseClientBuilder = func(ctx context.Context, conn *wsConn) (context.Context, error) {
			cl := client{
				namespace:           namespace,
				methodNameFormatter: c.methodNameFormatter,
			}

			// todo test that everything is closing correctly
			cl.exiting = conn.exiting

			requests := cl.setupRequestChan()
			conn.requests = requests

			calls := new(RP)

			err := cl.provide([]interface{}{
				calls,
			})
			if err != nil {
				return nil, fmt.Errorf("provide reverse client calls: %w", err)
			}

			return context.WithValue(ctx, jsonrpcReverseClient{reflect.TypeOf(calls).Elem()}, calls), nil
		}
	}
}

// ExtractReverseClient will extract reverse client from context. Reverse client for the type
// will only be present if the server was constructed with a matching WithReverseClient option
// and the connection was a websocket connection.
// If there is no reverse client, the call will return a zero value and `false`. Otherwise a reverse
// client and `true` will be returned.
func ExtractReverseClient[C any](ctx context.Context) (C, bool) {
	c, ok := ctx.Value(jsonrpcReverseClient{reflect.TypeFor[C]()}).(*C)
	if !ok {
		return *new(C), false
	}
	if c == nil {
		// something is very wrong, but don't panic
		return *new(C), false
	}

	return *c, ok
}
