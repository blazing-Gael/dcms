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
	TypeDecimal  FieldType = "decimal" // exact fixed-point (money), stored as int64 minor units (ADR-0017)
	TypeBoolean  FieldType = "boolean"
	TypeDate     FieldType = "date"
	TypeDateTime FieldType = "datetime"
	TypeEnum     FieldType = "enum"
	TypeJSON     FieldType = "json"
	TypeRelation FieldType = "relation"
	TypeFile     FieldType = "file"     // sugar: a relation to the engine's _media collection
	TypeRichText FieldType = "richtext" // structured (portable-text-style) content, stored as JSON (ADR-0014)

	// TODO(phase-2): i18n
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
	Scale    *int      `json:"scale,omitempty"`   // decimal: fixed fractional digits (default 2)
	Label    string    `json:"label,omitempty"`   // admin UI label
	Hint     string    `json:"hint,omitempty"`    // admin UI helper text

	// Relation fields (Type == relation):
	Target   string `json:"target,omitempty"`    // the collection this relation points at
	Many     bool   `json:"many,omitempty"`      // false: belongs-to (FK column); true: many-to-many (join table)
	OnDelete string `json:"on_delete,omitempty"` // belongs-to only: restrict (default) | cascade | set null

	// Rich content fields (Type == richtext) — per-field allowlists (ADR-0014).
	// Empty lists fall back to the built-in defaults (see richtext.go).
	Styles []string `json:"styles,omitempty"` // allowed block styles (free-form labels)
	Marks  []string `json:"marks,omitempty"`  // allowed decorators + annotation types
	Blocks []string `json:"blocks,omitempty"` // allowed custom (non-text) block types

	// Access is the field's per-direction policy (ADR-0016 milestone 2). Nil
	// means unrestricted. Read masks the field out of responses; Write drops an
	// unauthorized writer's value. Enforced at the gateway, never in the store.
	Access *FieldAccess `json:"access,omitempty"`
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

	// Publishing opts the collection into draft/published/scheduled/archived
	// states (ADR-0012): adds _status + _published_at, hides non-live records
	// from public reads, and exposes publish/unpublish/archive actions.
	Publishing bool `json:"publishing,omitempty"`
	// SoftDelete makes DELETE trash a record (_deleted_at) instead of removing
	// it, hides trashed records from public reads, and exposes restore + purge.
	SoftDelete bool `json:"soft_delete,omitempty"`
	// Revisions captures append-only version history for the collection in the
	// engine-managed _revisions collection (ADR-0013), with view/restore endpoints.
	Revisions bool `json:"revisions,omitempty"`
	// Events appends a row to the engine-managed _events log on each state change
	// (create/update/delete and lifecycle transitions), captured in the write
	// transaction, and exposes them on the change feed (ADR-0021, M-B). Opt-in:
	// a collection that never declares it writes no event rows and pays nothing.
	Events bool `json:"events,omitempty"`

	// Access is the per-operation authorization policy (ADR-0016), enforced at
	// the gateway. Nil (or a nil action) falls back to the engine default:
	// reads public, writes authenticated. See CollectionDef.AccessRule.
	Access *AccessRules `json:"access,omitempty"`

	// TODO(phase-2): I18n, Vectorize
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
	Auth        AuthConfig      `json:"auth,omitempty"`
	Collections []CollectionDef `json:"collections"`
}
