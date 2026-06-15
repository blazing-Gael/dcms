// Package sqlite implements the store.Adapter interface backed by SQLite.
//
// It is the development-default adapter: zero external dependencies and fast
// local iteration. Production deployments swap it for the Postgres adapter via
// one config line — the store.Adapter interface is identical across adapters.
package sqlite

import (
	"context"
	"errors"

	"github.com/blazing-Gael/dcms/core/store"
)

// errNotImplemented is a placeholder returned by skeleton methods.
// Each method is filled in test-first per DEV_ROADMAP.md section 1.1.
var errNotImplemented = errors.New("sqlite: not implemented")

// Config holds the connection settings for the SQLite adapter.
type Config struct {
	Path string // file path to the .db file, ":memory:" for in-memory
}

// Adapter is the SQLite-backed implementation of store.Adapter.
type Adapter struct {
	cfg Config
	// db *sql.DB — added when the driver is wired in (TODO: phase-1).
}

// Compile-time assertion that Adapter satisfies the locked store contract.
var _ store.Adapter = (*Adapter)(nil)

// New opens (or creates) a SQLite database at cfg.Path and returns an Adapter.
//
// On open it must enable WAL mode for concurrent read performance
// (see STORE_INTERFACE.md → SQLite-specific notes).
func New(cfg Config) (store.Adapter, error) {
	// TODO(phase-1): open *sql.DB, enable WAL, verify connectivity.
	return &Adapter{cfg: cfg}, nil
}

// ── Read operations ──────────────────────────────────────────────────────

func (a *Adapter) Find(ctx context.Context, q store.Query) (store.Page, error) {
	// TODO(phase-1): keyset pagination, sort, fields, eq filters.
	return store.Page{}, errNotImplemented
}

func (a *Adapter) FindOne(ctx context.Context, collection, id string) (store.Record, error) {
	// TODO(phase-1): SELECT by id, return store.ErrNotFound when missing.
	return nil, errNotImplemented
}

// ── Write operations ───────────────────────────────────────────────────────

func (a *Adapter) Create(ctx context.Context, in store.WriteInput) (store.Record, error) {
	// TODO(phase-1): generate UUID v4 id, set timestamps, INSERT, return record.
	return nil, errNotImplemented
}

func (a *Adapter) Update(ctx context.Context, in store.WriteInput) (store.Record, error) {
	// TODO(phase-1): partial update (PATCH semantics), return store.ErrNotFound when missing.
	return nil, errNotImplemented
}

func (a *Adapter) Delete(ctx context.Context, collection, id string) error {
	// TODO(phase-1): hard delete; return store.ErrNotFound when missing.
	return errNotImplemented
}

// ── Aggregation ──────────────────────────────────────────────────────────────

func (a *Adapter) Aggregate(ctx context.Context, q store.AggregateQuery) ([]store.AggResult, error) {
	// TODO(phase-1): count/sum/avg with optional group_by.
	return nil, errNotImplemented
}

// ── Raw access ─────────────────────────────────────────────────────────────

func (a *Adapter) RawQuery(ctx context.Context, sql string, args ...any) ([]store.Record, error) {
	// TODO(phase-1): translate $1,$2 placeholders to ?,? then query.
	return nil, errNotImplemented
}

func (a *Adapter) RawExec(ctx context.Context, sql string, args ...any) (int64, error) {
	// TODO(phase-1): translate placeholders then exec, return rows affected.
	return 0, errNotImplemented
}

// ── Schema management ────────────────────────────────────────────────────────

func (a *Adapter) Introspect(ctx context.Context, collection string) (store.CollectionMeta, error) {
	// TODO(phase-1): read pragma table_info / index_list.
	return store.CollectionMeta{}, errNotImplemented
}

func (a *Adapter) Diff(ctx context.Context, desired store.CollectionMeta) (store.MigrationPlan, error) {
	// TODO(phase-1): compare desired vs Introspect, emit Up/Down SQL.
	return store.MigrationPlan{}, errNotImplemented
}

func (a *Adapter) Migrate(ctx context.Context, plan store.MigrationPlan) error {
	// TODO(phase-1): apply plan.Up inside a transaction, rollback on failure.
	return errNotImplemented
}

// ── Transactions ─────────────────────────────────────────────────────────────

func (a *Adapter) Tx(ctx context.Context, fn store.TxFunc) error {
	// TODO(phase-1): BEGIN, run fn against a tx-backed store.DB, COMMIT/ROLLBACK.
	return errNotImplemented
}

// ── Lifecycle ────────────────────────────────────────────────────────────────

func (a *Adapter) Close() error {
	// TODO(phase-1): close the underlying *sql.DB.
	return nil
}

func (a *Adapter) Ping(ctx context.Context) error {
	// TODO(phase-1): ping the underlying *sql.DB.
	return errNotImplemented
}
