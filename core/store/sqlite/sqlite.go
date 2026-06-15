// Package sqlite implements the store.Adapter interface backed by SQLite.
//
// It is the development-default adapter: zero external dependencies and fast
// local iteration. Production deployments swap it for the Postgres adapter via
// one config line — the store.Adapter interface is identical across adapters.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/blazing-Gael/dcms/core/store"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver (pure Go, no CGO)
)

// errNotImplemented is a placeholder returned by skeleton methods.
// Each method is filled in test-first per DEV_ROADMAP.md section 1.1.
var errNotImplemented = errors.New("sqlite: not implemented")

// Pagination bounds for list queries (see SCHEMA_SPEC.md → list query params).
const (
	defaultLimit = 20
	maxLimit     = 100
)

// Config holds the connection settings for the SQLite adapter.
type Config struct {
	Path string // file path to the .db file, ":memory:" for in-memory
}

// Adapter is the SQLite-backed implementation of store.Adapter.
type Adapter struct {
	cfg Config
	db  *sql.DB
}

// Compile-time assertion that Adapter satisfies the locked store contract.
var _ store.Adapter = (*Adapter)(nil)

// New opens (or creates) a SQLite database at cfg.Path and returns an Adapter.
//
// It enables WAL mode for concurrent read performance and foreign-key
// enforcement (see STORE_INTERFACE.md → SQLite-specific notes), then verifies
// connectivity before returning.
func New(cfg Config) (store.Adapter, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("sqlite: Config.Path is required (use \":memory:\" for an in-memory DB)")
	}

	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", cfg.Path, err)
	}

	// Pragmas are set per connection; apply them as the connection opens.
	// WAL = better concurrent reads; foreign_keys = enforce relations (Phase 2);
	// busy_timeout = wait instead of erroring when the DB is briefly locked.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA foreign_keys=ON;",
		"PRAGMA busy_timeout=5000;",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("sqlite: %q: %w", pragma, err)
		}
	}

	a := &Adapter{cfg: cfg, db: db}
	if err := a.Ping(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return a, nil
}

// ── Read operations ──────────────────────────────────────────────────────

func (a *Adapter) Find(ctx context.Context, q store.Query) (store.Page, error) {
	if err := safeIdent(q.Collection); err != nil {
		return store.Page{}, err
	}

	// Column list: "*" unless a sparse fieldset was requested.
	sel := "*"
	if len(q.Fields) > 0 {
		parts := make([]string, 0, len(q.Fields))
		for _, f := range q.Fields {
			if err := safeIdent(f); err != nil {
				return store.Page{}, err
			}
			parts = append(parts, quote(f))
		}
		sel = strings.Join(parts, ", ")
	}

	where, args, err := buildWhere(q.Filters)
	if err != nil {
		return store.Page{}, err
	}
	whereSQL := ""
	if where != "" {
		whereSQL = " WHERE " + where
	}

	// Total reflects the filter but ignores limit/sort.
	var total int64
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM %s%s;", quote(q.Collection), whereSQL)
	if err := a.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return store.Page{}, err
	}

	orderSQL := ""
	if q.Sort != "" {
		field, dir := q.Sort, "ASC"
		if strings.HasPrefix(field, "-") {
			field, dir = field[1:], "DESC"
		}
		if err := safeIdent(field); err != nil {
			return store.Page{}, err
		}
		orderSQL = fmt.Sprintf(" ORDER BY %s %s", quote(field), dir)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	// limit is an int we control, so it's safe to inline.
	listSQL := fmt.Sprintf("SELECT %s FROM %s%s%s LIMIT %d;", sel, quote(q.Collection), whereSQL, orderSQL, limit)
	rows, err := a.db.QueryContext(ctx, listSQL, args...)
	if err != nil {
		return store.Page{}, err
	}
	defer rows.Close()

	data, err := scanRows(rows)
	if err != nil {
		return store.Page{}, err
	}

	// TODO(phase-1): keyset cursor — encode last row's sort value + id into NextCursor.
	return store.Page{Data: data, Total: total}, nil
}

func (a *Adapter) FindOne(ctx context.Context, collection, id string) (store.Record, error) {
	if err := safeIdent(collection); err != nil {
		return nil, err
	}
	q := fmt.Sprintf("SELECT * FROM %s WHERE %s = ? LIMIT 1;", quote(collection), quote("id"))
	rows, err := a.db.QueryContext(ctx, q, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	recs, err := scanRows(rows)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, store.ErrNotFound
	}
	return recs[0], nil
}

// ── Write operations ───────────────────────────────────────────────────────

func (a *Adapter) Create(ctx context.Context, in store.WriteInput) (store.Record, error) {
	cols, err := a.tableColumns(ctx, in.Collection)
	if err != nil {
		return nil, err
	}

	// Copy the caller's data so we never mutate their map.
	data := make(store.Record, len(in.Data)+3)
	for k, v := range in.Data {
		data[k] = v
	}

	// Engine-managed fields: generate an id, stamp timestamps — but only when the
	// table actually has those columns. The adapter never invents columns.
	if _, ok := cols["id"]; ok {
		if id, _ := data["id"].(string); id == "" {
			id, err := uuidv4()
			if err != nil {
				return nil, err
			}
			data["id"] = id
		}
	}
	now := nowRFC3339()
	if _, ok := cols["created_at"]; ok {
		data["created_at"] = now
	}
	if _, ok := cols["updated_at"]; ok {
		data["updated_at"] = now
	}

	// Build the INSERT from keys that are real columns; ignore stray keys.
	names := make([]string, 0, len(data))
	phs := make([]string, 0, len(data))
	args := make([]any, 0, len(data))
	for k, v := range data {
		if _, ok := cols[k]; !ok {
			continue
		}
		names = append(names, quote(k))
		phs = append(phs, "?")
		args = append(args, normalize(v))
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: no known columns to insert into %q", store.ErrInvalidInput, in.Collection)
	}

	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s);",
		quote(in.Collection), strings.Join(names, ", "), strings.Join(phs, ", "))
	if _, err := a.db.ExecContext(ctx, q, args...); err != nil {
		return nil, mapWriteErr(err)
	}

	// Return the stored row so the caller sees server-set fields and DB-coerced types.
	if id, _ := data["id"].(string); id != "" {
		return a.FindOne(ctx, in.Collection, id)
	}
	return data, nil
}

// tableColumns returns the set of column names for a collection's table.
// Returns ErrInvalidInput if the table does not exist (PRAGMA returns no rows).
func (a *Adapter) tableColumns(ctx context.Context, collection string) (map[string]struct{}, error) {
	if err := safeIdent(collection); err != nil {
		return nil, err
	}
	rows, err := a.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s);", quote(collection)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := make(map[string]struct{})
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dflt      any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("%w: unknown collection %q", store.ErrInvalidInput, collection)
	}
	return cols, nil
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
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, stmt := range plan.Up {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback() // best-effort; the original error is what matters
			return fmt.Errorf("sqlite: migrate: %w", err)
		}
	}
	return tx.Commit()
}

// ── Transactions ─────────────────────────────────────────────────────────────

func (a *Adapter) Tx(ctx context.Context, fn store.TxFunc) error {
	// TODO(phase-1): BEGIN, run fn against a tx-backed store.DB, COMMIT/ROLLBACK.
	return errNotImplemented
}

// ── Lifecycle ────────────────────────────────────────────────────────────────

func (a *Adapter) Close() error {
	return a.db.Close()
}

func (a *Adapter) Ping(ctx context.Context) error {
	return a.db.PingContext(ctx)
}
