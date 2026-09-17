// Package config resolves DCMS runtime settings from four layered sources.
//
// Precedence, highest wins:
//
//	command-line flags  >  environment variables  >  config file  >  built-in defaults
//
// The goal is that a deployment (a container, a systemd unit) can ship a single
// dcms.config.yaml instead of threading half a dozen flags through its launch
// command, while an operator can still override any one value on the command
// line or via an env var without editing the file.
//
// Per the project's configurability tenet, every knob here has a sensible
// default so the zero-config path (`dcms dev` in a directory with a schema)
// keeps working untouched.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultConfigPath is where we look for a config file when none is given.
const DefaultConfigPath = "./dcms.config.yaml"

// Config is the fully-resolved runtime configuration.
type Config struct {
	Schema   string   `yaml:"schema"`
	Database Database `yaml:"database"`
	Server   Server   `yaml:"server"`
	Media    Media    `yaml:"media"`
	Content  Content  `yaml:"content"`
	Auth     Auth     `yaml:"auth"`
	Events   Events   `yaml:"events"`
}

// Events configures the change-events subsystem (ADR-0021, M-B). The change feed
// itself needs no config; this is for webhook delivery.
type Events struct {
	// Webhooks are the endpoints change events are POSTed to. Empty ⇒ no delivery
	// worker runs.
	Webhooks []Webhook `yaml:"webhooks"`
	// WebhookPollSeconds is how often the delivery worker enqueues + delivers; 0
	// uses the engine default (a couple of seconds).
	WebhookPollSeconds int `yaml:"webhook_poll_seconds"`
}

// Webhook is one delivery endpoint. The HMAC secret is env-only (secrets rule):
// secret_env names the environment variable holding it, resolved at load.
type Webhook struct {
	Name        string   `yaml:"name"`
	URL         string   `yaml:"url"`
	SecretEnv   string   `yaml:"secret_env"`  // env var NAME holding the HMAC secret
	Secret      string   `yaml:"-"`           // resolved from SecretEnv; never from the file
	Events      []string `yaml:"events"`      // event-type filter; empty = all
	Collections []string `yaml:"collections"` // collection filter; empty = all
	MaxAttempts int      `yaml:"max_attempts"`
}

// Auth carries runtime auth secrets (ADR-0016). Both fields are env-only
// (DCMS_ADMIN_EMAIL / DCMS_ADMIN_PASSWORD) — the yaml:"-" keeps the bootstrap
// credential out of any committed config file, per the secrets rule (ADR-0009).
// They seed the first admin on startup when no users exist yet.
type Auth struct {
	AdminEmail    string `yaml:"-"`
	AdminPassword string `yaml:"-"`
	// Provider selects which Authenticator resolves the request principal
	// (ADR-0020, issue #9). "" / "session" ⇒ the built-in opaque-session source;
	// "proxy_header" ⇒ trust a verified-identity header set by a front proxy.
	Provider string `yaml:"provider"`
	// ProxyHeader configures the proxy_header provider.
	ProxyHeader ProxyHeader `yaml:"proxy_header"`
	// AdminRoles are the roles permitted to use the /admin/users API (ADR-0019).
	// Empty defaults to ["admin"].
	AdminRoles []string `yaml:"admin_roles"`
	// Registration configures self-registration (off unless enabled).
	Registration Registration `yaml:"registration"`
	// Password tunes the password policy.
	Password Password `yaml:"password"`
	// Reset configures the password-reset flow (ADR-0019 phase 2).
	Reset AuthReset `yaml:"reset"`
	// OTP configures passwordless email-OTP login (issue #11). Off unless enabled.
	OTP AuthOTP `yaml:"otp"`
	// SMTP configures the mailer for account emails. Host empty ⇒ a dev-log
	// mailer that prints reset links to the console.
	SMTP SMTP `yaml:"smtp"`
}

// ProxyHeader configures the proxy_header authenticator (issue #9): DCMS trusts
// an identity a front proxy (oauth2-proxy, Cloudflare Access, a JWT-validating
// gateway) has already verified and passes as request headers.
//
// SECURITY: only safe behind a proxy you control that STRIPS these headers from
// inbound client requests and re-sets them itself — otherwise any caller can set
// UserHeader and impersonate anyone. Same trust model as `server.trust_proxy`.
type ProxyHeader struct {
	// UserHeader carries the caller's stable id (becomes Principal.ID). Required
	// when the provider is proxy_header.
	UserHeader string `yaml:"user_header"`
	// RolesHeader carries the caller's roles, separated by RolesSeparator. Optional.
	RolesHeader string `yaml:"roles_header"`
	// RolesSeparator splits RolesHeader; empty defaults to ",".
	RolesSeparator string `yaml:"roles_separator"`
}

// AuthReset configures password reset (ADR-0019).
type AuthReset struct {
	// LinkBase is the frontend URL a reset link points at; the token is appended
	// as ?token=. Empty ⇒ the raw token is delivered (dev).
	LinkBase string `yaml:"link_base"`
	// LinkBases is an allowlist of reset URLs for an instance with several
	// frontends (issue #32). A `POST /auth/forgot` may name a `return_to`; it is
	// honoured only when it exactly matches one of these, else the first is used —
	// never an arbitrary URL, so this is not an open redirect. Takes precedence over
	// LinkBase when set (LinkBase is then the single-frontend shorthand).
	LinkBases []string `yaml:"link_bases"`
	// TTLMinutes is how long a reset token is valid; 0 uses the default (60).
	TTLMinutes int `yaml:"ttl_minutes"`
}

// AuthOTP configures passwordless email-OTP login (issue #11, ADR-0029). Off by
// default: enabling it lets anyone with access to a user's inbox obtain a session,
// so it is an explicit operator choice.
type AuthOTP struct {
	Enabled bool `yaml:"enabled"`
	// TTLMinutes is how long an emailed code is valid; 0 uses the default (10).
	TTLMinutes int `yaml:"ttl_minutes"`
	// MaxAttempts is the wrong-guess budget for one code; 0 uses the default (5).
	MaxAttempts int `yaml:"max_attempts"`
}

// SMTP configures outbound mail. Credentials are env-only (secrets rule).
type SMTP struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	From     string `yaml:"from"`
	Username string `yaml:"-"` // DCMS_SMTP_USERNAME
	Password string `yaml:"-"` // DCMS_SMTP_PASSWORD
}

// Registration configures self-registration (ADR-0019). Off by default.
type Registration struct {
	Enabled bool `yaml:"enabled"`
	// DefaultRoles are granted to a self-registered user. Must be declared roles,
	// and none may be an admin role (a self-registrant can't self-grant admin).
	DefaultRoles []string `yaml:"default_roles"`
}

// Password tunes the password policy (ADR-0019).
type Password struct {
	// MinLength is the minimum password length; 0 uses the engine default (8).
	MinLength int `yaml:"min_length"`
}

// Content configures record-lifecycle behavior (ADR-0012).
type Content struct {
	// PreviewToken unlocks the lifecycle preview bypass. It is a secret and so is
	// env-only (DCMS_PREVIEW_TOKEN) — never read from the config file.
	PreviewToken string `yaml:"-"`
}

// Media configures the file/blob storage backing the media library (ADR-0011).
type Media struct {
	// Driver selects the blob backend: "local" (default) or "s3".
	Driver string `yaml:"driver"`
	// Dir is the base directory for the local driver.
	Dir string `yaml:"dir"`
	// MaxUploadBytes caps a single upload; 0 uses the engine default (32 MiB).
	MaxUploadBytes int64 `yaml:"max_upload_bytes"`
	// AllowedContentTypes optionally restricts uploads (exact types, or a
	// trailing-slash prefix like "image/"). Empty accepts any type.
	AllowedContentTypes []string `yaml:"allowed_content_types"`
	// Quotas caps total uploaded bytes per principal (issue #31): a `default`
	// size and optional per-role overrides (the most permissive matching role
	// wins). Sizes are strings like "200MiB" / "5GiB" / "unlimited". Empty ⇒ no quota.
	Quotas MediaQuotas `yaml:"quotas"`

	// S3-compatible driver settings (MinIO, SeaweedFS, Cloudflare R2, AWS S3, …).
	Endpoint       string `yaml:"endpoint"`
	Region         string `yaml:"region"`
	Bucket         string `yaml:"bucket"`
	UseSSL         *bool  `yaml:"use_ssl"`
	ForcePathStyle bool   `yaml:"force_path_style"`
	PublicBaseURL  string `yaml:"public_base_url"`
	// AccessKey/SecretKey are credentials and therefore env-only — never read
	// from the config file (the yaml:"-" enforces this; see the secrets rule).
	AccessKey string `yaml:"-"`
	SecretKey string `yaml:"-"`
}

// MediaQuotas configures per-principal storage limits (issue #31). Sizes are
// human strings ("200MiB", "5GiB", "unlimited"); ParseByteSize converts them.
type MediaQuotas struct {
	Default string            `yaml:"default"`
	Roles   map[string]string `yaml:"roles"`
}

// ParseByteSize parses a human byte size ("200MiB", "5GB", "1024", "unlimited")
// into a byte count. "unlimited" (any case) returns -1. Binary units (KiB/MiB/GiB)
// are powers of 1024; decimal units (KB/MB/GB) are powers of 1000; a bare number
// is bytes.
func ParseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	if strings.EqualFold(s, "unlimited") {
		return -1, nil
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s[:i]), 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	var mult int64
	switch strings.ToLower(strings.TrimSpace(s[i:])) {
	case "", "b":
		mult = 1
	case "kb":
		mult = 1000
	case "kib", "k":
		mult = 1 << 10
	case "mb":
		mult = 1000 * 1000
	case "mib", "m":
		mult = 1 << 20
	case "gb":
		mult = 1000 * 1000 * 1000
	case "gib", "g":
		mult = 1 << 30
	case "tb":
		mult = 1000 * 1000 * 1000 * 1000
	case "tib", "t":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("unknown size unit in %q", s)
	}
	return int64(f * float64(mult)), nil
}

// Database selects and locates the backing store.
type Database struct {
	// Driver names the store adapter. Phase 1 ships "sqlite"; "postgres" and
	// others slot in here without touching the rest of the config surface.
	Driver string `yaml:"driver"`
	// Path is the SQLite file for the sqlite driver. For networked drivers a
	// future DSN field will live alongside it.
	Path string `yaml:"path"`
}

// Server holds HTTP listener settings.
type Server struct {
	Port int `yaml:"port"`
	// ValidateResponses enables strict response validation (see gateway.Options).
	// A pointer so "unset" is distinguishable from an explicit false: when unset,
	// `dcms dev` defaults it on (dev/CI is where you want the guardrail) while
	// production stays off. Set it explicitly to override that.
	ValidateResponses *bool `yaml:"validate_responses"`
	// MaxBodyBytes caps a JSON request body (create/update/auth). 0 uses the
	// engine default (1 MiB). Media uploads are capped by Media.MaxUploadBytes.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// RequestTimeoutSeconds bounds a single JSON request before its context is
	// cancelled. 0 uses the engine default (15s); a negative value disables it.
	RequestTimeoutSeconds int `yaml:"request_timeout_seconds"`
	// RateLimit configures request rate limiting.
	RateLimit RateLimit `yaml:"rate_limit"`
	// Idempotency configures idempotency-key handling on POST creates.
	Idempotency Idempotency `yaml:"idempotency"`
	// TrustProxy trusts a fronting reverse proxy's X-Forwarded-* headers:
	// X-Forwarded-For for client-IP rate-limit keying, and X-Forwarded-Proto for
	// the session cookie's Secure flag when TLS is terminated at the proxy. Enable
	// ONLY behind a proxy you control — otherwise a client can spoof these.
	TrustProxy bool `yaml:"trust_proxy"`
	// Introspection gates the schema/docs routes (/__schema, /__openapi, /__docs):
	// "public" (default) exposes them, "admin" requires an admin principal, "off"
	// returns 404. Those routes are always marked noindex. Env: DCMS_INTROSPECTION.
	Introspection string `yaml:"introspection"`
	// CORS configures cross-origin access. Empty allowed_origins ⇒ CORS off
	// (same-origin only), the safe default.
	CORS CORS `yaml:"cors"`
	// TLS optionally serves HTTPS directly. When both files are set the server
	// terminates TLS itself; otherwise it serves HTTP (terminate TLS at a proxy).
	TLS TLS `yaml:"tls"`
}

// CORS configures cross-origin resource sharing (see gateway.CORSOptions). It is
// off unless AllowedOrigins is non-empty.
type CORS struct {
	AllowedOrigins   []string `yaml:"allowed_origins"`
	AllowedMethods   []string `yaml:"allowed_methods"`
	AllowedHeaders   []string `yaml:"allowed_headers"`
	ExposedHeaders   []string `yaml:"exposed_headers"`
	AllowCredentials bool     `yaml:"allow_credentials"`
	MaxAgeSeconds    int      `yaml:"max_age_seconds"`
}

// TLS points at a certificate/key pair for native HTTPS. Both must be set to
// enable it; DCMS does not manage or renew certificates (front it with a proxy
// that does ACME, or supply certs here).
type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Idempotency configures idempotent-write handling (ADR-0018). It is inert until
// a client actually sends an Idempotency-Key header, so enabling it by default
// costs nothing for clients that don't use it.
type Idempotency struct {
	// Enabled is a pointer so "unset" differs from an explicit false: unset
	// defaults ON. When false, the Idempotency-Key header is ignored.
	Enabled *bool `yaml:"enabled"`
	// TTLHours is how long a recorded key is honored for replay; 0 uses 24h.
	TTLHours int `yaml:"ttl_hours"`
}

// RateLimit configures the two rate-limit tiers (see gateway.RateLimitOptions).
// Zero per-tier values take the engine defaults.
type RateLimit struct {
	// Enabled is a pointer so "unset" differs from an explicit false: unset
	// defaults ON (rate limiting is a production-hardening default). Set it false
	// to turn limiting off entirely.
	Enabled       *bool `yaml:"enabled"`
	APIPerMinute  int   `yaml:"api_per_minute"`
	APIBurst      int   `yaml:"api_burst"`
	AuthPerMinute int   `yaml:"auth_per_minute"`
	AuthBurst     int   `yaml:"auth_burst"`
	// AnonWrite* meter unauthenticated collection writes (a public signup form)
	// in their own tight per-IP tier, so tightening them doesn't throttle
	// authenticated callers (issue #34). Zero ⇒ engine defaults.
	AnonWritePerMinute int `yaml:"anon_write_per_minute"`
	AnonWriteBurst     int `yaml:"anon_write_burst"`
}

// splitList parses a comma-separated env value into trimmed, non-empty entries.
func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Default returns the built-in configuration used when nothing overrides it.
// These values match the historical flag defaults so behavior is unchanged for
// anyone who never writes a config file.
func Default() Config {
	return Config{
		Schema: "./dcms.schema.yaml",
		Database: Database{
			Driver: "sqlite",
			Path:   "./dcms.db",
		},
		Server: Server{
			Port: 3000,
		},
		Media: Media{
			Driver: "local",
			Dir:    "./dcms-media",
		},
	}
}

// Load reads a config file over the top of the defaults. Keys absent from the
// file keep their default value (yaml.Unmarshal only overwrites present keys),
// so a partial config file is valid and only specifies what it wants to change.
//
// The returned bool reports whether a file was actually found and read. A
// missing file is not an error — callers decide whether absence matters (it
// does when the user explicitly passed --config; it doesn't for the default
// path). Any other read/parse failure is returned as an error.
func Load(path string) (Config, bool, error) {
	cfg := Default()
	src, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, false, nil
		}
		return cfg, false, fmt.Errorf("read config %q: %w", path, err)
	}
	// Strict decode: an unknown key is a hard error, not a silent drop. A typo like
	// `server.rate_limit.requests_per_minute` (a key that doesn't exist) must not
	// leave the real setting on its default with nothing said — that is how a
	// public form ended up on the 6000/min default (issue #35). KnownFields walks
	// the whole nested struct, so a mistyped key at any depth is caught. Keys bound
	// to env-only secret fields (yaml:"-") are unknown here too, so a secret left
	// in the file is rejected rather than silently ignored (secrets rule, ADR-0009).
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return cfg, true, nil // empty file — keep the defaults
		}
		return cfg, true, fmt.Errorf("parse config %q: %w\nan unrecognized key is usually a typo or a renamed/removed setting; check it against the documented config keys (see examples/dcms.config.yaml)", path, err)
	}
	return cfg, true, nil
}

// ApplyEnv overlays environment variables onto the config. Only variables that
// are actually set take effect, so env acts as a targeted override of the file
// and defaults without wiping unset values.
//
//	DCMS_SCHEMA     → Schema
//	DCMS_DB_DRIVER  → Database.Driver
//	DCMS_DB         → Database.Path
//	DCMS_PORT       → Server.Port
func (c *Config) ApplyEnv() error {
	if v, ok := os.LookupEnv("DCMS_SCHEMA"); ok {
		c.Schema = v
	}
	if v, ok := os.LookupEnv("DCMS_DB_DRIVER"); ok {
		c.Database.Driver = v
	}
	if v, ok := os.LookupEnv("DCMS_DB"); ok {
		c.Database.Path = v
	}
	if v, ok := os.LookupEnv("DCMS_PORT"); ok {
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("DCMS_PORT %q: not a number", v)
		}
		c.Server.Port = port
	}
	if v, ok := os.LookupEnv("DCMS_MAX_BODY_BYTES"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("DCMS_MAX_BODY_BYTES %q: not a number", v)
		}
		c.Server.MaxBodyBytes = n
	}
	if v, ok := os.LookupEnv("DCMS_REQUEST_TIMEOUT_SECONDS"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("DCMS_REQUEST_TIMEOUT_SECONDS %q: not a number", v)
		}
		c.Server.RequestTimeoutSeconds = n
	}
	if v, ok := os.LookupEnv("DCMS_RATE_LIMIT_ENABLED"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("DCMS_RATE_LIMIT_ENABLED %q: not a bool", v)
		}
		c.Server.RateLimit.Enabled = &b
	}
	if v, ok := os.LookupEnv("DCMS_TRUST_PROXY"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("DCMS_TRUST_PROXY %q: not a bool", v)
		}
		c.Server.TrustProxy = b
	}
	if v, ok := os.LookupEnv("DCMS_CORS_ALLOWED_ORIGINS"); ok {
		c.Server.CORS.AllowedOrigins = splitList(v)
	}
	if v, ok := os.LookupEnv("DCMS_TLS_CERT_FILE"); ok {
		c.Server.TLS.CertFile = v
	}
	if v, ok := os.LookupEnv("DCMS_TLS_KEY_FILE"); ok {
		c.Server.TLS.KeyFile = v
	}
	if v, ok := os.LookupEnv("DCMS_IDEMPOTENCY_ENABLED"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("DCMS_IDEMPOTENCY_ENABLED %q: not a bool", v)
		}
		c.Server.Idempotency.Enabled = &b
	}
	if v, ok := os.LookupEnv("DCMS_MEDIA_DIR"); ok {
		c.Media.Dir = v
	}
	// S3 credentials are env-only; endpoint/bucket/region may also be supplied via
	// env for 12-factor deployments.
	if v, ok := os.LookupEnv("DCMS_S3_ACCESS_KEY"); ok {
		c.Media.AccessKey = v
	}
	if v, ok := os.LookupEnv("DCMS_S3_SECRET_KEY"); ok {
		c.Media.SecretKey = v
	}
	if v, ok := os.LookupEnv("DCMS_S3_ENDPOINT"); ok {
		c.Media.Endpoint = v
	}
	if v, ok := os.LookupEnv("DCMS_S3_BUCKET"); ok {
		c.Media.Bucket = v
	}
	if v, ok := os.LookupEnv("DCMS_S3_REGION"); ok {
		c.Media.Region = v
	}
	if v, ok := os.LookupEnv("DCMS_PREVIEW_TOKEN"); ok {
		c.Content.PreviewToken = v
	}
	if v, ok := os.LookupEnv("DCMS_INTROSPECTION"); ok {
		c.Server.Introspection = v
	}
	switch c.Server.Introspection {
	case "", "public", "admin", "off":
	default:
		return fmt.Errorf("server.introspection %q is not one of public, admin, off", c.Server.Introspection)
	}
	// Bootstrap admin credentials are env-only (secrets rule); they seed the first
	// admin when the user table is empty (ADR-0016).
	if v, ok := os.LookupEnv("DCMS_ADMIN_EMAIL"); ok {
		c.Auth.AdminEmail = v
	}
	if v, ok := os.LookupEnv("DCMS_ADMIN_PASSWORD"); ok {
		c.Auth.AdminPassword = v
	}
	if v, ok := os.LookupEnv("DCMS_ADMIN_ROLES"); ok {
		c.Auth.AdminRoles = splitList(v)
	}
	// Authenticator selection (issue #9) — non-secret, but env-overridable for
	// 12-factor deployments behind a proxy.
	if v, ok := os.LookupEnv("DCMS_AUTH_PROVIDER"); ok {
		c.Auth.Provider = v
	}
	if v, ok := os.LookupEnv("DCMS_AUTH_PROXY_USER_HEADER"); ok {
		c.Auth.ProxyHeader.UserHeader = v
	}
	if v, ok := os.LookupEnv("DCMS_AUTH_PROXY_ROLES_HEADER"); ok {
		c.Auth.ProxyHeader.RolesHeader = v
	}
	if v, ok := os.LookupEnv("DCMS_AUTH_PROXY_ROLES_SEPARATOR"); ok {
		c.Auth.ProxyHeader.RolesSeparator = v
	}
	if v, ok := os.LookupEnv("DCMS_REGISTRATION_ENABLED"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("DCMS_REGISTRATION_ENABLED %q: not a bool", v)
		}
		c.Auth.Registration.Enabled = b
	}
	// SMTP credentials are env-only (secrets rule); host/from may also come from
	// env for 12-factor deployments.
	if v, ok := os.LookupEnv("DCMS_SMTP_USERNAME"); ok {
		c.Auth.SMTP.Username = v
	}
	if v, ok := os.LookupEnv("DCMS_SMTP_PASSWORD"); ok {
		c.Auth.SMTP.Password = v
	}
	if v, ok := os.LookupEnv("DCMS_SMTP_HOST"); ok {
		c.Auth.SMTP.Host = v
	}
	if v, ok := os.LookupEnv("DCMS_SMTP_FROM"); ok {
		c.Auth.SMTP.From = v
	}
	// Webhook HMAC secrets: each endpoint names the env var holding its secret
	// (secrets are never read from the config file).
	for i := range c.Events.Webhooks {
		if name := c.Events.Webhooks[i].SecretEnv; name != "" {
			c.Events.Webhooks[i].Secret = os.Getenv(name)
		}
	}
	return nil
}
