package schema

import "github.com/blazing-Gael/dcms/internal/store"

// Record-lifecycle managed columns (ADR-0012). Like audit columns, these are
// engine-managed and reserved: added to a collection when it opts into a
// lifecycle directive, never declarable or client-settable, always readable.
const (
	// LifecycleStatus holds the publishing state (see the Status* values). Added
	// when a collection sets `publishing: true`.
	LifecycleStatus = "_status"
	// LifecyclePublishedAt is the go-live instant (nullable). A value in the
	// future means "scheduled": visible only once now() passes it.
	LifecyclePublishedAt = "_published_at"
	// LifecycleDeletedAt is the soft-delete trash marker (nullable). Added when a
	// collection sets `soft_delete: true`.
	LifecycleDeletedAt = "_deleted_at"
)

// Publishing states stored in LifecycleStatus. "scheduled" is deliberately not a
// stored state — it is derived (status == published AND published_at > now).
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
	StatusArchived  = "archived"
)

// lifecycleColumns returns the managed columns a collection contributes for its
// enabled lifecycle directives, plus indexes (each column is part of the default
// read predicate, so it is indexed for cheap filtering).
func (c CollectionDef) lifecycleColumns() (cols []store.ColumnMeta, idx []store.IndexMeta) {
	if c.Publishing {
		cols = append(cols,
			store.ColumnMeta{Name: LifecycleStatus, Type: string(TypeString), Nullable: false, Default: StatusDraft},
			store.ColumnMeta{Name: LifecyclePublishedAt, Type: string(TypeDateTime), Nullable: true},
		)
		idx = append(idx,
			store.IndexMeta{Columns: []string{LifecycleStatus}},
			store.IndexMeta{Columns: []string{LifecyclePublishedAt}},
		)
	}
	if c.SoftDelete {
		cols = append(cols, store.ColumnMeta{Name: LifecycleDeletedAt, Type: string(TypeDateTime), Nullable: true})
		idx = append(idx, store.IndexMeta{Columns: []string{LifecycleDeletedAt}})
	}
	return cols, idx
}
