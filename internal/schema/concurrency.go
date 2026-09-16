package schema

import "github.com/blazing-Gael/dcms/internal/store"

// Optimistic concurrency (issue #26). A collection that opts into `concurrency:
// true` gains an engine-managed `_version` column: an integer that starts at 1 and
// the gateway increments on every write. A client reads it and passes it back as
// `If-Match` on a write; a mismatch means someone else wrote in between, so the
// write is refused (412) instead of silently clobbering their change. Like the
// lifecycle columns it is reserved, readonly, and never client-settable.
const ConcurrencyVersion = "_version"

// versionColumn returns the managed column a concurrency-enabled collection
// contributes. NOT NULL DEFAULT 1, so a create (and every pre-existing row on
// migration) starts at version 1 without the gateway having to set it.
func (c CollectionDef) versionColumn() (store.ColumnMeta, bool) {
	if !c.Concurrency {
		return store.ColumnMeta{}, false
	}
	return store.ColumnMeta{Name: ConcurrencyVersion, Type: string(TypeInteger), Nullable: false, Default: 1}, true
}
