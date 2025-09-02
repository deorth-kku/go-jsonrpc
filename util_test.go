package jsonrpc

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/deorth-kku/go-common"
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
	common.SetLogRaw(os.Stderr, slog.LevelDebug, common.TextFormat)
	slog.Error("test stack", "test", 1, "stack", stackstring{})
}

func getreq() *request {
	list := make([]string, 1)
	return &request{
		Jsonrpc: "2.0",
		ID:      1,
		Method:  "test",
		Params:  getParam(common.AnySlice(list)...),
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

func BenchmarkDiscard(b *testing.B) {
	rd := NewJsonReader(getreq())
	b.ResetTimer()
	for b.Loop() {
		_, _ = rd.WriteTo(io.Discard)
	}
}

func BenchmarkLen(b *testing.B) {
	rd := NewJsonReader(getreq())
	b.ResetTimer()
	for b.Loop() {
		_, _ = rd.Len()
	}
}

func TestRead(t *testing.T) {
	rd := NewJsonReader(getreq())
	discard := make([]byte, 10)
	var err error
	count := 0
	for {
		var l int
		l, err = rd.Read(discard)
		if err != nil {
			break
		}
		count += l
	}
	fmt.Println(count, err)
	fmt.Println(rd.Len())
}
