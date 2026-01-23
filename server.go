package jsonrpc

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

// ConnectionType indicates the type of connection, this is set in the context and can be retrieved
// with GetConnectionType.
type ConnectionType string

const (
	// ConnectionTypeUnknown indicates that the connection type cannot be determined, likely because
	// it hasn't passed through an RPCServer.
	ConnectionTypeUnknown ConnectionType = "unknown"
	// ConnectionTypeHTTP indicates that the connection is an HTTP connection.
	ConnectionTypeHTTP ConnectionType = "http"
	// ConnectionTypeWS indicates that the connection is a WebSockets connection.
	ConnectionTypeWS ConnectionType = "websockets"
)

type connectionTypeKey struct{}

var connectionTypeCtxKey = connectionTypeKey{}

// GetConnectionType returns the connection type of the request if it was set by an RPCServer.
// A connection type of ConnectionTypeUnknown means that the connection type was not set.
func GetConnectionType(ctx context.Context) ConnectionType {
	if v := ctx.Value(connectionTypeCtxKey); v != nil {
		return v.(ConnectionType)
	}
	return ConnectionTypeUnknown
}

// RPCServer provides a jsonrpc 2.0 http server handler
type RPCServer struct {
	*handler
	reverseClientBuilder func(context.Context, *wsConn) (context.Context, error)

	pingInterval time.Duration
}

// NewServer creates new RPCServer instance
func NewServer(opts ...ServerOption) *RPCServer {
	config := defaultServerConfig()
	for _, o := range opts {
		o(&config)
	}

	return &RPCServer{
		handler:              makeHandler(config),
		reverseClientBuilder: config.reverseClientBuilder,

		pingInterval: config.pingInterval,
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

func (s *RPCServer) handleWS(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	// TODO: allow setting
	// (note that we still are mostly covered by jwt tokens)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Header.Get("Sec-WebSocket-Protocol") != "" {
		w.Header().Set("Sec-WebSocket-Protocol", r.Header.Get("Sec-WebSocket-Protocol"))
	}

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("upgrading connection", "error", err)
		// note that upgrader.Upgrade will set http error if there is an error
		return
	}

	wc := &wsConn{
		conn:         c,
		handler:      s,
		pingInterval: s.pingInterval,
		ctx:          ctx,
	}

	if s.reverseClientBuilder != nil {
		ctx, err = s.reverseClientBuilder(ctx, wc)
		if err != nil {
			s.logger.Error("failed to build reverse client", "err", err)
			w.WriteHeader(500)
			return
		}
	}

	lbl := pprof.Labels("jrpc-mode", "wsserver", "jrpc-remote", r.RemoteAddr, "jrpc-uuid", uuid.New().String())
	pprof.Do(ctx, lbl, wc.handleWsConn)

	if err := c.Close(); err != nil {
		s.logger.Error("closing websocket connection", "error", err)
		return
	}
}

// TODO: return errors to clients per spec
func (s *RPCServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	h := strings.ToLower(r.Header.Get("Connection"))
	if strings.Contains(h, "upgrade") {
		ctx = context.WithValue(ctx, connectionTypeCtxKey, ConnectionTypeWS)
		s.handleWS(ctx, w, r)
		return
	}

	ctx = context.WithValue(ctx, connectionTypeCtxKey, ConnectionTypeHTTP)
	s.handleReader(ctx, r.Body, w, rpcError(s.logger, w.WriteHeader))
}

func (s *RPCServer) HandleRequest(ctx context.Context, r io.Reader, w io.Writer) {
	s.handleReader(ctx, r, w, rpcError(s.logger, nil))
}

func (s *RPCServer) GetLogger() *slog.Logger {
	return s.logger
}

func rpcError(logger *slog.Logger, writeHeader func(int)) rpcErrFunc {
	return func(w func(arg any) error, req *request, code ErrorCode, err error) {
		logger.Error("RPC Error", "err", err)
		if writeHeader != nil {
			if code == rpcInvalidRequest {
				writeHeader(http.StatusBadRequest)
			} else {
				writeHeader(http.StatusInternalServerError)
			}
		}
		resp := response{
			Jsonrpc: "2.0",
			Error: &JSONRPCError{
				Code:    code,
				Message: err.Error(),
			},
		}
		if req != nil {
			resp.ID = req.ID
		}
		err = w(resp)
		if err != nil {
			logger.Warn("failed to write rpc error", "err", err)
			return
		}
	}
}

// Register registers new RPC handler
//
// Handler is any value with methods defined
func (s *RPCServer) Register(namespace string, handler any) {
	s.register(namespace, handler)
}

func (s *RPCServer) AliasMethod(alias, original string) {
	s.aliasedMethods[alias] = original
}

var _ error = &JSONRPCError{}
