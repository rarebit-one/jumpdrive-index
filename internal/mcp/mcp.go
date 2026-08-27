// Package mcp is a hand-rolled JSON-RPC 2.0 MCP server exposing the graph over
// the service layer. It is intentionally not an SDK: the whole surface is
// initialize / tools/list / tools/call / ping, and keeping it in one readable file
// means the authorization seam (every tool call carries the caller's bearer,
// which the service authenticates) stays auditable — the in-estate precedent
// (heyarr, jumpdrive-web) is two hand-rolled servers.
//
// Tools are thin adapters over internal/service; a service error (unauthenticated
// / forbidden / not found) is returned as a tool result with isError:true so the
// model can react, never as a transport error.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/service"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

// ProtocolVersion is the MCP revision this server speaks.
const ProtocolVersion = "2025-06-18"

// ServerName identifies this server in the initialize handshake.
const ServerName = "jumpdrive-index"

// handlerFunc runs one tool. bearer is the caller's token (supplied by the
// transport, e.g. the HTTP Authorization header). It returns the structured
// result, or an error the dispatcher renders as an isError tool result.
type handlerFunc func(ctx context.Context, bearer string, args json.RawMessage) (any, error)

type tool struct {
	name        string
	description string
	inputSchema json.RawMessage
	handler     handlerFunc
}

// Server dispatches MCP JSON-RPC over a service.
type Server struct {
	svc   *service.Service
	tools map[string]tool
	names []string // sorted, for a stable tools/list
}

// New builds the server and registers the tool set.
func New(svc *service.Service) *Server {
	s := &Server{svc: svc, tools: map[string]tool{}}
	s.registerTools()
	for n := range s.tools {
		s.names = append(s.names, n)
	}
	sort.Strings(s.names)
	return s
}

// ---- JSON-RPC wire types ----

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC standard error codes used here.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
)

// Handle dispatches a single JSON-RPC request body and returns the response body.
// bearer is the caller's token. A nil return means "notification, no response".
func (s *Server) Handle(ctx context.Context, bearer string, body []byte) []byte {
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return marshal(rpcResponse{JSONRPC: "2.0", Error: &rpcError{codeParse, "parse error"}})
	}
	if req.JSONRPC != "2.0" {
		return marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{codeInvalidRequest, "jsonrpc must be 2.0"}})
	}

	switch req.Method {
	case "initialize":
		return s.ok(req.ID, map[string]any{
			"protocolVersion": ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": ServerName, "version": "0.1.0"},
		})
	case "ping":
		return s.ok(req.ID, map[string]any{})
	case "tools/list":
		return s.ok(req.ID, map[string]any{"tools": s.toolSpecs()})
	case "tools/call":
		return s.callTool(ctx, bearer, req)
	case "notifications/initialized":
		return nil // notification, no response
	default:
		return marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{codeMethodNotFound, "unknown method: " + req.Method}})
	}
}

func (s *Server) toolSpecs() []map[string]any {
	specs := make([]map[string]any, 0, len(s.names))
	for _, n := range s.names {
		t := s.tools[n]
		specs = append(specs, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": t.inputSchema,
		})
	}
	return specs
}

func (s *Server) callTool(ctx context.Context, bearer string, req rpcRequest) []byte {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return marshal(rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{codeInvalidRequest, "invalid params"}})
	}
	t, ok := s.tools[p.Name]
	if !ok {
		return s.ok(req.ID, toolError("unknown tool: "+p.Name))
	}
	result, err := t.handler(ctx, bearer, p.Arguments)
	if err != nil {
		// Service/domain errors surface as an isError tool result, not transport errors.
		return s.ok(req.ID, toolError(err.Error()))
	}
	return s.ok(req.ID, toolResult(sanitizeForAgent(result)))
}

// sanitizeForAgent strips the embedding vectors from entities in a tool result.
// Vectors are internal machinery (dedup + KNN); an agent never needs them, and a
// single entity's 1024-float vector inflates a result by ~10KB — enough to
// swamp an agent's context window and time out its model on an otherwise tiny
// graph. The fact log keeps the vectors (this only shapes the MCP read view).
func sanitizeForAgent(v any) any {
	switch t := v.(type) {
	case domain.Entity:
		t.Embeddings = nil
		return t
	case []domain.Entity:
		for i := range t {
			t[i].Embeddings = nil
		}
		return t
	case []store.ScoredEntity:
		for i := range t {
			t[i].Entity.Embeddings = nil
		}
		return t
	case store.Subgraph:
		for i := range t.Entities {
			t.Entities[i].Embeddings = nil
		}
		return t
	case store.ResolveResult:
		t.Entity.Embeddings = nil
		return t
	default:
		return v
	}
}

// ---- result shaping ----

func toolResult(v any) map[string]any {
	text, _ := json.Marshal(v)
	out := map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(text)}},
		"isError": false,
	}
	// structuredContent MUST be a JSON object per the MCP spec. A tool that
	// returns a top-level array (e.g. search, resolve_external, get_neighbors)
	// would otherwise make a spec-compliant MCP client reject the whole result
	// ("expected record, received array") — which silently starves an agent of
	// tool output. Emit it only when the value marshals to an object; an array or
	// scalar still rides in the text content above, so no data is lost.
	if len(text) > 0 && text[0] == '{' {
		out["structuredContent"] = v
	}
	return out
}

func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}

func (s *Server) ok(id json.RawMessage, result any) []byte {
	return marshal(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func marshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":%d,"message":"marshal error"}}`, codeParse))
	}
	return b
}
