package schema

import "github.com/blazing-Gael/dcms/core/store"

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
// drive migrations, one per collection, in declaration order.
func (s *SchemaDefinition) CollectionMetas() []store.CollectionMeta {
	metas := make([]store.CollectionMeta, 0, len(s.Collections))
	for _, c := range s.Collections {
		metas = append(metas, c.ToCollectionMeta())
	}
	return metas
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
		meta.Columns = append(meta.Columns, store.ColumnMeta{
			Name:     f.Name,
			Type:     string(f.Type),
			Nullable: !f.Required,
			Default:  f.Default,
		})
		if f.Unique {
			meta.Indexes = append(meta.Indexes, store.IndexMeta{
				Columns: []string{f.Name},
				Unique:  true,
			})
		}
	}

	meta.Columns = append(meta.Columns, auditColumns...)

	for _, idx := range c.Indexes {
		meta.Indexes = append(meta.Indexes, store.IndexMeta{Columns: idx.Columns})
	}

	return meta
}
