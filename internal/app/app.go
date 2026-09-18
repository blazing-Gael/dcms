// Package app is the internal server orchestration shared by the `dcms` binary
// and the public github.com/blazing-Gael/dcms library facade: it loads the schema
// and config, opens the store, and serves. Keeping it here (not in the public
// package) means the public API stays a thin surface and this glue — which pulls
// in the whole engine — is encapsulated, while both entrypoints run one code path
// so they can never drift.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/blazing-Gael/dcms/internal/config"
	"github.com/blazing-Gael/dcms/internal/engine"
	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// LoadOptions configures Load. Paths resolve like the CLI (config file, then
// environment, then these overrides).
type LoadOptions struct {
	SchemaPath     string
	ConfigPath     string
	ConfigRequired bool
	DBPath         string
	Port           int
	AutoMigrate    bool
	Dev            bool
	Label          string // shown in the startup banner (e.g. "dev"/"serve"); optional
	Logger         *slog.Logger
}

// Server is a loaded but not-yet-listening DCMS server.
type Server struct {
	cfg         config.Config
	def         *schema.SchemaDefinition
	db          store.Adapter
	logger      *slog.Logger
	autoMigrate bool
	dev         bool
	label       string
}

// Load reads the schema and config, opens the store, and returns a Server. It does
// not migrate or listen.
func Load(o LoadOptions) (*Server, error) {
	cfg, found, err := config.Load(configPath(o.ConfigPath))
	if err != nil {
		return nil, err
	}
	if o.ConfigRequired && !found {
		return nil, fmt.Errorf("config file %q not found", configPath(o.ConfigPath))
	}
	if err := cfg.ApplyEnv(); err != nil {
		return nil, err
	}
	if o.SchemaPath != "" {
		cfg.Schema = o.SchemaPath
	}
	if o.DBPath != "" {
		cfg.Database.Path = o.DBPath
	}
	if o.Port != 0 {
		cfg.Server.Port = o.Port
	}
	// Guard against a config naming a driver we don't ship yet, so a stale setting
	// fails loudly instead of being silently ignored.
	if cfg.Database.Driver != "sqlite" {
		return nil, fmt.Errorf("database driver %q not supported yet (only %q)", cfg.Database.Driver, "sqlite")
	}

	if _, statErr := os.Stat(cfg.Schema); os.IsNotExist(statErr) {
		return nil, fmt.Errorf("no schema at %s — run `dcms init` to scaffold a project first", cfg.Schema)
	}
	def, err := engine.LoadSchema(cfg.Schema)
	if err != nil {
		return nil, err
	}
	db, err := engine.OpenStore(cfg.Database.Path)
	if err != nil {
		return nil, err
	}

	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Server{cfg: cfg, def: def, db: db, logger: logger, autoMigrate: o.AutoMigrate, dev: o.Dev, label: o.Label}, nil
}

// DB returns the opened store adapter (the public layer needs it to give custom
// routes a store handle).
func (s *Server) DB() store.Adapter { return s.db }

// Def returns the parsed schema.
func (s *Server) Def() *schema.SchemaDefinition { return s.def }

// Logger returns the server logger.
func (s *Server) Logger() *slog.Logger { return s.logger }

// Close releases the store.
func (s *Server) Close() error { return s.db.Close() }

// Serve applies or checks migrations, seeds the bootstrap admin, and serves until
// ctx is cancelled, with the given hooks and custom routes wired in. It is the same
// orchestration the `dcms` binary and the library App both run.
func (s *Server) Serve(ctx context.Context, hooks *gateway.HookRegistry, routes []gateway.CustomRoute) error {
	// Recognized-but-unimplemented directives (issue #35) load but do nothing yet.
	for _, warn := range s.def.Warnings {
		s.logger.Warn("schema", "note", warn)
	}

	if s.autoMigrate {
		if err := engine.Apply(ctx, s.db, s.def); err != nil {
			return err
		}
	} else {
		pending, err := engine.Plan(ctx, s.db, s.def)
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			return fmt.Errorf("database has %d pending migration(s); run `dcms migrate` before serving", len(pending))
		}
	}

	if err := gateway.EnsureSeedAdmin(ctx, s.db, s.cfg.Auth.AdminEmail, s.cfg.Auth.AdminPassword, s.logger); err != nil {
		return err
	}

	opts, tlsCfg, err := s.GatewayOptions(ctx, hooks, routes)
	if err != nil {
		return err
	}
	scheme := "http"
	if tlsCfg.CertFile != "" && tlsCfg.KeyFile != "" {
		scheme = "https"
	}
	name := "dcms"
	if s.label != "" {
		name = "dcms " + s.label
	}
	fmt.Printf("%s — %d collection(s) from %s\n", name, len(s.def.Collections), s.cfg.Schema)
	fmt.Printf("listening on %s://localhost:%d  (Ctrl+C to stop)\n", scheme, s.cfg.Server.Port)
	return engine.Serve(ctx, s.def, s.db, fmt.Sprintf(":%d", s.cfg.Server.Port), s.logger, opts, tlsCfg)
}

// configPath returns p, or the default config path when p is empty.
func configPath(p string) string {
	if p == "" {
		return config.DefaultConfigPath
	}
	return p
}
