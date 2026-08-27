// Package ingest turns an external video (a YouTube analysis video) into
// knowledge-graph nodes and edges, the "@kroft_movies about Alien" loop of the
// plan's rung 1:
//
//   - a VideoObject entity for the video (title/url/uploadDate/description and,
//     when the transcription capability is present, the transcript),
//   - an Organization entity for its channel,
//   - a VideoObject --author--> Organization edge, and
//   - the payoff: a VideoObject --about--> <heyarr work> edge, where the heyarr
//     work is reconciled BY ID (a tmdb id → a heyarr work id) and referenced by a
//     heyarr-work external id — never by copying heyarr's catalogue.
//
// The transcription toolchain (yt-dlp + Whisper) runs OUT OF PROCESS behind the
// Capability seam (heyarr ADR-0025 shape), so the main binary stays pure-Go /
// CGO-off / distroless-safe and degrades gracefully when the toolchain is absent.
//
// The heyarr reconciliation (4b) is STUBBED pending heyarr ADR-0050 (heyarr PR
// #349): it calls a PROPOSED get_external_ids MCP tool. The tests pin that
// proposed contract against a mock; it must be re-pinned when #349 lands.
package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/heyarr"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

// The external-id schemes anchoring the two YouTube nodes for dedup. The scheme
// set is open (see domain.ExternalID), so these need no allow-list change; naming
// them keeps every caller consistent and a re-ingest idempotent.
const (
	// SchemeYouTubeVideo anchors a VideoObject to its YouTube video id.
	SchemeYouTubeVideo = "youtube"
	// SchemeYouTubeChannel anchors an Organization to its YouTube channel id.
	SchemeYouTubeChannel = "youtube-channel"
	// SchemeYouTubeClip anchors a Clip to its parent video id plus its
	// start/end offsets, so a re-ingest dedups onto the same timecoded span
	// rather than minting a duplicate Clip.
	SchemeYouTubeClip = "youtube-clip"
)

// ingestWriter is the fixed writer id stamped on ingested facts, so a re-ingest
// of the same video replays idempotently rather than duplicating.
const ingestWriter = domain.WriterID("ingest")

// Metadata is what the Capability extracts about a video. Transcript is empty
// when the transcription capability is absent (a degraded, metadata-only
// ingestion) — that is a valid outcome, not an error.
type Metadata struct {
	VideoID     string
	Title       string
	URL         string
	Description string
	UploadDate  string // schema.org date string (e.g. "2019-05-25"), as the toolchain reports it
	ChannelID   string
	ChannelName string
	ChannelURL  string
	Transcript  string
	// Segments are the transcript's timecoded spans (rung 2). Each becomes a
	// Clip of the VideoObject. Empty (the capability yields no timecodes) means
	// no Clips — rung-1 behaviour is unchanged.
	Segments []Segment
}

// Segment is one timecoded span of a video's transcript: the text spoken
// between StartOffset and EndOffset (schema.org offsets, in seconds). SubjectTMDB
// carries the tmdb ids the span is ABOUT, which become timecoded about edges on
// the span's Clip (rung 2) — "at 3:12–4:05 this video is about Alien".
type Segment struct {
	StartOffset float64
	EndOffset   float64
	Text        string
	SubjectTMDB []string
}

// Capability fetches a video's metadata and best-effort transcript OUT OF
// PROCESS. Implementations shell out to yt-dlp/Whisper (ExecCapability) or return
// canned data (StaticCapability, for tests and toolchain-free runs). A capability
// that cannot even reach basic metadata returns ErrCapabilityUnavailable, so the
// caller can distinguish "no toolchain" from "no such video".
type Capability interface {
	Fetch(ctx context.Context, ref string) (Metadata, error)
}

// ErrCapabilityUnavailable signals the out-of-process toolchain is not installed,
// so ingestion cannot proceed for this ref (ADR-0025 optional-capability shape).
var ErrCapabilityUnavailable = errors.New("ingest: video capability unavailable")

// Request is one ingestion. SubjectTMDB carries the tmdb ids the video is ABOUT
// (rung 1 takes them as input; a later rung will extract them from the
// transcript). Owner/Space/Visibility scope the created nodes and edges.
type Request struct {
	Ref         string
	Owner       domain.PrincipalID
	Space       domain.SpaceID
	Visibility  domain.Visibility
	SubjectTMDB []string
}

// Result reports what the ingestion created or attached, so a caller (or a demo)
// can verify the loop closed.
type Result struct {
	Video          domain.EntityID
	Channel        domain.EntityID
	AboutWorks     []domain.EntityID // one per video-level reconciled heyarr work (rung 1)
	Clips          []domain.EntityID // one per timecoded transcript segment (rung 2)
	ClipAboutWorks []domain.EntityID // one per Clip-level (timecoded) about edge (rung 2)
	Transcribed    bool              // true when a non-empty transcript was stored
}

// caller is the minimal slice of a heyarr MCP client ingest needs for
// reconciliation: the generic tools/call primitive. *heyarr.Client satisfies it;
// a nil caller disables reconciliation (4b is skipped, 4a still runs).
type caller interface {
	Call(ctx context.Context, tool string, args any, out any) error
}

// Ingestor drives ingestion against a store, a capability, and an optional heyarr
// reconciler.
type Ingestor struct {
	store    store.Store
	cap      Capability
	reconcil caller
	now      func() time.Time
}

// New builds an Ingestor. resolver may be nil to disable heyarr reconciliation
// (4b); pass a real *heyarr.Client to enable it. A typed-nil client is a footgun,
// so pass an untyped nil to disable.
func New(st store.Store, cap Capability, resolver caller) *Ingestor {
	return &Ingestor{store: st, cap: cap, reconcil: resolver, now: time.Now}
}

// Ingest runs the full rung-1 loop for req.Ref: fetch → VideoObject + channel
// Organization + author edge (4a) → reconcile each SubjectTMDB to a heyarr work
// and add an about edge (4b). Reconciliation degrades to "no about edge" on any
// heyarr error (ADR-0025); 4a still succeeds.
func (i *Ingestor) Ingest(ctx context.Context, req Request) (Result, error) {
	if req.Ref == "" {
		return Result{}, fmt.Errorf("%w: empty ref", store.ErrInvalidInput)
	}
	vis := req.Visibility
	if vis == "" {
		vis = domain.VisPrivate
	}
	if !vis.Valid() {
		return Result{}, fmt.Errorf("%w: invalid visibility %q", store.ErrInvalidInput, vis)
	}

	md, err := i.cap.Fetch(ctx, req.Ref)
	if err != nil {
		return Result{}, fmt.Errorf("fetch %q: %w", req.Ref, err)
	}
	if md.VideoID == "" {
		return Result{}, fmt.Errorf("%w: capability returned no video id", store.ErrInvalidInput)
	}

	var res Result
	res.Transcribed = md.Transcript != ""

	// --- 4a: the video node ---
	videoProps := map[string]any{"name": md.Title, "url": md.URL}
	putIf(videoProps, "description", md.Description)
	putIf(videoProps, "uploadDate", md.UploadDate)
	putIf(videoProps, "transcript", md.Transcript)
	video, err := i.upsert(ctx, req, "VideoObject", videoProps,
		domain.ExternalID{Scheme: SchemeYouTubeVideo, Value: md.VideoID}, "ingest:yt-dlp")
	if err != nil {
		return Result{}, fmt.Errorf("video node: %w", err)
	}
	res.Video = video

	// --- 4a: the channel node + author edge ---
	if md.ChannelID != "" || md.ChannelName != "" {
		chProps := map[string]any{"name": md.ChannelName}
		putIf(chProps, "url", md.ChannelURL)
		channel, err := i.upsert(ctx, req, "Organization", chProps,
			domain.ExternalID{Scheme: SchemeYouTubeChannel, Value: md.ChannelID}, "ingest:yt-dlp")
		if err != nil {
			return Result{}, fmt.Errorf("channel node: %w", err)
		}
		res.Channel = channel
		// VideoObject --author--> Organization (schema.org: a work's author).
		if err := i.link(ctx, req, "author", video, channel,
			fmt.Sprintf("ingest|author|%s|%s", video, channel)); err != nil {
			return Result{}, fmt.Errorf("author edge: %w", err)
		}
	}

	// --- 4b (rung 1): reconcile each subject tmdb id to a heyarr work, add a
	// video-level about edge ---
	for _, tmdb := range req.SubjectTMDB {
		ref, ok, err := i.aboutWork(ctx, req, tmdb)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			continue // ADR-0025: a heyarr miss/error degrades to "no about edge"
		}
		if err := i.link(ctx, req, "about", video, ref,
			fmt.Sprintf("ingest|about|%s|%s", video, ref)); err != nil {
			return Result{}, fmt.Errorf("about edge: %w", err)
		}
		res.AboutWorks = append(res.AboutWorks, ref)
	}

	// --- rung 2: a Clip per timecoded transcript segment, isPartOf the video,
	// with a TIMECODED about edge when the segment's subject reconciles. Purely
	// additive: no segments → no Clips, leaving rung-1 behaviour untouched. ---
	for _, seg := range md.Segments {
		clip, err := i.upsertClip(ctx, req, md.VideoID, seg)
		if err != nil {
			return Result{}, fmt.Errorf("clip node: %w", err)
		}
		res.Clips = append(res.Clips, clip)
		// Clip --isPartOf--> VideoObject (schema.org: a part of the whole video).
		if err := i.link(ctx, req, "isPartOf", clip, video,
			fmt.Sprintf("ingest|isPartOf|%s|%s", clip, video)); err != nil {
			return Result{}, fmt.Errorf("clip isPartOf edge: %w", err)
		}
		// The rung-2 payoff: attach the about edge to the Clip, so the link is
		// timecoded to the span rather than the whole video.
		for _, tmdb := range seg.SubjectTMDB {
			ref, ok, err := i.aboutWork(ctx, req, tmdb)
			if err != nil {
				return Result{}, err
			}
			if !ok {
				continue
			}
			if err := i.link(ctx, req, "about", clip, ref,
				fmt.Sprintf("ingest|about|%s|%s", clip, ref)); err != nil {
				return Result{}, fmt.Errorf("clip about edge: %w", err)
			}
			res.ClipAboutWorks = append(res.ClipAboutWorks, ref)
		}
	}

	return res, nil
}

// aboutWork reconciles a tmdb id to a heyarr work and materialises the local
// reference node an about edge will point at: resolve the work id, upsert a
// CreativeWork carrying the heyarr-work anchor, and attach the tmdb join key so a
// future tmdb-based assertion dedups onto it. ok is false with no error when
// heyarr knows no work for the id (or errors), so every caller degrades to "no
// about edge" (ADR-0025). A repeat tmdb dedups onto the same reference, so a
// video-level and a Clip-level about edge share one work node.
func (i *Ingestor) aboutWork(ctx context.Context, req Request, tmdb string) (domain.EntityID, bool, error) {
	if i.reconcil == nil {
		return "", false, nil
	}
	workID, ok, rerr := i.resolveHeyarrWork(ctx, tmdb)
	if rerr != nil || !ok {
		return "", false, nil
	}
	ref, err := i.upsert(ctx, req, "CreativeWork",
		map[string]any{"name": "heyarr work " + workID},
		heyarr.WorkExternalID(workID), "ingest:heyarr")
	if err != nil {
		return "", false, fmt.Errorf("heyarr work ref: %w", err)
	}
	if err := i.attachTMDB(ctx, req, ref, tmdb); err != nil {
		return "", false, fmt.Errorf("tmdb anchor: %w", err)
	}
	return ref, true, nil
}

// upsertClip asserts a Clip entity for one timecoded transcript segment — a span
// of the parent video carrying schema.org startOffset/endOffset (seconds) and the
// segment text — anchored by a youtube-clip external id so a re-ingest dedups onto
// the same span rather than duplicating it.
func (i *Ingestor) upsertClip(ctx context.Context, req Request, videoID string, seg Segment) (domain.EntityID, error) {
	props := map[string]any{
		"startOffset": seg.StartOffset,
		"endOffset":   seg.EndOffset,
	}
	putIf(props, "text", seg.Text)
	ext := domain.ExternalID{Scheme: SchemeYouTubeClip, Value: clipKey(videoID, seg)}
	return i.upsert(ctx, req, "Clip", props, ext, "ingest:yt-dlp")
}

// clipKey is the stable dedup value for a Clip: its parent video id plus the
// span's offsets, so the same segment re-ingests idempotently.
func clipKey(videoID string, seg Segment) string {
	return fmt.Sprintf("%s@%s-%s",
		videoID,
		strconv.FormatFloat(seg.StartOffset, 'f', -1, 64),
		strconv.FormatFloat(seg.EndOffset, 'f', -1, 64))
}

// upsert asserts an entity through resolve-before-create and returns its id,
// treating an idempotent replay (ErrDuplicateFact) as success.
func (i *Ingestor) upsert(ctx context.Context, req Request, typ domain.Type, props map[string]any, ext domain.ExternalID, asserter string) (domain.EntityID, error) {
	raw, err := json.Marshal(props)
	if err != nil {
		return "", err
	}
	in := store.AppendEntityInput{
		Candidate: domain.Entity{
			Type: typ, Props: raw, Owner: req.Owner, Space: req.Space, Visibility: req.visOrDefault(),
			ExternalIDs: []domain.ExternalID{ext},
			Provenance:  domain.Provenance{Asserter: asserter, Method: domain.Asserted, Source: req.Ref},
		},
		Writer: ingestWriter, Actor: req.Owner, Policy: domain.ResolveAuto,
	}
	r, err := i.store.AppendEntityFact(ctx, in)
	if err != nil && !errors.Is(err, store.ErrDuplicateFact) {
		return "", err
	}
	return r.Entity.ID, nil
}

// attachTMDB adds a tmdb external id to an existing heyarr-work reference by
// re-asserting it (an attach unions external ids), so the node carries both the
// heyarr-work and the tmdb join keys.
func (i *Ingestor) attachTMDB(ctx context.Context, req Request, id domain.EntityID, tmdb string) error {
	ent, err := i.store.GetEntity(ctx, ownerFilter(req), id)
	if err != nil {
		return err
	}
	ent.ExternalIDs = append(ent.ExternalIDs, domain.ExternalID{Scheme: "tmdb", Value: tmdb})
	_, err = i.store.AppendEntityFact(ctx, store.AppendEntityInput{
		Candidate: ent, Writer: ingestWriter, Actor: req.Owner, Policy: domain.ResolveAuto,
	})
	if err != nil && !errors.Is(err, store.ErrDuplicateFact) {
		return err
	}
	return nil
}

// link asserts an edge, treating an idempotent replay as success. The dedupe key
// makes a re-ingest of the same relationship a no-op.
func (i *Ingestor) link(ctx context.Context, req Request, pred domain.Predicate, from, to domain.EntityID, dedupe string) error {
	_, err := i.store.AppendEdgeFact(ctx, store.AppendEdgeInput{
		Edge: domain.Edge{
			Predicate: pred, From: from, To: to,
			Owner: req.Owner, Space: req.Space, Visibility: req.visOrDefault(),
			Provenance: domain.Provenance{Asserter: "ingest", Method: domain.Asserted, Source: req.Ref},
		},
		Writer: ingestWriter, DedupeKey: dedupe, Actor: req.Owner,
	})
	if err != nil && !errors.Is(err, store.ErrDuplicateFact) {
		return err
	}
	return nil
}

// resolveHeyarrWork asks heyarr which work carries a tmdb id.
//
// Pinned to heyarr's shipped get_external_ids MCP tool (ADR-0050, heyarr PR
// #355): the reverse lookup {source,value} returns
// {external_ids:[{source,value,entity_type,entity_id}]} — an empty list on no
// match, never an error. We take the first row whose entity_type is "work". A
// full contract-drift gate over heyarr's vendored MCP contract is still a
// follow-up; this decode matches the tool heyarr now exposes.
func (i *Ingestor) resolveHeyarrWork(ctx context.Context, tmdb string) (string, bool, error) {
	var out struct {
		ExternalIDs []struct {
			Source     string `json:"source"`
			Value      string `json:"value"`
			EntityType string `json:"entity_type"`
			EntityID   string `json:"entity_id"`
		} `json:"external_ids"`
	}
	if err := i.reconcil.Call(ctx, "get_external_ids",
		map[string]string{"source": "tmdb", "value": tmdb}, &out); err != nil {
		return "", false, err
	}
	for _, x := range out.ExternalIDs {
		if x.EntityType == "work" && x.EntityID != "" {
			return x.EntityID, true, nil
		}
	}
	return "", false, nil
}

// visOrDefault resolves the request visibility, defaulting to private.
func (r Request) visOrDefault() domain.Visibility {
	if r.Visibility == "" {
		return domain.VisPrivate
	}
	return r.Visibility
}

// ownerFilter builds the access filter for the ingesting owner (its own private
// nodes plus its space and public), used for the read-back in attachTMDB.
func ownerFilter(req Request) access.Filter {
	af := access.Filter{Principal: req.Owner, AllowPublic: true}
	if req.Space != "" {
		af.Spaces = []domain.SpaceID{req.Space}
	}
	return af
}

// putIf sets key=val only when val is non-empty, keeping the JSON-LD bag tidy.
func putIf(m map[string]any, key, val string) {
	if val != "" {
		m[key] = val
	}
}
