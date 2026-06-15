// Package store is the storage abstraction layer for DCMS.
//
// Every storage adapter (SQLite, PostgreSQL, Couchbase) implements the Adapter
// interface defined here. This interface is locked after Phase 1 — do not add or
// remove methods without a major version bump, and do not add adapter-specific
// methods to it (extend via separate interfaces instead).
//
// See STORE_INTERFACE.md for the full contract.
package store

import "context"

// Record is a generic map representing a single document from any collection.
// Keys are field names. Values are Go native types:
//
//	string, float64, int64, bool, time.Time, []byte (JSON), nil
type Record map[string]any

// Op is a filter operator.
type Op string

const (
	Eq         Op = "eq"
	Ne         Op = "ne"
	Gt         Op = "gt"
	Gte        Op = "gte"
	Lt         Op = "lt"
	Lte        Op = "lte"
	Contains   Op = "contains"
	StartsWith Op = "starts_with"
	In         Op = "in"
	NotIn      Op = "nin"
)

// Filter represents a single field-level filter condition.
type Filter struct {
	Field    string
	Operator Op
	Value    any
}

// Query holds all parameters for a list operation.
type Query struct {
	Collection string
	Filters    []Filter
	Sort       string   // field name, prefix "-" for descending
	Limit      int      // 0 = use default (20)
	Cursor     string   // opaque cursor from previous response
	Fields     []string // sparse fieldset — empty = all fields
	Locale     string   // for i18n field resolution (Phase 2)
}

// Page is the result of a list operation.
type Page struct {
	Data       []Record
	Total      int64
	NextCursor string // empty string = no more pages
}

// WriteInput holds data for create or update operations.
type WriteInput struct {
	Collection string
	Data       Record
}

// AggMetric is the aggregation function.
type AggMetric string

const (
	Count AggMetric = "count"
	Sum   AggMetric = "sum"
	Avg   AggMetric = "avg"
)

// AggregateQuery holds parameters for an aggregation operation.
type AggregateQuery struct {
	Collection string
	Metric     AggMetric
	Field      string // the field to aggregate on (required for sum/avg)
	GroupBy    string // optional field to group results by
	Filters    []Filter
}

// AggResult is a single row from an aggregation.
type AggResult struct {
	GroupValue any // nil if no GroupBy
	Value      float64
}

// CollectionMeta describes the physical shape of a collection at the DB layer.
// Used by the schema compiler to drive migrations.
type CollectionMeta struct {
	Name    string
	Columns []ColumnMeta
	Indexes []IndexMeta
}

type ColumnMeta struct {
	Name     string
	Type     string // DB-native type string
	Nullable bool
	Default  any
}

type IndexMeta struct {
	Name    string
	Columns []string
	Unique  bool
}

// MigrationPlan describes the changes needed to bring the DB in sync with the
// schema. Returned by Diff — applied by Migrate.
type MigrationPlan struct {
	Up   []string // SQL statements to apply
	Down []string // SQL statements to reverse
}

// TxFunc is the function passed to Tx.
// If it returns a non-nil error, the transaction is rolled back.
// If it returns nil, the transaction is committed.
type TxFunc func(ctx context.Context, tx DB) error

// DB is the core storage interface.
//
// Both the top-level adapter and a transaction object implement this interface,
// which means all business logic can be written against DB and is automatically
// transactional when called inside Tx().
type DB interface {
	// ── Read operations ─────────────────────────────────────────────────

	// Find returns a paginated list of records matching the query.
	Find(ctx context.Context, q Query) (Page, error)

	// FindOne returns a single record by its id field.
	// Returns ErrNotFound if no record exists with that id.
	FindOne(ctx context.Context, collection, id string) (Record, error)

	// ── Write operations ─────────────────────────────────────────────────

	// Create inserts a new record and returns the created record (including
	// server-set fields like id, created_at, updated_at).
	Create(ctx context.Context, in WriteInput) (Record, error)

	// Update applies a partial update to an existing record.
	// Returns ErrNotFound if no record exists with that id.
	// The id field in in.Data is used to identify the record.
	Update(ctx context.Context, in WriteInput) (Record, error)

	// Delete removes a record by id.
	// Returns ErrNotFound if no record exists with that id.
	// If the collection has soft_delete: true, sets _deleted_at instead.
	Delete(ctx context.Context, collection, id string) error

	// ── Aggregation ──────────────────────────────────────────────────────

	// Aggregate runs a count/sum/avg query and returns one result per group.
	// If AggregateQuery.GroupBy is empty, returns a single AggResult.
	Aggregate(ctx context.Context, q AggregateQuery) ([]AggResult, error)

	// ── Raw access ───────────────────────────────────────────────────────

	// RawQuery executes a raw SQL string and returns the rows as []Record.
	// USE SPARINGLY — only for operations impossible to express via Find/Aggregate.
	// Never use for user-facing queries — always use Find with filters.
	// The sql parameter must use $1, $2, ... placeholders (Postgres style).
	// SQLite adapter translates automatically.
	RawQuery(ctx context.Context, sql string, args ...any) ([]Record, error)

	// RawExec executes a raw SQL statement (INSERT/UPDATE/DELETE) and returns
	// the number of rows affected.
	RawExec(ctx context.Context, sql string, args ...any) (int64, error)

	// ── Schema management ────────────────────────────────────────────────

	// Introspect returns the current physical schema of a collection as it
	// exists in the database. Used by the migration planner.
	Introspect(ctx context.Context, collection string) (CollectionMeta, error)

	// Diff compares the desired CollectionMeta (from schema) with the current
	// database state (from Introspect) and returns a MigrationPlan.
	Diff(ctx context.Context, desired CollectionMeta) (MigrationPlan, error)

	// Migrate applies a MigrationPlan inside a transaction.
	// If any statement fails, the entire plan is rolled back.
	Migrate(ctx context.Context, plan MigrationPlan) error
}

// Transactor is implemented by adapters that support transactions.
// Embed this in the top-level adapter — not in the transaction object itself.
type Transactor interface {
	// Tx runs fn inside a database transaction.
	// If fn returns nil, the transaction is committed.
	// If fn returns an error (or panics), the transaction is rolled back.
	// The tx DB passed to fn must not be used after fn returns.
	Tx(ctx context.Context, fn TxFunc) error
}

// Adapter is the full interface an adapter must implement.
// This is what the engine holds — a combined DB + Transactor.
type Adapter interface {
	DB
	Transactor

	// Close releases all connections and resources held by the adapter.
	// Must be called on shutdown.
	Close() error

	// Ping checks that the database is reachable.
	// Called on startup and by the readiness probe.
	Ping(ctx context.Context) error
}
