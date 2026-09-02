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
	"errors"
	"fmt"
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
}

// Auth carries runtime auth secrets (ADR-0016). Both fields are env-only
// (DCMS_ADMIN_EMAIL / DCMS_ADMIN_PASSWORD) — the yaml:"-" keeps the bootstrap
// credential out of any committed config file, per the secrets rule (ADR-0009).
// They seed the first admin on startup when no users exist yet.
type Auth struct {
	AdminEmail    string `yaml:"-"`
	AdminPassword string `yaml:"-"`
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
	if err := yaml.Unmarshal(src, &cfg); err != nil {
		return cfg, true, fmt.Errorf("parse config %q: %w", path, err)
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
	// Bootstrap admin credentials are env-only (secrets rule); they seed the first
	// admin when the user table is empty (ADR-0016).
	if v, ok := os.LookupEnv("DCMS_ADMIN_EMAIL"); ok {
		c.Auth.AdminEmail = v
	}
	if v, ok := os.LookupEnv("DCMS_ADMIN_PASSWORD"); ok {
		c.Auth.AdminPassword = v
	}
	return nil
}
