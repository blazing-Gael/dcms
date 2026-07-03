package schema

// Relation helpers used by the gateway to resolve `?expand=` requests. They read
// the parsed schema (the single source of truth) so expansion can never expose a
// relationship the schema doesn't declare.

// Inverse is a has-many edge pointing *at* a collection: some Source collection
// has a belongs-to Field whose target is that collection.
type Inverse struct {
	Source string // the collection holding the FK, e.g. "posts"
	Field  string // the relation field on Source, e.g. "author"
}

// BelongsTo reports the target collection of a belongs-to relation field on c,
// if field is exactly that. (many-to-many fields are not belongs-to.)
func (c CollectionDef) BelongsTo(field string) (target string, ok bool) {
	for _, f := range c.Fields {
		if f.Name == field && f.Type == TypeRelation && !f.Many {
			return f.Target, true
		}
	}
	return "", false
}

// ManyToMany reports the target collection of a many-to-many relation field on
// c, if field is exactly that.
func (c CollectionDef) ManyToMany(field string) (target string, ok bool) {
	for _, f := range c.Fields {
		if f.Name == field && f.Type == TypeRelation && f.Many {
			return f.Target, true
		}
	}
	return "", false
}

// InverseRelations returns every belongs-to relation across the schema that
// points at the target collection — i.e. the has-many edges into it.
func (s *SchemaDefinition) InverseRelations(target string) []Inverse {
	var out []Inverse
	for _, c := range s.Collections {
		for _, f := range c.Fields {
			if f.Type == TypeRelation && !f.Many && f.Target == target {
				out = append(out, Inverse{Source: c.Name, Field: f.Name})
			}
		}
	}
	return out
}
