package httpio

import (
	"context"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ctest "github.com/deorth-kku/go-common/test"
	"github.com/filecoin-project/go-jsonrpc"
)

type ReaderHandler struct {
}

func (h *ReaderHandler) ReadAll(ctx context.Context, r io.Reader) ([]byte, error) {
	return io.ReadAll(r)
}

func (h *ReaderHandler) StringReader(ctx context.Context, u string) (io.Reader, error) {
	return strings.NewReader(u), nil
}

func TestReaderProxy(t *testing.T) {
	serverHandler := &ReaderHandler{}

	pdh, pdopt := ReaderParamDecoder()
	reh, reopt := ReaderResultEncoder()
	rpcServer := jsonrpc.NewServer(pdopt, reopt)
	rpcServer.Register("ReaderHandler", serverHandler)

	mux := MuxRPCServerWithReader(rpcServer, pdh, reh)
	testServ := httptest.NewServer(mux)
	defer testServ.Close()

	full_url := "http://" + testServ.Listener.Addr().String() + "/rpc"
	rd := ReaderResultDecoder(full_url)
	pe := ReaderParamEncoder(full_url)

	for name, url := range map[string]string{
		"http": full_url, // since http uses POST and ws uses GET, we need to test both to make sure [MuxRPCServerWithReader] works as intended
		"ws":   "ws://" + testServ.Listener.Addr().String() + "/rpc",
	} {
		t.Run(name, func(t *testing.T) {
			var client struct {
				ReadAll      func(ctx context.Context, r io.Reader) ([]byte, error)
				StringReader func(ctx context.Context, u string) (io.Reader, error)
			}
			closer, err := jsonrpc.NewMergeClient(t.Context(), url, "ReaderHandler", []any{&client}, nil, pe, rd)
			ctest.NoError(t, err)
			defer closer()

			const potatos = "pooooootato"

			read, err := client.ReadAll(t.Context(), strings.NewReader(potatos))
			ctest.NoError(t, err)
			ctest.Equal(t, potatos, string(read), "potatos weren't equal")

			reader, err := client.StringReader(t.Context(), potatos)
			ctest.NoError(t, err)
			str, err := readString(reader)
			ctest.NoError(t, err)
			ctest.Equal(t, potatos, str, "potatos weren't equal")
		})
	}
}

type block struct {
	ctx context.Context
}

func (b *block) SetContext(ctx context.Context) {
	b.ctx = ctx
}

func (b *block) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (h *ReaderHandler) BlockReader() (io.Reader, error) {
	return new(block), nil
}

func TestBlockReader(t *testing.T) {
	serverHandler := &ReaderHandler{}

	reh, reopt := ReaderResultEncoder()
	rpcServer := jsonrpc.NewServer(reopt)
	rpcServer.Register("ReaderHandler", serverHandler)

	mux := MuxRPCServerWithReader(rpcServer, nil, reh)
	testServ := httptest.NewServer(mux)
	defer testServ.Close()

	full_url := "http://" + testServ.Listener.Addr().String() + "/rpc"
	rd := ReaderResultDecoder(full_url)

	for url, expected := range map[string]error{
		full_url: context.DeadlineExceeded,
		"ws://" + testServ.Listener.Addr().String() + "/rpc": net.ErrClosed,
	} {
		name, _, _ := strings.Cut(url, "://")
		t.Run(name, func(t *testing.T) {
			var client struct {
				BlockReader func() (io.Reader, error)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			closer, err := jsonrpc.NewMergeClient(ctx, url, "ReaderHandler", []any{&client}, nil, rd, jsonrpc.WithTimeout(-1))
			ctest.NoError(t, err)
			defer closer()

			_, err = client.BlockReader()
			ctest.IsError(t, err, expected)
		})
	}
}
