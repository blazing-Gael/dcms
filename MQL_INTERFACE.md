# mql — Storage Interface Contract

**This interface is locked after Phase 1.**
Every storage adapter (SQLite, PostgreSQL, Couchbase) implements it.
Do not add or remove methods without a major version bump.
Do not add adapter-specific methods to this interface — extend via separate interfaces.

---

## Core types

```go
package mql

import (
    "context"
    "time"
)

// Record is a generic map representing a single document from any collection.
// Keys are field names. Values are Go native types:
//   string, float64, int64, bool, time.Time, []byte (JSON), nil
type Record map[string]any

// Filter represents a single field-level filter condition.
type Filter struct {
    Field    string
    Operator Op
    Value    any
}

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

// Query holds all parameters for a list operation.
type Query struct {
    Collection string
    Filters    []Filter
    Sort       string    // field name, prefix "-" for descending
    Limit      int       // 0 = use default (20)
    Cursor     string    // opaque cursor from previous response
    Fields     []string  // sparse fieldset — empty = all fields
    Locale     string    // for i18n field resolution (Phase 2)
}

// Page is the result of a list operation.
type Page struct {
    Data       []Record
    Total      int64
    NextCursor string  // empty string = no more pages
}

// WriteInput holds data for create or update operations.
type WriteInput struct {
    Collection string
    Data       Record
}

// AggregateQuery holds parameters for an aggregation operation.
type AggregateQuery struct {
    Collection string
    Metric     AggMetric
    Field      string  // the field to aggregate on (required for sum/avg)
    GroupBy    string  // optional field to group results by
    Filters    []Filter
}

// AggMetric is the aggregation function.
type AggMetric string

const (
    Count AggMetric = "count"
    Sum   AggMetric = "sum"
    Avg   AggMetric = "avg"
)

// AggResult is a single row from an aggregation.
type AggResult struct {
    GroupValue any     // nil if no GroupBy
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

// MigrationPlan describes the changes needed to bring the DB in sync with the schema.
// Returned by Diff — applied by Migrate.
type MigrationPlan struct {
    Up   []string // SQL statements to apply
    Down []string // SQL statements to reverse
}

// TxFunc is the function passed to Tx.
// If it returns a non-nil error, the transaction is rolled back.
// If it returns nil, the transaction is committed.
type TxFunc func(ctx context.Context, tx DB) error
```

---

## The DB interface

```go
// DB is the core storage interface.
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
```

---

## Sentinel errors

```go
package mql

import "errors"

// Sentinel errors — use errors.Is() to check.
var (
    // ErrNotFound is returned by FindOne and Delete when no record matches.
    ErrNotFound = errors.New("mql: record not found")

    // ErrConflict is returned when a unique constraint is violated.
    ErrConflict = errors.New("mql: unique constraint violation")

    // ErrInvalidInput is returned when the caller provides malformed input
    // (e.g. unknown collection name, invalid field type in WriteInput).
    ErrInvalidInput = errors.New("mql: invalid input")

    // ErrTxAborted is returned when a transaction is rolled back.
    ErrTxAborted = errors.New("mql: transaction aborted")
)

// ValidationError is returned when field-level validation fails.
// Wraps ErrInvalidInput.
type ValidationError struct {
    Fields map[string]string // field name → error message
}

func (e *ValidationError) Error() string { ... }
func (e *ValidationError) Unwrap() error { return ErrInvalidInput }
```

---

## Adapter constructor signature

Every adapter must export a `New` function with this signature:

```go
// SQLite adapter
func New(cfg SQLiteConfig) (mql.Adapter, error)

// Postgres adapter
func New(cfg PostgresConfig) (mql.Adapter, error)

// Couchbase adapter (Phase 3)
func New(cfg CouchbaseConfig) (mql.Adapter, error)
```

Config structs:

```go
type SQLiteConfig struct {
    Path string // file path to the .db file, ":memory:" for in-memory
}

type PostgresConfig struct {
    DSN             string        // postgres connection string
    MaxConns        int           // default 10
    MinConns        int           // default 2
    MaxConnLifetime time.Duration // default 1h
    PgvectorEnabled bool          // enable vector extension (Phase 2)
}
```

---

## Usage example

```go
// In collection handler (simplified):
func (h *Handler) CreateProduct(ctx context.Context, data mql.Record) (mql.Record, error) {
    return h.db.Create(ctx, mql.WriteInput{
        Collection: "products",
        Data:       data,
    })
}

// Transactional order creation:
func (h *Handler) CreateOrder(ctx context.Context, orderData mql.Record) (mql.Record, error) {
    var order mql.Record

    err := h.db.Tx(ctx, func(ctx context.Context, tx mql.DB) error {
        // 1. Create the order
        created, err := tx.Create(ctx, mql.WriteInput{
            Collection: "orders",
            Data:       orderData,
        })
        if err != nil {
            return err // rolls back
        }
        order = created

        // 2. Decrement inventory — must be atomic with order creation
        product, err := tx.FindOne(ctx, "products", orderData["product_id"].(string))
        if err != nil {
            return err
        }
        newStock := product["stock"].(int64) - orderData["quantity"].(int64)
        if newStock < 0 {
            return errors.New("insufficient stock") // rolls back
        }
        _, err = tx.Update(ctx, mql.WriteInput{
            Collection: "products",
            Data:       mql.Record{"id": product["id"], "stock": newStock},
        })
        return err // nil = commit, non-nil = rollback
    })

    return order, err
}
```

---

## Implementation notes for adapters

### IDs

- All records have an `id` field of type `string` (UUID v4).
- The adapter generates the ID on Create if not provided.
- Never use auto-increment integers as the primary ID — UUIDs are stable, portable, and safe to expose.

### Field type mapping

The adapter is responsible for mapping Go types in `Record` to the database native types
and back. The schema compiler provides the expected Go type for each field.

| Schema type | Go type in Record | SQLite type   | Postgres type  |
|-------------|-------------------|---------------|----------------|
| string      | string            | TEXT          | VARCHAR(255)   |
| text        | string            | TEXT          | TEXT           |
| number      | float64           | REAL          | NUMERIC(12,4)  |
| integer     | int64             | INTEGER       | BIGINT         |
| boolean     | bool              | INTEGER (0/1) | BOOLEAN        |
| date        | time.Time         | TEXT (ISO)    | DATE           |
| datetime    | time.Time         | TEXT (ISO)    | TIMESTAMPTZ    |
| enum        | string            | TEXT          | VARCHAR(64)    |
| json        | []byte            | TEXT          | JSONB          |
| id (PK)     | string            | TEXT          | UUID           |

### Cursor pagination

Use keyset pagination, not OFFSET. The cursor encodes the sort field value + id of the
last seen record. Encode as base64(JSON). Never expose raw SQL values in the cursor.

### Null handling

- A field not present in WriteInput.Data → do not update that column (PATCH semantics).
- A field explicitly set to `nil` in WriteInput.Data → set the column to NULL.
- A NULL column in the DB → omit the key from the returned Record (do not include `"field": null`).

### SQLite-specific

- SQLite does not support `TIMESTAMPTZ` — store datetimes as TEXT in RFC3339 format, UTC.
- SQLite does not support `BOOLEAN` natively — store as INTEGER (0 = false, 1 = true).
- Translate `$1, $2, ...` placeholders to `?, ?, ...` for SQLite.
- WAL mode must be enabled on connection open for concurrent read performance.
