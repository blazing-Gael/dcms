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
	"syscall"

	"github.com/spf13/cobra"

	"github.com/blazing-Gael/dcms/internal/blob"
	"github.com/blazing-Gael/dcms/internal/codegen"
	"github.com/blazing-Gael/dcms/internal/config"
	"github.com/blazing-Gael/dcms/internal/engine"
	"github.com/blazing-Gael/dcms/internal/gateway"
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
		newDevCmd(),
		newValidateCmd(),
		newCodegenCmd(),
		newMigrateCmd(),
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

func newDevCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "Parse the schema, run migrations, and start the dev HTTP server",
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

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			if err := engine.Apply(ctx, db, def); err != nil {
				return err
			}

			// Strict response validation defaults ON for `dev` (the guardrail is
			// most useful while iterating) unless the config sets it explicitly.
			validateResponses := true
			if cfg.Server.ValidateResponses != nil {
				validateResponses = *cfg.Server.ValidateResponses
			}

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

			logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
			fmt.Printf("dcms dev — %d collection(s) from %s\n", len(def.Collections), cfg.Schema)
			fmt.Printf("listening on http://localhost:%d  (Ctrl+C to stop)\n", cfg.Server.Port)
			return engine.Serve(ctx, def, db, fmt.Sprintf(":%d", cfg.Server.Port), logger, gateway.Options{
				ValidateResponses:   validateResponses,
				Blob:                bs,
				MaxUploadBytes:      cfg.Media.MaxUploadBytes,
				AllowedContentTypes: cfg.Media.AllowedContentTypes,
			})
		},
	}
	cmd.Flags().String("schema", "./dcms.schema.yaml", "path to the schema file")
	cmd.Flags().Int("port", 3000, "HTTP port to listen on")
	cmd.Flags().String("db", "./dcms.db", "path to the SQLite database file")
	return cmd
}

func newValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Parse and validate the schema, exit non-zero on failure",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig(cmd)
			if err != nil {
				return err
			}
			def, err := engine.LoadSchema(cfg.Schema)
			if err != nil {
				return err
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

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version string",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("dcms", version)
		},
	}
}
