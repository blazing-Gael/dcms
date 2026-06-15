// Package schema parses dcms.schema.yaml into a SchemaDefinition and is the
// single source of truth for the engine: HTTP routes, migrations, TypeScript
// types, and the OpenAPI spec are all derived from the structs defined here.
//
// See SCHEMA_SPEC.md for the full schema language reference.
package schema

// FieldType enumerates the supported field types.
// Phase 1 implements the scalar types and enum; the rest are recognised but
// deferred to later phases (see SCHEMA_SPEC.md → Phase 1 subset).
type FieldType string

const (
	TypeString   FieldType = "string"
	TypeText     FieldType = "text"
	TypeNumber   FieldType = "number"
	TypeInteger  FieldType = "integer"
	TypeBoolean  FieldType = "boolean"
	TypeDate     FieldType = "date"
	TypeDateTime FieldType = "datetime"
	TypeEnum     FieldType = "enum"
	TypeJSON     FieldType = "json"

	// TODO(phase-2): relation, i18n
	// TODO(phase-3): media, geo, computed
)

// FieldDef is a single field within a collection.
type FieldDef struct {
	Name     string
	Type     FieldType
	Required bool
	Default  any
	Unique   bool
	Min      *float64 // string: min length; number/integer: min value
	Max      *float64 // string: max length; number/integer: max value
	Pattern  string   // string: regex pattern
	Values   []string // enum: allowed values
	Label    string   // admin UI label
	Hint     string   // admin UI helper text
}

// Index describes a database index. Columns has one entry for a single-column
// index, or several for a composite index.
type Index struct {
	Columns []string
}

// CollectionDef maps to a database table and a set of virtual HTTP endpoints.
type CollectionDef struct {
	Name       string
	Fields     []FieldDef
	Indexes    []Index
	Timestamps bool

	// TODO(phase-2): SoftDelete, Draft, I18n, Access, Vectorize
	// TODO(phase-3): Hooks, Schedule
}

// Meta holds optional project metadata.
type Meta struct {
	Name        string
	Description string
	BaseURL     string // default: /api/v1
}

// SchemaDefinition is the fully parsed schema file.
type SchemaDefinition struct {
	Version     string
	Meta        Meta
	Collections []CollectionDef
}
