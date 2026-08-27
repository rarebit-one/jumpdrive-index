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
// It answers heyarr's shipped get_external_ids reverse lookup (tmdb → work),
// pinned to the real ADR-0050 contract (heyarr PR #355):
// {external_ids:[{source,value,entity_type,entity_id}]}, an empty list on no
// match. works maps the tmdb ids it knows to heyarr work ids.
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

		// heyarr's get_external_ids returns a uniform list; empty on no match.
		structured := map[string]any{"external_ids": []any{}}
		if req.Params.Name == "get_external_ids" && req.Params.Arguments["source"] == "tmdb" {
			if workID, ok := works[req.Params.Arguments["value"]]; ok {
				structured = map[string]any{"external_ids": []map[string]string{{
					"source":      "tmdb",
					"value":       req.Params.Arguments["value"],
					"entity_type": "work",
					"entity_id":   workID,
				}}}
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

// TestIngest_Rung2Clips proves rung 2: a transcript carrying timecoded segments
// produces Clip nodes (each isPartOf the VideoObject, with the right
// startOffset/endOffset), and a segment whose subject reconciles gets a TIMECODED
// about edge from the Clip to the heyarr-work reference — while the rung-1
// video-level about edge still holds.
func TestIngest_Rung2Clips(t *testing.T) {
	st := openStore(t)
	hey := mockHeyarr(t, map[string]string{"348": "work-alien-123"})
	meta := kroftMeta()
	meta.Segments = []ingest.Segment{
		// 3:12–4:05, about Alien (tmdb 348) — this span reconciles.
		{StartOffset: 192, EndOffset: 245, Text: "The xenomorph first appears here.", SubjectTMDB: []string{"348"}},
		// A later span with no reconcilable subject — a Clip, but no about edge.
		{StartOffset: 300.5, EndOffset: 360, Text: "On the film's sound design."},
	}
	ing := ingest.New(st, ingest.StaticCapability{Meta: meta}, hey)

	res, err := ing.Ingest(ctx, ingest.Request{
		Ref: "yt-kroft-alien", Owner: "kate", Visibility: domain.VisPublic, SubjectTMDB: []string{"348"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// Two segments → two Clip nodes; one reconciling segment → one Clip about edge.
	if len(res.Clips) != 2 {
		t.Fatalf("expected 2 clips, got %d (%v)", len(res.Clips), res.Clips)
	}
	if len(res.ClipAboutWorks) != 1 {
		t.Fatalf("expected 1 clip-level about work, got %d", len(res.ClipAboutWorks))
	}

	// Rung 1 still holds: the video-level about edge to the reconciled work.
	if len(res.AboutWorks) != 1 {
		t.Fatalf("rung-1 video-level about broke: got %d works", len(res.AboutWorks))
	}
	sub, err := st.Neighbors(ctx, af("kate"), store.NeighborQuery{Start: res.Video, MaxHops: 1})
	if err != nil {
		t.Fatalf("neighbors: %v", err)
	}
	if !hasEdge(sub, "about", res.Video, res.AboutWorks[0]) {
		t.Error("rung-1 VideoObject --about--> heyarr-work edge missing")
	}

	// Each Clip is a Clip node, isPartOf the video, with the right offsets.
	wantOffsets := map[float64]float64{192: 245, 300.5: 360}
	sawAbout := false
	for _, cid := range res.Clips {
		clip, err := st.GetEntity(ctx, af("kate"), cid)
		if err != nil {
			t.Fatalf("get clip %s: %v", cid, err)
		}
		if clip.Type != "Clip" {
			t.Errorf("clip %s type = %s, want Clip", cid, clip.Type)
		}
		start, end := clipOffsets(t, clip)
		wantEnd, ok := wantOffsets[start]
		if !ok || wantEnd != end {
			t.Errorf("clip %s offsets = %v–%v, not an expected span", cid, start, end)
		}

		// Clip --isPartOf--> VideoObject.
		csub, err := st.Neighbors(ctx, af("kate"), store.NeighborQuery{Start: cid, MaxHops: 1})
		if err != nil {
			t.Fatalf("clip neighbors: %v", err)
		}
		if !hasEdge(csub, "isPartOf", cid, res.Video) {
			t.Errorf("clip %s missing --isPartOf--> VideoObject", cid)
		}
		// The reconciling span (starts at 192) must carry a Clip-level about edge
		// to the SAME heyarr-work reference the video links (shared work node).
		if start == 192 {
			if !hasEdge(csub, "about", cid, res.ClipAboutWorks[0]) {
				t.Errorf("timecoded clip %s missing --about--> heyarr-work edge", cid)
			}
			if res.ClipAboutWorks[0] != res.AboutWorks[0] {
				t.Errorf("clip about work %s != video about work %s (should dedup onto one node)",
					res.ClipAboutWorks[0], res.AboutWorks[0])
			}
			sawAbout = true
		} else if hasEdge(csub, "about", cid, res.ClipAboutWorks[0]) {
			t.Errorf("non-reconciling clip %s should not carry an about edge", cid)
		}
	}
	if !sawAbout {
		t.Error("no clip carried the timecoded about edge")
	}
}

// TestIngest_NoSegmentsNoClips proves the rung-1 path is unchanged when the
// transcript carries no timecodes: a video and channel, but no Clip nodes.
func TestIngest_NoSegmentsNoClips(t *testing.T) {
	st := openStore(t)
	hey := mockHeyarr(t, map[string]string{"348": "work-alien-123"})
	ing := ingest.New(st, ingest.StaticCapability{Meta: kroftMeta()}, hey)

	res, err := ing.Ingest(ctx, ingest.Request{
		Ref: "yt-kroft-alien", Owner: "kate", Visibility: domain.VisPublic, SubjectTMDB: []string{"348"},
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(res.Clips) != 0 || len(res.ClipAboutWorks) != 0 {
		t.Errorf("no segments → no clips, got clips=%d aboutWorks=%d", len(res.Clips), len(res.ClipAboutWorks))
	}
	if len(res.AboutWorks) != 1 {
		t.Errorf("rung-1 about edge should still hold, got %d", len(res.AboutWorks))
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

// clipOffsets pulls the schema.org startOffset/endOffset (seconds) out of a Clip's
// property bag; JSON numbers decode as float64.
func clipOffsets(t *testing.T, e domain.Entity) (start, end float64) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Props, &m); err != nil {
		t.Fatalf("unmarshal clip props: %v", err)
	}
	start, _ = m["startOffset"].(float64)
	end, _ = m["endOffset"].(float64)
	return start, end
}

func hasEdge(sub store.Subgraph, pred domain.Predicate, from, to domain.EntityID) bool {
	for _, e := range sub.Edges {
		if e.Predicate == pred && e.From == from && e.To == to {
			return true
		}
	}
	return false
}
