// Command dcms is the DCMS command-line entrypoint.
//
// Phase 1 commands: dev, validate, codegen, migrate, version.
// See DEV_ROADMAP.md section 1.5 for the full command spec.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/blazing-Gael/dcms/core/engine"
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
	root.AddCommand(
		newDevCmd(),
		newValidateCmd(),
		newCodegenCmd(),
		newMigrateCmd(),
		newVersionCmd(),
	)
	return root
}

func newDevCmd() *cobra.Command {
	var (
		schemaPath string
		port       int
		dbPath     string
	)
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "Parse the schema, run migrations, and start the dev HTTP server",
		RunE: func(cmd *cobra.Command, args []string) error {
			def, err := engine.LoadSchema(schemaPath)
			if err != nil {
				return err
			}
			db, err := engine.OpenStore(dbPath)
			if err != nil {
				return err
			}
			defer db.Close()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			if err := engine.Apply(ctx, db, def); err != nil {
				return err
			}

			logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
			fmt.Printf("dcms dev — %d collection(s) from %s\n", len(def.Collections), schemaPath)
			fmt.Printf("listening on http://localhost:%d  (Ctrl+C to stop)\n", port)
			return engine.Serve(ctx, def, db, fmt.Sprintf(":%d", port), logger)
		},
	}
	cmd.Flags().StringVar(&schemaPath, "schema", "./dcms.schema.yaml", "path to the schema file")
	cmd.Flags().IntVar(&port, "port", 3000, "HTTP port to listen on")
	cmd.Flags().StringVar(&dbPath, "db", "./dcms.db", "path to the SQLite database file")
	return cmd
}

func newValidateCmd() *cobra.Command {
	var schemaPath string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Parse and validate the schema, exit non-zero on failure",
		RunE: func(cmd *cobra.Command, args []string) error {
			def, err := engine.LoadSchema(schemaPath)
			if err != nil {
				return err
			}
			fmt.Printf("schema OK — %d collection(s)\n", len(def.Collections))
			return nil
		},
	}
	cmd.Flags().StringVar(&schemaPath, "schema", "./dcms.schema.yaml", "path to the schema file")
	return cmd
}

func newCodegenCmd() *cobra.Command {
	var (
		lang       string
		out        string
		schemaPath string
	)
	cmd := &cobra.Command{
		Use:   "codegen",
		Short: "Generate typed client code from the schema",
		RunE: func(cmd *cobra.Command, args []string) error {
			// TODO(phase-1): generate TypeScript types per collection.
			return fmt.Errorf("codegen: not implemented yet (lang=%s out=%s schema=%s)", lang, out, schemaPath)
		},
	}
	cmd.Flags().StringVar(&lang, "lang", "ts", "target language")
	cmd.Flags().StringVar(&out, "out", "./types", "output directory")
	cmd.Flags().StringVar(&schemaPath, "schema", "./dcms.schema.yaml", "path to the schema file")
	return cmd
}

func newMigrateCmd() *cobra.Command {
	var (
		schemaPath string
		dbPath     string
		dryRun     bool
	)
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Run pending migrations (or print the SQL with --dry-run)",
		RunE: func(cmd *cobra.Command, args []string) error {
			def, err := engine.LoadSchema(schemaPath)
			if err != nil {
				return err
			}
			db, err := engine.OpenStore(dbPath)
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
	cmd.Flags().StringVar(&schemaPath, "schema", "./dcms.schema.yaml", "path to the schema file")
	cmd.Flags().StringVar(&dbPath, "db", "./dcms.db", "path to the SQLite database file")
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
