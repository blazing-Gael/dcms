package sqlite

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/blazing-Gael/dcms/core/store"
)

// sqliteType maps a canonical schema type token (the values produced by the
// schema compiler — "string", "integer", "datetime", …) to a SQLite column
// affinity. Native SQLite type names are passed through unchanged so that a
// CollectionMeta produced by Introspect can round-trip back through DDL.
//
// SQLite is dynamically typed, so these are affinities, not hard constraints —
// the value normalization in helpers.go (bool→int, time→text) is what actually
// enforces our storage rules.
func sqliteType(t string) string {
	switch strings.ToLower(t) {
	case "string", "text", "date", "datetime", "enum", "json":
		return "TEXT"
	case "number", "real":
		return "REAL"
	case "integer", "boolean":
		return "INTEGER"
	case "blob":
		return "BLOB"
	default:
		switch strings.ToUpper(t) {
		case "TEXT", "INTEGER", "REAL", "BLOB", "NUMERIC":
			return strings.ToUpper(t)
		}
		return "TEXT"
	}
}

// columnDefSQL renders one column definition for a CREATE TABLE / ADD COLUMN.
// By convention the column named "id" is the primary key (every record has a
// string UUID id — see STORE_INTERFACE.md → IDs).
func columnDefSQL(col store.ColumnMeta) string {
	parts := []string{quote(col.Name), sqliteType(col.Type)}
	if col.Name == "id" {
		parts = append(parts, "PRIMARY KEY")
	}
	if !col.Nullable {
		parts = append(parts, "NOT NULL")
	}
	if lit, ok := defaultLiteral(col.Default); ok {
		parts = append(parts, "DEFAULT "+lit)
	}
	return strings.Join(parts, " ")
}

// defaultLiteral renders a Go default value as a SQL literal. Returns ok=false
// when there is no default (nil) or the type is unsupported.
func defaultLiteral(v any) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		return "'" + strings.ReplaceAll(t, "'", "''") + "'", true
	case bool:
		if t {
			return "1", true
		}
		return "0", true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), true
	default:
		return "", false
	}
}

// createTableSQL builds a CREATE TABLE statement from a desired CollectionMeta.
func createTableSQL(meta store.CollectionMeta) string {
	defs := make([]string, 0, len(meta.Columns))
	for _, col := range meta.Columns {
		defs = append(defs, columnDefSQL(col))
	}
	return fmt.Sprintf("CREATE TABLE %s (\n  %s\n);", quote(meta.Name), strings.Join(defs, ",\n  "))
}

// indexName derives a deterministic index name from the table and its columns,
// so the same desired index always maps to the same name.
func indexName(table string, idx store.IndexMeta) string {
	prefix := "idx"
	if idx.Unique {
		prefix = "uniq"
	}
	return prefix + "_" + table + "_" + strings.Join(idx.Columns, "_")
}

// createIndexSQL builds a CREATE [UNIQUE] INDEX statement.
func createIndexSQL(table string, idx store.IndexMeta) string {
	unique := ""
	if idx.Unique {
		unique = "UNIQUE "
	}
	cols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		cols[i] = quote(c)
	}
	return fmt.Sprintf("CREATE %sINDEX IF NOT EXISTS %s ON %s (%s);",
		unique, quote(indexName(table, idx)), quote(table), strings.Join(cols, ", "))
}

// idxKey identifies an index by uniqueness + ordered columns, so Diff can match
// a desired index against an existing one regardless of its name.
func idxKey(idx store.IndexMeta) string {
	return fmt.Sprintf("%t|%s", idx.Unique, strings.Join(idx.Columns, ","))
}
