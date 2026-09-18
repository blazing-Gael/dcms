// Package gateway builds the virtual HTTP router from a parsed schema.
//
// For every collection it registers CRUD routes (list/create/get/update/delete)
// plus the introspection endpoints (/__schema, /__health, /__ready). Authorization
// (the schema `access:` rules) is enforced here at the gateway layer — never inside
// the store (ADR-0003, ADR-0016).
//
// See DEV_ROADMAP.md section 1.3 for the Phase 1 router acceptance criteria.
package gateway

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// mediaBasePath is where the media library's byte-path endpoints live. It is
// outside the collection API (/api/v1) because media's write path is bytes, not
// JSON (ADR-0011).
const mediaBasePath = "/__media"

// defaultMaxUploadBytes caps a single upload when none is configured (32 MiB).
const defaultMaxUploadBytes = 32 << 20

// defaultMaxBodyBytes caps a JSON request body when none is configured (1 MiB).
// This bounds the memory a single create/update/auth request can force us to
// buffer, closing a trivial memory-exhaustion DoS. Media byte uploads are capped
// separately by MaxUploadBytes on their own byte path — this never applies there.
const defaultMaxBodyBytes = 1 << 20

// defaultRequestTimeout bounds how long a single JSON request may run before its
// context is cancelled when none is configured. It is cooperative: the store
// honors the deadline at query boundaries, so a pathological query can't pin a
// connection indefinitely. Media byte routes are exempt (large uploads over slow
// links legitimately outlast it).
const defaultRequestTimeout = 15 * time.Second

// engineColTypes are the columns the engine adds to every collection; they are
// valid for filtering/sorting even though they aren't declared in the schema.
var engineColTypes = map[string]schema.FieldType{
	"id":         schema.TypeString,
	"created_at": schema.TypeDateTime,
	"updated_at": schema.TypeDateTime,
	"created_by": schema.TypeString,
	"updated_by": schema.TypeString,
}

// Options tunes gateway behavior. The zero value is a valid, permissive config.
type Options struct {
	// ValidateResponses turns on strict response validation: every outgoing
	// record is checked against the schema, and a violation returns 500 instead
	// of shipping non-conforming data. It guards against store/adapter drift and
	// is best enabled in dev/CI (see ADR-0008). Off by default so production
	// pays nothing and never fails a read over a stored-data quirk.
	ValidateResponses bool

	// Blob is the byte store backing the media library (ADR-0011). When nil, the
	// /__media endpoints report 503 — media is unconfigured.
	Blob blob.Store
	// MaxUploadBytes caps a single media upload; 0 uses defaultMaxUploadBytes.
	MaxUploadBytes int64
	// AllowedContentTypes optionally restricts uploads (exact matches, or a
	// trailing-slash prefix like "image/"). Empty means any type is accepted.
	AllowedContentTypes []string
	// MediaQuota caps total uploaded bytes per principal (issue #31). Nil ⇒ no quota.
	MediaQuota *MediaQuotaOptions

	// PreviewToken, when set, unlocks the lifecycle preview bypass (ADR-0012): a
	// request presenting it via X-DCMS-Preview (or ?preview_token=) may view
	// drafts/scheduled/archived/trashed content through ?status / ?include_deleted.
	// Empty disables the bypass — public reads only. Supplied via env only.
	PreviewToken string

	// Introspection gates the schema/docs routes (/__schema, /__openapi, /__docs):
	// "public" (default/"") exposes them as today; "admin" requires an admin
	// principal; "off" returns 404. Those three routes always carry an
	// X-Robots-Tag: noindex header. /__health and /__ready are never gated.
	Introspection string

	// Authenticator resolves a request's principal (ADR-0016). When set,
	// authorization (the `access:` rules) is enforced; when nil, auth is not
	// configured and enforcement is bypassed (pre-auth behavior). Production wires
	// the session-backed authenticator; tests may leave it nil or supply a stub.
	Authenticator Authenticator

	// MaxBodyBytes caps a JSON request body on the collection API and /auth; 0
	// uses defaultMaxBodyBytes (1 MiB). Media uploads use MaxUploadBytes instead.
	MaxBodyBytes int64
	// RequestTimeout bounds a single JSON request's lifetime before its context is
	// cancelled; 0 uses defaultRequestTimeout (15s). A negative value disables it.
	// Media byte routes are exempt.
	RequestTimeout time.Duration

	// RateLimit configures request rate limiting (per-principal on the API,
	// per-IP on /auth). Nil disables limiting entirely — the zero-value gateway
	// and tests do none; production wires it. Zero fields inside take defaults.
	RateLimit *RateLimitOptions

	// Idempotency enables Idempotency-Key handling on POST creates (ADR-0018).
	// Nil disables it (the zero-value gateway and tests do none); production wires
	// it. A zero TTL inside takes the default (24h).
	Idempotency *IdempotencyOptions

	// TrustProxy trusts a fronting proxy's X-Forwarded-* headers: X-Forwarded-For
	// for rate-limit client-IP keying, and X-Forwarded-Proto for the session
	// cookie's Secure flag when TLS is terminated at the proxy. Off by default —
	// enable only behind a proxy you control, or these become client-spoofable.
	TrustProxy bool

	// CORS enables cross-origin access when non-nil (and its AllowedOrigins is
	// non-empty). Nil ⇒ no CORS headers (same-origin only), the safe default.
	CORS *CORSOptions

	// AdminRoles are the roles permitted to use the /admin/users API (ADR-0019).
	// Empty defaults to ["admin"].
	AdminRoles []string
	// Registration enables self-registration when non-nil (ADR-0019). Nil ⇒
	// disabled (POST /auth/register is 403).
	Registration *RegistrationOptions
	// PasswordMinLength is the minimum accepted password length; 0 uses the
	// default (8). A hard 72-byte maximum always applies (bcrypt truncates there).
	PasswordMinLength int

	// Notifier delivers account emails (password reset). Nil uses a dev-log
	// notifier that prints the link to the console (ADR-0019 phase 2).
	Notifier Notifier
	// ResetLinkBase is the frontend URL a reset link points at; the token is
	// appended as ?token=. Empty ⇒ the raw token is delivered (dev).
	ResetLinkBase string
	// ResetLinkBases is an allowlist of reset URLs for a multi-frontend instance
	// (issue #32). When set it takes precedence over ResetLinkBase; a forgot
	// request's `return_to` is honoured only if it exactly matches an entry, else
	// the first entry is used (never an arbitrary URL — no open redirect).
	ResetLinkBases []string
	// ResetTokenTTL is how long a reset token is valid; 0 uses the default (1h).
	ResetTokenTTL time.Duration

	// OTPLogin enables passwordless email-OTP login when non-nil (issue #11). Nil ⇒
	// the /auth/otp/* endpoints are not mounted (404).
	OTPLogin *OTPLoginOptions

	// Webhooks configures signed webhook delivery of change events (ADR-0021 M-B
	// phase 2). Nil ⇒ no delivery worker runs; the change feed still works. Only
	// meaningful for collections that opt into `events:`.
	Webhooks *WebhookOptions

	// Hooks holds opt-in, in-process business-logic hooks that run on the write
	// lifecycle of the generated endpoints (ADR-0031). Nil ⇒ no hooks. Build one
	// with NewHookRegistry().On(collection, event, fn).
	Hooks *HookRegistry
}

// RegistrationOptions configures self-registration (ADR-0019).
type RegistrationOptions struct {
	// DefaultRoles are granted to a self-registered user (validated at construction
	// against declared, non-admin roles).
	DefaultRoles []string
}

// OTPLoginOptions configures passwordless email-OTP login (issue #11, ADR-0029).
// Its zero value is usable: the defaults below apply to any unset field.
type OTPLoginOptions struct {
	// TTL is how long an emailed code stays valid; 0 uses the default (10m). Kept
	// short because a 6-digit code is low-entropy — the brief window plus the
	// per-code attempt cap is what makes it safe.
	TTL time.Duration
	// MaxAttempts is the number of wrong guesses a single code tolerates before it
	// is burned; 0 uses the default (5). Bounds brute-forcing of the 10^6 space.
	MaxAttempts int
}

const (
	defaultOTPTTL         = 10 * time.Minute
	defaultOTPMaxAttempts = 5
	// otpCodeDigits is the length of the emailed numeric code.
	otpCodeDigits = 6
	// otpRequestsPerMinutePerEmail throttles code requests per recipient (on top of
	// the per-IP auth tier), so the endpoint can't be used to bomb one inbox from
	// many IPs. Fixed — not a config knob — since it guards a person, not a client.
	otpRequestsPerMinutePerEmail = 4
	otpRequestBurstPerEmail      = 2
)

// IdempotencyOptions configures idempotent-write handling (ADR-0018).
type IdempotencyOptions struct {
	// TTL is how long a recorded key is honored for replay; 0 uses 24h.
	TTL time.Duration
}

// Server wires a parsed schema and a storage adapter into an http.Handler.
type Server struct {
	schema      *schema.SchemaDefinition
	db          store.Adapter
	collections map[string]schema.CollectionDef // by name, for O(1) lookup
	logger      *slog.Logger
	opts        Options
	// otpEmailLimiter throttles email-OTP code requests per recipient (issue #11);
	// nil when OTP login is disabled. Built once so its buckets persist.
	otpEmailLimiter RateLimiter
}

// New constructs a gateway Server. If logger is nil, slog.Default() is used.
// Options are optional; the zero value applies when none is passed.
func New(s *schema.SchemaDefinition, db store.Adapter, logger *slog.Logger, opts ...Options) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	cols := make(map[string]schema.CollectionDef, len(s.Collections))
	for _, c := range s.Collections {
		cols[c.Name] = c
	}
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	srv := &Server{schema: s, db: db, collections: cols, logger: logger, opts: o}
	if o.OTPLogin != nil {
		srv.otpEmailLimiter = newMemoryLimiter(otpRequestsPerMinutePerEmail, otpRequestBurstPerEmail)
	}
	return srv
}

// Handler returns the root http.Handler with all routes registered.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(s.requestLogger)
	r.Use(s.recoverer)
	// CORS runs before auth and the route-group limiters so a preflight OPTIONS is
	// answered without being authenticated or rate-limited.
	if s.opts.CORS != nil {
		r.Use(s.opts.CORS.withDefaults().middleware())
	}
	r.Use(s.withPrincipal)  // resolve the request's identity once (ADR-0016)
	r.Use(s.withVisibility) // resolve the request's lifecycle view once (ADR-0012)

	// Rate limiters (opt-in via Options.RateLimit). Built once here and shared
	// across requests so their buckets persist. The API tier keys per principal
	// (IP fallback) and so must run after withPrincipal, above; the auth tier
	// keys per client IP. Probes (/__health etc.) and media sit outside these
	// groups and are deliberately unlimited.
	var apiLimit, authLimit func(http.Handler) http.Handler
	if rl := s.opts.RateLimit; rl != nil {
		c := rl.withDefaults()
		apiLimiter := newMemoryLimiter(c.APIPerMinute, c.APIBurst)
		authLimiter := newMemoryLimiter(c.AuthPerMinute, c.AuthBurst)
		anonWriteLimiter := newMemoryLimiter(c.AnonWritePerMinute, c.AnonWriteBurst)
		trust := s.opts.TrustProxy
		// Anonymous collection writes get their own tight per-IP tier (issue #34).
		apiLimit = s.rateLimitAPI(apiLimiter, anonWriteLimiter, trust)
		authLimit = s.rateLimit(authLimiter, func(r *http.Request) string { return "ip:" + clientIP(r, trust) })
	}

	r.NotFound(s.handleNotFound)
	r.MethodNotAllowed(s.handleMethodNotAllowed)

	// Probes — never gated (load balancers / k8s need them, and they reveal
	// nothing about the data model).
	r.Get("/__health", s.handleHealth)
	r.Get("/__ready", s.handleReady)

	// Schema/docs introspection — gated by Options.Introspection and always marked
	// noindex, so the data model can be hidden from the public and from crawlers.
	r.Group(func(r chi.Router) {
		r.Use(s.introspectionGate)
		r.Get("/__schema", s.handleSchema)
		r.Get("/__openapi", s.handleOpenAPI)
		r.Get("/__docs", s.handleDocs)
	})

	// Local authentication (ADR-0016) — outside the collection API. JSON bodies,
	// so it gets the same body cap and request timeout as the collection API.
	r.Route("/auth", func(r chi.Router) {
		if authLimit != nil {
			r.Use(authLimit) // shed abusive auth traffic before any work
		}
		r.Use(s.limitBody)
		r.Use(s.withTimeout)
		r.Post("/login", s.handleLogin)
		r.Post("/logout", s.handleLogout)
		r.Get("/me", s.handleMe)
		// Account self-service (ADR-0019).
		r.Post("/register", s.handleRegister)
		r.Post("/password", s.handlePasswordChange)
		r.Post("/logout-all", s.handleLogoutAll)
		r.Post("/forgot", s.handleForgotPassword)
		r.Post("/reset", s.handleResetPassword)
		// Passwordless email-OTP login (issue #11), mounted only when enabled.
		if s.opts.OTPLogin != nil {
			r.Post("/otp/request", s.handleOTPRequest)
			r.Post("/otp/verify", s.handleOTPVerify)
		}
	})

	// User administration (ADR-0019) — admin-role-gated, JSON bodies. Gets the
	// API-tier rate limit (per principal) plus the body cap and timeout.
	r.Route("/admin/users", func(r chi.Router) {
		if apiLimit != nil {
			r.Use(apiLimit)
		}
		r.Use(s.limitBody)
		r.Use(s.withTimeout)
		r.Get("/", s.handleAdminUserList)
		r.Post("/", s.handleAdminUserCreate)
		r.Get("/{id}", s.handleAdminUserGet)
		r.Patch("/{id}", s.handleAdminUserUpdate)
		r.Delete("/{id}", s.handleAdminUserDelete)
		r.Post("/{id}/logout-all", s.handleAdminUserLogoutAll)
	})

	// Media library (byte path) — outside the collection API (ADR-0011).
	r.Route(mediaBasePath, func(r chi.Router) {
		r.Get("/", s.handleMediaList)
		r.Post("/", s.handleMediaUpload)
		r.Get("/{id}", s.handleMediaGet)
		r.Post("/{id}", s.handleMediaReplace)
		r.Patch("/{id}", s.handleMediaPatch)
		r.Delete("/{id}", s.handleMediaDelete)
		r.Get("/{id}/raw", s.handleMediaRaw)
	})

	// Virtual collection routes under the configured base URL. All JSON, so the
	// body cap and request timeout apply; the media byte path (registered above)
	// is deliberately left out — it has its own size cap and latency profile.
	r.Route(s.baseURL(), func(r chi.Router) {
		if apiLimit != nil {
			r.Use(apiLimit) // shed early, before body read / timeout setup
		}
		r.Use(s.limitBody)
		r.Use(s.withTimeout)
		// Change feed + webhook delivery ops (ADR-0021, M-B) — static routes,
		// matched before /{collection}.
		r.Get("/_changes", s.handleChanges)
		r.Get("/_events/deliveries", s.handleDeliveries)
		r.Post("/_events/deliveries/{id}/retry", s.handleRetryDelivery)
		r.Get("/{collection}", s.handleList)
		r.Post("/{collection}", s.handleCreate)
		r.Get("/{collection}/{id}", s.handleGetOne)
		r.Patch("/{collection}/{id}", s.handleUpdate)
		r.Delete("/{collection}/{id}", s.handleDelete)

		// Lifecycle transitions (ADR-0012) — 404 on collections without the
		// matching directive.
		r.Post("/{collection}/{id}/publish", s.handlePublish)
		r.Post("/{collection}/{id}/unpublish", s.handleUnpublish)
		r.Post("/{collection}/{id}/archive", s.handleArchive)
		r.Post("/{collection}/{id}/restore", s.handleRestore)

		// Version history (ADR-0013) — 404 on collections without `revisions`.
		r.Get("/{collection}/{id}/revisions", s.handleRevisionList)
		r.Get("/{collection}/{id}/revisions/{version}", s.handleRevisionGet)
		r.Post("/{collection}/{id}/revisions/{version}/restore", s.handleRevisionRestore)
	})

	return r
}

// baseURL returns the API base path. Delegates to the schema so the router and
// the generated OpenAPI spec always agree on the prefix.
func (s *Server) baseURL() string {
	return s.schema.BaseURL()
}

// maxBodyBytes is the configured JSON body cap, falling back to the default.
func (s *Server) maxBodyBytes() int64 {
	if s.opts.MaxBodyBytes > 0 {
		return s.opts.MaxBodyBytes
	}
	return defaultMaxBodyBytes
}

// requestTimeout is the configured per-request timeout, falling back to the
// default. A negative RequestTimeout disables the timeout (returns 0).
func (s *Server) requestTimeout() time.Duration {
	if s.opts.RequestTimeout < 0 {
		return 0
	}
	if s.opts.RequestTimeout > 0 {
		return s.opts.RequestTimeout
	}
	return defaultRequestTimeout
}

// knownCollection reports whether name is a collection declared in the schema.
func (s *Server) knownCollection(name string) bool {
	_, ok := s.collections[name]
	return ok
}

// routableCollection reports whether a name is reachable through the public
// collection API. Engine-managed collections (a leading underscore, e.g. _media)
// exist for expansion, migration, and typing but are not JSON-CRUD routable —
// _media is served through its own byte-path endpoints instead (ADR-0011).
func (s *Server) routableCollection(name string) bool {
	return s.knownCollection(name) && !strings.HasPrefix(name, "_")
}

// fieldType returns the schema type of a column (declared field or engine column)
// and whether it exists at all on the collection.
func (s *Server) fieldType(collection, field string) (schema.FieldType, bool) {
	if t, ok := engineColTypes[field]; ok {
		return t, true
	}
	cd, ok := s.collections[collection]
	if !ok {
		return "", false
	}
	// Lifecycle columns exist only on collections that opted in (ADR-0012); once
	// present they are sortable/filterable like any column.
	switch field {
	case schema.LifecycleStatus:
		if cd.Publishing {
			return schema.TypeString, true
		}
	case schema.LifecyclePublishedAt:
		if cd.Publishing {
			return schema.TypeDateTime, true
		}
	case schema.LifecycleDeletedAt:
		if cd.SoftDelete {
			return schema.TypeDateTime, true
		}
	}
	for _, f := range cd.Fields {
		if f.Name == field {
			return f.Type, true
		}
	}
	return "", false
}

func (s *Server) hasColumn(collection, field string) bool {
	_, ok := s.fieldType(collection, field)
	return ok
}
