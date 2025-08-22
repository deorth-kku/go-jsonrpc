package jsonrpc

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/deorth-kku/go-common"
)

type response struct {
	Jsonrpc string        `json:"jsonrpc"`
	Result  interface{}   `json:"result,omitzero"`
	ID      interface{}   `json:"id,omitzero"`
	Error   *JSONRPCError `json:"error,omitzero"`
}

var (
	_ json.UnmarshalerFrom = (*response)(nil)
	_ json.MarshalerTo     = (*response)(nil)
)

func (r response) MarshalJSONTo(enc *jsontext.Encoder) error {
	// Custom marshal logic as per JSON-RPC 2.0 spec:
	// > `result`:
	// > This member is REQUIRED on success.
	// > This member MUST NOT exist if there was an error invoking the method.
	//
	// > `error`:
	// > This member is REQUIRED on error.
	// > This member MUST NOT exist if there was no error triggered during invocation.

	err := enc.WriteToken(jsontext.BeginObject)
	if err != nil {
		return err
	}
	m := common.PairSlice[string, any]{
		common.NewPair("jsonrpc", any(r.Jsonrpc)),
		common.NewPair("id", r.ID),
		common.NewPair("result", r.Result),
	}
	if r.Error != nil {
		m[2] = common.NewPair("error", any(r.Error))
	}
	for k, v := range m.Range {
		err = json.MarshalEncode(enc, k)
		if err != nil {
			return err
		}
		err = json.MarshalEncode(enc, v)
		if err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

func (f *response) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	type t0 response
	err := json.UnmarshalDecode(dec, (*t0)(f))
	if err != nil {
		return err
	}
	f.ID, err = normalizeID(f.ID)
	return err
}

type JSONRPCError struct {
	Code    ErrorCode      `json:"code"`
	Message string         `json:"message"`
	Meta    jsontext.Value `json:"meta,omitzero"`
	Data    interface{}    `json:"data,omitzero"`
}

func (e *JSONRPCError) Error() string {
	if e.Code >= -32768 && e.Code <= -32000 {
		return fmt.Sprintf("RPC error (%d): %s", e.Code, e.Message)
	}
	return e.Message
}

var (
	_             error = (*JSONRPCError)(nil)
	marshalableRT       = reflect.TypeFor[marshalable]()
	errorCodecRT        = reflect.TypeFor[RPCErrorCodec]()
)

func (e *JSONRPCError) val(errors *Errors) reflect.Value {
	if errors != nil {
		t, ok := errors.byCode[e.Code]
		if ok {
			var v reflect.Value
			if t.Kind() == reflect.Ptr {
				v = reflect.New(t.Elem())
			} else {
				v = reflect.New(t)
			}

			if v.Type().Implements(errorCodecRT) {
				if err := common.MustOk(reflect.TypeAssert[RPCErrorCodec](v)).FromJSONRPCError(*e); err != nil {
					slog.Error("Error converting JSONRPCError to custom error", "type", t.String(), "code", e.Code, "err", err)
					return reflect.ValueOf(e)
				}
			} else if len(e.Meta) > 0 && v.Type().Implements(marshalableRT) {
				if err := common.MustOk(reflect.TypeAssert[marshalable](v)).UnmarshalJSON(e.Meta); err != nil {
					slog.Error("Error unmarshalling error metadata to custom error", "type", t.String(), "code", e.Code, "err", err)
					return reflect.ValueOf(e)
				}
			}

			if t.Kind() != reflect.Ptr {
				v = v.Elem()
			}
			return v
		}
	}

	return reflect.ValueOf(e)
}
