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

	"github.com/blazing-Gael/dcms/core/store"
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
	return err
}

// nowRFC3339 is the canonical timestamp format we store in SQLite (TEXT, UTC).
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// normalize converts a Go value from a store.Record into something the SQLite
// driver can bind, following STORE_INTERFACE.md's storage rules:
//   - bool      → INTEGER 0/1
//   - time.Time → TEXT in RFC3339 (UTC)
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

// buildWhere turns a list of store.Filter into a SQL WHERE clause (without the
// leading "WHERE") and its bound arguments. Multiple filters are ANDed together.
// Returns ("", nil, nil) when there are no filters.
func buildWhere(filters []store.Filter) (string, []any, error) {
	if len(filters) == 0 {
		return "", nil, nil
	}
	conds := make([]string, 0, len(filters))
	args := make([]any, 0, len(filters))

	for _, f := range filters {
		if err := safeIdent(f.Field); err != nil {
			return "", nil, err
		}
		col := quote(f.Field)
		op := f.Operator
		if op == "" {
			op = store.Eq
		}
		switch op {
		case store.Eq:
			conds = append(conds, col+" = ?")
			args = append(args, normalize(f.Value))
		case store.Ne:
			conds = append(conds, col+" <> ?")
			args = append(args, normalize(f.Value))
		case store.Gt:
			conds = append(conds, col+" > ?")
			args = append(args, normalize(f.Value))
		case store.Gte:
			conds = append(conds, col+" >= ?")
			args = append(args, normalize(f.Value))
		case store.Lt:
			conds = append(conds, col+" < ?")
			args = append(args, normalize(f.Value))
		case store.Lte:
			conds = append(conds, col+" <= ?")
			args = append(args, normalize(f.Value))
		case store.Contains:
			conds = append(conds, col+" LIKE ?")
			args = append(args, "%"+fmt.Sprint(f.Value)+"%")
		case store.StartsWith:
			conds = append(conds, col+" LIKE ?")
			args = append(args, fmt.Sprint(f.Value)+"%")
		case store.In, store.NotIn:
			placeholders, vals, err := expandSlice(f.Value)
			if err != nil {
				return "", nil, err
			}
			kw := "IN"
			if op == store.NotIn {
				kw = "NOT IN"
			}
			conds = append(conds, fmt.Sprintf("%s %s (%s)", col, kw, placeholders))
			args = append(args, vals...)
		default:
			return "", nil, fmt.Errorf("%w: unsupported operator %q", store.ErrInvalidInput, op)
		}
	}
	return strings.Join(conds, " AND "), args, nil
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
