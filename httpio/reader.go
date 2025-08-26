package httpio

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"

	"github.com/deorth-kku/go-common"
	"github.com/google/uuid"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-jsonrpc"
)

// this use [slog.Logger] and [http.Client] from [jsonrpc.Config].
// if you're using [jsonrpc.WithLogger] or [jsonrpc.WithHTTPClient] as well, make sure they're before this one.
func ReaderParamEncoder(addr string) jsonrpc.Option {
	return func(c *jsonrpc.Config) {
		logger := c.GetLogger()
		client := c.GetHTTPClient()
		jsonrpc.WithParamMarshaler(func(enc *jsontext.Encoder, rd io.Reader) error {
			u, err := url.Parse(addr)
			if err != nil {
				return err
			}
			reqID := uuid.New()
			query := u.Query()
			query.Add("uuid", reqID.String())
			u.RawQuery = query.Encode()
			go func() {
				// TODO: figure out errors here
				resp, err := client.Post(u.String(), "application/octet-stream", rd)
				if err != nil {
					logger.Error("sending reader param", "err", err)
					return
				}

				defer resp.Body.Close()

				if resp.StatusCode != 200 {
					logger.Error("sending reader param: non-200 status", "code", resp.Status)
					return
				}

			}()

			return json.MarshalEncode(enc, reqID)
		})(c)
	}
}

type waitReadCloser struct {
	io.ReadCloser
	wait chan struct{}
}

func (w *waitReadCloser) Read(p []byte) (int, error) {
	n, err := w.ReadCloser.Read(p)
	if err != nil {
		close(w.wait)
	}
	return n, err
}

func (w *waitReadCloser) Close() error {
	close(w.wait)
	return w.ReadCloser.Close()
}

func ReaderParamDecoder() (http.HandlerFunc, jsonrpc.ServerOption) {
	var readers sync.Map // value is [chan *waitReadCloser]
	var logger *slog.Logger

	hnd := func(resp http.ResponseWriter, req *http.Request) {
		strId := req.URL.Query().Get("uuid")
		if len(strId) == 0 {
			http.Error(resp, "no uuid arg", 400)
			return
		}
		u, err := uuid.Parse(strId)
		if err != nil {
			http.Error(resp, fmt.Sprintf("parsing reader uuid: %s", err), 400)
			return
		}
		defer readers.Delete(u)

		ch := common.Drop1(readers.LoadOrStore(u, make(chan *waitReadCloser))).(chan *waitReadCloser)
		logger.Debug("reader handler get channel", "uuid", u, "ch", ch)

		wr := &waitReadCloser{
			ReadCloser: req.Body,
			wait:       make(chan struct{}),
		}

		select {
		case ch <- wr:
		case <-req.Context().Done():
			logger.Error("context error in reader stream handler (1)", "err", req.Context().Err())
			resp.WriteHeader(500)
			return
		}

		select {
		case <-wr.wait:
		case <-req.Context().Done():
			logger.Error("context error in reader stream handler (2)", "err", req.Context().Err())
			resp.WriteHeader(500)
			return
		}

		resp.WriteHeader(200)
	}

	dec := func(c *jsonrpc.ServerConfig) {
		logger = c.GetLogger()
		jsonrpc.WithParamUnmarshaler(func(dec *jsontext.Decoder, rd *io.Reader) error {
			var u uuid.UUID
			if err := json.UnmarshalDecode(dec, &u); err != nil {
				return xerrors.Errorf("unmarshaling reader id: %w", err)
			}
			defer readers.Delete(u)

			ch := common.Drop1(readers.LoadOrStore(u, make(chan *waitReadCloser))).(chan *waitReadCloser)
			logger.Debug("reader unmarshal get channel", "uuid", u, "ch", ch)
			*rd = <-ch
			return nil
		})(c)
	}
	return hnd, dec
}
