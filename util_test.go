package jsonrpc

import (
	"encoding/json/v2"
	"log/slog"
	"os"
	"testing"

	cslices "github.com/deorth-kku/go-common/datatypes/slices"
	clog "github.com/deorth-kku/go-common/log"
)

func TestParams(t *testing.T) {
	data := "{\"jsonrpc\": \"2.0\", \"method\": \"SimpleServerHandler.Inc\", \"params\": null, \"id\": 1}"
	var req request
	req.Params.getMethodHandler = func() (methodHandler, bool) {
		return methodHandler{}, true
	}
	err := json.Unmarshal([]byte(data), &req)
	if err != nil {
		t.Error(err)
		return
	}
}

var _ slog.LogValuer = stackstring{}

func TestStack(t *testing.T) {
	clog.SetRaw(os.Stderr, slog.LevelDebug, clog.TextFormat)
	slog.Error("test stack", "test", 1, "stack", stackstring{})
}

func getreq() *request {
	list := make([]string, 1)
	return &request{
		Jsonrpc: "2.0",
		ID:      1,
		Method:  "test",
		Params:  getParam(cslices.ToAny(list)...),
	}
}

func BenchmarkJsonMarshal(b *testing.B) {
	data := getreq()
	b.ResetTimer()
	for b.Loop() {
		data, _ := json.Marshal(data)
		_ = len(data)
	}
}
