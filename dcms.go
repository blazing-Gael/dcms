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
	"sync"

	"github.com/blazing-Gael/dcms/internal/app"
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
	// WriteInput is a create/update payload for HookStore.Create / .Update.
	WriteInput = store.WriteInput
	// Query is a list query for HookStore.Find.
	Query = store.Query
	// Page is a HookStore.Find result.
	Page = store.Page
	// Filter is one field-level condition on a Query.
	Filter = store.Filter
	// Op is a Filter operator (see the Eq/Gt/… consts).
	Op = store.Op
)

// Filter operators, for building a Query in a hook or custom route.
const (
	Eq         = store.Eq
	Ne         = store.Ne
	Gt         = store.Gt
	Gte        = store.Gte
	Lt         = store.Lt
	Lte        = store.Lte
	Contains   = store.Contains
	StartsWith = store.StartsWith
	In         = store.In
	NotIn      = store.NotIn
	IsNull     = store.IsNull
	NotNull    = store.NotNull
)

// ── Authorization surface (ADR-0038) ─────────────────────────────────────────

// Action is an operation Can authorizes.
type Action = schema.AccessAction

// Actions for Can.
const (
	ActionRead    = schema.ActionRead
	ActionCreate  = schema.ActionCreate
	ActionUpdate  = schema.ActionUpdate
	ActionDelete  = schema.ActionDelete
	ActionPublish = schema.ActionPublish
	ActionHistory = schema.ActionHistory
)

// ValidationError lists per-field problems with a write (HTTP 422).
type ValidationError = store.ValidationError

// Errors returned by Can, As(p), and Media(). Test them with errors.Is.
var (
	ErrNotFound          = store.ErrNotFound          // absent, or hidden from the caller (404)
	ErrForbidden         = gateway.ErrForbidden       // denied, authenticated caller (403)
	ErrUnauthenticated   = gateway.ErrUnauthenticated // denied, anonymous caller (401)
	ErrVersionConflict   = gateway.ErrVersionConflict // IfVersion precondition failed (412)
	ErrReferenced        = gateway.ErrReferenced      // hard delete of a still-referenced record (409)
	ErrUnknownCollection = gateway.ErrUnknownCollection

	ErrMediaUnavailable     = gateway.ErrMediaUnavailable
	ErrSigningDisabled      = gateway.ErrSigningDisabled
	ErrUnsupportedMediaType = gateway.ErrUnsupportedMediaType
	ErrTooLarge             = gateway.ErrTooLarge
	ErrQuotaExceeded        = gateway.ErrQuotaExceeded
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

	mu     sync.Mutex
	frozen bool // set once the gateway is built; On/Route after that panic
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
	a.mustRegister("On")
	a.hooks.On(collection, event, fn)
	return a
}

// Route registers a custom endpoint that mounts inside the DCMS middleware stack
// (identity resolution, body cap, timeout, rate limit). The handler receives a
// Request with the verified principal, a store acting as them, and the privileged
// store. Returns the App so calls can chain.
func (a *App) Route(method, path string, fn RouteFunc) *App {
	a.mustRegister("Route")
	a.routes = append(a.routes, gateway.CustomRoute{Method: method, Path: path, Handler: a.wrapRoute(fn)})
	return a
}

// mustRegister panics when registration happens after the gateway was built (by
// Serve or the first Can/As/Media call): the hook or route would silently never
// run. Register everything first — the same rule as http.ServeMux misuse.
func (a *App) mustRegister(what string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.frozen {
		panic("dcms: App." + what + " called after Serve or after the first Can/As/Media call; register hooks and routes first")
	}
}

// gateway returns the App's gateway, building it (and freezing registration) on
// first use. HTTP and the Go API share this one instance (ADR-0038).
func (a *App) gateway(ctx context.Context) (*gateway.Server, error) {
	a.mu.Lock()
	a.frozen = true
	a.mu.Unlock()
	return a.srv.Gateway(ctx, a.hooks, a.routes)
}

// Serve runs the server (migrations, admin seed, listen) with the registered hooks
// and routes, until ctx is cancelled. It is the same orchestration `dcms serve`
// runs.
func (a *App) Serve(ctx context.Context) error {
	a.mu.Lock()
	a.frozen = true
	a.mu.Unlock()
	return a.srv.Serve(ctx, a.hooks, a.routes)
}

// Can reports whether p may perform action on record id of collection, by the same
// rules as the HTTP API (id is ignored for ActionCreate). False covers both "not
// allowed" and "not found / not visible to p"; err is a real failure.
func (a *App) Can(ctx context.Context, p Principal, action Action, collection, id string) (bool, error) {
	gw, err := a.gateway(ctx)
	if err != nil {
		return false, err
	}
	return gw.Can(ctx, p, action, collection, id)
}

// ReadFilters returns the filters that confine a query on collection to what p may
// read (read rule + the published lifecycle view), for AND-ing onto your own query
// through the privileged store. ok is false when p may read nothing.
func (a *App) ReadFilters(ctx context.Context, p Principal, collection string) ([]Filter, bool, error) {
	gw, err := a.gateway(ctx)
	if err != nil {
		return nil, false, err
	}
	return gw.ReadFilters(ctx, p, collection)
}

// As returns a store that reads and writes as p, enforcing everything the HTTP API
// does: read rules, lifecycle visibility, field masks, write-field rules,
// validation, hooks, events and revisions. It is the safe default for custom routes.
func (a *App) As(p Principal) ScopedStore { return ScopedStore{app: a, p: p} }

// Media returns in-process media operations (derived files, signed URLs).
func (a *App) Media() Media { return Media{app: a} }

// Close releases the store. Serve blocks until ctx is cancelled, so this is
// typically deferred right after New.
func (a *App) Close() error { return a.srv.Close() }
