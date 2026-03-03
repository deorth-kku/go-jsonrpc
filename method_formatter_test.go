package jsonrpc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ctest "github.com/deorth-kku/go-common/test"
)

func TestDifferentMethodNamers(t *testing.T) {
	tests := map[string]struct {
		namer MethodNameFormatter

		requestedMethod string
	}{
		"default namer": {
			namer:           DefaultMethodNameFormatter,
			requestedMethod: "SimpleServerHandler.Inc",
		},
		"lower fist char": {
			namer:           NewMethodNameFormatter(true, LowerFirstCharCase),
			requestedMethod: "SimpleServerHandler.inc",
		},
		"no namespace namer": {
			namer:           NewMethodNameFormatter(false, OriginalCase),
			requestedMethod: "Inc",
		},
		"no namespace & lower fist char": {
			namer:           NewMethodNameFormatter(false, LowerFirstCharCase),
			requestedMethod: "inc",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			rpcServer := NewServer(WithServerMethodNameFormatter(test.namer))

			serverHandler := &SimpleServerHandler{}
			rpcServer.Register("SimpleServerHandler", serverHandler)

			testServ := httptest.NewServer(rpcServer)
			defer testServ.Close()

			req := fmt.Sprintf(`{"jsonrpc": "2.0", "method": "%s", "params": [], "id": 1}`, test.requestedMethod)

			res := ctest.Must31(t, http.Post, testServ.URL, "application/json", io.Reader(strings.NewReader(req)))

			ctest.Equal(t, http.StatusOK, res.StatusCode)
			ctest.Equal(t, int32(1), serverHandler.n)
		})
	}
}

func TestDifferentMethodNamersWithClient(t *testing.T) {
	tests := map[string]struct {
		namer     MethodNameFormatter
		urlPrefix string
	}{
		"default namer & http": {
			namer:     DefaultMethodNameFormatter,
			urlPrefix: "http://",
		},
		"default namer & ws": {
			namer:     DefaultMethodNameFormatter,
			urlPrefix: "ws://",
		},
		"lower first char namer & http": {
			namer:     NewMethodNameFormatter(true, LowerFirstCharCase),
			urlPrefix: "http://",
		},
		"lower first char namer & ws": {
			namer:     NewMethodNameFormatter(true, LowerFirstCharCase),
			urlPrefix: "ws://",
		},
		"no namespace namer & http": {
			namer:     NewMethodNameFormatter(false, OriginalCase),
			urlPrefix: "http://",
		},
		"no namespace namer & ws": {
			namer:     NewMethodNameFormatter(false, OriginalCase),
			urlPrefix: "ws://",
		},
		"no namespace & lower first char & http": {
			namer:     NewMethodNameFormatter(false, LowerFirstCharCase),
			urlPrefix: "http://",
		},
		"no namespace & lower first char & ws": {
			namer:     NewMethodNameFormatter(false, LowerFirstCharCase),
			urlPrefix: "ws://",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			rpcServer := NewServer(WithServerMethodNameFormatter(test.namer))

			serverHandler := &SimpleServerHandler{}
			rpcServer.Register("SimpleServerHandler", serverHandler)

			testServ := httptest.NewServer(rpcServer)
			defer testServ.Close()

			var client struct {
				AddGet func(int) int
			}
			closer := ctest.Must51Ex(t, NewMergeClient,
				t.Context(),
				test.urlPrefix+testServ.Listener.Addr().String(),
				"SimpleServerHandler",
				[]any{&client},
				nil,
				WithHTTPClient(testServ.Client()),
				WithMethodNameFormatter(test.namer),
			)
			defer closer()

			n := client.AddGet(123)
			ctest.Equal(t, 123, n)
		})
	}
}

func TestDifferentMethodNamersWithClientHandler(t *testing.T) {
	tests := map[string]struct {
		namer MethodNameFormatter
	}{
		"default namer & ws": {
			namer: DefaultMethodNameFormatter,
		},
		"lower first char namer & ws": {
			namer: NewMethodNameFormatter(true, LowerFirstCharCase),
		},
		"no namespace namer & ws": {
			namer: NewMethodNameFormatter(false, OriginalCase),
		},
		"no namespace & lower first char & ws": {
			namer: NewMethodNameFormatter(false, LowerFirstCharCase),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			rpcServer := NewServer(WithReverseClient[RevCallTestClientProxy]("Client"), WithServerMethodNameFormatter(test.namer))
			rpcServer.Register("Server", &RevCallTestServerHandler{})

			// httptest stuff
			testServ := httptest.NewServer(rpcServer)
			defer testServ.Close()
			// setup client

			var client struct {
				Call func() error
			}

			closer, err := NewMergeClient(
				context.Background(),
				"ws://"+testServ.Listener.Addr().String(),
				"Server",
				[]any{&client},
				nil,
				WithMethodNameFormatter(test.namer),
				WithClientHandler("Client", &RevCallTestClientHandler{}),
				WithClientHandlerFormatter(test.namer),
			)
			ctest.NoError(t, err)
			defer closer()

			e := client.Call()
			ctest.NoError(t, e)
		})
	}
}
