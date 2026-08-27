package ingest_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/heyarr"
	"github.com/rarebit-one/jumpdrive-index/internal/ingest"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
	"github.com/rarebit-one/jumpdrive-index/internal/store/sqlite"
)

var ctx = context.Background()

// openStore returns a fresh, migrated SQLite store for one test.
func openStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(sqlite.Options{
		Path:       filepath.Join(t.TempDir(), "ingest.db"),
		Thresholds: domain.Thresholds{AutoMerge: 0.94, Review: 0.86},
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// mockHeyarr is an httptest server speaking heyarr's MCP JSON-RPC 2.0 wire shape.
// It answers the PROPOSED get_external_ids reverse lookup (tmdb → work), the
// contract ADR-0050 (heyarr PR #349) will make real; works maps the tmdb ids it
// knows to heyarr work ids.
func mockHeyarr(t *testing.T, works map[string]string) *heyarr.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64 `json:"id"`
			Params struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		var structured any
		if req.Params.Name == "get_external_ids" && req.Params.Arguments["source"] == "tmdb" {
			if workID, ok := works[req.Params.Arguments["value"]]; ok {
				structured = map[string]string{"entity_type": "work", "entity_id": workID}
			} else {
				structured = map[string]any{} // no match
			}
		}
		sc, _ := json.Marshal(structured)
		resp := map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{
				"content":           []map[string]string{{"type": "text", "text": "ok"}},
				"structuredContent": json.RawMessage(sc),
				"isError":           false,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	c, err := heyarr.New(heyarr.Options{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("heyarr client: %v", err)
	}
	return c
}

func kroftMeta() ingest.Metadata {
	return ingest.Metadata{
		VideoID: "yt-kroft-alien", Title: "Kroft on Alien",
		URL: "https://youtube.com/watch?v=yt-kroft-alien", Description: "A video essay on Alien.",
		UploadDate: "2019-05-25", Transcript: "In space no one can hear you scream...",
		ChannelID: "chan-kroft", ChannelName: "@kroft_movies", ChannelURL: "https://youtube.com/@kroft_movies",
	}
}

func af(owner string) access.Filter {
	return access.Filter{Principal: domain.PrincipalID(owner), AllowPublic: true}
}

// TestIngest_FullLoop proves the rung-1 loop end to end: a VideoObject, its
// channel Organization, an author edge, and — via the mock heyarr reconciliation
// — an about edge to a heyarr-work reference resolved from a tmdb id.
func TestIngest_FullLoop(t *testing.T) {
	st := openStore(t)
	hey := mockHeyarr(t, map[string]string{"348": "work-alien-123"})
	ing := ingest.New(st, ingest.StaticCapability{Meta: kroftMeta()}, hey)

	res, err := ing.Ingest(ctx, ingest.Request{
		Ref: "yt-kroft-alien", Owner: "kate", Visibility: domain.VisPublic, SubjectTMDB: []string{"348"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Video == "" || res.Channel == "" {
		t.Fatalf("expected video+channel ids, got %+v", res)
	}
	if !res.Transcribed {
		t.Error("expected Transcribed=true (transcript was provided)")
	}
	if len(res.AboutWorks) != 1 {
		t.Fatalf("expected 1 about-work, got %d", len(res.AboutWorks))
	}

	// The video node.
	video, err := st.GetEntity(ctx, af("kate"), res.Video)
	if err != nil {
		t.Fatalf("get video: %v", err)
	}
	if video.Type != "VideoObject" || propName(t, video) != "Kroft on Alien" {
		t.Errorf("video = %s/%q", video.Type, propName(t, video))
	}

	// The channel node.
	channel, err := st.GetEntity(ctx, af("kate"), res.Channel)
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if channel.Type != "Organization" || propName(t, channel) != "@kroft_movies" {
		t.Errorf("channel = %s/%q", channel.Type, propName(t, channel))
	}

	// The heyarr-work reference carries the reconciled work id AND the tmdb anchor.
	hits, _ := st.ResolveByExternalID(ctx, af("kate"), []string{"heyarr-work:work-alien-123"})
	if len(hits) != 1 || hits[0].ID != res.AboutWorks[0] {
		t.Fatalf("heyarr-work ref not found; hits=%v", hits)
	}
	if tm, _ := st.ResolveByExternalID(ctx, af("kate"), []string{"tmdb:348"}); len(tm) != 1 || tm[0].ID != res.AboutWorks[0] {
		t.Errorf("tmdb anchor not attached to the work ref; got %v", tm)
	}

	// Both edges are reachable from the video, with the right predicates.
	sub, err := st.Neighbors(ctx, af("kate"), store.NeighborQuery{Start: res.Video, MaxHops: 1})
	if err != nil {
		t.Fatalf("neighbors: %v", err)
	}
	if !hasEdge(sub, "author", res.Video, res.Channel) {
		t.Error("missing VideoObject --author--> Organization edge")
	}
	if !hasEdge(sub, "about", res.Video, res.AboutWorks[0]) {
		t.Error("missing VideoObject --about--> heyarr-work edge")
	}
}

// TestIngest_NoHeyarr proves 4a still runs without a reconciler: the video and
// channel land, but no about edge is created.
func TestIngest_NoHeyarr(t *testing.T) {
	st := openStore(t)
	ing := ingest.New(st, ingest.StaticCapability{Meta: kroftMeta()}, nil)

	res, err := ing.Ingest(ctx, ingest.Request{
		Ref: "yt-kroft-alien", Owner: "kate", Visibility: domain.VisPublic, SubjectTMDB: []string{"348"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Video == "" || res.Channel == "" {
		t.Fatal("4a should still create the video and channel")
	}
	if len(res.AboutWorks) != 0 {
		t.Errorf("no reconciler → no about edges, got %d", len(res.AboutWorks))
	}
}

// TestIngest_Idempotent proves a re-ingest of the same video does not duplicate
// nodes or edges (resolve-before-create + stable edge dedupe keys).
func TestIngest_Idempotent(t *testing.T) {
	st := openStore(t)
	hey := mockHeyarr(t, map[string]string{"348": "work-alien-123"})
	ing := ingest.New(st, ingest.StaticCapability{Meta: kroftMeta()}, hey)
	req := ingest.Request{Ref: "yt-kroft-alien", Owner: "kate", Visibility: domain.VisPublic, SubjectTMDB: []string{"348"}}

	first, err := ing.Ingest(ctx, req)
	if err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	second, err := ing.Ingest(ctx, req)
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if first.Video != second.Video || first.Channel != second.Channel {
		t.Errorf("re-ingest minted new ids: %+v vs %+v", first, second)
	}
}

// TestIngest_DegradedNoTranscript proves a metadata-only ingestion (empty
// transcript) still produces a VideoObject and reports Transcribed=false.
func TestIngest_DegradedNoTranscript(t *testing.T) {
	st := openStore(t)
	meta := kroftMeta()
	meta.Transcript = "" // Whisper absent → metadata-only
	ing := ingest.New(st, ingest.StaticCapability{Meta: meta}, nil)

	res, err := ing.Ingest(ctx, ingest.Request{Ref: "yt-kroft-alien", Owner: "kate", Visibility: domain.VisPublic})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Video == "" {
		t.Fatal("expected a video node even without a transcript")
	}
	if res.Transcribed {
		t.Error("expected Transcribed=false for an empty transcript")
	}
}

func propName(t *testing.T, e domain.Entity) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Props, &m); err != nil {
		return ""
	}
	s, _ := m["name"].(string)
	return s
}

func hasEdge(sub store.Subgraph, pred domain.Predicate, from, to domain.EntityID) bool {
	for _, e := range sub.Edges {
		if e.Predicate == pred && e.From == from && e.To == to {
			return true
		}
	}
	return false
}
