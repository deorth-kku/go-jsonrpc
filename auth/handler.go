package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

type Handler struct {
	Verify func(ctx context.Context, token string) ([]Permission, error)
	Next   http.HandlerFunc
	Logger *slog.Logger
}

func (h *Handler) getlogger() *slog.Logger {
	if h.Logger == nil {
		return slog.Default()
	}
	return h.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	token := r.Header.Get("Authorization")
	if token == "" {
		token = r.FormValue("token")
		if token != "" {
			token = "Bearer " + token
		}
	}

	if token != "" {
		if !strings.HasPrefix(token, "Bearer ") {
			h.getlogger().Warn("missing Bearer prefix in auth header")
			w.WriteHeader(401)
			return
		}
		token = strings.TrimPrefix(token, "Bearer ")

		allow, err := h.Verify(ctx, token)
		if err != nil {
			h.getlogger().Warn("JWT Verification failed", "remote", r.RemoteAddr, "err", err)
			w.WriteHeader(401)
			return
		}

		ctx = WithPerm(ctx, allow)
	}

	h.Next(w, r.WithContext(ctx))
}
