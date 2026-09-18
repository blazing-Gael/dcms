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
// This package is a thin facade: the server orchestration lives in internal/app,
// which both this library and the `dcms` binary run, so the two never diverge.
// Hooks and their contract are documented in docs/HOOKS.md and ADR-0031.
package dcms

import (
	"context"
	"log/slog"

	"github.com/blazing-Gael/dcms/internal/app"
	"github.com/blazing-Gael/dcms/internal/gateway"
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
	// Label is shown in the startup banner (e.g. the CLI passes "dev"/"serve").
	// Optional; empty prints just "dcms".
	Label string
	// Logger receives server logs. Nil ⇒ a text logger on stderr.
	Logger *slog.Logger
}

// App is a configured DCMS server: the loaded server plus any hooks and routes
// registered before Serve. Build one with New.
type App struct {
	srv    *app.Server
	hooks  *gateway.HookRegistry
	routes []gateway.CustomRoute
}

// New loads the schema and config and opens the store, returning an App ready for
// hook/route registration and Serve. It does not migrate or listen.
func New(o Options) (*App, error) {
	srv, err := app.Load(app.LoadOptions{
		SchemaPath:     o.SchemaPath,
		ConfigPath:     o.ConfigPath,
		ConfigRequired: o.ConfigRequired,
		DBPath:         o.DBPath,
		Port:           o.Port,
		AutoMigrate:    o.AutoMigrate,
		Dev:            o.Dev,
		Label:          o.Label,
		Logger:         o.Logger,
	})
	if err != nil {
		return nil, err
	}
	return &App{srv: srv, hooks: gateway.NewHookRegistry()}, nil
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

// Serve runs the server (migrations, admin seed, listen) with the registered hooks
// and routes, until ctx is cancelled. It is the same orchestration `dcms serve`
// runs.
func (a *App) Serve(ctx context.Context) error {
	return a.srv.Serve(ctx, a.hooks, a.routes)
}

// Close releases the store. Serve blocks until ctx is cancelled, so this is
// typically deferred right after New.
func (a *App) Close() error { return a.srv.Close() }
