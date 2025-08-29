package jsonrpc

import (
	"encoding/json"
	"testing"
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
