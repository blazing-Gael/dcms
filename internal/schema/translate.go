package schema

import (
	"strings"

	"github.com/blazing-Gael/dcms/internal/store"
)

// normalizeOnDelete canonicalizes a belongs-to on_delete directive to the form
// the store layer understands. "restrict" (and the empty default) render to "",
// meaning no ON DELETE action — SQLite's default, which blocks deleting a
// referenced parent. ok reports whether the value is a recognized policy.
func normalizeOnDelete(s string) (canonical string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "restrict":
		return "", true
	case "cascade":
		return "cascade", true
	case "set null", "set_null":
		return "set null", true
	default:
		return "", false
	}
}

// auditColumns are appended to every collection regardless of the `timestamps`
// directive. Audit trails and timestamps are the non-negotiable essentials —
// they are always present so attribution works everywhere (created_by/updated_by
// are stamped by the store from the request actor).
var auditColumns = []store.ColumnMeta{
	{Name: "created_at", Type: string(TypeDateTime), Nullable: true},
	{Name: "updated_at", Type: string(TypeDateTime), Nullable: true},
	{Name: "created_by", Type: string(TypeString), Nullable: true},
	{Name: "updated_by", Type: string(TypeString), Nullable: true},
}

// CollectionMetas translates the whole schema into the store-layer metas that
// drive migrations, in declaration order: each collection's table, followed by
// the engine-managed join table for any many-to-many field it declares.
func (s *SchemaDefinition) CollectionMetas() []store.CollectionMeta {
	metas := make([]store.CollectionMeta, 0, len(s.Collections))
	for _, c := range s.Collections {
		metas = append(metas, c.ToCollectionMeta())
		for _, f := range c.Fields {
			if f.Type == TypeRelation && f.Many {
				metas = append(metas, joinTableMeta(c.Name, f.Name, f.Target))
			}
		}
	}
	return metas
}

// JoinTableName is the engine-managed table backing a many-to-many field. Keyed
// on the field name (not the target) so a self-referential m2m — e.g.
// users.friends → users — gets a distinct table ("users_friends") with no
// column collision.
func JoinTableName(collection, field string) string {
	return collection + "_" + field
}

// joinTableMeta builds the physical shape of one m2m join table: an id, the two
// endpoint ids, and the full audit set (a link records when/who connected it).
// A unique index on (source_id, target_id) makes links idempotent; a target_id
// index serves reverse lookups. Both endpoints carry a foreign key with
// ON DELETE CASCADE (ADR-0010): deleting either endpoint record removes its link
// rows automatically — the one place orphaned rows would otherwise accumulate.
func joinTableMeta(collection, field, target string) store.CollectionMeta {
	m := store.CollectionMeta{Name: JoinTableName(collection, field)}
	m.Columns = append(m.Columns,
		store.ColumnMeta{Name: "id", Type: string(TypeString), Nullable: false},
		store.ColumnMeta{Name: "source_id", Type: string(TypeString), Nullable: false, References: collection, OnDelete: "cascade"},
		store.ColumnMeta{Name: "target_id", Type: string(TypeString), Nullable: false, References: target, OnDelete: "cascade"},
	)
	m.Columns = append(m.Columns, auditColumns...)
	m.Indexes = append(m.Indexes,
		store.IndexMeta{Columns: []string{"source_id", "target_id"}, Unique: true},
		store.IndexMeta{Columns: []string{"target_id"}},
	)
	return m
}

// ToCollectionMeta turns one parsed collection into the physical shape the store
// adapter migrates to: the engine-managed id first, the declared fields in order,
// then the audit columns. Unique fields and declared indexes become IndexMeta.
//
// Column Type carries the canonical schema token (e.g. "string", "datetime") —
// each adapter maps it to its own native SQL type (see STORE_INTERFACE.md).
func (c CollectionDef) ToCollectionMeta() store.CollectionMeta {
	meta := store.CollectionMeta{Name: c.Name}

	// id is always the primary key.
	meta.Columns = append(meta.Columns, store.ColumnMeta{
		Name: "id", Type: string(TypeString), Nullable: false,
	})

	for _, f := range c.Fields {
		// A many-to-many relation lives in a join table, not a column here, so
		// it contributes no column. (Rejected in validation until implemented.)
		if f.Type == TypeRelation && f.Many {
			continue
		}
		// A belongs-to relation is stored as a string FK column holding the
		// target record's id, with a database foreign key to the target's id
		// (ADR-0010). No ON DELETE action → the RESTRICT default: a parent that
		// is still referenced can't be deleted.
		colType := string(f.Type)
		references, onDelete := "", ""
		if f.Type == TypeRelation {
			colType = string(TypeString)
			references = f.Target
			onDelete, _ = normalizeOnDelete(f.OnDelete) // validated already
		}
		// A richtext value is a structured document persisted as JSON; it needs no
		// new physical column type (ADR-0014), keeping the store interface untouched.
		if f.Type == TypeRichText {
			colType = string(TypeJSON)
		}
		meta.Columns = append(meta.Columns, store.ColumnMeta{
			Name:       f.Name,
			Type:       colType,
			Nullable:   !f.Required,
			Default:    f.Default,
			References: references,
			OnDelete:   onDelete,
		})
		if f.Unique {
			meta.Indexes = append(meta.Indexes, store.IndexMeta{
				Columns: []string{f.Name},
				Unique:  true,
			})
		}
	}

	meta.Columns = append(meta.Columns, auditColumns...)

	// Lifecycle columns (ADR-0012), for whichever directives this collection
	// opted into, with their supporting indexes.
	lcCols, lcIdx := c.lifecycleColumns()
	meta.Columns = append(meta.Columns, lcCols...)
	meta.Indexes = append(meta.Indexes, lcIdx...)

	for _, idx := range c.Indexes {
		meta.Indexes = append(meta.Indexes, store.IndexMeta{Columns: idx.Columns})
	}

	return meta
}
