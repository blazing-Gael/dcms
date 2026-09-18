// Package dcms is the public embedding surface for DCMS: build a server from a
// schema and config, register in-process business-logic hooks and custom routes,
// and serve — all from your own Go module, without forking the tree.
//
//	app, err := dcms.New(dcms.Options{SchemaPath: "schema.yaml", ConfigPath: "dcms.config.yaml"})
//	if err != nil { log.Fatal(err) }
//	app.On("orders", dcms.BeforeCreate, requireStock)
//	app.Route("POST", "/checkout", checkout)
//	log.Fatal(app.Serve(context.Background()))
//
// The `dcms` binary is itself a thin consumer of this package, so the library
// path and the CLI never diverge. Hooks and their contract are documented in
// docs/HOOKS.md and ADR-0031.
package dcms

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
	"github.com/blazing-Gael/dcms/pkg/auth"
)

// ── Public hook surface (re-exported from the gateway) ────────────────────────
//
// These aliases are the whole hook API an embedder needs; the implementations
// live in the (internal) gateway but the types are stable here.

type (
	// HookEvent names a point in a write's lifecycle. See the Before*/After* consts.
	HookEvent = gateway.HookEvent
	// HookContext is what a hook receives about the write it runs on.
	HookContext = gateway.HookContext
	// HookStore is the narrow, transaction-enrolled store handle a hook may use.
	HookStore = gateway.HookStore
	// HookError lets a hook choose the HTTP status of a rejection (default 422).
	HookError = gateway.HookError
	// WriteHook is one registered hook function.
	WriteHook = gateway.WriteHook
	// Principal is the verified identity behind a request.
	Principal = auth.Principal
	// Record is a single stored record (a field map).
	Record = store.Record
)

// Write-lifecycle events. Delete events fire for both hard and soft deletes.
const (
	BeforeCreate = gateway.BeforeCreate
	AfterCreate  = gateway.AfterCreate
	BeforeUpdate = gateway.BeforeUpdate
	AfterUpdate  = gateway.AfterUpdate
	BeforeDelete = gateway.BeforeDelete
	AfterDelete  = gateway.AfterDelete
)

// Options configures a new App. Paths are loaded exactly as the CLI loads them
// (config file, then environment, then these overrides), so a library user gets
// the same CORS/rate-limit/media/auth wiring as `dcms serve`.
type Options struct {
	// SchemaPath is the schema file. Empty ⇒ the config's `schema:` value.
	SchemaPath string
	// ConfigPath is the config file. Empty ⇒ the default (dcms.config.yaml); a
	// missing default is fine, a missing explicit path is an error.
	ConfigPath string
	// ConfigRequired makes a missing ConfigPath an error (the CLI sets this when
	// --config was passed explicitly).
	ConfigRequired bool
	// DBPath overrides the config's database path. Empty ⇒ the config value.
	DBPath string
	// Port overrides the config's server port. Zero ⇒ the config value.
	Port int
	// AutoMigrate applies pending additive migrations on Serve instead of refusing
	// to start (the `dev` behaviour). Off by default (the `serve` behaviour): a
	// backend with pending migrations fails loudly at boot.
	AutoMigrate bool
	// Dev turns on strict response validation by default (unless the config sets it
	// explicitly). Off by default.
	Dev bool
	// Logger receives server logs. Nil ⇒ a text logger on stderr.
	Logger *slog.Logger
}

// App is a configured DCMS server: schema, store, and any hooks/routes registered
// before Serve. Build one with New.
type App struct {
	cfg    config.Config
	def    *schema.SchemaDefinition
	db     store.Adapter
	logger *slog.Logger
	opts   Options

	hooks  *gateway.HookRegistry
	routes []gateway.CustomRoute
}

// New loads the schema and config, opens the store, and returns an App ready for
// hook/route registration and Serve. It does not migrate or listen.
func New(o Options) (*App, error) {
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
	return &App{cfg: cfg, def: def, db: db, logger: logger, opts: o, hooks: gateway.NewHookRegistry()}, nil
}

// On registers a write-lifecycle hook for a collection (ADR-0031). Hooks fire in
// registration order. Returns the App so calls can chain.
func (a *App) On(collection string, event HookEvent, fn WriteHook) *App {
	a.hooks.On(collection, event, fn)
	return a
}

// Route registers a custom endpoint that mounts inside the DCMS middleware stack
// (identity resolution, body cap, timeout, rate limit). The handler receives a
// Request with the verified principal and a store handle. Returns the App so calls
// can chain.
func (a *App) Route(method, path string, fn RouteFunc) *App {
	a.routes = append(a.routes, gateway.CustomRoute{Method: method, Path: path, Handler: a.wrapRoute(fn)})
	return a
}

// Close releases the store. Call it when the App is done (Serve blocks until the
// context is cancelled, so this is typically deferred right after New).
func (a *App) Close() error { return a.db.Close() }

// Serve builds the gateway options from config (merging in the registered hooks
// and routes), applies or checks migrations, seeds the bootstrap admin, and serves
// until ctx is cancelled. It is the same orchestration the `dcms` binary runs.
func (a *App) Serve(ctx context.Context) error {
	// Recognized-but-unimplemented directives (issue #35) load but do nothing yet.
	for _, warn := range a.def.Warnings {
		a.logger.Warn("schema", "note", warn)
	}

	if a.opts.AutoMigrate {
		if err := engine.Apply(ctx, a.db, a.def); err != nil {
			return err
		}
	} else {
		pending, err := engine.Plan(ctx, a.db, a.def)
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			return fmt.Errorf("database has %d pending migration(s); run `dcms migrate` before serving", len(pending))
		}
	}

	if err := gateway.EnsureSeedAdmin(ctx, a.db, a.cfg.Auth.AdminEmail, a.cfg.Auth.AdminPassword, a.logger); err != nil {
		return err
	}

	opts, tlsCfg, err := a.gatewayOptions(ctx)
	if err != nil {
		return err
	}
	scheme := "http"
	if tlsCfg.CertFile != "" && tlsCfg.KeyFile != "" {
		scheme = "https"
	}
	fmt.Printf("dcms — %d collection(s) from %s\n", len(a.def.Collections), a.cfg.Schema)
	fmt.Printf("listening on %s://localhost:%d  (Ctrl+C to stop)\n", scheme, a.cfg.Server.Port)
	return engine.Serve(ctx, a.def, a.db, fmt.Sprintf(":%d", a.cfg.Server.Port), a.logger, opts, tlsCfg)
}

// configPath returns p, or the default config path when p is empty.
func configPath(p string) string {
	if p == "" {
		return config.DefaultConfigPath
	}
	return p
}
