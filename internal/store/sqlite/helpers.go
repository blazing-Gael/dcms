package sqlite

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/blazing-Gael/dcms/internal/store"
)

// identRe is the only shape of table/column name we allow into SQL.
//
// SQL placeholders (?) protect *values* from injection, but they cannot stand
// in for identifiers (table/column names). So any identifier we splice into a
// query by hand must be validated against this allowlist first. It mirrors the
// schema naming rules (lowercase snake_case, starts with a letter or underscore).
var identRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func safeIdent(name string) error {
	if !identRe.MatchString(name) {
		return fmt.Errorf("%w: invalid identifier %q", store.ErrInvalidInput, name)
	}
	return nil
}

// pageCursor is the opaque payload encoded into Page.NextCursor for keyset
// pagination. It records the sort spec it was built for (so a cursor can't be
// reused against a different sort) plus the last row's sort value and id —
// enough to seek to "the row right after this one".
type pageCursor struct {
	Sort  string `json:"s"`           // the Query.Sort this cursor was produced for
	Value any    `json:"v,omitempty"` // last row's value of the sort field (nil when sorting by id)
	ID    string `json:"id"`          // last row's id — the unique tiebreaker
}

// encodeCursor serializes a cursor to a URL-safe base64 string. The contract
// forbids exposing raw SQL values, so we wrap it in JSON+base64.
func encodeCursor(c pageCursor) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// decodeCursor parses a cursor string. A malformed cursor is caller error, so it
// maps to ErrInvalidInput rather than an internal error.
func decodeCursor(s string) (pageCursor, error) {
	var c pageCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, fmt.Errorf("%w: malformed cursor", store.ErrInvalidInput)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%w: malformed cursor", store.ErrInvalidInput)
	}
	return c, nil
}

// placeholderRe matches Postgres-style positional placeholders ($1, $2, …).
var placeholderRe = regexp.MustCompile(`\$\d+`)

// translatePlaceholders rewrites Postgres-style $1,$2,… placeholders to SQLite's
// "?". The store contract requires callers to write RawQuery/RawExec SQL in the
// Postgres style for portability; this is where the SQLite adapter honors that.
// Placeholders are expected to appear in order ($1 then $2 …), matching the args.
func translatePlaceholders(sql string) string {
	return placeholderRe.ReplaceAllString(sql, "?")
}

// quote wraps a (validated) identifier in double quotes for use in SQL.
func quote(ident string) string { return `"` + ident + `"` }

// mapWriteErr translates driver-specific write errors into the store package's
// sentinel errors, so callers can use errors.Is regardless of the backend.
func mapWriteErr(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return store.ErrConflict
	}
	if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		return store.ErrIntegrity
	}
	return err
}

// nowRFC3339 is the canonical timestamp format we store in SQLite (TEXT, UTC).
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// normalize converts a Go value from a store.Record into something the SQLite
// driver can bind, following STORE_INTERFACE.md's storage rules:
//   - bool      → INTEGER 0/1
//   - time.Time → TEXT in RFC3339 (UTC)
//
// everything else (string, float64, int64, []byte, nil) passes through.
func normalize(v any) any {
	switch t := v.(type) {
	case bool:
		if t {
			return int64(1)
		}
		return int64(0)
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case map[string]any, []any:
		// SQLite has no native object/array type, so a structured value (a JSON
		// column — see the `json` and `richtext` field types) is persisted as JSON
		// text. On error we fall through with the raw value and let the driver
		// reject it rather than silently dropping data.
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
		return v
	default:
		return v
	}
}

// scanRows turns a *sql.Rows into []store.Record, keyed by column name.
//
// SQLite hands back int64 / float64 / string / []byte / nil. We normalize []byte
// to string (SQLite TEXT often arrives as bytes) and, per the contract, OMIT any
// column whose value is NULL rather than storing an explicit nil.
func scanRows(rows *sql.Rows) ([]store.Record, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []store.Record
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		rec := make(store.Record, len(cols))
		for i, c := range cols {
			v := vals[i]
			if v == nil {
				continue // NULL → omit key
			}
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			rec[c] = v
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// joinConds builds each filter with buildCond and joins the conditions with sep,
// accumulating their args in order. It is the shared core of the top-level WHERE and
// of an And/Or group (ADR-0036); callers add the leading "WHERE" or the parentheses.
func joinConds(filters []store.Filter, sep string) (string, []any, error) {
	conds := make([]string, 0, len(filters))
	args := make([]any, 0, len(filters))
	for _, f := range filters {
		cond, a, err := buildCond(f)
		if err != nil {
			return "", nil, err
		}
		conds = append(conds, cond)
		args = append(args, a...)
	}
	return strings.Join(conds, " "+sep+" "), args, nil
}

// buildWhere turns a list of store.Filter into a SQL WHERE clause (without the
// leading "WHERE") and its bound arguments. The top-level filters are ANDed together.
// Returns ("", nil, nil) when there are no filters.
func buildWhere(filters []store.Filter) (string, []any, error) {
	if len(filters) == 0 {
		return "", nil, nil
	}
	return joinConds(filters, "AND")
}

// buildCond turns one store.Filter into a single SQL condition and its args. It
// recurses for And/Or grouping nodes and for In/NotIn whose value is a Subquery.
func buildCond(f store.Filter) (string, []any, error) {
	op := f.Operator
	if op == "" {
		op = store.Eq
	}

	// Boolean grouping nodes carry no Field; handle them before identifier checks.
	// Value is always a []store.Filter (built in-process; a Filter never crosses a
	// wire or the driver), so a plain assertion is enough.
	switch op {
	case store.And, store.Or:
		sub, ok := f.Value.([]store.Filter)
		if !ok {
			return "", nil, fmt.Errorf("%w: %s value must be a []store.Filter", store.ErrInvalidInput, op)
		}
		if len(sub) == 0 {
			return "", nil, fmt.Errorf("%w: %s group requires at least one sub-filter", store.ErrInvalidInput, op)
		}
		sep := "AND"
		if op == store.Or {
			sep = "OR"
		}
		inner, args, err := joinConds(sub, sep)
		if err != nil {
			return "", nil, err
		}
		return "(" + inner + ")", args, nil
	}

	if err := safeIdent(f.Field); err != nil {
		return "", nil, err
	}
	col := quote(f.Field)

	switch op {
	case store.Eq:
		return col + " = ?", []any{normalize(f.Value)}, nil
	case store.Ne:
		return col + " <> ?", []any{normalize(f.Value)}, nil
	case store.Gt:
		return col + " > ?", []any{normalize(f.Value)}, nil
	case store.Gte:
		return col + " >= ?", []any{normalize(f.Value)}, nil
	case store.Lt:
		return col + " < ?", []any{normalize(f.Value)}, nil
	case store.Lte:
		return col + " <= ?", []any{normalize(f.Value)}, nil
	case store.Contains:
		return col + " LIKE ?", []any{"%" + fmt.Sprint(f.Value) + "%"}, nil
	case store.StartsWith:
		return col + " LIKE ?", []any{fmt.Sprint(f.Value) + "%"}, nil
	case store.In, store.NotIn:
		kw := "IN"
		if op == store.NotIn {
			kw = "NOT IN"
		}
		// A Subquery value is a semi-join (ADR-0036); anything else is a literal set.
		if sq, ok := f.Value.(store.Subquery); ok {
			inner, a, err := buildSubquery(sq)
			if err != nil {
				return "", nil, err
			}
			return fmt.Sprintf("%s %s (%s)", col, kw, inner), a, nil
		}
		placeholders, vals, err := expandSlice(f.Value)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("%s %s (%s)", col, kw, placeholders), vals, nil
	case store.IsNull:
		return col + " IS NULL", nil, nil
	case store.NotNull:
		return col + " IS NOT NULL", nil, nil
	default:
		return "", nil, fmt.Errorf("%w: unsupported operator %q", store.ErrInvalidInput, op)
	}
}

// buildSubquery compiles a store.Subquery into `SELECT <select> FROM <collection>
// [WHERE …]` plus its args (ADR-0036). The projected column and collection are
// identifier-checked; the WHERE reuses buildWhere, so a sub-filter may itself be any
// supported condition. The uncorrelated invariant is the caller's contract — this
// never splices an outer column, because a Filter only ever carries literal values.
func buildSubquery(sq store.Subquery) (string, []any, error) {
	if err := safeIdent(sq.Collection); err != nil {
		return "", nil, err
	}
	if err := safeIdent(sq.Select); err != nil {
		return "", nil, err
	}
	where, args, err := buildWhere(sq.Filters)
	if err != nil {
		return "", nil, err
	}
	sql := fmt.Sprintf("SELECT %s FROM %s", quote(sq.Select), quote(sq.Collection))
	if where != "" {
		sql += " WHERE " + where
	}
	return sql, args, nil
}

// expandSlice turns the value of an In/NotIn filter into "?, ?, ?" plus its args.
func expandSlice(v any) (string, []any, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return "", nil, fmt.Errorf("%w: in/nin requires a slice value", store.ErrInvalidInput)
	}
	n := rv.Len()
	if n == 0 {
		return "", nil, fmt.Errorf("%w: in/nin requires a non-empty slice", store.ErrInvalidInput)
	}
	args := make([]any, n)
	for i := range n {
		args[i] = normalize(rv.Index(i).Interface())
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", "), args, nil
}
