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
	"slices"
	"strings"

	"github.com/blazing-Gael/dcms/core/store"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver (pure Go, no CGO)
)

// Pagination bounds for list queries (see SCHEMA_SPEC.md → list query params).
const (
	defaultLimit = 20
	maxLimit     = 100
)

// execer is the slice of database/sql shared by *sql.DB and *sql.Tx.
//
// Writing every query method against this interface (instead of *sql.DB) is
// what lets the exact same logic run either directly OR inside a transaction —
// which is the whole reason store.DB is implemented by both the adapter and the
// tx object (see STORE_INTERFACE.md → the DB interface).
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// conn carries all of the store.DB query logic over some execer (a connection
// pool or a single transaction). It does not own the connection and does not
// begin/commit — that is the job of whoever constructed it.
type conn struct {
	exec  execer
	idGen store.IDGenerator
}

// Compile-time assertion that conn satisfies the read/write contract.
var _ store.DB = (*conn)(nil)

// Config holds the connection settings for the SQLite adapter.
type Config struct {
	Path string // file path to the .db file, ":memory:" for in-memory

	// IDGen is the primary-key id strategy. Optional — defaults to
	// store.DefaultIDGenerator (UUIDv7). Set store.UUIDv4 or a custom generator
	// to change it; id generation is never forced on callers.
	IDGen store.IDGenerator
}

// Adapter is the SQLite-backed implementation of store.Adapter.
//
// It embeds conn (backed by the *sql.DB pool), so every store.DB method is
// available directly on the adapter; it adds lifecycle (Close/Ping), transactions
// (Tx), and a transactional Migrate on top.
type Adapter struct {
	conn
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

	idGen := cfg.IDGen
	if idGen == nil {
		idGen = store.DefaultIDGenerator
	}

	a := &Adapter{conn: conn{exec: db, idGen: idGen}, cfg: cfg, db: db}
	if err := a.Ping(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return a, nil
}

// ── Read operations ──────────────────────────────────────────────────────

func (c *conn) Find(ctx context.Context, q store.Query) (store.Page, error) {
	if err := safeIdent(q.Collection); err != nil {
		return store.Page{}, err
	}

	// Sort field + direction. Default to id ASC for a deterministic total order.
	// When sorting by any other field we append id as a unique tiebreaker, so the
	// ordering is total (no ties) — a hard requirement for stable keyset paging.
	sortField, sortDir := "id", "ASC"
	if q.Sort != "" {
		sortField = q.Sort
		if strings.HasPrefix(sortField, "-") {
			sortField, sortDir = sortField[1:], "DESC"
		}
		if err := safeIdent(sortField); err != nil {
			return store.Page{}, err
		}
	}
	idOnly := sortField == "id"

	// Total reflects the filter only — never the cursor or limit.
	where, filterArgs, err := buildWhere(q.Filters)
	if err != nil {
		return store.Page{}, err
	}
	whereSQL := ""
	if where != "" {
		whereSQL = " WHERE " + where
	}
	var total int64
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM %s%s;", quote(q.Collection), whereSQL)
	if err := c.exec.QueryRowContext(ctx, countSQL, filterArgs...).Scan(&total); err != nil {
		return store.Page{}, err
	}

	// The list query's WHERE = filters AND (optional) keyset "seek" condition.
	listConds := make([]string, 0, 2)
	if where != "" {
		listConds = append(listConds, where)
	}
	listArgs := append([]any{}, filterArgs...)

	if q.Cursor != "" {
		cur, err := decodeCursor(q.Cursor)
		if err != nil {
			return store.Page{}, err
		}
		if cur.Sort != q.Sort {
			return store.Page{}, fmt.Errorf("%w: cursor does not match sort %q", store.ErrInvalidInput, q.Sort)
		}
		cmp := ">"
		if sortDir == "DESC" {
			cmp = "<"
		}
		if idOnly {
			listConds = append(listConds, fmt.Sprintf("%s %s ?", quote("id"), cmp))
			listArgs = append(listArgs, cur.ID)
		} else {
			// (field cmp ?) OR (field = ? AND id cmp ?)
			listConds = append(listConds, fmt.Sprintf("(%s %s ? OR (%s = ? AND %s %s ?))",
				quote(sortField), cmp, quote(sortField), quote("id"), cmp))
			listArgs = append(listArgs, normalize(cur.Value), normalize(cur.Value), cur.ID)
		}
	}
	listWhere := ""
	if len(listConds) > 0 {
		listWhere = " WHERE " + strings.Join(listConds, " AND ")
	}

	// Column list. Keyset paging needs id (and the sort field) on every row, so if
	// a sparse fieldset omits them we add them, remember they were "extra", and
	// strip them from the output after building the cursor.
	sel := "*"
	var extra []string
	if len(q.Fields) > 0 {
		cols := append([]string{}, q.Fields...)
		need := []string{"id"}
		if !idOnly {
			need = append(need, sortField)
		}
		for _, n := range need {
			if !slices.Contains(cols, n) {
				cols = append(cols, n)
				extra = append(extra, n)
			}
		}
		parts := make([]string, 0, len(cols))
		for _, f := range cols {
			if err := safeIdent(f); err != nil {
				return store.Page{}, err
			}
			parts = append(parts, quote(f))
		}
		sel = strings.Join(parts, ", ")
	}

	order := fmt.Sprintf(" ORDER BY %s %s", quote(sortField), sortDir)
	if !idOnly {
		order += fmt.Sprintf(", %s %s", quote("id"), sortDir)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	// Fetch one extra row to learn whether a next page exists without a second query.
	listSQL := fmt.Sprintf("SELECT %s FROM %s%s%s LIMIT %d;", sel, quote(q.Collection), listWhere, order, limit+1)
	rows, err := c.exec.QueryContext(ctx, listSQL, listArgs...)
	if err != nil {
		return store.Page{}, err
	}
	defer rows.Close()

	data, err := scanRows(rows)
	if err != nil {
		return store.Page{}, err
	}

	nextCursor := ""
	if len(data) > limit {
		data = data[:limit]
		last := data[limit-1]
		if id, ok := last["id"].(string); ok {
			cur := pageCursor{Sort: q.Sort, ID: id}
			if !idOnly {
				cur.Value = last[sortField]
			}
			if nextCursor, err = encodeCursor(cur); err != nil {
				return store.Page{}, err
			}
		}
	}

	// Drop columns we added only to support paging.
	for _, e := range extra {
		for _, rec := range data {
			delete(rec, e)
		}
	}

	return store.Page{Data: data, Total: total, NextCursor: nextCursor}, nil
}

func (c *conn) FindOne(ctx context.Context, collection, id string) (store.Record, error) {
	if err := safeIdent(collection); err != nil {
		return nil, err
	}
	q := fmt.Sprintf("SELECT * FROM %s WHERE %s = ? LIMIT 1;", quote(collection), quote("id"))
	rows, err := c.exec.QueryContext(ctx, q, id)
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

func (c *conn) Create(ctx context.Context, in store.WriteInput) (store.Record, error) {
	cols, err := c.tableColumns(ctx, in.Collection)
	if err != nil {
		return nil, err
	}

	// Copy the caller's data so we never mutate their map.
	data := make(store.Record, len(in.Data)+3)
	for k, v := range in.Data {
		data[k] = v
	}

	// Engine-managed fields: generate an id, stamp timestamps and the actor — but
	// only when the table actually has those columns. The adapter never invents
	// columns; the schema compiler decides which audit columns exist.
	if _, ok := cols["id"]; ok {
		if id, _ := data["id"].(string); id == "" {
			id, err := c.idGen()
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
	// created_by / updated_by are authoritative: set from the trusted actor in
	// context, never from client-supplied data (which can't be trusted to say
	// who it is). Left unset when there is no actor (system/anonymous write).
	if actor := store.ActorFromContext(ctx); actor != "" {
		if _, ok := cols["created_by"]; ok {
			data["created_by"] = actor
		}
		if _, ok := cols["updated_by"]; ok {
			data["updated_by"] = actor
		}
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
	if _, err := c.exec.ExecContext(ctx, q, args...); err != nil {
		return nil, mapWriteErr(err)
	}

	// Return the stored row so the caller sees server-set fields and DB-coerced types.
	if id, _ := data["id"].(string); id != "" {
		return c.FindOne(ctx, in.Collection, id)
	}
	return data, nil
}

func (c *conn) Update(ctx context.Context, in store.WriteInput) (store.Record, error) {
	cols, err := c.tableColumns(ctx, in.Collection)
	if err != nil {
		return nil, err
	}

	id, _ := in.Data["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("%w: update requires an id in Data", store.ErrInvalidInput)
	}

	// PATCH semantics: only touch the columns present in Data. The audit columns
	// (id, created_at, created_by, updated_by) are engine-owned and never settable
	// by clients — updated_by is re-stamped from the actor below.
	sets := make([]string, 0, len(in.Data))
	args := make([]any, 0, len(in.Data))
	for k, v := range in.Data {
		switch k {
		case "id", "created_at", "created_by", "updated_by":
			continue
		}
		if _, ok := cols[k]; !ok {
			continue
		}
		sets = append(sets, quote(k)+" = ?")
		args = append(args, normalize(v))
	}

	// No updatable fields supplied → nothing to write; just return the current row
	// (which surfaces ErrNotFound if the id does not exist).
	if len(sets) == 0 {
		return c.FindOne(ctx, in.Collection, id)
	}

	if _, ok := cols["updated_at"]; ok {
		sets = append(sets, quote("updated_at")+" = ?")
		args = append(args, nowRFC3339())
	}
	if actor := store.ActorFromContext(ctx); actor != "" {
		if _, ok := cols["updated_by"]; ok {
			sets = append(sets, quote("updated_by")+" = ?")
			args = append(args, actor)
		}
	}

	args = append(args, id)
	q := fmt.Sprintf("UPDATE %s SET %s WHERE %s = ?;", quote(in.Collection), strings.Join(sets, ", "), quote("id"))
	res, err := c.exec.ExecContext(ctx, q, args...)
	if err != nil {
		return nil, mapWriteErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, store.ErrNotFound
	}
	return c.FindOne(ctx, in.Collection, id)
}

func (c *conn) Delete(ctx context.Context, collection, id string) error {
	if err := safeIdent(collection); err != nil {
		return err
	}
	// TODO(phase-2): soft delete — set _deleted_at instead of removing the row.
	q := fmt.Sprintf("DELETE FROM %s WHERE %s = ?;", quote(collection), quote("id"))
	res, err := c.exec.ExecContext(ctx, q, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// tableColumns returns the set of column names for a collection's table.
// Returns ErrInvalidInput if the table does not exist (PRAGMA returns no rows).
func (c *conn) tableColumns(ctx context.Context, collection string) (map[string]struct{}, error) {
	if err := safeIdent(collection); err != nil {
		return nil, err
	}
	rows, err := c.exec.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s);", quote(collection)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols := make(map[string]struct{})
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    any
			pk      int
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

// ── Aggregation ──────────────────────────────────────────────────────────────

func (c *conn) Aggregate(ctx context.Context, q store.AggregateQuery) ([]store.AggResult, error) {
	if err := safeIdent(q.Collection); err != nil {
		return nil, err
	}

	// Build the metric expression. sum/avg require a field; count does not.
	var metricExpr string
	switch q.Metric {
	case store.Count:
		metricExpr = "COUNT(*)"
	case store.Sum, store.Avg:
		if q.Field == "" {
			return nil, fmt.Errorf("%w: %s requires a field", store.ErrInvalidInput, q.Metric)
		}
		if err := safeIdent(q.Field); err != nil {
			return nil, err
		}
		fn := "SUM"
		if q.Metric == store.Avg {
			fn = "AVG"
		}
		metricExpr = fmt.Sprintf("%s(%s)", fn, quote(q.Field))
	default:
		return nil, fmt.Errorf("%w: unsupported metric %q", store.ErrInvalidInput, q.Metric)
	}

	where, args, err := buildWhere(q.Filters)
	if err != nil {
		return nil, err
	}
	whereSQL := ""
	if where != "" {
		whereSQL = " WHERE " + where
	}

	// No grouping → a single scalar result.
	if q.GroupBy == "" {
		var v sql.NullFloat64
		sqlStr := fmt.Sprintf("SELECT %s FROM %s%s;", metricExpr, quote(q.Collection), whereSQL)
		if err := c.exec.QueryRowContext(ctx, sqlStr, args...).Scan(&v); err != nil {
			return nil, err
		}
		return []store.AggResult{{Value: v.Float64}}, nil
	}

	// Grouped → one result row per distinct group value.
	if err := safeIdent(q.GroupBy); err != nil {
		return nil, err
	}
	g := quote(q.GroupBy)
	sqlStr := fmt.Sprintf("SELECT %s AS group_value, %s AS metric_value FROM %s%s GROUP BY %s;",
		g, metricExpr, quote(q.Collection), whereSQL, g)
	rows, err := c.exec.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.AggResult
	for rows.Next() {
		var (
			gv any
			mv sql.NullFloat64
		)
		if err := rows.Scan(&gv, &mv); err != nil {
			return nil, err
		}
		if b, ok := gv.([]byte); ok {
			gv = string(b)
		}
		out = append(out, store.AggResult{GroupValue: gv, Value: mv.Float64})
	}
	return out, rows.Err()
}

// ── Raw access ─────────────────────────────────────────────────────────────

func (c *conn) RawQuery(ctx context.Context, sqlStr string, args ...any) ([]store.Record, error) {
	return c.queryRecords(ctx, translatePlaceholders(sqlStr), args...)
}

func (c *conn) RawExec(ctx context.Context, sqlStr string, args ...any) (int64, error) {
	res, err := c.exec.ExecContext(ctx, translatePlaceholders(sqlStr), args...)
	if err != nil {
		return 0, mapWriteErr(err)
	}
	return res.RowsAffected()
}

// ── Schema management ────────────────────────────────────────────────────────

// queryRecords runs a query and returns its rows as []store.Record. Handy for
// reading PRAGMA output by column name (robust to SQLite version differences).
func (c *conn) queryRecords(ctx context.Context, query string, args ...any) ([]store.Record, error) {
	rows, err := c.exec.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

// asInt coerces a PRAGMA integer cell (int64 from the driver) to int.
func asInt(v any) int {
	if n, ok := v.(int64); ok {
		return int(n)
	}
	return 0
}

func (c *conn) Introspect(ctx context.Context, collection string) (store.CollectionMeta, error) {
	if err := safeIdent(collection); err != nil {
		return store.CollectionMeta{}, err
	}

	colRecs, err := c.queryRecords(ctx, fmt.Sprintf("PRAGMA table_info(%s);", quote(collection)))
	if err != nil {
		return store.CollectionMeta{}, err
	}
	if len(colRecs) == 0 {
		// A real table always has ≥1 column, so zero rows means "no such table".
		return store.CollectionMeta{}, store.ErrNotFound
	}

	meta := store.CollectionMeta{Name: collection}
	for _, r := range colRecs {
		name, _ := r["name"].(string)
		ctype, _ := r["type"].(string)
		meta.Columns = append(meta.Columns, store.ColumnMeta{
			Name:     name,
			Type:     ctype,
			Nullable: asInt(r["notnull"]) == 0,
			Default:  r["dflt_value"], // absent (NULL) → nil
		})
	}

	idxRecs, err := c.queryRecords(ctx, fmt.Sprintf("PRAGMA index_list(%s);", quote(collection)))
	if err != nil {
		return store.CollectionMeta{}, err
	}
	for _, ir := range idxRecs {
		idxName, _ := ir["name"].(string)
		infoRecs, err := c.queryRecords(ctx, fmt.Sprintf("PRAGMA index_info(%s);", quote(idxName)))
		if err != nil {
			return store.CollectionMeta{}, err
		}
		var cols []string
		for _, info := range infoRecs {
			if cn, ok := info["name"].(string); ok {
				cols = append(cols, cn)
			}
		}
		if len(cols) == 0 {
			continue // expression index — not something we model
		}
		meta.Indexes = append(meta.Indexes, store.IndexMeta{
			Name:    idxName,
			Columns: cols,
			Unique:  asInt(ir["unique"]) == 1,
		})
	}

	return meta, nil
}

// Diff compares the desired CollectionMeta against the database's current state
// and returns the migration to reconcile them.
//
// Phase 1 is purely additive: it creates a missing table, or ADDs missing columns
// and CREATEs missing indexes on an existing one. It never drops or retypes
// existing columns (destructive changes need an explicit, reviewed migration).
func (c *conn) Diff(ctx context.Context, desired store.CollectionMeta) (store.MigrationPlan, error) {
	if err := safeIdent(desired.Name); err != nil {
		return store.MigrationPlan{}, err
	}

	current, err := c.Introspect(ctx, desired.Name)
	if errors.Is(err, store.ErrNotFound) {
		// Table does not exist → create it from scratch.
		up := []string{createTableSQL(desired)}
		for _, idx := range desired.Indexes {
			up = append(up, createIndexSQL(desired.Name, idx))
		}
		down := []string{fmt.Sprintf("DROP TABLE IF EXISTS %s;", quote(desired.Name))}
		return store.MigrationPlan{Up: up, Down: down}, nil
	}
	if err != nil {
		return store.MigrationPlan{}, err
	}

	haveCol := make(map[string]struct{}, len(current.Columns))
	for _, col := range current.Columns {
		haveCol[col.Name] = struct{}{}
	}
	haveIdx := make(map[string]struct{}, len(current.Indexes))
	for _, idx := range current.Indexes {
		haveIdx[idxKey(idx)] = struct{}{}
	}

	var up, down []string
	for _, col := range desired.Columns {
		if _, ok := haveCol[col.Name]; ok {
			continue
		}
		up = append(up, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;", quote(desired.Name), columnDefSQL(col)))
		down = append(down, fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s;", quote(desired.Name), quote(col.Name)))
	}
	for _, idx := range desired.Indexes {
		if _, ok := haveIdx[idxKey(idx)]; ok {
			continue
		}
		up = append(up, createIndexSQL(desired.Name, idx))
		down = append(down, fmt.Sprintf("DROP INDEX IF EXISTS %s;", quote(indexName(desired.Name, idx))))
	}

	return store.MigrationPlan{Up: up, Down: down}, nil
}

// Migrate (on conn) applies the plan's statements on the current executor without
// managing a transaction. Inside Tx the surrounding transaction provides atomicity;
// at the top level the adapter's Migrate wraps this in one (see Adapter.Migrate).
func (c *conn) Migrate(ctx context.Context, plan store.MigrationPlan) error {
	for _, stmt := range plan.Up {
		if _, err := c.exec.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("sqlite: migrate: %w", err)
		}
	}
	return nil
}

// ── Transactions ─────────────────────────────────────────────────────────────

// Tx runs fn inside a single database transaction. fn receives a store.DB backed
// by that transaction; returning nil commits, returning an error (or panicking)
// rolls back.
func (a *Adapter) Tx(ctx context.Context, fn store.TxFunc) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback() // no-op if already committed; safety net on panic/early return
		}
	}()

	if err := fn(ctx, &conn{exec: tx, idGen: a.idGen}); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// Migrate applies a MigrationPlan atomically: all statements succeed and commit,
// or any failure rolls the whole plan back.
func (a *Adapter) Migrate(ctx context.Context, plan store.MigrationPlan) error {
	return a.Tx(ctx, func(ctx context.Context, tx store.DB) error {
		return tx.Migrate(ctx, plan)
	})
}

// ── Lifecycle ────────────────────────────────────────────────────────────────

func (a *Adapter) Close() error {
	return a.db.Close()
}

func (a *Adapter) Ping(ctx context.Context) error {
	return a.db.PingContext(ctx)
}
