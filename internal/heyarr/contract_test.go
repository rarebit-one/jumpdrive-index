package heyarr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const contractWorkID = "01990000-0000-7000-8000-0000000000w1"

// TestGetExternalIDsContract is the heyarr↔index drift gate for get_external_ids
// (heyarr ADR-0050). The vendored fixtures in contract/ pin the byte-for-byte
// wire shape; this asserts the Client emits the pinned request params AND decodes
// the pinned response payload over the real JSON-RPC + tool-result path. If
// heyarr's tool drifts, re-vendor contract/ (see SOURCE.md) and fix the client so
// they agree again — a red here means the two have diverged.
func TestGetExternalIDsContract(t *testing.T) {
	cases := []struct {
		name        string
		req         ExternalIDsRequest
		reqFixture  string
		respFixture string
		want        []ExternalID
	}{
		{
			name:        "reverse",
			req:         ExternalIDsRequest{Source: "tmdb", Value: "603"},
			reqFixture:  "get_external_ids.reverse.request.json",
			respFixture: "get_external_ids.reverse.response.json",
			want: []ExternalID{
				{Source: "tmdb", Value: "603", EntityType: "work", EntityID: contractWorkID},
			},
		},
		{
			name:        "forward",
			req:         ExternalIDsRequest{WorkID: contractWorkID},
			reqFixture:  "get_external_ids.forward.request.json",
			respFixture: "get_external_ids.forward.response.json",
			want: []ExternalID{
				{Source: "imdb", Value: "tt0133093", EntityType: "work", EntityID: contractWorkID},
				{Source: "tmdb", Value: "603", EntityType: "work", EntityID: contractWorkID},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantParams := readContractJSON(t, tc.reqFixture)
			respPayload := readContractBytes(t, tc.respFixture)

			var gotParams any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var rpc struct {
					Params json.RawMessage `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
					t.Errorf("decode request body: %v", err)
				}
				if err := json.Unmarshal(rpc.Params, &gotParams); err != nil {
					t.Errorf("unmarshal request params: %v", err)
				}
				resp := map[string]any{
					"jsonrpc": "2.0", "id": 1,
					"result": map[string]any{
						"content":           []map[string]string{{"type": "text", "text": "ok"}},
						"structuredContent": json.RawMessage(respPayload),
						"isError":           false,
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()

			client, err := New(Options{BaseURL: srv.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got, err := client.GetExternalIDs(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("GetExternalIDs: %v", err)
			}

			// The client must emit exactly the vendored request params.
			if !reflect.DeepEqual(gotParams, wantParams) {
				t.Errorf("request params drifted from the vendored contract:\n got  %v\n want %v", gotParams, wantParams)
			}
			// And decode exactly the vendored response into the pinned values.
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("decoded result drifted from the vendored contract:\n got  %+v\n want %+v", got, tc.want)
			}
		})
	}
}

// TestGetExternalIDsNoMatch pins heyarr's ADR-0025 contract that an unknown id is
// a no-match — an empty list with a nil error, never a failure.
func TestGetExternalIDsNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{
				"content":           []map[string]string{{"type": "text", "text": "ok"}},
				"structuredContent": json.RawMessage(`{"external_ids":[]}`),
				"isError":           false,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := New(Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := client.GetExternalIDs(context.Background(), ExternalIDsRequest{Source: "tmdb", Value: "does-not-exist"})
	if err != nil {
		t.Fatalf("a no-match must not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a no-match must be empty; got %+v", got)
	}
}

func readContractBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("contract", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func readContractJSON(t *testing.T, name string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(readContractBytes(t, name), &v); err != nil {
		t.Fatalf("unmarshal fixture %s: %v", name, err)
	}
	return v
}
