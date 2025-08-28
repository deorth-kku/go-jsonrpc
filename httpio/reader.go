package httpio

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/deorth-kku/go-common"
	"github.com/google/uuid"

	"github.com/filecoin-project/go-jsonrpc"
)

func urlWithUUID(ustr string, id uuid.UUID) (*url.URL, error) {
	u, err := url.Parse(ustr)
	if err != nil {
		return nil, err
	}
	query := u.Query()
	query.Add("uuid", id.String())
	u.RawQuery = query.Encode()
	return u, nil
}

func newRequest(u *url.URL, method string, body io.Reader, timeout time.Duration) (*http.Request, context.CancelFunc) {
	req := &http.Request{
		Method: method,
		URL:    u,
		Header: make(http.Header),
	}
	if body != nil {
		var ok bool
		req.Body, ok = body.(io.ReadCloser)
		if !ok {
			req.Body = io.NopCloser(body)
		}
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if timeout == 0 {
		ctx, cancel = context.WithCancel(context.Background())
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
	}
	return req.WithContext(ctx), cancel
}

// this use [slog.Logger] and [http.Client] from [jsonrpc.Config].
// if you're using [jsonrpc.WithLogger] or [jsonrpc.WithHTTPClient] as well, make sure they're before this one.
// Parameters:
//   - addr: the full http url that reader server is served on.
//   - timeout: timeout for the whole http request. 0 for no timeout.
func ReaderParamEncoder(addr string) jsonrpc.Option {
	return func(c *jsonrpc.Config) {
		logger := c.GetLogger()
		client := c.GetHTTPClient()
		timeout := c.GetTimeout()
		jsonrpc.WithParamMarshaler(func(enc *jsontext.Encoder, rd io.Reader) error {
			reqID := uuid.New()
			u, err := urlWithUUID(addr, reqID)
			if err != nil {
				return err
			}
			go func() {
				// TODO: figure out errors here
				req, cancel := newRequest(u, http.MethodPost, rd, timeout)
				defer cancel()
				req.Header.Set(contentType, streamContentType)
				resp, err := client.Do(req)
				if err != nil {
					logger.Error("sending reader param", "err", err)
					return
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					logger.Error("sending reader param: non-200 status", "code", resp.Status)
					return
				}
			}()

			return json.MarshalEncode(enc, reqID)
		})(c)
	}
}

func ReaderResultDecoder(addr string) jsonrpc.Option {
	return func(c *jsonrpc.Config) {
		client := c.GetHTTPClient()
		timeout := c.GetTimeout()
		jsonrpc.WithResultUnmarshaler(func(dec *jsontext.Decoder, rd *io.Reader) error {
			var reqID uuid.UUID
			err := json.UnmarshalDecode(dec, &reqID)
			if err != nil {
				return err
			}
			u, err := urlWithUUID(addr, reqID)
			if err != nil {
				return err
			}
			req, cancel := newRequest(u, http.MethodGet, nil, timeout)
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				defer resp.Body.Close()
				str, err := readString(resp.Body)
				if err != nil {
					return fmt.Errorf("error when getting reader, status %d, uuid: %s, err: %w", resp.StatusCode, reqID, err)
				} else {
					return fmt.Errorf("error when getting reader, status %d, uuid: %s, msg: %s", resp.StatusCode, reqID, str)
				}
			}
			ctx, wrc := newWaitReader(req.Context(), resp.Body)
			*rd = wrc
			context.AfterFunc(ctx, cancel)
			return nil
		})(c)
	}
}

func readString(rd io.Reader) (string, error) {
	str := new(strings.Builder)
	_, err := io.Copy(str, rd)
	if err != nil {
		return "", err
	}
	return str.String(), nil
}

type waitReadCloser struct {
	io.ReadCloser
	cancal context.CancelCauseFunc
}

func newWaitReader(ctx context.Context, reader io.ReadCloser) (context.Context, *waitReadCloser) {
	wrc := &waitReadCloser{
		ReadCloser: reader,
	}
	ctx, wrc.cancal = context.WithCancelCause(ctx)
	return ctx, wrc
}

func (w *waitReadCloser) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	if err != nil {
		w.cancal(err)
	}
	return n, err
}

func (w *waitReadCloser) Close() error {
	err := w.ReadCloser.Close()
	w.cancal(err)
	return err
}

const (
	contentType       = "Content-Type"
	streamContentType = "application/octet-stream"
)

func ReaderParamDecoder() (http.HandlerFunc, jsonrpc.ServerOption) {
	var readers sync.Map // value is [chan *waitReadCloser]
	var logger *slog.Logger

	hnd := func(resp http.ResponseWriter, req *http.Request) {
		strId := req.URL.Query().Get("uuid")
		if len(strId) == 0 {
			http.Error(resp, "no uuid arg", http.StatusBadRequest)
			return
		}
		u, err := uuid.Parse(strId)
		if err != nil {
			http.Error(resp, fmt.Sprintf("parsing reader uuid: %s", err), http.StatusBadRequest)
			return
		}
		defer readers.Delete(u)

		ch := common.Drop1(readers.LoadOrStore(u, make(chan *waitReadCloser))).(chan *waitReadCloser)
		logger.Debug("reader handler get channel", "uuid", u, "ch", ch)

		ctx, wrc := newWaitReader(req.Context(), req.Body)

		select {
		case ch <- wrc:
		case <-req.Context().Done():
			logger.Error("context error in reader stream handler (1)", "err", req.Context().Err())
			resp.WriteHeader(http.StatusInternalServerError)
			return
		}

		select {
		case <-ctx.Done():
		case <-req.Context().Done():
			logger.Error("context error in reader stream handler (2)", "err", req.Context().Err())
			resp.WriteHeader(http.StatusInternalServerError)
			return
		}

		resp.WriteHeader(http.StatusOK)
	}

	dec := func(c *jsonrpc.ServerConfig) {
		logger = c.GetLogger()
		jsonrpc.WithParamUnmarshaler(func(dec *jsontext.Decoder, rd *io.Reader) error {
			var u uuid.UUID
			if err := json.UnmarshalDecode(dec, &u); err != nil {
				return fmt.Errorf("unmarshaling reader id: %w", err)
			}
			defer readers.Delete(u)

			ch := common.Drop1(readers.LoadOrStore(u, make(chan *waitReadCloser))).(chan *waitReadCloser)
			logger.Debug("reader unmarshal get channel", "uuid", u, "ch", ch)
			ctx := jsonrpc.ContextFrom(dec.Options())
			select {
			case *rd = <-ch:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})(c)
	}
	return hnd, dec
}

func ReaderResultEncoder() (http.HandlerFunc, jsonrpc.ServerOption) {
	var readers sync.Map // map[uuid.UUID]io.Reader
	var logger *slog.Logger

	hnd := func(resp http.ResponseWriter, req *http.Request) {
		strId := req.URL.Query().Get("uuid")
		if len(strId) == 0 {
			http.Error(resp, "no uuid arg", http.StatusBadRequest)
			return
		}
		u, err := uuid.Parse(strId)
		if err != nil {
			http.Error(resp, fmt.Sprintf("parsing reader uuid: %s, %s", strId, err), http.StatusBadRequest)
			return
		}
		rd, ok := jsonrpc.LoadAssert[io.Reader](readers.LoadAndDelete, u)
		if !ok {
			http.Error(resp, fmt.Sprintf("reader not found by uuid: %s", strId), http.StatusBadRequest)
		}
		resp.Header().Set(contentType, streamContentType)
		_, err = io.Copy(resp, rd)
		if err != nil {
			logger.Warn("error when sending reader to client", "uuid", u, "err", err)
		}
	}

	enc := func(c *jsonrpc.ServerConfig) {
		logger = c.GetLogger()
		jsonrpc.WithResultMarshaler(func(enc *jsontext.Encoder, rd io.Reader) error {
			u := uuid.New()
			if err := json.MarshalEncode(enc, u); err != nil {
				return fmt.Errorf("marshaling reader id: %w", err)
			}
			readers.Store(u, rd)
			return nil
		})(c)
	}
	return hnd, enc
}
