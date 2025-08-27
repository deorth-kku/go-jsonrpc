package httpio

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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
			closer, err := jsonrpc.NewMergeClient(context.Background(), url, "ReaderHandler", []interface{}{&client}, nil, pe, rd)
			require.NoError(t, err)
			defer closer()

			const potatos = "pooooootato"

			read, err := client.ReadAll(context.TODO(), strings.NewReader(potatos))
			require.NoError(t, err)
			require.Equal(t, potatos, string(read), "potatos weren't equal")

			reader, err := client.StringReader(context.TODO(), potatos)
			require.NoError(t, err)
			str, err := readString(reader)
			require.NoError(t, err)
			require.Equal(t, potatos, str, "potatos weren't equal")
		})
	}
}
