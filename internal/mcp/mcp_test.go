package mcp_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/access/starchart"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/mcp"
	"github.com/rarebit-one/jumpdrive-index/internal/service"
	"github.com/rarebit-one/jumpdrive-index/internal/store/sqlite"
)

var ctx = context.Background()

func newServer(t *testing.T) *mcp.Server {
	t.Helper()
	st, err := sqlite.Open(sqlite.Options{
		Path:       filepath.Join(t.TempDir(), "mcp.db"),
		Thresholds: domain.Thresholds{AutoMerge: 0.94, Review: 0.86},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	am, _ := starchart.New(starchart.Config{Principals: []starchart.PrincipalConfig{
		{Token: "kate-tok", ID: "kate", Spaces: []domain.SpaceID{"fam"}},
		{Token: "bob-tok", ID: "bob"},
	}})
	return mcp.New(service.New(st, am, nil))
}

// call drives one JSON-RPC request and returns the parsed response.
func call(t *testing.T, s *mcp.Server, bearer, method string, params any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	out := s.Handle(ctx, bearer, body)
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("unmarshal response: %v (%s)", err, out)
	}
	return resp
}

// callTool drives tools/call and returns (structuredContent, isError).
func callTool(t *testing.T, s *mcp.Server, bearer, name string, args any) (map[string]any, bool) {
	t.Helper()
	resp := call(t, s, bearer, "tools/call", map[string]any{"name": name, "arguments": args})
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", resp)
	}
	isErr, _ := result["isError"].(bool)
	sc, _ := result["structuredContent"].(map[string]any)
	return sc, isErr
}

func TestInitializeAndToolsList(t *testing.T) {
	s := newServer(t)
	init := call(t, s, "", "initialize", map[string]any{})
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != mcp.ProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s", res["protocolVersion"], mcp.ProtocolVersion)
	}
	list := call(t, s, "", "tools/list", map[string]any{})
	tools := list["result"].(map[string]any)["tools"].([]any)
	seen := map[string]bool{}
	for _, tl := range tools {
		seen[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"create_entity", "get_entity", "link", "get_neighbors", "propose_entity", "decide_proposal"} {
		if !seen[want] {
			t.Errorf("tools/list missing %q", want)
		}
	}
}

func TestCreateThenGetViaMCP(t *testing.T) {
	s := newServer(t)
	sc, isErr := callTool(t, s, "kate-tok", "create_entity", map[string]any{
		"type": "Movie", "props": map[string]any{"name": "Alien"}, "space": "fam", "visibility": "space",
		"external_ids": []map[string]string{{"scheme": "tmdb", "value": "348"}},
	})
	if isErr {
		t.Fatalf("create returned isError: %v", sc)
	}
	entity := sc["Entity"].(map[string]any)
	id := entity["ID"].(string)
	if id == "" {
		t.Fatal("no entity id returned")
	}

	got, isErr := callTool(t, s, "kate-tok", "get_entity", map[string]any{"id": id})
	if isErr {
		t.Fatalf("get returned isError: %v", got)
	}
	if got["Type"] != "Movie" {
		t.Errorf("type = %v, want Movie", got["Type"])
	}
	if got["Owner"] != "kate" {
		t.Errorf("owner = %v, want kate", got["Owner"])
	}
}

func TestForbiddenAndUnauthenticatedAreToolErrors(t *testing.T) {
	s := newServer(t)
	// bob has no write access to fam → isError, not a transport error.
	_, isErr := callTool(t, s, "bob-tok", "create_entity", map[string]any{
		"type": "Movie", "props": map[string]any{"name": "X"}, "space": "fam", "visibility": "space",
	})
	if !isErr {
		t.Error("bob create should be an isError tool result")
	}
	// Unknown bearer → isError.
	_, isErr = callTool(t, s, "who?", "get_entity", map[string]any{"id": "x"})
	if !isErr {
		t.Error("unauthenticated get should be an isError tool result")
	}
}
