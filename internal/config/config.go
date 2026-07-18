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
