// Command jumpdrive-index is the entrypoint for the knowledge-graph index
// service. Following jumpdrive-broker's shape, main() is tiny: it wires logging,
// then delegates to run() so every failure path returns an error rather than
// calling os.Exit mid-stack.
//
// This is a scaffold: it loads and validates config and reports the resolved
// mode. The HTTP/MCP server, the storage adapters, and the access model are
// wired in subsequent milestones.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/rarebit-one/jumpdrive-index/internal/config"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
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
	log.Info("jumpdrive-index configured",
		"mode", cfg.Mode,
		"backend", cfg.Backend,
		"identity", cfg.IdentityMode,
		"addr", cfg.HTTPAddr,
		"auth", cfg.AuthEnabled,
	)
	// TODO(M0+): open store (backend), wire access model (identity), start
	// HTTP+MCP server, or run migrations when Mode==migrate.
	log.Info("scaffold: server not yet implemented")
	return nil
}
