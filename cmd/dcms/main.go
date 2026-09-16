// Command dcms is the DCMS command-line entrypoint.
//
// Phase 1 commands: dev, validate, codegen, migrate, version.
// See docs/DEV_ROADMAP.md section 1.5 for the full command spec.
//
// Settings resolve with this precedence (highest wins):
//
//	flags  >  env vars (DCMS_*)  >  --config file  >  built-in defaults
//
// so a deployment can ship one dcms.config.yaml and still override any single
// value at launch. See internal/config.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/codegen"
	"github.com/blazing-Gael/dcms/internal/config"
	"github.com/blazing-Gael/dcms/internal/engine"
	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/store"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "dcms",
		Short:         "DCMS — schema-first, AI-native, sovereign content engine",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// --config is a persistent flag: every subcommand inherits it.
	root.PersistentFlags().String("config", config.DefaultConfigPath, "path to the config file")
	root.AddCommand(
		newInitCmd(),
		newDevCmd(),
		newServeCmd(),
		newValidateCmd(),
		newCodegenCmd(),
		newMigrateCmd(),
		newAdminCmd(),
		newTokenCmd(),
		newVersionCmd(),
	)
	return root
}

// resolveConfig builds the effective configuration for a command by layering
// (in increasing precedence) defaults, the config file, environment variables,
// and finally any flags the user explicitly set on this invocation.
func resolveConfig(cmd *cobra.Command) (config.Config, error) {
	path := config.DefaultConfigPath
	explicit := false
	if f := cmd.Flags().Lookup("config"); f != nil {
		path = f.Value.String()
		explicit = f.Changed
	}

	cfg, found, err := config.Load(path)
	if err != nil {
		return cfg, err
	}
	// A missing file is fine for the default path, but if the user pointed us
	// at a specific --config that doesn't exist, that's almost certainly a typo.
	if explicit && !found {
		return cfg, fmt.Errorf("config file %q not found", path)
	}

	if err := cfg.ApplyEnv(); err != nil {
		return cfg, err
	}

	// Flags win over everything, but only when the user actually set them —
	// otherwise a flag's own default would silently clobber the config file.
	if flagChanged(cmd, "schema") {
		cfg.Schema, _ = cmd.Flags().GetString("schema")
	}
	if flagChanged(cmd, "db") {
		cfg.Database.Path, _ = cmd.Flags().GetString("db")
	}
	if flagChanged(cmd, "port") {
		cfg.Server.Port, _ = cmd.Flags().GetInt("port")
	}
	return cfg, nil
}

func flagChanged(cmd *cobra.Command, name string) bool {
	f := cmd.Flags().Lookup(name)
	return f != nil && f.Changed
}

// requireSQLite guards against a config naming a driver we don't ship yet, so a
// stale setting fails loudly instead of being silently ignored.
func requireSQLite(cfg config.Config) error {
	if cfg.Database.Driver != "sqlite" {
		return fmt.Errorf("database driver %q not supported yet (only %q)", cfg.Database.Driver, "sqlite")
	}
	return nil
}

// serverMode captures the two ways the HTTP server is started. `dcms dev` and
// `dcms serve` share one code path (runServer) and differ only in these defaults:
// dev migrates on start and turns the response-validation guardrail on; serve
// assumes an already-migrated database (migrate is a separate deploy step) and
// leaves the guardrail off, so a deployment does not silently pay dev defaults.
type serverMode struct {
	name            string // banner label ("dev" / "serve")
	migrate         bool   // run pending migrations on start
	validateDefault bool   // ValidateResponses when the config leaves it unset
}

func newDevCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "Migrate, then start the HTTP server with dev guardrails on",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServer(cmd, serverMode{name: "dev", migrate: true, validateDefault: true})
		},
	}
	cmd.Flags().String("schema", "./dcms.schema.yaml", "path to the schema file")
	cmd.Flags().Int("port", 3000, "HTTP port to listen on")
	cmd.Flags().String("db", "./dcms.db", "path to the SQLite database file")
	return cmd
}

func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP server for production (no auto-migrate; guardrails off)",
		Long: "Run the HTTP server for a deployment. Unlike `dcms dev`, it does not run\n" +
			"migrations (run `dcms migrate` as a separate deploy step) and leaves strict\n" +
			"response validation off unless the config enables it. Refuses to start if the\n" +
			"database has pending migrations.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServer(cmd, serverMode{name: "serve", migrate: false, validateDefault: false})
		},
	}
	cmd.Flags().String("schema", "./dcms.schema.yaml", "path to the schema file")
	// Same default as dev: the effective port comes from config (server.port,
	// which `dcms init` scaffolds to 8080) unless --port is passed explicitly, so
	// advertising a different flag default here would be misleading.
	cmd.Flags().Int("port", 3000, "HTTP port to listen on (config server.port takes precedence)")
	cmd.Flags().String("db", "./dcms.db", "path to the SQLite database file")
	return cmd
}

// resolveValidateResponses decides whether strict response validation is on: the
// config's explicit setting wins, else the mode default (on for dev, off for serve).
func resolveValidateResponses(mode serverMode, cfg config.Config) bool {
	if cfg.Server.ValidateResponses != nil {
		return *cfg.Server.ValidateResponses
	}
	return mode.validateDefault
}

// runServer is the shared body of `dev` and `serve`; serverMode supplies the two
// differing defaults.
func runServer(cmd *cobra.Command, mode serverMode) error {
	cfg, err := resolveConfig(cmd)
	if err != nil {
		return err
	}
	if err := requireSQLite(cfg); err != nil {
		return err
	}
	if _, statErr := os.Stat(cfg.Schema); os.IsNotExist(statErr) {
		return fmt.Errorf("no schema at %s — run `dcms init` here to scaffold a project first", cfg.Schema)
	}
	def, err := engine.LoadSchema(cfg.Schema)
	if err != nil {
		return err
	}
	db, err := engine.OpenStore(cfg.Database.Path)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if mode.migrate {
		if err := engine.Apply(ctx, db, def); err != nil {
			return err
		}
	} else {
		// serve treats migration as a separate deploy step and refuses to start
		// with additive migrations pending (a new table/column), so a forgotten
		// `dcms migrate` fails loudly at boot instead of at query time. Note the
		// SQLite adapter's Diff is additive-only: an in-place change to an existing
		// column's type/nullability/default is not reported here, so serve cannot
		// catch that class of drift (neither can `dcms migrate` apply it today).
		pending, err := engine.Plan(ctx, db, def)
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			return fmt.Errorf("database has %d pending migration(s); run `dcms migrate` before `dcms serve`", len(pending))
		}
	}

	// Seed the first admin from env when the user table is empty (ADR-0016),
	// so a fresh backend is reachable without a chicken-and-egg lockout.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	// Recognized-but-unimplemented schema directives (issue #35) load fine but do
	// nothing yet — log them so an operator isn't misled into thinking they're active.
	for _, warn := range def.Warnings {
		logger.Warn("schema", "note", warn)
	}
	if err := gateway.EnsureSeedAdmin(ctx, db, cfg.Auth.AdminEmail, cfg.Auth.AdminPassword, logger); err != nil {
		return err
	}

	// Strict response validation follows the mode default (on for dev, off for
	// serve) unless the config sets it explicitly.
	validateResponses := resolveValidateResponses(mode, cfg)

	bs, err := blob.New(blob.Config{
		Driver:         cfg.Media.Driver,
		Dir:            cfg.Media.Dir,
		Endpoint:       cfg.Media.Endpoint,
		Region:         cfg.Media.Region,
		Bucket:         cfg.Media.Bucket,
		AccessKey:      cfg.Media.AccessKey,
		SecretKey:      cfg.Media.SecretKey,
		UseSSL:         cfg.Media.UseSSL,
		ForcePathStyle: cfg.Media.ForcePathStyle,
		PublicBaseURL:  cfg.Media.PublicBaseURL,
	})
	if err != nil {
		return err
	}

	// Rate limiting defaults ON (a production-hardening default); an
	// explicit `enabled: false` (or DCMS_RATE_LIMIT_ENABLED=false) turns it
	// off by leaving the option nil. Zero per-tier values take engine
	// defaults inside the gateway.
	var rateLimit *gateway.RateLimitOptions
	rlEnabled := cfg.Server.RateLimit.Enabled == nil || *cfg.Server.RateLimit.Enabled
	if rlEnabled {
		rateLimit = &gateway.RateLimitOptions{
			APIPerMinute:  cfg.Server.RateLimit.APIPerMinute,
			APIBurst:      cfg.Server.RateLimit.APIBurst,
			AuthPerMinute: cfg.Server.RateLimit.AuthPerMinute,
			AuthBurst:     cfg.Server.RateLimit.AuthBurst,
		}
	}

	// CORS is off unless origins are configured (same-origin only).
	var cors *gateway.CORSOptions
	if len(cfg.Server.CORS.AllowedOrigins) > 0 {
		cors = &gateway.CORSOptions{
			AllowedOrigins:   cfg.Server.CORS.AllowedOrigins,
			AllowedMethods:   cfg.Server.CORS.AllowedMethods,
			AllowedHeaders:   cfg.Server.CORS.AllowedHeaders,
			ExposedHeaders:   cfg.Server.CORS.ExposedHeaders,
			AllowCredentials: cfg.Server.CORS.AllowCredentials,
			MaxAgeSeconds:    cfg.Server.CORS.MaxAgeSeconds,
		}
	}

	// Idempotency defaults ON (inert until a client sends the header); an
	// explicit `enabled: false` leaves the option nil to disable it.
	var idempotency *gateway.IdempotencyOptions
	if cfg.Server.Idempotency.Enabled == nil || *cfg.Server.Idempotency.Enabled {
		idempotency = &gateway.IdempotencyOptions{
			TTL: time.Duration(cfg.Server.Idempotency.TTLHours) * time.Hour,
		}
	}

	// Self-registration (ADR-0019): off unless enabled. Default roles must
	// be declared and must not be admin roles — a self-registrant can never
	// self-grant administration.
	adminRoles := cfg.Auth.AdminRoles
	if len(adminRoles) == 0 {
		adminRoles = []string{"admin"}
	}
	var registration *gateway.RegistrationOptions
	if cfg.Auth.Registration.Enabled {
		for _, role := range cfg.Auth.Registration.DefaultRoles {
			if !def.HasRole(role) {
				return fmt.Errorf("registration default_role %q is not a declared role", role)
			}
			for _, ar := range adminRoles {
				if role == ar {
					return fmt.Errorf("registration default_role %q may not be an admin role", role)
				}
			}
		}
		registration = &gateway.RegistrationOptions{DefaultRoles: cfg.Auth.Registration.DefaultRoles}
	}

	// Mailer for account emails (ADR-0019). SMTP host set ⇒ send via SMTP;
	// otherwise the gateway falls back to a dev-log notifier.
	var notifier gateway.Notifier
	if cfg.Auth.SMTP.Host != "" {
		notifier = gateway.NewSMTPNotifier(cfg.Auth.SMTP.Host, cfg.Auth.SMTP.Port,
			cfg.Auth.SMTP.From, cfg.Auth.SMTP.Username, cfg.Auth.SMTP.Password)
		// Pre-flight the mail path so a broken SMTP config (unreachable host,
		// wrong port, bad credentials, TLS) surfaces at startup rather than only
		// when a user's reset email silently fails to arrive. Non-fatal: a mail
		// server briefly unreachable at boot must not stop the whole backend.
		if v, ok := notifier.(gateway.ConnectionVerifier); ok {
			vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if err := v.VerifyConnection(vctx); err != nil {
				logger.Warn("SMTP pre-flight failed — password-reset emails will not send until this is resolved",
					"host", cfg.Auth.SMTP.Host, "port", cfg.Auth.SMTP.Port, "err", err)
			} else {
				logger.Info("SMTP connection verified", "host", cfg.Auth.SMTP.Host)
			}
			cancel()
		}
	}

	// Webhook delivery (ADR-0021 phase 2). Each endpoint needs a resolved
	// HMAC secret; a configured endpoint without one is a startup error so a
	// misconfigured secret_env fails loudly rather than shipping unsigned.
	var webhooks *gateway.WebhookOptions
	if len(cfg.Events.Webhooks) > 0 {
		eps := make([]gateway.WebhookEndpoint, 0, len(cfg.Events.Webhooks))
		for _, wh := range cfg.Events.Webhooks {
			if wh.Name == "" || wh.URL == "" {
				return fmt.Errorf("webhook: name and url are required")
			}
			if wh.Secret == "" {
				return fmt.Errorf("webhook %q: secret is empty (set secret_env to an environment variable holding the HMAC secret)", wh.Name)
			}
			eps = append(eps, gateway.WebhookEndpoint{
				Name: wh.Name, URL: wh.URL, Secret: wh.Secret,
				Events: wh.Events, Collections: wh.Collections, MaxAttempts: wh.MaxAttempts,
			})
		}
		webhooks = &gateway.WebhookOptions{
			Endpoints:    eps,
			PollInterval: time.Duration(cfg.Events.WebhookPollSeconds) * time.Second,
		}
	}

	// Authenticator selection (ADR-0020, issue #9). Default is the built-in
	// opaque-session source; proxy_header trusts a verified-identity header set by
	// a front proxy — a config choice, not a code change.
	var authenticator gateway.Authenticator
	switch cfg.Auth.Provider {
	case "", "session":
		authenticator = gateway.NewSessionAuthenticator(db)
	case "proxy_header":
		if cfg.Auth.ProxyHeader.UserHeader == "" {
			return fmt.Errorf("auth.provider proxy_header requires auth.proxy_header.user_header")
		}
		authenticator = gateway.NewProxyHeaderAuthenticator(
			cfg.Auth.ProxyHeader.UserHeader, cfg.Auth.ProxyHeader.RolesHeader, cfg.Auth.ProxyHeader.RolesSeparator)
		logger.Warn("auth provider is proxy_header — DCMS trusts the identity header verbatim; your proxy MUST strip it from inbound client requests",
			"user_header", cfg.Auth.ProxyHeader.UserHeader)
	default:
		return fmt.Errorf("auth.provider %q is not supported (want session or proxy_header)", cfg.Auth.Provider)
	}
	// Long-lived machine tokens (issue #8) work under ANY provider: layer their
	// resolution over the selected interactive authenticator.
	authenticator = gateway.WithAPITokens(db, authenticator)

	tlsCfg := engine.TLSConfig{CertFile: cfg.Server.TLS.CertFile, KeyFile: cfg.Server.TLS.KeyFile}
	scheme := "http"
	if tlsCfg.CertFile != "" && tlsCfg.KeyFile != "" {
		scheme = "https"
	}

	fmt.Printf("dcms %s — %d collection(s) from %s\n", mode.name, len(def.Collections), cfg.Schema)
	fmt.Printf("listening on %s://localhost:%d  (Ctrl+C to stop)\n", scheme, cfg.Server.Port)
	return engine.Serve(ctx, def, db, fmt.Sprintf(":%d", cfg.Server.Port), logger, gateway.Options{
		ValidateResponses:   validateResponses,
		Blob:                bs,
		MaxUploadBytes:      cfg.Media.MaxUploadBytes,
		AllowedContentTypes: cfg.Media.AllowedContentTypes,
		PreviewToken:        cfg.Content.PreviewToken,
		Introspection:       cfg.Server.Introspection,
		Authenticator:       authenticator,
		MaxBodyBytes:        cfg.Server.MaxBodyBytes,
		RequestTimeout:      time.Duration(cfg.Server.RequestTimeoutSeconds) * time.Second,
		RateLimit:           rateLimit,
		Idempotency:         idempotency,
		TrustProxy:          cfg.Server.TrustProxy,
		CORS:                cors,
		AdminRoles:          adminRoles,
		Registration:        registration,
		PasswordMinLength:   cfg.Auth.Password.MinLength,
		Notifier:            notifier,
		ResetLinkBase:       cfg.Auth.Reset.LinkBase,
		ResetTokenTTL:       time.Duration(cfg.Auth.Reset.TTLMinutes) * time.Minute,
		Webhooks:            webhooks,
	}, tlsCfg)
}

func newValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Parse and validate the schema and config, exit non-zero on failure",
		RunE: func(cmd *cobra.Command, args []string) error {
			// resolveConfig loads the config with strict key checking, so an unknown
			// config key fails here too (issue #35), not just at server start.
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			def, err := engine.LoadSchema(cfg.Schema)
			if err != nil {
				return err
			}
			for _, warn := range def.Warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warn)
			}
			fmt.Printf("schema OK — %d collection(s)\n", len(def.Collections))
			return nil
		},
	}
	cmd.Flags().String("schema", "./dcms.schema.yaml", "path to the schema file")
	return cmd
}

func newCodegenCmd() *cobra.Command {
	var (
		lang string
		out  string
	)
	cmd := &cobra.Command{
		Use:   "codegen",
		Short: "Generate typed client code from the schema",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			def, err := engine.LoadSchema(cfg.Schema)
			if err != nil {
				return err
			}
			filename, code, err := codegen.Generate(def, lang)
			if err != nil {
				return fmt.Errorf("codegen: %w", err)
			}
			if err := os.MkdirAll(out, 0o755); err != nil {
				return fmt.Errorf("create output dir %q: %w", out, err)
			}
			dest := filepath.Join(out, filename)
			if err := os.WriteFile(dest, []byte(code), 0o644); err != nil {
				return fmt.Errorf("write %q: %w", dest, err)
			}
			fmt.Printf("generated %s (%d collection(s), contract %s)\n", dest, len(def.Collections), def.ContractVersion())
			return nil
		},
	}
	cmd.Flags().StringVar(&lang, "lang", "ts", "target language")
	cmd.Flags().StringVar(&out, "out", "./types", "output directory")
	cmd.Flags().String("schema", "./dcms.schema.yaml", "path to the schema file")
	return cmd
}

func newMigrateCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Run pending migrations (or print the SQL with --dry-run)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			if err := requireSQLite(cfg); err != nil {
				return err
			}
			def, err := engine.LoadSchema(cfg.Schema)
			if err != nil {
				return err
			}
			db, err := engine.OpenStore(cfg.Database.Path)
			if err != nil {
				return err
			}
			defer db.Close()

			ctx := context.Background()
			if dryRun {
				up, err := engine.Plan(ctx, db, def)
				if err != nil {
					return err
				}
				if len(up) == 0 {
					fmt.Println("-- no migrations needed; schema and database are in sync")
					return nil
				}
				for _, stmt := range up {
					fmt.Println(stmt)
				}
				return nil
			}
			if err := engine.Apply(ctx, db, def); err != nil {
				return err
			}
			fmt.Println("migrations applied")
			return nil
		},
	}
	cmd.Flags().String("schema", "./dcms.schema.yaml", "path to the schema file")
	cmd.Flags().String("db", "./dcms.db", "path to the SQLite database file")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the migration SQL without applying it")
	return cmd
}

// newAdminCmd groups administrative commands. Today it holds `admin create`, the
// bootstrap path for the first (or any) user account (ADR-0016).
func newAdminCmd() *cobra.Command {
	admin := &cobra.Command{
		Use:   "admin",
		Short: "Administrative commands (user bootstrap, etc.)",
	}
	admin.AddCommand(newAdminCreateCmd())
	return admin
}

// newAdminCreateCmd creates a user directly in the store, bypassing the HTTP
// authz layer — the only way to make the first admin before anyone can log in.
// The password is a secret; prefer supplying it via DCMS_ADMIN_PASSWORD over the
// --password flag (which can leak into shell history).
func newAdminCreateCmd() *cobra.Command {
	var email, password string
	var roles []string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a user (bootstrap the first admin)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			if err := requireSQLite(cfg); err != nil {
				return err
			}
			// Credentials fall back to the same env the server seeds from.
			if email == "" {
				email = cfg.Auth.AdminEmail
			}
			if password == "" {
				password = cfg.Auth.AdminPassword
			}
			if email == "" || password == "" {
				return fmt.Errorf("both --email and --password (or DCMS_ADMIN_EMAIL/DCMS_ADMIN_PASSWORD) are required")
			}

			def, err := engine.LoadSchema(cfg.Schema)
			if err != nil {
				return err
			}
			db, err := engine.OpenStore(cfg.Database.Path)
			if err != nil {
				return err
			}
			defer db.Close()

			ctx := context.Background()
			// Ensure the identity tables exist before writing to them.
			if err := engine.Apply(ctx, db, def); err != nil {
				return err
			}
			rec, err := gateway.CreateUser(ctx, db, email, password, roles)
			if err != nil {
				return err
			}
			fmt.Printf("created user %s (id %v, roles %v)\n", email, rec["id"], roles)
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "user email (login identifier)")
	cmd.Flags().StringVar(&password, "password", "", "user password (prefer DCMS_ADMIN_PASSWORD)")
	cmd.Flags().StringSliceVar(&roles, "role", []string{"admin"}, "roles to assign (repeatable)")
	cmd.Flags().String("schema", "./dcms.schema.yaml", "path to the schema file")
	cmd.Flags().String("db", "./dcms.db", "path to the SQLite database file")
	return cmd
}

// newTokenCmd groups the long-lived machine-token commands (issue #8): mint,
// list, and revoke API tokens for non-human callers (SSG builds, CI, webhook
// receivers). Tokens authenticate as first-class principals with their own roles,
// so every access: rule applies unchanged.
func newTokenCmd() *cobra.Command {
	tok := &cobra.Command{
		Use:   "token",
		Short: "Manage long-lived API tokens for machine callers",
	}
	tok.AddCommand(newTokenCreateCmd(), newTokenListCmd(), newTokenRevokeCmd())
	return tok
}

// withStore opens the store for a token subcommand, ensuring the engine tables
// exist (Apply) before use — the token collection is engine-managed.
func withStore(cmd *cobra.Command, fn func(ctx context.Context, db store.Adapter) error) error {
	cfg, err := resolveConfig(cmd)
	if err != nil {
		return err
	}
	if err := requireSQLite(cfg); err != nil {
		return err
	}
	def, err := engine.LoadSchema(cfg.Schema)
	if err != nil {
		return err
	}
	db, err := engine.OpenStore(cfg.Database.Path)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	if err := engine.Apply(ctx, db, def); err != nil {
		return err
	}
	return fn(ctx, db)
}

func newTokenCreateCmd() *cobra.Command {
	var name, expires string
	var roles []string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Mint a token (the raw value is shown once); e.g. --name ci --role reader --expires 90d",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			d, err := parseExpiry(expires)
			if err != nil {
				return err
			}
			return withStore(cmd, func(ctx context.Context, db store.Adapter) error {
				var expiresAt time.Time
				if d > 0 {
					expiresAt = time.Now().Add(d)
				}
				raw, rec, err := gateway.CreateAPIToken(ctx, db, name, roles, expiresAt)
				if err != nil {
					return err
				}
				fmt.Printf("created token %q (id %v)\n", name, rec["id"])
				if d > 0 {
					fmt.Printf("expires in %s\n", d)
				}
				fmt.Println("\nsave this now — it is not shown again:")
				fmt.Println(raw)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "human label for the token (e.g. ci, ssg-build)")
	cmd.Flags().StringSliceVar(&roles, "role", nil, "role to grant the token (repeatable)")
	cmd.Flags().StringVar(&expires, "expires", "", "lifetime, e.g. 90d or 720h; empty = never")
	cmd.Flags().String("schema", "./dcms.schema.yaml", "path to the schema file")
	cmd.Flags().String("db", "./dcms.db", "path to the SQLite database file")
	return cmd
}

func newTokenListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List API tokens (metadata only; raw values are unrecoverable)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(cmd, func(ctx context.Context, db store.Adapter) error {
				toks, err := gateway.ListAPITokens(ctx, db)
				if err != nil {
					return err
				}
				if len(toks) == 0 {
					fmt.Println("no API tokens")
					return nil
				}
				for _, tk := range toks {
					fmt.Printf("%v\t%v\troles=%v\texpires=%v\tlast_used=%v\n",
						tk["id"], tk["name"], tk["roles"], orDash(tk["expires_at"]), orDash(tk["last_used_at"]))
				}
				return nil
			})
		},
	}
	cmd.Flags().String("schema", "./dcms.schema.yaml", "path to the schema file")
	cmd.Flags().String("db", "./dcms.db", "path to the SQLite database file")
	return cmd
}

func newTokenRevokeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a token by id (from `token list`); it stops working immediately",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(cmd, func(ctx context.Context, db store.Adapter) error {
				if err := gateway.RevokeAPIToken(ctx, db, args[0]); err != nil {
					return err
				}
				fmt.Printf("revoked token %s\n", args[0])
				return nil
			})
		},
	}
	cmd.Flags().String("schema", "./dcms.schema.yaml", "path to the schema file")
	cmd.Flags().String("db", "./dcms.db", "path to the SQLite database file")
	return cmd
}

// parseExpiry accepts an empty string (never expires), a "<n>d" day count, or any
// Go duration (e.g. 720h, 30m). A non-empty zero (`0d`, `0s`) is rejected — only
// the empty string means "never", so a zero lifetime is a mistake, not an
// accidental non-expiring token.
func parseExpiry(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid --expires %q (want e.g. 90d or 720h)", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		parsed, err := time.ParseDuration(s)
		if err != nil || parsed < 0 {
			return 0, fmt.Errorf("invalid --expires %q (want e.g. 90d or 720h)", s)
		}
		d = parsed
	}
	if d == 0 {
		return 0, fmt.Errorf("invalid --expires %q: a zero lifetime is not allowed (omit --expires for a token that never expires)", s)
	}
	return d, nil
}

func orDash(v any) any {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	if v == nil {
		return "-"
	}
	return v
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version string",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("dcms", version)
		},
	}
}
