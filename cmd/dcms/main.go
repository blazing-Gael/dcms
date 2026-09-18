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
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/blazing-Gael/dcms"
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
	opts := dcms.Options{AutoMigrate: mode.migrate, Dev: mode.validateDefault, Label: mode.name}
	if f := cmd.Flags().Lookup("config"); f != nil {
		opts.ConfigPath = f.Value.String()
		opts.ConfigRequired = f.Changed
	}
	if flagChanged(cmd, "schema") {
		opts.SchemaPath, _ = cmd.Flags().GetString("schema")
	}
	if flagChanged(cmd, "db") {
		opts.DBPath, _ = cmd.Flags().GetString("db")
	}
	if flagChanged(cmd, "port") {
		opts.Port, _ = cmd.Flags().GetInt("port")
	}

	app, err := dcms.New(opts)
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Serve(ctx)
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
