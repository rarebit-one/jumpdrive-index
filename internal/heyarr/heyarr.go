// Package heyarr is an OUTBOUND MCP client to a heyarr media server's
// hand-rolled JSON-RPC endpoint (POST /api/v1/mcp). jumpdrive-index LINKS to
// heyarr titles by id; it never copies heyarr's catalogue. This client is how the
// index reads heyarr at query time — calling the heyarr `search_content` tool to
// reconcile a title to a heyarr work id, which then anchors a jumpdrive-index
// entity as a heyarr-work/-edition/-asset/-blake3 external id (see link.go).
//
// It mirrors the RunnerClient discipline the plan calls for: an explicit base
// URL + redacting bearer, a per-request X-Request-Id, hard timeouts (both an
// http.Client timeout and the caller's context), a typed Error, and no panics —
// a heyarr that is empty, erroring, or unreachable degrades to a typed error /
// empty result the caller can treat as "no match" (heyarr ADR-0025 shape). It
// speaks the SAME MCP JSON-RPC 2.0 wire shape as the index's own internal/mcp
// server (tools/call with a name + arguments; a tool result carrying
// structuredContent + an isError flag), so the two stay in lockstep.
package heyarr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/jumpdrive-index/internal/secret"
)

// DefaultTimeout bounds a single heyarr call when Options.Timeout is unset. A
// query-time reconciliation must fail fast rather than stall the index.
const DefaultTimeout = 10 * time.Second

// Error is a typed heyarr client failure. Callers reconciling a title can treat
// any Error as "no match" (ADR-0025 graceful degradation) while still being able
// to log/circuit-break on it; it never masquerades as a successful empty result.
type Error struct {
	Op      string // the client operation (e.g. "search_content")
	Code    int    // JSON-RPC error code, when the failure came back as one; else 0
	Message string // human-readable cause
	Err     error  // wrapped transport/decode error, if any
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("heyarr %s: rpc error %d: %s", e.Op, e.Code, e.Message)
	}
	if e.Err != nil {
		return fmt.Sprintf("heyarr %s: %s: %v", e.Op, e.Message, e.Err)
	}
	return fmt.Sprintf("heyarr %s: %s", e.Op, e.Message)
}

// Unwrap exposes the wrapped transport/decode error for errors.Is/As.
func (e *Error) Unwrap() error { return e.Err }

// Options configures a Client. BaseURL is heyarr's MCP endpoint
// (e.g. "https://heyarr.example/api/v1/mcp"); Token is the bearer. HTTPClient,
// Timeout and NewRequestID are injectable so tests can drive an httptest server
// deterministically.
type Options struct {
	BaseURL      string
	Token        secret.Value
	HTTPClient   *http.Client  // defaults to a client with Timeout
	Timeout      time.Duration // per-call ceiling; DefaultTimeout when zero
	NewRequestID func() string // X-Request-Id source; uuid when nil
}

// Client calls a heyarr MCP endpoint. It is safe for concurrent use.
type Client struct {
	baseURL  string
	token    secret.Value
	http     *http.Client
	newReqID func() string
	seq      atomic.Int64
}

// New builds a Client from opts. It returns an error only for a missing BaseURL;
// a missing token is allowed (a loopback/unauthenticated heyarr), matching the
// index's own default-deny-but-loopback-ok posture.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, fmt.Errorf("heyarr: empty BaseURL")
	}
	hc := opts.HTTPClient
	if hc == nil {
		to := opts.Timeout
		if to <= 0 {
			to = DefaultTimeout
		}
		hc = &http.Client{Timeout: to}
	}
	newID := opts.NewRequestID
	if newID == nil {
		newID = uuid.NewString
	}
	return &Client{
		baseURL:  strings.TrimRight(opts.BaseURL, "/"),
		token:    opts.Token,
		http:     hc,
		newReqID: newID,
	}, nil
}

// ---- MCP JSON-RPC wire types (the heyarr side, mirroring internal/mcp) ----

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type callParams struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// toolResult is the tools/call result envelope: a text content block plus the
// structured payload and the isError flag, exactly as internal/mcp shapes it.
type toolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}

// Call invokes one heyarr MCP tool and decodes its structured result into out
// (which may be nil to ignore the payload). It is the low-level primitive the
// typed methods build on; a future get_work/get_external_ids call is one line.
func (c *Client) Call(ctx context.Context, tool string, args any, out any) error {
	body, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      c.seq.Add(1),
		Method:  "tools/call",
		Params:  callParams{Name: tool, Arguments: args},
	})
	if err != nil {
		return &Error{Op: tool, Message: "encode request", Err: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return &Error{Op: tool, Message: "build request", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Request-Id", c.newReqID())
	if !c.token.IsZero() {
		req.Header.Set("Authorization", "Bearer "+c.token.Reveal())
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return &Error{Op: tool, Message: "request failed", Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB ceiling
	if err != nil {
		return &Error{Op: tool, Message: "read response", Err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return &Error{Op: tool, Code: resp.StatusCode, Message: "http status " + resp.Status}
	}

	var rpc rpcResponse
	if err := json.Unmarshal(raw, &rpc); err != nil {
		return &Error{Op: tool, Message: "decode response", Err: err}
	}
	if rpc.Error != nil {
		return &Error{Op: tool, Code: rpc.Error.Code, Message: rpc.Error.Message}
	}

	var tr toolResult
	if err := json.Unmarshal(rpc.Result, &tr); err != nil {
		return &Error{Op: tool, Message: "decode tool result", Err: err}
	}
	if tr.IsError {
		return &Error{Op: tool, Message: toolErrorText(tr)}
	}
	if out == nil {
		return nil
	}
	// Prefer structuredContent; fall back to the text content block (some servers
	// send only text). An absent payload leaves out at its zero value.
	payload := tr.StructuredContent
	if len(payload) == 0 && len(tr.Content) > 0 {
		payload = json.RawMessage(tr.Content[0].Text)
	}
	if len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return &Error{Op: tool, Message: "decode structured content", Err: err}
	}
	return nil
}

func toolErrorText(tr toolResult) string {
	if len(tr.Content) > 0 && tr.Content[0].Text != "" {
		return tr.Content[0].Text
	}
	return "tool reported an error"
}
