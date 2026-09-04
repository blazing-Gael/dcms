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

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
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

// TLSConfig points at a certificate/key pair for native HTTPS. When both are
// set, Serve terminates TLS itself; otherwise it serves plain HTTP (the norm
// behind a TLS-terminating proxy). DCMS does not manage or renew certificates.
type TLSConfig struct {
	CertFile string
	KeyFile  string
}

// enabled reports whether both a cert and key were supplied.
func (t TLSConfig) enabled() bool { return t.CertFile != "" && t.KeyFile != "" }

// Serve starts the HTTP(S) gateway on addr and blocks until ctx is cancelled,
// then shuts down gracefully (draining in-flight requests). opts tunes gateway
// behavior; tls, when both files are set, makes the server terminate TLS itself.
func Serve(ctx context.Context, def *schema.SchemaDefinition, db store.Adapter, addr string, logger *slog.Logger, opts gateway.Options, tls TLSConfig) error {
	gw := gateway.New(def, db, logger, opts)
	srv := &http.Server{
		Addr:    addr,
		Handler: gw.Handler(),
		// Bound how long a client may take to send request headers, so a
		// slow-header (Slowloris) client can't hold a connection open for free.
		// The per-request body/handler deadline is the gateway's withTimeout
		// middleware; this covers the pre-handler header phase it can't see.
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Background maintenance (e.g. sweeping expired idempotency keys) runs until
	// ctx is cancelled. Tests that build a gateway directly never start it.
	go gw.RunMaintenance(ctx)
	// Webhook delivery worker (ADR-0021 phase 2); a no-op when no endpoints are
	// configured, so it is always safe to launch.
	go gw.RunWebhooks(ctx)
	// Notification delivery worker (ADR-0021 phase 3): durable, retried account
	// email off the request path.
	go gw.RunNotifications(ctx)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr, "tls", tls.enabled())
		var err error
		if tls.enabled() {
			err = srv.ListenAndServeTLS(tls.CertFile, tls.KeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
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
