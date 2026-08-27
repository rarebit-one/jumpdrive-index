// Command jumpdrive-index is the service entrypoint. main() is tiny and delegates
// to run() so every failure returns an error rather than calling os.Exit mid-stack
// (jumpdrive-broker's shape). It assembles the stack from config: a storage
// backend (sqlite | postgres) under an access model (starchart | jumpdrive),
// wrapped by the service authorization layer, exposed as an MCP endpoint over
// HTTP. MODE=migrate runs migrations and exits; MODE=serve serves.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/access/jumpdrive"
	"github.com/rarebit-one/jumpdrive-index/internal/access/starchart"
	"github.com/rarebit-one/jumpdrive-index/internal/config"
	"github.com/rarebit-one/jumpdrive-index/internal/embed"
	"github.com/rarebit-one/jumpdrive-index/internal/httpapi"
	"github.com/rarebit-one/jumpdrive-index/internal/mcp"
	"github.com/rarebit-one/jumpdrive-index/internal/secret"
	"github.com/rarebit-one/jumpdrive-index/internal/service"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
	"github.com/rarebit-one/jumpdrive-index/internal/store/postgres"
	"github.com/rarebit-one/jumpdrive-index/internal/store/sqlite"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// The `ingest` subcommand is a one-shot tool (YouTube video → graph nodes),
	// distinct from the config-driven migrate|serve modes; dispatch it before
	// run() reads the serve config.
	if len(os.Args) > 1 && os.Args[1] == "ingest" {
		if err := runIngest(log, os.Args[2:]); err != nil {
			log.Error("fatal", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx := context.Background()
	st, err := openStore(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	if cfg.Mode == config.ModeMigrate {
		if err := st.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		log.Info("migrations applied", "backend", cfg.Backend)
		return nil
	}

	// Serve mode: read the projection head up front so a store that never migrated
	// fails loudly here rather than on the first request.
	head, err := st.ProjectionHead(ctx)
	if err != nil {
		return fmt.Errorf("projection head: %w (did you run MODE=migrate?)", err)
	}

	am, err := buildAccessModel(cfg)
	if err != nil {
		return fmt.Errorf("access model: %w", err)
	}

	em, err := buildEmbedder()
	if err != nil {
		return fmt.Errorf("embedder: %w", err)
	}

	srv := httpapi.New(mcp.New(service.New(st, am, em)), log)

	sigctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("jumpdrive-index serving",
		"addr", cfg.HTTPAddr, "backend", cfg.Backend, "identity", cfg.IdentityMode,
		"auth", cfg.AuthEnabled, "projection_head", head)
	return srv.Serve(sigctx, cfg.HTTPAddr)
}

// openStore opens the configured storage backend behind the store.Store seam.
func openStore(ctx context.Context, cfg *config.Config) (store.Store, error) {
	switch cfg.Backend {
	case config.BackendSQLite:
		path := cfg.DSN
		if path == "" {
			path = "jumpdrive-index.db"
		}
		return sqlite.Open(sqlite.Options{Path: path, Thresholds: cfg.Thresholds})
	case config.BackendPostgres:
		return postgres.Open(ctx, postgres.Options{DSN: cfg.DSN, Thresholds: cfg.Thresholds})
	default:
		return nil, fmt.Errorf("unknown backend %q", cfg.Backend)
	}
}

// buildAccessModel wires the configured identity seam.
func buildAccessModel(cfg *config.Config) (access.Model, error) {
	switch cfg.IdentityMode {
	case config.IdentityStarchart:
		var scfg starchart.Config
		if cfg.PrincipalsFile != "" {
			b, err := os.ReadFile(cfg.PrincipalsFile)
			if err != nil {
				return nil, fmt.Errorf("read principals file: %w", err)
			}
			if err := json.Unmarshal(b, &scfg); err != nil {
				return nil, fmt.Errorf("parse principals file: %w", err)
			}
		}
		return starchart.New(scfg)
	case config.IdentityJumpdrive:
		return jumpdrive.New(jumpdrive.Config{
			BaseURL:      cfg.JumpdriveURL,
			SharedSecret: os.Getenv("JDX_JUMPDRIVE_SECRET"),
		})
	default:
		return nil, fmt.Errorf("unknown identity mode %q", cfg.IdentityMode)
	}
}

// buildEmbedder wires the optional embedding provider from env. "none" (the
// default) disables the semantic path; "ollama" needs JDX_EMBED_URL +
// JDX_EMBED_MODEL; "fabric" is the TechnoCore switchboard (Farcaster) — an
// OpenAI-compatible /v1/embeddings endpoint — and additionally accepts an
// optional JDX_EMBED_TOKEN bearer, keeping the model off-host so the binary stays
// static / CGO-off.
func buildEmbedder() (embed.Embedder, error) {
	switch p := os.Getenv("JDX_EMBED_PROVIDER"); p {
	case "", "none":
		return nil, nil
	case "ollama":
		return embed.NewOllama(os.Getenv("JDX_EMBED_URL"), os.Getenv("JDX_EMBED_MODEL"))
	case "fabric":
		return embed.NewFabric(os.Getenv("JDX_EMBED_URL"), os.Getenv("JDX_EMBED_MODEL"), secret.Value(os.Getenv("JDX_EMBED_TOKEN")))
	default:
		return nil, fmt.Errorf("unknown embed provider %q (want none|ollama|fabric)", p)
	}
}
