package jsonrpc

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"slices"
	"time"
)

type param struct {
	data []byte // from unmarshal

	v reflect.Value // to marshal
}

var (
	_ json.UnmarshalerFrom = (*param)(nil)
	_ json.MarshalerTo     = (*param)(nil)
)

func (p *param) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	v, err := dec.ReadValue()
	if err != nil {
		return err
	}
	p.data = slices.Clone(v)
	return nil
}

func (p *param) MarshalJSONTo(enc *jsontext.Encoder) error {
	if p.v.Kind() == reflect.Invalid {
		return enc.WriteValue(p.data)
	}
	return json.MarshalEncode(enc, p.v.Interface())
}

// processFuncOut finds value and error Outs in function
func processFuncOut(funcType reflect.Type) (valOut int, errOut int, n int) {
	errOut = -1 // -1 if not found
	valOut = -1
	n = funcType.NumOut()

	switch n {
	case 0:
	case 1:
		if funcType.Out(0) == errorType {
			errOut = 0
		} else {
			valOut = 0
		}
	case 2:
		valOut = 0
		errOut = 1
		if funcType.Out(1) != errorType {
			panic("expected error as second return value")
		}
	default:
		errstr := fmt.Sprintf("too many return values: %s", funcType)
		panic(errstr)
	}

	return
}

type backoff struct {
	minDelay time.Duration
	maxDelay time.Duration
}

func (b *backoff) next(attempt int) time.Duration {
	if attempt < 0 {
		return b.minDelay
	}

	minf := float64(b.minDelay)
	durf := minf * math.Pow(1.5, float64(attempt))
	durf = durf + rand.Float64()*minf

	delay := time.Duration(durf)

	if delay > b.maxDelay {
		return b.maxDelay
	}

	return delay
}
