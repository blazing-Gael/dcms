// Package engine composes the pieces — schema, store, gateway — into the
// runnable behaviors the CLI exposes: load a schema, apply migrations, serve.
// Keeping this orchestration here keeps cmd/dcms thin and lets every command
// (dev, validate, migrate) share one implementation.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/blazing-Gael/dcms/core/gateway"
	"github.com/blazing-Gael/dcms/core/schema"
	"github.com/blazing-Gael/dcms/core/store"
	"github.com/blazing-Gael/dcms/core/store/sqlite"
)

// LoadSchema reads and parses (and thereby validates) a schema file.
func LoadSchema(path string) (*schema.SchemaDefinition, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read schema %q: %w", path, err)
	}
	return schema.Parse(src)
}

// OpenStore opens the SQLite-backed store at dbPath.
// TODO(phase-2): select the adapter (sqlite/postgres) from config.
func OpenStore(dbPath string) (store.Adapter, error) {
	return sqlite.New(sqlite.Config{Path: dbPath})
}

// Plan returns the migration SQL needed to bring the database in line with the
// schema, without applying it (used by `migrate --dry-run`).
func Plan(ctx context.Context, db store.Adapter, def *schema.SchemaDefinition) ([]string, error) {
	var up []string
	for _, meta := range def.CollectionMetas() {
		plan, err := db.Diff(ctx, meta)
		if err != nil {
			return nil, fmt.Errorf("diff %s: %w", meta.Name, err)
		}
		up = append(up, plan.Up...)
	}
	return up, nil
}

// Apply migrates every collection to match the schema. It is idempotent: a
// collection already in sync produces an empty plan and is skipped.
func Apply(ctx context.Context, db store.Adapter, def *schema.SchemaDefinition) error {
	for _, meta := range def.CollectionMetas() {
		plan, err := db.Diff(ctx, meta)
		if err != nil {
			return fmt.Errorf("diff %s: %w", meta.Name, err)
		}
		if len(plan.Up) == 0 {
			continue
		}
		if err := db.Migrate(ctx, plan); err != nil {
			return fmt.Errorf("migrate %s: %w", meta.Name, err)
		}
	}
	return nil
}

// Serve starts the HTTP gateway on addr and blocks until ctx is cancelled, then
// shuts down gracefully (draining in-flight requests).
func Serve(ctx context.Context, def *schema.SchemaDefinition, db store.Adapter, addr string, logger *slog.Logger) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: gateway.New(def, db, logger).Handler(),
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	// TODO(phase-1.5): hot-reload the schema on file change (fsnotify).
}
