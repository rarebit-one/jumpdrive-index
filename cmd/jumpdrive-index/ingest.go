package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"strings"

	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/heyarr"
	"github.com/rarebit-one/jumpdrive-index/internal/ingest"
	"github.com/rarebit-one/jumpdrive-index/internal/secret"
	"github.com/rarebit-one/jumpdrive-index/internal/store/sqlite"
)

// runIngest is the `jumpdrive-index ingest` subcommand: it turns one YouTube
// video into graph nodes/edges (a VideoObject + its channel + author edge, and —
// when --heyarr-url is set and --subject-tmdb ids resolve — about edges to heyarr
// works). It drives the SQLite store directly (the homelab/Starchart backend);
// the transcription toolchain (yt-dlp + Whisper) runs out of process.
func runIngest(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	var (
		db          = fs.String("db", "jumpdrive-index.db", "SQLite database path (must be migrated)")
		ref         = fs.String("ref", "", "YouTube URL or video id (required)")
		owner       = fs.String("owner", "", "owning principal id")
		space       = fs.String("space", "", "space id")
		visibility  = fs.String("visibility", "private", "private|space|public")
		subjectTMDB = fs.String("subject-tmdb", "", "comma-separated tmdb ids the video is about")
		heyarrURL   = fs.String("heyarr-url", "", "heyarr MCP endpoint for reconciliation (optional)")
		heyarrToken = fs.String("heyarr-token", "", "heyarr bearer token (optional)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*ref) == "" {
		return fmt.Errorf("ingest: --ref is required")
	}

	st, err := sqlite.Open(sqlite.Options{
		Path:       *db,
		Thresholds: domain.Thresholds{AutoMerge: 0.94, Review: 0.86},
	})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	var reconciler *heyarr.Client
	if strings.TrimSpace(*heyarrURL) != "" {
		reconciler, err = heyarr.New(heyarr.Options{BaseURL: *heyarrURL, Token: secret.Value(*heyarrToken)})
		if err != nil {
			return fmt.Errorf("heyarr client: %w", err)
		}
	}

	// A typed-nil *heyarr.Client would be a non-nil interface (a footgun the
	// ingestor documents against), so pass an untyped nil when disabled.
	var ing *ingest.Ingestor
	if reconciler != nil {
		ing = ingest.New(st, ingest.NewExecCapability(), reconciler)
	} else {
		ing = ingest.New(st, ingest.NewExecCapability(), nil)
	}

	res, err := ing.Ingest(ctx, ingest.Request{
		Ref:         *ref,
		Owner:       domain.PrincipalID(*owner),
		Space:       domain.SpaceID(*space),
		Visibility:  domain.Visibility(*visibility),
		SubjectTMDB: splitCSV(*subjectTMDB),
	})
	if err != nil {
		return fmt.Errorf("ingest: %w", err)
	}

	log.Info("ingested video",
		"video", res.Video, "channel", res.Channel,
		"about_works", res.AboutWorks, "transcribed", res.Transcribed)
	return nil
}

// splitCSV splits a comma-separated flag value into trimmed, non-empty items.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
