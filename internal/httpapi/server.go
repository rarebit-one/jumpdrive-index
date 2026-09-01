// Package httpapi is the HTTP surface: the /mcp endpoint speaking the MCP
// Streamable HTTP transport (spec revision 2025-06-18) — POST for JSON-RPC
// request/response (a single application/json reply, with an Mcp-Session-Id
// stamped on the initialize response), GET to open the server->client SSE
// stream, and DELETE to terminate a session — plus the estate's health baseline
// (alive / ready). Routing is the stdlib ServeMux (Go 1.22 method+pattern) — no
// framework. The server is effectively stateless: each POST is self-authorized by
// the bearer in the Authorization header, which is handed to the MCP server and
// authenticated via the service, so the session id is nominal (echoed, never
// hard-required) and the plain-POST path (no session id) keeps working.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rarebit-one/jumpdrive-index/internal/mcp"
	"github.com/rarebit-one/jumpdrive-index/internal/mcpauth"
)

const maxBodyBytes = 4 << 20 // 4 MiB request cap

// Server is the HTTP handler set.
type Server struct {
	mcp  *mcp.Server
	log  *slog.Logger
	auth mcpauth.Authorizer
}

// Option configures a Server.
type Option func(*Server)

// WithAuthorizer overrides how a request is reduced to its effective bearer. The
// default is the legacy Bearer extractor; voidbind mode supplies an
// mcpauth.Voidbind that verifies a Device credential instead.
func WithAuthorizer(a mcpauth.Authorizer) Option {
	return func(s *Server) {
		if a != nil {
			s.auth = a
		}
	}
}

// New builds the HTTP server around an MCP server. By default callers authenticate
// with the raw Authorization bearer; WithAuthorizer swaps in Device-credential
// (voidbind) auth.
func New(m *mcp.Server, log *slog.Logger, opts ...Option) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		mcp:  m,
		log:  log,
		auth: mcpauth.BearerFunc(func(r *http.Request) string { return bearerFrom(r.Header.Get("Authorization")) }),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Routes returns the wrapped handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", s.handleMCPPost)
	mux.HandleFunc("GET /mcp", s.handleMCPStream)
	mux.HandleFunc("DELETE /mcp", s.handleMCPDelete)
	mux.HandleFunc("GET /health/alive", func(w http.ResponseWriter, _ *http.Request) { writeText(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, _ *http.Request) { writeText(w, http.StatusOK, "ready") })
	return s.recoverPanics(mux)
}

// handleMCPPost is the Streamable HTTP request/response leg: it accepts a JSON-RPC
// request and returns the single JSON-RPC response as application/json. On an
// initialize request it also stamps a generated Mcp-Session-Id on the response;
// on any other request it echoes back a client-supplied Mcp-Session-Id. The
// session id is validated loosely — a POST WITHOUT one is never rejected (the M0
// acceptance demo drives the endpoint with plain curl and no session id), because
// the server is stateless and every request is self-authorized by its bearer.
func (s *Server) handleMCPPost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeText(w, http.StatusBadRequest, "read error")
		return
	}
	// Session id: fresh on initialize, echoed on everything else. Peek at the
	// method without consuming the body the MCP server re-parses.
	sessionID := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
	if isInitialize(body) {
		if sessionID == "" {
			sessionID = uuid.NewString()
		}
	}
	if sessionID != "" {
		w.Header().Set("Mcp-Session-Id", sessionID)
	}

	bearer := s.auth.EffectiveBearer(r)
	resp := s.mcp.Handle(r.Context(), bearer, body)
	if resp == nil { // a notification: no body
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp) //nolint:gosec // G705: a marshaled JSON-RPC body served as application/json — not HTML, no XSS vector
}

// handleMCPStream is the Streamable HTTP server->client leg: it opens a
// text/event-stream that a standard MCP client holds open after initialize.
// jumpdrive-index emits no server-initiated messages, so the stream sends an
// opening heartbeat comment and then simply blocks until the client disconnects
// or the request context is cancelled. Bearer auth applies as on POST.
func (s *Server) handleMCPStream(w http.ResponseWriter, r *http.Request) {
	if s.auth.EffectiveBearer(r) == "" {
		writeText(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeText(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	if sid := strings.TrimSpace(r.Header.Get("Mcp-Session-Id")); sid != "" {
		h.Set("Mcp-Session-Id", sid)
	}
	w.WriteHeader(http.StatusOK)
	// An SSE comment line: proves the stream is open without being a protocol
	// message (there are none to send).
	_, _ = io.WriteString(w, ": jumpdrive-index mcp stream open\n\n")
	flusher.Flush()
	<-r.Context().Done() // block until client disconnect / shutdown, then return cleanly
}

// handleMCPDelete acknowledges session termination. The server holds no session
// state, so this is nominal — a 200 is enough. Bearer auth applies.
func (s *Server) handleMCPDelete(w http.ResponseWriter, r *http.Request) {
	if s.auth.EffectiveBearer(r) == "" {
		writeText(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	writeText(w, http.StatusOK, "ok")
}

// isInitialize reports whether a JSON-RPC request body is an MCP initialize call,
// so the POST leg knows to stamp a session id. It tolerates a malformed body
// (returns false) — the MCP server does the authoritative parse.
func isInitialize(body []byte) bool {
	var probe struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(body, &probe) == nil && probe.Method == "initialize"
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
