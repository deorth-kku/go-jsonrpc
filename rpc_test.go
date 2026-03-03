package jsonrpc

import (
	"bytes"
	"context"
	v1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deorth-kku/go-common"
	ctest "github.com/deorth-kku/go-common/test"
	"github.com/gorilla/websocket"
)

func setlog(lv string) {
	common.SetLog("", lv, common.DefaultFormat.String(), common.SlogAddSource{})
}

func init() {
	lv, exists := os.LookupEnv("GOLOG_LOG_LEVEL")
	if !exists {
		lv = "DEBUG"
	}
	setlog(lv)
	debugTrace = true
}

var v1opts = v1.DefaultOptionsV1()

type SimpleServerHandler struct {
	n int32
}

type TestType struct {
	S string
	I int
}

type TestOut struct {
	TestType
	Ok bool
}

func (h *SimpleServerHandler) Inc() error {
	h.n++

	return nil
}

func (h *SimpleServerHandler) Add(in int) error {
	if in == -3546 {
		return errors.New("test")
	}

	atomic.AddInt32(&h.n, int32(in))

	return nil
}

func (h *SimpleServerHandler) AddGet(in int) int {
	atomic.AddInt32(&h.n, int32(in))
	return int(h.n)
}

func (h *SimpleServerHandler) StringMatch(t TestType, i2 int64) (out TestOut, err error) {
	if strconv.FormatInt(i2, 10) == t.S {
		out.Ok = true
	}
	if i2 != int64(t.I) {
		return TestOut{}, errors.New(":(")
	}
	out.I = t.I
	out.S = t.S
	return
}

func removeSpaces(jsonStr string) (string, error) {
	value := jsontext.Value(jsonStr)
	err := value.Format()
	if err != nil {
		return "", err
	}
	return string(value), nil
}

func TestRawRequests(t *testing.T) {
	rpcHandler := SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", &rpcHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	tc := func(req, resp string, n int32, statusCode int) func(t *testing.T) {
		return func(t *testing.T) {
			rpcHandler.n = 0

			res := ctest.Must31(t, http.Post, testServ.URL, "application/json", io.Reader(strings.NewReader(req)))
			b := ctest.Must11(t, io.ReadAll, io.Reader(res.Body))

			expectedResp := ctest.Must11(t, removeSpaces, resp)
			responseBody := ctest.Must11(t, removeSpaces, string(b))

			ctest.Equal(t, expectedResp, responseBody)
			ctest.Equal(t, n, rpcHandler.n)
			ctest.Equal(t, statusCode, res.StatusCode)
		}
	}

	t.Run("inc", tc(`{"jsonrpc": "2.0", "method": "SimpleServerHandler.Inc", "params": [], "id": 1}`, `{"jsonrpc":"2.0","id":1,"result":null}`, 1, 200))
	t.Run("inc-null", tc(`{"jsonrpc": "2.0", "method": "SimpleServerHandler.Inc", "params": null, "id": 1}`, `{"jsonrpc":"2.0","id":1,"result":null}`, 1, 200))
	t.Run("inc-noparam", tc(`{"jsonrpc": "2.0", "method": "SimpleServerHandler.Inc", "id": 2}`, `{"jsonrpc":"2.0","id":2,"result":null}`, 1, 200))
	t.Run("add", tc(`{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [10], "id": 4}`, `{"jsonrpc":"2.0","id":4,"result":null}`, 10, 200))
	// Batch requests
	t.Run("bad_end", tc(`[{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [123], "id": 5}`, `[{"jsonrpc":"2.0","id":5,"result":null},{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"unexpected end of JSON input"}}]`, 123, 500))
	t.Run("batch1", tc(`[{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [123], "id": 6}]`, `[{"jsonrpc":"2.0","id":6,"result":null}]`, 123, 200))
	t.Run("batch2", tc(`[{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [123], "id": 7},{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [-122], "id": 8}]`, `[{"jsonrpc":"2.0","id":7,"result":null},{"jsonrpc":"2.0","id":8,"result":null}]`, 1, 200))
	t.Run("batch2err", tc(`[{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [123], "id": 9},{"jsonrpc": "2.0", "params": [-122], "id": 10}]`, `[{"jsonrpc":"2.0","id":9,"result":null},{"jsonrpc":"2.0","id":10,"error":{"code":-32601,"message":"method '' not found"}}]`, 123, 500))
	t.Run("batch_with_space", tc(`     [{"jsonrpc": "2.0", "method": "SimpleServerHandler.Add", "params": [-1], "id": 11}]   `, `[{"jsonrpc":"2.0","id":11,"result":null}]`, -1, 200))
	t.Run("empty", tc(``, `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"EOF"}}`, 0, 500))
}

func TestReconnection(t *testing.T) {
	var rpcClient struct {
		Add func(int) error
	}

	rpcHandler := SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", &rpcHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// capture connection attempts for this duration
	captureDuration := 3 * time.Second

	// run the test until the timer expires
	timer := time.NewTimer(captureDuration)

	// record the number of connection attempts during this test
	connectionAttempts := int64(1)

	closer, err := NewMergeClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", []any{&rpcClient}, nil, func(c *Config) {
		c.proxyConnFactory = func(f func() (*websocket.Conn, error)) func() (*websocket.Conn, error) {
			return func() (*websocket.Conn, error) {
				defer func() {
					atomic.AddInt64(&connectionAttempts, 1)
				}()

				if atomic.LoadInt64(&connectionAttempts) > 1 {
					return nil, errors.New("simulates a failed reconnect attempt")
				}

				c, err := f()
				if err != nil {
					return nil, err
				}

				// closing the connection here triggers the reconnect logic
				_ = c.Close()

				return c, nil
			}
		}
	})
	ctest.NoError(t, err)
	defer closer()

	// let the JSON-RPC library attempt to reconnect until the timer runs out
	<-timer.C

	// do some math
	attemptsPerSecond := atomic.LoadInt64(&connectionAttempts) / int64(captureDuration/time.Second)

	ctest.Less(t, attemptsPerSecond, int64(50))
}

func (h *SimpleServerHandler) ErrChanSub(ctx context.Context) (<-chan int, error) {
	return nil, errors.New("expect to return an error")
}

// BlockingAdd blocks until the provided context is cancelled, then returns.
func (h *SimpleServerHandler) BlockingAdd(ctx context.Context, in int) (int, error) {
	atomic.AddInt32(&h.n, int32(in))
	<-ctx.Done()
	return int(atomic.LoadInt32(&h.n)), nil
}

func TestNextWriterCleanup(t *testing.T) {
	// Verify that handler goroutines are properly cleaned up when a websocket
	// connection is closed while the server is still processing a request.

	serverHandler := &SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", serverHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// Stabilize goroutine count — let the test server's listener settle
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	goroutinesBefore := runtime.NumGoroutine()

	const iterations = 10
	for i := 0; i < iterations; i++ {
		wsURL := "ws://" + testServ.Listener.Addr().String()
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		ctest.NoError(t, err)

		reqMsg := fmt.Sprintf(`{"jsonrpc":"2.0","method":"SimpleServerHandler.BlockingAdd","params":[%d],"id":%d}`, i+1, i+1)
		err = conn.WriteMessage(websocket.TextMessage, []byte(reqMsg))
		ctest.NoError(t, err)

		time.Sleep(50 * time.Millisecond)

		closeMsg := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
		err = conn.WriteMessage(websocket.CloseMessage, closeMsg)
		ctest.NoError(t, err)

		time.Sleep(50 * time.Millisecond)
		_ = conn.Close()
	}

	// Headroom for runtime background goroutines (GC, finalizers, etc.)
	const goroutineMargin = 5

	settled := false
	for attempt := 0; attempt < 30; attempt++ {
		time.Sleep(100 * time.Millisecond)
		current := runtime.NumGoroutine()
		if current <= goroutinesBefore+goroutineMargin {
			settled = true
			break
		}
	}

	goroutinesAfter := runtime.NumGoroutine()
	if !settled {
		t.Errorf("goroutine leak: before=%d after=%d; expected at most %d",
			goroutinesBefore, goroutinesAfter, goroutinesBefore+goroutineMargin)
	}
}

func TestRPCBadConnection(t *testing.T) {
	// setup server

	serverHandler := &SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()
	// setup client

	var client struct {
		Add         func(int) error
		AddGet      func(int) int
		StringMatch func(t TestType, i2 int64) (out TestOut, err error)
		ErrChanSub  func(context.Context) (<-chan int, error)
	}
	closer, err := NewClient(t.Context(), "http://"+testServ.Listener.Addr().String()+"0", "SimpleServerHandler", &client, nil)
	ctest.NoError(t, err)
	defer closer()
	err = client.Add(2)
	ctest.AsErrorType[*RPCConnectionError](t, err)
}

func TestRPC(t *testing.T) {
	// setup server

	serverHandler := &SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()
	// setup client

	var client struct {
		Add         func(int) error
		AddGet      func(int) int
		StringMatch func(t TestType, i2 int64) (out TestOut, err error)
		ErrChanSub  func(context.Context) (<-chan int, error)
	}
	closer, err := NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &client, nil)
	ctest.NoError(t, err)
	defer closer()

	// Add(int) error

	ctest.NoError(t, client.Add(2))
	ctest.Equal(t, 2, int(serverHandler.n))

	err = client.Add(-3546)
	ctest.Equal(t, err.Error(), "test")

	// AddGet(int) int

	n := client.AddGet(3)
	ctest.Equal(t, 5, n)
	ctest.Equal(t, 5, int(serverHandler.n))

	// StringMatch

	o, err := client.StringMatch(TestType{S: "0"}, 0)
	ctest.NoError(t, err)
	ctest.Equal(t, "0", o.S)
	ctest.Equal(t, 0, o.I)

	_, err = client.StringMatch(TestType{S: "5"}, 5)
	ctest.Equal(t, err.Error(), ":(")

	o, err = client.StringMatch(TestType{S: "8", I: 8}, 8)
	ctest.NoError(t, err)
	ctest.Equal(t, "8", o.S)
	ctest.Equal(t, 8, o.I)

	// ErrChanSub
	ctx := context.TODO()
	_, err = client.ErrChanSub(ctx)
	if err == nil {
		t.Fatal("expect an err return, but got nil")
	}

	// Invalid client handlers

	var noret struct {
		Add func(int)
	}
	closer, err = NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &noret, nil)
	ctest.NoError(t, err)

	// this one should actually work
	noret.Add(4)
	ctest.Equal(t, 9, int(serverHandler.n))
	closer()

	var noparam struct {
		Add func()
	}
	closer, err = NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &noparam, nil)
	ctest.NoError(t, err)

	// shouldn't panic
	noparam.Add()
	closer()

	var erronly struct {
		AddGet func() (int, error)
	}
	closer, err = NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &erronly, nil)
	ctest.NoError(t, err)

	_, err = erronly.AddGet()
	matchJSONRPCError(t, err, rpcInvalidParams, ErrShortParams.Error())
	closer()

	var wrongtype struct {
		Add func(string) error
	}
	closer, err = NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &wrongtype, nil)
	ctest.NoError(t, err)

	err = wrongtype.Add("not an int")
	matchJSONRPCError(t, err, rpcParseError, "json: cannot unmarshal string into int.params.0 of type int")
	closer()

	var notfound struct {
		NotThere func(string) error
	}
	closer, err = NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &notfound, nil)
	ctest.NoError(t, err)

	err = notfound.NotThere("hello?")
	matchJSONRPCError(t, err, rpcMethodNotFound, "method 'SimpleServerHandler.NotThere' not found")
	closer()
}

func matchJSONRPCError(t *testing.T, err error, code ErrorCode, Msg string) {
	t.Helper()
	switch v := err.(type) {
	case *JSONRPCError:
		if v.Code == code && v.Message == Msg {
			return
		}
	default:
		t.Error("wrong error:", err)
	}
}

func TestRPCHttpClient(t *testing.T) {
	// setup server

	serverHandler := &SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()
	// setup client

	var client struct {
		Add         func(int) error
		AddGet      func(int) int
		StringMatch func(t TestType, i2 int64) (out TestOut, err error)
	}
	closer, err := NewClient(t.Context(), "http://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &client, nil)
	ctest.NoError(t, err)
	defer closer()

	// Add(int) error

	ctest.NoError(t, client.Add(2))
	ctest.Equal(t, 2, int(serverHandler.n))

	err = client.Add(-3546)
	ctest.Equal(t, err.Error(), "test")

	// AddGet(int) int

	n := client.AddGet(3)
	ctest.Equal(t, 5, n)
	ctest.Equal(t, 5, int(serverHandler.n))

	// StringMatch

	o, err := client.StringMatch(TestType{S: "0"}, 0)
	ctest.NoError(t, err)
	ctest.Equal(t, "0", o.S)
	ctest.Equal(t, 0, o.I)

	_, err = client.StringMatch(TestType{S: "5"}, 5)
	ctest.Equal(t, err.Error(), ":(")

	o, err = client.StringMatch(TestType{S: "8", I: 8}, 8)
	ctest.NoError(t, err)
	ctest.Equal(t, "8", o.S)
	ctest.Equal(t, 8, o.I)

	// Invalid client handlers

	var noret struct {
		Add func(int)
	}
	closer, err = NewClient(t.Context(), "http://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &noret, nil)
	ctest.NoError(t, err)

	// this one should actually work
	noret.Add(4)
	ctest.Equal(t, 9, int(serverHandler.n))
	closer()

	var noparam struct {
		Add func()
	}
	closer, err = NewClient(t.Context(), "http://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &noparam, nil)
	ctest.NoError(t, err)

	// shouldn't panic
	noparam.Add()
	closer()

	var erronly struct {
		AddGet func() (int, error)
	}
	closer, err = NewClient(t.Context(), "http://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &erronly, nil)
	ctest.NoError(t, err)

	_, err = erronly.AddGet()
	matchJSONRPCError(t, err, rpcInvalidParams, ErrShortParams.Error())
	closer()

	var wrongtype struct {
		Add func(string) error
	}
	closer, err = NewClient(t.Context(), "http://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &wrongtype, nil)
	ctest.NoError(t, err)

	err = wrongtype.Add("not an int")
	matchJSONRPCError(t, err, rpcParseError, "json: cannot unmarshal string into int.params.0 of type int")
	closer()

	var notfound struct {
		NotThere func(string) error
	}
	closer, err = NewClient(t.Context(), "http://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &notfound, nil)
	ctest.NoError(t, err)

	err = notfound.NotThere("hello?")
	matchJSONRPCError(t, err, rpcMethodNotFound, "method 'SimpleServerHandler.NotThere' not found")
	closer()
}

func TestParallelRPC(t *testing.T) {
	// setup server

	serverHandler := &SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()
	// setup client

	var client struct {
		Add func(int) error
	}
	closer, err := NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &client, nil)
	ctest.NoError(t, err)
	defer closer()

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			for range 100 {
				ctest.NoError(t, client.Add(2))
			}
		})
	}
	wg.Wait()

	ctest.Equal(t, 20000, int(serverHandler.n))
}

type CtxHandler struct {
	lk     context.Context
	cancel context.CancelFunc

	cancelled      bool
	i              int
	connectionType ConnectionType
}

func (h *CtxHandler) Test(ctx context.Context) {
	defer h.cancel()
	timeout := time.After(300 * time.Millisecond)
	h.i++
	h.connectionType = GetConnectionType(ctx)

	select {
	case <-timeout:
	case <-ctx.Done():
		h.cancelled = true
	}
}

func (h *CtxHandler) setCond() {
	h.lk, h.cancel = context.WithCancel(context.Background())
}

func TestCtx(t *testing.T) {
	// setup server

	serverHandler := &CtxHandler{}

	rpcServer := NewServer()
	rpcServer.Register("CtxHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client

	var client struct {
		Test func(ctx context.Context)
	}
	closer, err := NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "CtxHandler", &client, nil)
	ctest.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	serverHandler.setCond()
	client.Test(ctx)
	<-serverHandler.lk.Done()

	if !serverHandler.cancelled {
		t.Error("expected cancellation on the server side")
	}
	if serverHandler.connectionType != ConnectionTypeWS {
		t.Error("wrong connection type")
	}

	serverHandler.cancelled = false

	closer()

	var noCtxClient struct {
		Test func()
	}
	closer, err = NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "CtxHandler", &noCtxClient, nil)
	if err != nil {
		t.Fatal(err)
	}

	serverHandler.setCond()
	noCtxClient.Test()
	<-serverHandler.lk.Done()

	if serverHandler.cancelled || serverHandler.i != 2 {
		t.Error("wrong serverHandler state")
	}
	if serverHandler.connectionType != ConnectionTypeWS {
		t.Error("wrong connection type")
	}

	closer()
}

func TestCtxHttp(t *testing.T) {
	// setup server

	serverHandler := &CtxHandler{}

	rpcServer := NewServer()
	rpcServer.Register("CtxHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewUnstartedServer(rpcServer)
	testServ.Config.Protocols = new(http.Protocols)
	testServ.Config.Protocols.SetHTTP1(true)
	testServ.Config.Protocols.SetUnencryptedHTTP2(true)
	testServ.Start()
	defer testServ.Close()

	// setup client

	var client struct {
		Test func(ctx context.Context)
	}
	closer, err := NewClient(t.Context(), "http://"+testServ.Listener.Addr().String(), "CtxHandler", &client, nil)
	ctest.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	serverHandler.setCond()
	client.Test(ctx)
	<-serverHandler.lk.Done()

	if !serverHandler.cancelled {
		t.Error("expected cancellation on the server side")
	}
	if serverHandler.connectionType != ConnectionTypeHTTP {
		t.Error("wrong connection type")
	}

	serverHandler.cancelled = false

	// h2c client
	client.Test = nil

	closer()
	h2c := new(http.Protocols)
	h2c.SetUnencryptedHTTP2(true)
	closer, err = NewClient(t.Context(), "http://"+testServ.Listener.Addr().String(), "CtxHandler", &client, nil,
		WithHTTPClient(&http.Client{
			Transport: &http.Transport{
				Protocols:         h2c,
				DisableKeepAlives: true,
			}}))
	ctest.NoError(t, err)

	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	serverHandler.setCond()
	client.Test(ctx)
	<-serverHandler.lk.Done()

	if !serverHandler.cancelled {
		// I don't believe this will work on http2
		t.Log("expected cancellation on the server side, but it's http2, so that's ok")
	}
	if serverHandler.connectionType != ConnectionTypeHTTP {
		t.Error("wrong connection type")
	}

	serverHandler.cancelled = false

	closer()

	// no context

	var noCtxClient struct {
		Test func()
	}
	closer, err = NewClient(t.Context(), "http://"+testServ.Listener.Addr().String(), "CtxHandler", &noCtxClient, nil)
	if err != nil {
		t.Fatal(err)
	}

	serverHandler.setCond()
	noCtxClient.Test()
	<-serverHandler.lk.Done()

	if serverHandler.cancelled || serverHandler.i != 3 {
		t.Error("wrong serverHandler state")
	}
	if serverHandler.connectionType != ConnectionTypeHTTP {
		t.Error("wrong connection type")
	}

	closer()
}

type UnUnmarshalable int

func (*UnUnmarshalable) UnmarshalJSON([]byte) error {
	return errors.New("nope")
}

type UnUnmarshalableHandler struct{}

func (*UnUnmarshalableHandler) GetUnUnmarshalableStuff() (UnUnmarshalable, error) {
	return UnUnmarshalable(5), nil
}

func TestUnmarshalableResult(t *testing.T) {
	var client struct {
		GetUnUnmarshalableStuff func() (UnUnmarshalable, error)
	}

	rpcServer := NewServer()
	rpcServer.Register("Handler", &UnUnmarshalableHandler{})

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	closer, err := NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "Handler", &client, nil)
	ctest.NoError(t, err)
	defer closer()

	_, err = client.GetUnUnmarshalableStuff()
	ctest.Equal(t, err.Error(), "RPC error (-32700): nope")
}

type ChanHandler struct {
	wait    chan struct{}
	ctxdone <-chan struct{}
}

func (h *ChanHandler) Sub(ctx context.Context, i int, eq int) (<-chan int, error) {
	out := make(chan int)
	h.ctxdone = ctx.Done()

	wait := h.wait

	slog.Warn("SERVER SUB!")
	go func() {
		defer close(out)
		var n int

		for {
			select {
			case <-ctx.Done():
				fmt.Println("ctxdone1", i, eq)
				return
			case <-wait:
				//fmt.Println("CONSUMED WAIT: ", i)
			}

			n += i

			if n == eq {
				fmt.Println("eq")
				return
			}

			select {
			case <-ctx.Done():
				fmt.Println("ctxdone2")
				return
			case out <- n:
			}
		}
	}()

	return out, nil
}

func TestChan(t *testing.T) {
	var client struct {
		Sub func(context.Context, int, int) (<-chan int, error)
	}

	serverHandler := &ChanHandler{
		wait: make(chan struct{}, 5),
	}

	rpcServer := NewServer()
	rpcServer.Register("ChanHandler", serverHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	closer, err := NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "ChanHandler", &client, nil)
	ctest.NoError(t, err)

	defer closer()

	serverHandler.wait <- struct{}{}

	ctx, cancel := context.WithCancel(t.Context())

	// sub

	sub, err := client.Sub(ctx, 2, -1)
	ctest.NoError(t, err)

	// recv one

	ctest.Equal(t, 2, <-sub)

	// recv many (order)

	serverHandler.wait <- struct{}{}
	serverHandler.wait <- struct{}{}
	serverHandler.wait <- struct{}{}

	ctest.Equal(t, 4, <-sub)
	ctest.Equal(t, 6, <-sub)
	ctest.Equal(t, 8, <-sub)

	// close (through ctx)
	cancel()

	_, ok := <-sub
	ctest.Equal(t, false, ok)

	// sub (again)

	serverHandler.wait = make(chan struct{}, 5)
	serverHandler.wait <- struct{}{}

	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()

	slog.Warn("last sub")
	sub, err = client.Sub(ctx, 3, 6)
	ctest.NoError(t, err)

	slog.Warn("waiting for value now")
	ctest.Equal(t, 3, <-sub)
	slog.Warn("not equal")

	// close (remote)
	serverHandler.wait <- struct{}{}
	_, ok = <-sub
	ctest.Equal(t, false, ok)
}

func TestChanClosing(t *testing.T) {
	var client struct {
		Sub func(context.Context, int, int) (<-chan int, error)
	}

	serverHandler := &ChanHandler{
		wait: make(chan struct{}, 5),
	}

	rpcServer := NewServer()
	rpcServer.Register("ChanHandler", serverHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	closer, err := NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "ChanHandler", &client, nil)
	ctest.NoError(t, err)

	defer closer()

	ctx1, cancel1 := context.WithCancel(t.Context())
	ctx2, cancel2 := context.WithCancel(t.Context())

	// sub

	sub1, err := client.Sub(ctx1, 2, -1)
	ctest.NoError(t, err)

	sub2, err := client.Sub(ctx2, 3, -1)
	ctest.NoError(t, err)

	// recv one

	serverHandler.wait <- struct{}{}
	serverHandler.wait <- struct{}{}

	ctest.Equal(t, 2, <-sub1)
	ctest.Equal(t, 3, <-sub2)

	cancel1()

	ctest.Equal(t, 0, <-sub1)
	time.Sleep(time.Millisecond * 50) // make sure the loop has exited (having a shared wait channel makes this annoying)

	serverHandler.wait <- struct{}{}
	ctest.Equal(t, 6, <-sub2)

	cancel2()
	ctest.Equal(t, 0, <-sub2)
}

func TestChanServerClose(t *testing.T) {
	var client struct {
		Sub func(context.Context, int, int) (<-chan int, error)
	}

	serverHandler := &ChanHandler{
		wait: make(chan struct{}, 5),
	}

	rpcServer := NewServer()
	rpcServer.Register("ChanHandler", serverHandler)

	tctx, tcancel := context.WithCancel(t.Context())

	testServ := httptest.NewUnstartedServer(rpcServer)
	testServ.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return tctx
	}
	testServ.Start()

	closer, err := NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "ChanHandler", &client, nil)
	ctest.NoError(t, err)

	defer closer()

	serverHandler.wait <- struct{}{}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// sub

	sub, err := client.Sub(ctx, 2, -1)
	ctest.NoError(t, err)

	// recv one

	ctest.Equal(t, 2, <-sub)

	// make sure we're blocked

	select {
	case <-time.After(200 * time.Millisecond):
	case <-sub:
		t.Fatal("didn't expect to get anything from sub")
	}

	// close server

	tcancel()
	testServ.Close()

	_, ok := <-sub
	ctest.Equal(t, false, ok)
}

func TestServerChanLockClose(t *testing.T) {
	var client struct {
		Sub func(context.Context, int, int) (<-chan int, error)
	}

	serverHandler := &ChanHandler{
		wait: make(chan struct{}),
	}

	rpcServer := NewServer()
	rpcServer.Register("ChanHandler", serverHandler)

	testServ := httptest.NewServer(rpcServer)

	var closeConn func() error

	_, err := NewMergeClient(t.Context(), "ws://"+testServ.Listener.Addr().String(),
		"ChanHandler",
		[]any{&client}, nil,
		func(c *Config) {
			c.proxyConnFactory = func(f func() (*websocket.Conn, error)) func() (*websocket.Conn, error) {
				return func() (*websocket.Conn, error) {
					c, err := f()
					if err != nil {
						return nil, err
					}

					closeConn = c.UnderlyingConn().Close

					return c, nil
				}
			}
		})
	ctest.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// sub

	sub, err := client.Sub(ctx, 2, -1)
	ctest.NoError(t, err)

	// recv one

	go func() {
		serverHandler.wait <- struct{}{}
	}()
	ctest.Equal(t, 2, <-sub)

	for range 100 {
		serverHandler.wait <- struct{}{}
	}

	if err := closeConn(); err != nil {
		t.Fatal(err)
	}

	<-serverHandler.ctxdone
}

type StreamingHandler struct {
}

func (h *StreamingHandler) GetData(ctx context.Context, n int) (<-chan int, error) {
	out := make(chan int)

	go func() {
		defer close(out)

		for i := range n {
			out <- i
		}
	}()

	return out, nil
}

func TestChanClientReceiveAll(t *testing.T) {
	var client struct {
		GetData func(context.Context, int) (<-chan int, error)
	}

	serverHandler := &StreamingHandler{}

	rpcServer := NewServer()
	rpcServer.Register("ChanHandler", serverHandler)

	tctx, tcancel := context.WithCancel(t.Context())

	testServ := httptest.NewUnstartedServer(rpcServer)
	testServ.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return tctx
	}
	testServ.Start()

	closer, err := NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "ChanHandler", &client, nil)
	ctest.NoError(t, err)

	defer closer()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// sub

	sub, err := client.GetData(ctx, 100)
	ctest.NoError(t, err)

	for i := range 100 {
		select {
		case v, ok := <-sub:
			if !ok {
				t.Fatal("channel closed", i)
			}

			if v != i {
				t.Fatal("got wrong value", v, i)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for values")
		}
	}

	tcancel()
	testServ.Close()

}

func TestControlChanDeadlock(t *testing.T) {
	if _, exists := os.LookupEnv("GOLOG_LOG_LEVEL"); !exists {
		setlog("ERROR")
		defer func() {
			setlog("DEBUG")
		}()
	}

	for range 20 {
		testControlChanDeadlock(t)
	}
}

func testControlChanDeadlock(t *testing.T) {
	var client struct {
		Sub func(context.Context, int, int) (<-chan int, error)
	}

	n := 5000

	serverHandler := &ChanHandler{
		wait: make(chan struct{}, n),
	}

	rpcServer := NewServer()
	rpcServer.Register("ChanHandler", serverHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	closer, err := NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "ChanHandler", &client, nil)
	ctest.NoError(t, err)

	defer closer()

	for range n {
		serverHandler.wait <- struct{}{}
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	sub, err := client.Sub(ctx, 1, -1)
	ctest.NoError(t, err)

	done := make(chan struct{})

	go func() {
		defer close(done)
		for i := range n {
			if <-sub != i+1 {
				panic("bad!")
				// ctest.Equal(t, i+1, <-sub)
			}
		}
	}()

	// reset this channel so its not shared between the sub requests...
	serverHandler.wait = make(chan struct{}, n)
	for range n {
		serverHandler.wait <- struct{}{}
	}

	_, err = client.Sub(ctx, 2, -1)
	ctest.NoError(t, err)
	<-done
}

type InterfaceHandler struct {
}

func (h *InterfaceHandler) ReadAll(ctx context.Context, r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

func TestInterfaceHandler(t *testing.T) {
	var client struct {
		ReadAll func(ctx context.Context, r io.Reader) ([]byte, error)
	}

	serverHandler := &InterfaceHandler{}

	rpcServer := NewServer(WithParamUnmarshaler(readerDec))
	rpcServer.Register("InterfaceHandler", serverHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	closer, err := NewMergeClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "InterfaceHandler", []any{&client}, nil, WithParamMarshaler(readerEnc))
	ctest.NoError(t, err)

	defer closer()

	read, err := client.ReadAll(context.TODO(), strings.NewReader("pooooootato"))
	ctest.NoError(t, err)
	ctest.Equal(t, "pooooootato", string(read), "potatos weren't equal")
}

var (
	readerRegistery   = map[int]io.Reader{}
	readerRegisteryN  = 31
	readerRegisteryLk sync.Mutex
)

func readerEnc(enc *jsontext.Encoder, rd io.Reader) error {
	readerRegisteryLk.Lock()
	defer readerRegisteryLk.Unlock()
	n := readerRegisteryN
	readerRegisteryN++
	readerRegistery[n] = rd
	return json.MarshalEncode(enc, n)
}

func readerDec(dec *jsontext.Decoder, rd *io.Reader) error {
	var id int
	if err := json.UnmarshalDecode(dec, &id); err != nil {
		return err
	}

	readerRegisteryLk.Lock()
	defer readerRegisteryLk.Unlock()
	*rd = readerRegistery[id]
	return nil
}

type ErrSomethingBad struct{}

func (e ErrSomethingBad) Error() string {
	return "something bad has happened"
}

type ErrMyErr struct{ str string }

var _ error = ErrSomethingBad{}

func (e *ErrMyErr) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &e.str)
}

func (e *ErrMyErr) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.str)
}

func (e *ErrMyErr) Error() string {
	return fmt.Sprintf("this happened: %s", e.str)
}

type ErrHandler struct{}

func (h *ErrHandler) Test() error {
	return ErrSomethingBad{}
}

func (h *ErrHandler) TestP() error {
	return &ErrSomethingBad{}
}

func (h *ErrHandler) TestMy(s string) error {
	return &ErrMyErr{
		str: s,
	}
}

func (h *ErrHandler) TestStringError(str string) error {
	return common.ErrorString(str)
}

func TestUserError(t *testing.T) {
	// setup server

	serverHandler := &ErrHandler{}

	const (
		EBad = iota + FirstUserCode
		EBad2
		EMy
		EStr
	)

	errs := NewErrors()
	errs.Register(EBad, new(ErrSomethingBad))
	errs.Register(EBad2, new(*ErrSomethingBad))
	errs.Register(EMy, new(*ErrMyErr))
	RegisterError[common.ErrorString](errs, EStr)

	rpcServer := NewServer(WithServerErrors(errs))
	rpcServer.Register("ErrHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client

	var client struct {
		Test            func() error
		TestP           func() error
		TestStringError func(s string) error
		TestMy          func(s string) error
	}
	closer, err := NewMergeClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "ErrHandler", []any{
		&client,
	}, nil, WithErrors(errs))
	ctest.NoError(t, err)

	e := client.Test()
	ctest.IsError(t, e, ErrSomethingBad{})

	e = client.TestP()
	ctest.IsError(t, e, &ErrSomethingBad{})

	e = client.TestMy("some event")
	ctest.Error(t, e)
	ctest.Equal(t, "this happened: some event", e.Error())
	ctest.Equal(t, "this happened: some event", e.(*ErrMyErr).Error())

	e = client.TestStringError("some event")
	ctest.Error(t, e)
	ctest.Equal(t, "some event", e.Error())

	closer()
}

// Unit test for request/response ID translation.
func TestIDHandling(t *testing.T) {
	cases := []struct {
		str       string
		expect    any
		expectErr bool
	}{
		{
			`{"id":"8116d306-56cc-4637-9dd7-39ce1548a5a0","jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`,
			"8116d306-56cc-4637-9dd7-39ce1548a5a0",
			false,
		},
		{`{"id":1234,"jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`, float64(1234), false},
		{`{"id":null,"jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`, nil, false},
		{`{"id":1234.0,"jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`, 1234.0, false},
		{`{"id":1.2,"jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`, 1.2, false},
		{`{"id":["1"],"jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`, nil, true},
		{`{"id":{"a":"b"},"jsonrpc":"2.0","method":"eth_blockNumber","params":[]}`, nil, true},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%v", tc.expect), func(t *testing.T) {
			var decoded request
			err := json.UnmarshalRead(strings.NewReader(tc.str), &decoded, v1opts)
			if tc.expectErr {
				ctest.Error(t, err)
			} else {
				ctest.NoError(t, err)
				ctest.Equal(t, tc.expect, decoded.ID)
			}
		})
	}
}

func TestAliasedCall(t *testing.T) {
	// setup server

	rpcServer := NewServer()
	rpcServer.Register("ServName", &SimpleServerHandler{n: 3})

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client
	var client struct {
		WhateverMethodName func(int) (int, error) `rpc_method:"ServName.AddGet"`
	}
	closer, err := NewMergeClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "Server", []any{
		&client,
	}, nil)
	ctest.NoError(t, err)

	// do the call!

	n, err := client.WhateverMethodName(1)
	ctest.NoError(t, err)

	ctest.Equal(t, 4, n)

	closer()
}

type NotifHandler struct {
	notified chan struct{}
}

func (h *NotifHandler) Notif() {
	close(h.notified)
}

func TestNotif(t *testing.T) {
	tc := func(proto string) func(t *testing.T) {
		return func(t *testing.T) {
			// setup server

			nh := &NotifHandler{
				notified: make(chan struct{}),
			}

			rpcServer := NewServer()
			rpcServer.Register("Notif", nh)

			// httptest stuff
			testServ := httptest.NewServer(rpcServer)
			defer testServ.Close()

			// setup client
			var client struct {
				Notif func() error `notify:"true"`
			}
			closer, err := NewMergeClient(t.Context(), proto+"://"+testServ.Listener.Addr().String(), "Notif", []any{
				&client,
			}, nil)
			ctest.NoError(t, err)

			// do the call!

			// this will block if it's not sent as a notification
			err = client.Notif()
			ctest.NoError(t, err)

			<-nh.notified

			closer()
		}
	}

	t.Run("ws", tc("ws"))
	t.Run("http", tc("http"))
}

type CustomParams struct {
	I int
}

var (
	_ json.UnmarshalerFrom = (*CustomParams)(nil)
	_ json.MarshalerTo     = (*CustomParams)(nil)
)

func (CustomParams) MarshalJSONTo(*jsontext.Encoder) error {
	// no idea why, but it seems that marshaler methods won't be call for the inlined field.
	// this behavior is inconsistent with the documentation. https://pkg.go.dev/encoding/json/v2
	return errors.ErrUnsupported
}

func (CustomParams) UnmarshalJSONFrom(*jsontext.Decoder) error {
	return errors.ErrUnsupported
}

type ObjectParamHandler struct{}

func (h *ObjectParamHandler) Call(ctx context.Context, ps Object[CustomParams]) (int, error) {
	return ps.Value.I + 1, nil
}

func TestCallWithObjectParams(t *testing.T) {
	// setup server

	rpcServer := NewServer()
	rpcServer.Register("Raw", &ObjectParamHandler{})

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client
	var client struct {
		Call func(ctx context.Context, ps Object[CustomParams]) (int, error)
	}
	closer, err := NewMergeClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "Raw", []any{
		&client,
	}, nil)
	ctest.NoError(t, err)
	n, err := client.Call(t.Context(), GetObject(CustomParams{I: 1}))
	ctest.NoError(t, err)
	ctest.Equal(t, 2, n)

	closer()
}

type ctxHandler struct{}

func (h *ctxHandler) Call(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestHttpClientContext(t *testing.T) {
	// setup server

	rpcServer := NewServer()
	rpcServer.Register("Raw", &ctxHandler{})

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client
	var client struct {
		Call func(ctx context.Context) error
	}
	clientctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	closer, err := NewMergeClient(clientctx, "http://"+testServ.Listener.Addr().String(), "Raw", []any{
		&client,
	}, nil)
	ctest.NoError(t, err)
	err = client.Call(t.Context())
	ctest.IsError(t, err, context.DeadlineExceeded)

	closer()
}

type VarOptParamHandler struct{}

func (h *VarOptParamHandler) CallVariadic(ctx context.Context, ps ...string) (int, error) {
	return len(ps), nil
}

func lennilstring(opt Optional[string]) int {
	if opt == nil {
		return -1
	}
	return len(*opt)
}

func (h *VarOptParamHandler) CallOptional(ctx context.Context, opt Optional[string]) (int, error) {
	return lennilstring(opt), nil
}

func (h *VarOptParamHandler) CallOptionalLen(ctx context.Context, opt Optional[bool], opt2 Optional[bool]) (int, error) {
	var count int
	if opt != nil {
		count++
	}
	if opt2 != nil {
		count++
	}
	return count, nil
}

func (h *VarOptParamHandler) CallMixed(ctx context.Context, opt Optional[string], ps ...string) (common.Pair[int, int], error) {
	return common.NewPair(lennilstring(opt), len(ps)), nil
}

func TestCallWithOptionalVariadicParams(t *testing.T) {
	// setup server

	rpcServer := NewServer()
	rpcServer.Register("Raw", &VarOptParamHandler{})

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client
	var client struct {
		CallVariadic    func(ctx context.Context, ps ...string) (int, error)
		CallOptional    func(ctx context.Context, opt Optional[string]) (int, error)
		CallOptionalLen func(ctx context.Context, opt Optional[bool], opt2 Optional[bool]) (int, error)
		CallMixed       func(ctx context.Context, opt Optional[string], ps ...string) (common.Pair[int, int], error)
	}
	for _, proto := range []string{"ws", "http"} {
		t.Run(proto, func(t *testing.T) {
			closer, err := NewMergeClient(t.Context(), proto+"://"+testServ.Listener.Addr().String(), "Raw", []any{
				&client,
			}, nil)
			ctest.NoError(t, err)
			defer closer()

			n, err := client.CallVariadic(t.Context(), "test", "1")
			ctest.NoError(t, err)
			ctest.Equal(t, 2, n)

			n, err = client.CallVariadic(t.Context())
			ctest.NoError(t, err)
			ctest.Equal(t, 0, n)

			n, err = client.CallOptional(t.Context(), nil)
			ctest.NoError(t, err)
			ctest.Equal(t, -1, n)

			var teststring = "test"
			n, err = client.CallOptional(t.Context(), &teststring)
			ctest.NoError(t, err)
			ctest.Equal(t, 4, n)

			var testbool = true
			n, err = client.CallOptionalLen(t.Context(), &testbool, nil)
			ctest.NoError(t, err)
			ctest.Equal(t, 1, n)

			_, err = client.CallOptionalLen(t.Context(), nil, &testbool)
			ctest.IsError(t, err, ErrExtraOptionalParams)

			p, err := client.CallMixed(t.Context(), nil)
			ctest.NoError(t, err)
			ctest.Equal(t, p.Key, -1)
			ctest.Equal(t, p.Value, 0)

			p, err = client.CallMixed(t.Context(), &teststring)
			ctest.NoError(t, err)
			ctest.Equal(t, p.Key, 4)
			ctest.Equal(t, p.Value, 0)

			p, err = client.CallMixed(t.Context(), &teststring, "1")
			ctest.NoError(t, err)
			ctest.Equal(t, p.Key, 4)
			ctest.Equal(t, p.Value, 1)

			_, err = client.CallMixed(t.Context(), nil, "1")
			ctest.IsError(t, err, ErrExtraVariadicParams)
		})
	}
}

type RevCallTestServerHandler struct{}

func (h *RevCallTestServerHandler) Call(ctx context.Context) error {
	revClient, ok := ExtractReverseClient[RevCallTestClientProxy](ctx)
	if !ok {
		return fmt.Errorf("no reverse client")
	}

	r, err := revClient.CallOnClient(7) // multiply by 2 on client
	if err != nil {
		return fmt.Errorf("call on client: %w", err)
	}

	if r != 14 {
		return fmt.Errorf("unexpected result: %d", r)
	}

	return nil
}

type RevCallTestClientProxy struct {
	CallOnClient func(int) (int, error)
}

type RevCallTestClientHandler struct {
}

func (h *RevCallTestClientHandler) CallOnClient(a int) (int, error) {
	return a * 2, nil
}

func TestReverseCall(t *testing.T) {
	// setup server

	rpcServer := NewServer(WithReverseClient[RevCallTestClientProxy]("Client"))
	rpcServer.Register("Server", &RevCallTestServerHandler{})

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client

	var client struct {
		Call func() error
	}
	closer, err := NewMergeClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "Server", []any{
		&client,
	}, nil, WithClientHandler("Client", &RevCallTestClientHandler{}))
	ctest.NoError(t, err)

	// do the call!

	e := client.Call()
	ctest.NoError(t, e)

	closer()
}

type RevCallTestServerHandlerAliased struct {
}

func (h *RevCallTestServerHandlerAliased) Call(ctx context.Context) error {
	revClient, ok := ExtractReverseClient[RevCallTestClientProxyAliased](ctx)
	if !ok {
		return fmt.Errorf("no reverse client")
	}

	r, err := revClient.CallOnClient(8) // multiply by 2 on client
	if err != nil {
		return fmt.Errorf("call on client: %w", err)
	}

	if r != 16 {
		return fmt.Errorf("unexpected result: %d", r)
	}

	return nil
}

type RevCallTestClientProxyAliased struct {
	CallOnClient func(int) (int, error) `rpc_method:"rpc_thing"`
}

func TestReverseCallAliased(t *testing.T) {
	// setup server

	rpcServer := NewServer(WithReverseClient[RevCallTestClientProxyAliased]("Client"))
	rpcServer.Register("Server", &RevCallTestServerHandlerAliased{})

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client

	var client struct {
		Call func() error
	}
	closer, err := NewMergeClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "Server", []any{
		&client,
	}, nil, WithClientHandler("Client", &RevCallTestClientHandler{}), WithClientHandlerAlias("rpc_thing", "Client.CallOnClient"))
	ctest.NoError(t, err)

	// do the call!

	e := client.Call()
	ctest.NoError(t, e)

	closer()
}

// RevCallDropTestServerHandler attempts to make a client call on a closed connection.
type RevCallDropTestServerHandler struct {
	closeConn func()
	res       chan error
}

func (h *RevCallDropTestServerHandler) Call(ctx context.Context) error {
	revClient, ok := ExtractReverseClient[RevCallTestClientProxy](ctx)
	if !ok {
		return fmt.Errorf("no reverse client")
	}

	h.closeConn()
	time.Sleep(time.Second)

	_, err := revClient.CallOnClient(7)
	h.res <- err

	return nil
}

func TestReverseCallDroppedConn(t *testing.T) {
	// setup server

	hnd := &RevCallDropTestServerHandler{
		res: make(chan error),
	}

	rpcServer := NewServer(
		WithReverseClient[RevCallTestClientProxy]("Client"),
		WithServerTimeout(5*time.Second),
	)
	rpcServer.Register("Server", hnd)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client

	var client struct {
		Call func() error
	}
	closer, err := NewMergeClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "Server", []any{
		&client,
	}, nil, WithClientHandler("Client", &RevCallTestClientHandler{}))
	ctest.NoError(t, err)

	hnd.closeConn = closer

	// do the call!
	e := client.Call()

	ctest.Error(t, e)
	ctest.True20(t, strings.Contains, e.Error(), "websocket connection closed")

	res := <-hnd.res
	if errors.Is(res, ErrWsExiting) {
		return
	}
	istimeout := ctest.AsErrorType[timeouter](t, res).Timeout()
	ctest.True(t, istimeout)
}

type timeouter interface {
	error
	Timeout() bool
}

type BigCallTestServerHandler struct {
}

type RecRes struct {
	I int
	R []RecRes
}

func (h *BigCallTestServerHandler) Do() (RecRes, error) {
	var res RecRes
	res.I = 123

	for i := range 15000 {
		var ires RecRes
		ires.I = i

		for j := range 15000 {
			var jres RecRes
			jres.I = j

			ires.R = append(ires.R, jres)
		}

		res.R = append(res.R, ires)
	}

	fmt.Println("sending result")

	return res, nil
}

func (h *BigCallTestServerHandler) Ch(ctx context.Context) (<-chan int, error) {
	out := make(chan int)

	go func() {
		var i int
		for {
			select {
			case <-ctx.Done():
				fmt.Println("closing")
				close(out)
				return
			case <-time.After(time.Second):
			}
			fmt.Println("sending")
			out <- i
			i++
		}
	}()

	return out, nil
}

// TestBigResult tests that the connection doesn't die when sending a large result,
// and that requests which happen while a large result is being sent don't fail.
func TestBigResult(t *testing.T) {
	if os.Getenv("I_HAVE_A_LOT_OF_MEMORY_AND_TIME") != "1" {
		// needs ~40GB of memory and ~4 minutes to run
		t.Skip("skipping test due to required resources, set I_HAVE_A_LOT_OF_MEMORY_AND_TIME=1 to run")
	}

	// setup server

	serverHandler := &BigCallTestServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", serverHandler)

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()
	// setup client

	var client struct {
		Do func() (RecRes, error)
		Ch func(ctx context.Context) (<-chan int, error)
	}
	closer, err := NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "SimpleServerHandler", &client, nil)
	ctest.NoError(t, err)
	defer closer()

	chctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// client.Ch will generate some requests, which will require websocket locks,
	// and before fixes in #97 would cause deadlocks / timeouts when combined with
	// the large result processing from client.Do
	ch, err := client.Ch(chctx)
	ctest.NoError(t, err)

	prevN := <-ch

	go func() {
		for n := range ch {
			if n != prevN+1 {
				panic("bad order")
			}
			prevN = n
		}
	}()

	_, err = client.Do()
	ctest.NoError(t, err)

	fmt.Println("done")
}

func TestNewCustomClient(t *testing.T) {
	// Setup server
	serverHandler := &SimpleServerHandler{}
	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", serverHandler)

	// Custom doRequest function
	doRequest := func(ctx context.Context, body []byte) (io.ReadCloser, error) {
		reader := bytes.NewReader(body)
		pr, pw := io.Pipe()
		go func() {
			defer func() { _ = pw.Close() }()
			rpcServer.HandleRequest(ctx, reader, pw)
		}()
		return pr, nil
	}

	var client struct {
		Add    func(int) error
		AddGet func(int) int
	}

	// Create custom client
	closer, err := NewCustomClient("SimpleServerHandler", []any{&client}, doRequest)
	ctest.NoError(t, err)
	defer closer()

	// Add(int) error
	ctest.NoError(t, client.Add(10))
	ctest.Equal(t, int32(10), serverHandler.n)

	err = client.Add(-3546)
	ctest.Equal(t, err.Error(), "test")

	// AddGet(int) int
	n := client.AddGet(3)
	ctest.Equal(t, 13, n)
	ctest.Equal(t, int32(13), serverHandler.n)
}

func TestReverseCallWithCustomMethodName(t *testing.T) {
	// setup server

	rpcServer := NewServer(WithServerMethodNameFormatter(func(namespace, method string) string { return namespace + "_" + method }))
	rpcServer.Register("Server", &ObjectParamHandler{})

	// httptest stuff
	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// setup client

	var client struct {
		Call func(ctx context.Context, ps Object[CustomParams]) error `rpc_method:"Server_Call"`
	}
	closer, err := NewMergeClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "Server", []any{
		&client,
	}, nil)
	ctest.NoError(t, err)

	// do the call!

	e := client.Call(t.Context(), GetObject(CustomParams{I: 1}))
	ctest.NoError(t, e)

	closer()
}

type nilcheckerserver struct{}

func (nilcheckerserver) CheckMapNil(m map[string]struct{}, isNil bool) error {
	if (m == nil) == isNil {
		return nil
	}
	return errors.New("map nil not matched")
}

func (nilcheckerserver) CheckSliceNil(m []struct{}, isNil bool) error {
	if (m == nil) == isNil {
		return nil
	}
	return errors.New("slice nil not matched")
}

func TestNilRoundTrip(t *testing.T) {
	serverHandler := nilcheckerserver{}
	rpcServer := NewServer()
	rpcServer.Register("NilCheckerServer", serverHandler)

	var client struct {
		CheckSliceNil func(m []struct{}, isNil bool) error
		CheckMapNil   func(m map[string]struct{}, isNil bool) error
	}

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()
	// setup client

	closer, err := NewClient(t.Context(), "ws://"+testServ.Listener.Addr().String(), "NilCheckerServer", &client, nil)
	ctest.NoError(t, err)
	defer closer()

	ctest.NoError(t, client.CheckMapNil(nil, true))
	ctest.NoError(t, client.CheckMapNil(make(map[string]struct{}), false))
	ctest.NoError(t, client.CheckSliceNil(nil, true))
	ctest.NoError(t, client.CheckSliceNil(make([]struct{}, 0), false))
}

func TestContext(t *testing.T) {
	ctx := context.WithValue(t.Context(), nilcheckerserver{}, "test")
	opt := WithContext(jsonDefault(), ctx)

	extracted := ContextFrom(opt)

	ctest.Equal(t, ctx, extracted, "extracted context is not the original one")
}

func TestContentTypeHeader(t *testing.T) {
	rpcHandler := SimpleServerHandler{}

	rpcServer := NewServer()
	rpcServer.Register("SimpleServerHandler", &rpcHandler)

	testServ := httptest.NewServer(rpcServer)
	defer testServ.Close()

	// Test that the Content-Type header is set correctly
	resp, err := http.Post(testServ.URL, "application/json", strings.NewReader(`{"jsonrpc": "2.0", "method": "SimpleServerHandler.Inc", "params": [], "id": 1}`))
	ctest.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	contentType := resp.Header.Get("Content-Type")
	ctest.Equal(t, "application/json", contentType, "Content-Type header should be application/json")

	// Verify the response is still valid JSON
	var jsonResp response
	err = json.UnmarshalRead(resp.Body, &jsonResp)
	ctest.NoError(t, err)
	ctest.Equal(t, "2.0", jsonResp.Jsonrpc)
	ctest.Equal[any](t, float64(1), jsonResp.ID) // JSON numbers are unmarshaled as float64
}
