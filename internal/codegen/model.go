package codegen

import (
	"strings"

	"github.com/blazing-Gael/dcms/internal/schema"
)

// This file holds the language-NEUTRAL codegen model. It walks the schema IR
// once and computes the three shapes every SDK needs — the record (response),
// the create input, and the update input — along with each field's neutral
// type, optionality, and readonly-ness. Language backends (TypeScript, Python,
// …) consume this model and only supply a type mapping + templates, so the
// non-trivial logic (optionality rules, audit columns, id handling) lives in
// exactly one place regardless of how many languages we target.

// Kind is a language-neutral field type. Backends map it to their own syntax.
type Kind int

const (
	KindString Kind = iota
	KindText
	KindNumber
	KindInteger
	KindDecimal
	KindBoolean
	KindDate
	KindDateTime
	KindEnum
	KindJSON
	KindRichText
	KindUnknown
)

// Field is one property in a generated shape.
type Field struct {
	Name     string
	Kind     Kind
	Enum     []string // populated when Kind == KindEnum
	Optional bool     // may be absent
	ReadOnly bool     // engine-managed; clients cannot set it

	// Relation metadata (set when the schema field is a relation). Backends use
	// these to type the field differently in inputs vs responses: an input takes
	// id(s), a response holds the id(s) OR the expanded target object(s).
	Relation bool
	Many     bool   // true: many-to-many (a list); false: belongs-to (a single ref)
	Target   string // the target's generated type name (PascalCase), e.g. "Users"
	// NoInlineCreate suppresses the "or an inline object to create" input variant.
	// Set for media relations: a media asset is created by uploading bytes, never
	// as an inline JSON object, so its input is only the id.
	NoInlineCreate bool
}

// Collection is one collection's three generated shapes plus its names.
type Collection struct {
	Name   string // route/schema name (plural), e.g. "products"
	Type   string // PascalCase type name, e.g. "Products"
	Record []Field
	Create []Field
	Update []Field
	// Reserved marks an engine-managed collection (a leading underscore, e.g.
	// _media). Its record type is still generated (relations expand to it), but it
	// has no JSON create/update inputs and no generic CRUD resource — it's served
	// through dedicated endpoints (media upload/serve).
	Reserved bool
	// Publishing / SoftDelete mirror the collection's lifecycle directives
	// (ADR-0012): backends use them to emit transition methods (publish/unpublish/
	// archive, restore) and a purge option on delete.
	Publishing bool
	SoftDelete bool
}

// Model is the full neutral input to a backend.
type Model struct {
	ContractVersion string
	BasePath        string
	Collections     []Collection
}

// kindOf maps a schema field type to a neutral Kind.
func kindOf(t schema.FieldType) Kind {
	switch t {
	case schema.TypeString:
		return KindString
	case schema.TypeText:
		return KindText
	case schema.TypeNumber:
		return KindNumber
	case schema.TypeInteger:
		return KindInteger
	case schema.TypeDecimal:
		// Exact money: a decimal string on the wire, never a JS number (ADR-0017).
		return KindDecimal
	case schema.TypeBoolean:
		return KindBoolean
	case schema.TypeDate:
		return KindDate
	case schema.TypeDateTime:
		return KindDateTime
	case schema.TypeEnum:
		return KindEnum
	case schema.TypeJSON:
		return KindJSON
	case schema.TypeRichText:
		return KindRichText
	case schema.TypeRelation:
		// A belongs-to relation is the target's id (a string) on the wire.
		// Typed relation expansion comes in a later codegen increment.
		return KindString
	default:
		// Deferred types (media, geo, …) until their generators land.
		return KindUnknown
	}
}

// fieldOf builds a neutral Field from a schema field with the given optionality,
// attaching relation metadata so backends can type it per context.
func fieldOf(f schema.FieldDef, optional bool) Field {
	fld := Field{Name: f.Name, Kind: kindOf(f.Type), Enum: f.Values, Optional: optional}
	if f.Type == schema.TypeRelation {
		fld.Relation = true
		fld.Many = f.Many
		fld.Target = pascal(f.Target)
		fld.NoInlineCreate = f.Target == schema.MediaCollection
	}
	return fld
}

// BuildModel derives the neutral model from a parsed schema.
func BuildModel(def *schema.SchemaDefinition) Model {
	m := Model{
		ContractVersion: def.ContractVersion(),
		BasePath:        def.BaseURL(),
	}
	for _, c := range def.Collections {
		coll := Collection{
			Name: c.Name, Type: pascal(c.Name), Reserved: strings.HasPrefix(c.Name, "_"),
			Publishing: c.Publishing, SoftDelete: c.SoftDelete,
		}

		// Record (response): id + audit columns are engine-managed → readonly.
		// A declared field is optional in the response when it is neither
		// required nor defaulted (the server omits NULL columns).
		coll.Record = append(coll.Record, Field{Name: "id", Kind: KindString, ReadOnly: true})
		for _, f := range c.Fields {
			coll.Record = append(coll.Record, fieldOf(f, !f.Required && f.Default == nil))
		}
		coll.Record = append(coll.Record,
			Field{Name: "created_at", Kind: KindDateTime, ReadOnly: true},
			Field{Name: "updated_at", Kind: KindDateTime, ReadOnly: true},
			Field{Name: "created_by", Kind: KindString, Optional: true, ReadOnly: true},
			Field{Name: "updated_by", Kind: KindString, Optional: true, ReadOnly: true},
		)
		// Lifecycle-managed columns are readonly response fields (ADR-0012).
		if c.Publishing {
			coll.Record = append(coll.Record,
				Field{Name: schema.LifecycleStatus, Kind: KindString, ReadOnly: true},
				Field{Name: schema.LifecyclePublishedAt, Kind: KindDateTime, Optional: true, ReadOnly: true},
			)
		}
		if c.SoftDelete {
			coll.Record = append(coll.Record,
				Field{Name: schema.LifecycleDeletedAt, Kind: KindDateTime, Optional: true, ReadOnly: true},
			)
		}

		// Create input: optional client id, then writeable fields. Required only
		// when the schema requires it AND there's no default to fall back on.
		coll.Create = append(coll.Create, Field{Name: "id", Kind: KindString, Optional: true})
		for _, f := range c.Fields {
			coll.Create = append(coll.Create, fieldOf(f, !(f.Required && f.Default == nil)))
		}

		// Update input (PATCH): every writeable field, all optional.
		for _, f := range c.Fields {
			coll.Update = append(coll.Update, fieldOf(f, true))
		}

		m.Collections = append(m.Collections, coll)
	}
	return m
}

// pascal converts a snake_case collection name to PascalCase. We keep the
// (plural) collection name rather than guessing an English singular — naive
// singularization is wrong often enough (stories, categories, data) that a
// deterministic name beats a clever one.
func pascal(name string) string {
	parts := strings.Split(name, "_")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}
