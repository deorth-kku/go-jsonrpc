package jsonrpc

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"time"
)

type params struct {
	getMethodHandler func() (methodHandler, bool)
	values           []reflect.Value
}

var (
	_ json.UnmarshalerFrom = (*params)(nil)
	_ json.MarshalerTo     = (*params)(nil)
)

type paramError string

func (pe paramError) Error() string {
	return string(pe)
}

const (
	errNoParam     paramError = "no params when params is required"
	errShortParams paramError = "not enough params"
	errExtraParams paramError = "extra params"
	errNotAnArray  paramError = "param value is not an array"
)

func (p *params) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	var handler methodHandler
	var ok bool
	if p.getMethodHandler != nil {
		handler, ok = p.getMethodHandler()
	}
	if !ok {
		data, err := dec.ReadValue()
		if err != nil {
			return err
		}
		p.values = []reflect.Value{
			reflect.ValueOf(deferredData{slices.Clone(data), dec.Options()}),
		}
		return nil
	}
	if handler.hasObjectParams {
		rp := reflect.New(handler.paramReceivers[0])
		if err := json.UnmarshalDecode(dec, rp.Interface()); err != nil {
			return err
		}
		p.values = []reflect.Value{rp.Elem()}
		return nil
	}
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	switch tok.Kind() {
	case '[':
	case 'n':
		if handler.nParams != 0 {
			return errNoParam
		}
		p.values = nil
		return nil
	default:
		return errNotAnArray
	}
	p.values = make([]reflect.Value, handler.nParams)
	for i := range handler.nParams {
		if dec.PeekKind() == ']' {
			return errShortParams
		}
		rp := reflect.New(handler.paramReceivers[i])
		err = json.UnmarshalDecode(dec, rp.Interface())
		if err != nil {
			return err
		}
		p.values[i] = rp.Elem()
	}
	tok, err = dec.ReadToken()
	if err != nil {
		return err
	}
	if tok.Kind() != ']' {
		return errExtraParams
	}
	return nil
}

func (p *params) getdeferredData() (deferredData, bool) {
	if len(p.values) != 1 {
		return deferredData{}, false
	}
	return reflect.TypeAssert[deferredData](p.values[0])
}

func (p *params) deferredUnmarshal(mh methodHandler) ([]byte, error) {
	dd, ok := p.getdeferredData()
	if !ok {
		return nil, nil
	}
	p.getMethodHandler = func() (methodHandler, bool) { return mh, true }
	dec := jsontext.NewDecoder(bytes.NewReader(dd.data), dd.opt)
	return dd.data, p.UnmarshalJSONFrom(dec)
}

func (p params) MarshalJSONTo(enc *jsontext.Encoder) error {
	if len(p.values) == 1 && p.values[0].Type().Implements(isObjectType) {
		return json.MarshalEncode(enc, p.values[0].Interface())
	}
	anylist := make([]any, len(p.values))
	for i, v := range p.values {
		anylist[i] = v.Interface()
	}
	return json.MarshalEncode(enc, anylist)
}

func getParam(args ...any) params {
	p := params{
		values: make([]reflect.Value, len(args)),
	}
	for i, v := range args {
		p.values[i] = reflect.ValueOf(v)
	}
	return p
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

var ErrDataTooLarge = errors.New("request bigger than maximum allowed")

// LimitReader returns a Reader that reads from r
// but stops with [ErrDataTooLarge] after n bytes.
// The underlying implementation is a *LimitedReader.
func LimitReader(r io.Reader, n int64) io.Reader { return &LimitedReader{r, n} }

type LimitedReader struct {
	R io.Reader // underlying reader
	N int64     // max bytes remaining
}

func (l *LimitedReader) Read(p []byte) (n int, err error) {
	if l.N <= 0 {
		return 0, ErrDataTooLarge
	}
	if int64(len(p)) > l.N {
		p = p[0:l.N]
	}
	n, err = l.R.Read(p)
	l.N -= int64(n)
	return
}

type JsonReader struct {
	data    any
	options json.Options
	rd      *io.PipeReader
}

func NewJsonReader(data any, opts ...json.Options) *JsonReader {
	return &JsonReader{
		data:    data,
		options: json.JoinOptions(opts...),
	}
}

func (j *JsonReader) Read(b []byte) (int, error) {
	if j.rd == nil {
		var wt *io.PipeWriter
		j.rd, wt = io.Pipe()
		go func(wt io.WriteCloser) {
			defer wt.Close()
			j.WriteTo(wt)
		}(wt)
	}
	return j.rd.Read(b)
}

func (j JsonReader) WriteTo(rd io.Writer) (int64, error) {
	enc := jsontext.NewEncoder(rd, j.options)
	err := json.MarshalEncode(enc, j.data)
	return enc.OutputOffset(), err
}

type countWriter struct {
	n int
}

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += len(p)
	return len(p), nil
}

func (j JsonReader) Len() (int64, error) {
	wt := new(countWriter)
	err := json.MarshalWrite(wt, j.data, j.options)
	return int64(wt.n), err
}

type stackstring struct{}

func (stackstring) LogValue() slog.Value {
	const bufsize = 4096
	buf := make([]byte, bufsize)
	l := runtime.Stack(buf, false)
	buf = buf[:l]
	return slog.StringValue(string(buf))
}

type teeValue struct {
	str *strings.Builder
	r   io.Reader
}

func (t teeValue) LogValue() slog.Value {
	return slog.StringValue(t.str.String())
}

func (t teeValue) Reader() io.Reader {
	return t.r
}

func NewTeeLogValue(r io.Reader) teeValue {
	v := teeValue{str: new(strings.Builder)}
	v.r = io.TeeReader(r, v.str)
	return v
}
