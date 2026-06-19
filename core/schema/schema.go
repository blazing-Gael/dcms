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
	Name     string    `json:"name"`
	Type     FieldType `json:"type"`
	Required bool      `json:"required,omitempty"`
	Default  any       `json:"default,omitempty"`
	Unique   bool      `json:"unique,omitempty"`
	Min      *float64  `json:"min,omitempty"`     // string: min length; number/integer: min value
	Max      *float64  `json:"max,omitempty"`     // string: max length; number/integer: max value
	Pattern  string    `json:"pattern,omitempty"` // string: regex pattern
	Values   []string  `json:"values,omitempty"`  // enum: allowed values
	Label    string    `json:"label,omitempty"`   // admin UI label
	Hint     string    `json:"hint,omitempty"`    // admin UI helper text
}

// Index describes a database index. Columns has one entry for a single-column
// index, or several for a composite index.
type Index struct {
	Columns []string `json:"columns"`
}

// CollectionDef maps to a database table and a set of virtual HTTP endpoints.
type CollectionDef struct {
	Name       string     `json:"name"`
	Fields     []FieldDef `json:"fields"`
	Indexes    []Index    `json:"indexes,omitempty"`
	Timestamps bool       `json:"timestamps,omitempty"`

	// TODO(phase-2): SoftDelete, Draft, I18n, Access, Vectorize
	// TODO(phase-3): Hooks, Schedule
}

// Meta holds optional project metadata.
type Meta struct {
	Name        string `yaml:"name" json:"name,omitempty"`
	Description string `yaml:"description" json:"description,omitempty"`
	BaseURL     string `yaml:"base_url" json:"base_url,omitempty"` // default: /api/v1
}

// SchemaDefinition is the fully parsed schema file.
type SchemaDefinition struct {
	Version     string          `json:"version"`
	Meta        Meta            `json:"meta"`
	Collections []CollectionDef `json:"collections"`
}
