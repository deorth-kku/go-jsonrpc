package httpio

import (
	"net/http"
	"strings"

	"github.com/filecoin-project/go-jsonrpc"
)

func MuxRPCServerWithReader(rpc *jsonrpc.RPCServer, paramDecoder, resultEncoder http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if resultEncoder == nil {
				break
			}
			if strings.EqualFold(r.Header.Get("Connection"), "upgrade") {
				// ws connection alway handle by RPCServer
				break
			}
			resultEncoder(w, r)
			return
		case http.MethodPost:
			if paramDecoder == nil {
				break
			}
			if strings.EqualFold(r.Header.Get(contentType), streamContentType) {
				paramDecoder(w, r)
				return
			}
		}
		rpc.ServeHTTP(w, r)
	}
}
