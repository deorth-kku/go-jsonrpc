package jsonrpc

import (
	"encoding/json"
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
