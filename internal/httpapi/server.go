// Package httpapi is the HTTP surface: a single POST /mcp endpoint carrying the
// JSON-RPC MCP protocol, plus the estate's four-tier health baseline (alive /
// ready). Routing is the stdlib ServeMux (Go 1.22 method+pattern) — no framework.
// The bearer travels in the Authorization header and is handed to the MCP server,
// which authenticates it via the service.
package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/mcp"
)

const maxBodyBytes = 4 << 20 // 4 MiB request cap

// Server is the HTTP handler set.
type Server struct {
	mcp *mcp.Server
	log *slog.Logger
}

// New builds the HTTP server around an MCP server.
func New(m *mcp.Server, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{mcp: m, log: log}
}

// Routes returns the wrapped handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", s.handleMCP)
	mux.HandleFunc("GET /health/alive", func(w http.ResponseWriter, _ *http.Request) { writeText(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, _ *http.Request) { writeText(w, http.StatusOK, "ready") })
	return s.recoverPanics(mux)
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeText(w, http.StatusBadRequest, "read error")
		return
	}
	bearer := bearerFrom(r.Header.Get("Authorization"))
	resp := s.mcp.Handle(r.Context(), bearer, body)
	if resp == nil { // a notification: no body
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp) //nolint:gosec // G705: a marshaled JSON-RPC body served as application/json — not HTML, no XSS vector
}

// bearerFrom extracts the token from an "Authorization: Bearer <tok>" header,
// tolerating a bare token too.
func bearerFrom(h string) string {
	h = strings.TrimSpace(h)
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return h
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic in handler", "path", r.URL.Path, "panic", v)
				writeText(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func writeText(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg)
}

// Serve runs an http.Server with sane timeouts and shuts it down gracefully when
// ctx is cancelled (e.g. on SIGTERM), draining in-flight requests up to a bound.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}
