package schema

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ValidationError lists schema validation failures with their field paths,
// e.g. "collections.products.fields.category".
type ValidationError struct {
	Issues []string
}

func (e *ValidationError) Error() string {
	return "schema validation failed:\n  " + strings.Join(e.Issues, "\n  ")
}

// Parse reads YAML bytes into a SchemaDefinition and validates it. A successful
// return guarantees the schema is structurally sound and safe to compile.
func Parse(src []byte) (*SchemaDefinition, error) {
	var raw rawSchema
	if err := yaml.Unmarshal(src, &raw); err != nil {
		return nil, fmt.Errorf("schema: parse yaml: %w", err)
	}
	def, err := raw.toDefinition()
	if err != nil {
		return nil, err
	}
	if err := def.Validate(); err != nil {
		return nil, err
	}
	return def, nil
}

// ── raw YAML shapes ─────────────────────────────────────────────────────────
//
// We decode the top level into structs but keep `collections` (and within it,
// `fields` / `indexes`) as yaml.Node, because those use shorthand forms that a
// plain struct can't express: a field may be a scalar ("string") OR a mapping
// (full form), and an index entry may be a scalar OR a list (composite).
// Walking the nodes ourselves also preserves document order — which keeps
// generated columns, migrations, and codegen deterministic.

type rawSchema struct {
	Version     string    `yaml:"version"`
	Meta        Meta      `yaml:"meta"`
	Collections yaml.Node `yaml:"collections"`
}

// rawField is the full ("long") form of a field definition. Unknown keys
// (Phase 2+ directives like access) are ignored by yaml's struct decoder.
type rawField struct {
	Type     string   `yaml:"type"`
	Required bool     `yaml:"required"`
	Default  any      `yaml:"default"`
	Unique   bool     `yaml:"unique"`
	Min      *float64 `yaml:"min"`
	Max      *float64 `yaml:"max"`
	Pattern  string   `yaml:"pattern"`
	Values   []string `yaml:"values"`
	Label    string   `yaml:"label"`
	Hint     string   `yaml:"hint"`
}

type nodeEntry struct {
	Key string
	Val *yaml.Node
}

// mappingEntries returns the key/value pairs of a YAML mapping node, in order.
func mappingEntries(n *yaml.Node) ([]nodeEntry, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected a mapping, got %s", kindName(n.Kind))
	}
	out := make([]nodeEntry, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, nodeEntry{Key: n.Content[i].Value, Val: n.Content[i+1]})
	}
	return out, nil
}

func (r rawSchema) toDefinition() (*SchemaDefinition, error) {
	def := &SchemaDefinition{Version: r.Version, Meta: r.Meta}
	if r.Collections.Kind == 0 {
		return def, nil // no collections — Validate reports it
	}
	entries, err := mappingEntries(&r.Collections)
	if err != nil {
		return nil, fmt.Errorf("schema: collections: %w", err)
	}
	for _, e := range entries {
		col, err := toCollection(e.Key, e.Val)
		if err != nil {
			return nil, fmt.Errorf("schema: collections.%s: %w", e.Key, err)
		}
		def.Collections = append(def.Collections, col)
	}
	return def, nil
}

func toCollection(name string, node *yaml.Node) (CollectionDef, error) {
	col := CollectionDef{Name: name}
	entries, err := mappingEntries(node)
	if err != nil {
		return col, err
	}
	for _, e := range entries {
		switch e.Key {
		case "fields":
			col.Fields, err = toFields(e.Val)
			if err != nil {
				return col, fmt.Errorf("fields: %w", err)
			}
		case "timestamps":
			if err := e.Val.Decode(&col.Timestamps); err != nil {
				return col, fmt.Errorf("timestamps: %w", err)
			}
		case "indexes":
			col.Indexes, err = toIndexes(e.Val)
			if err != nil {
				return col, fmt.Errorf("indexes: %w", err)
			}
		default:
			// Phase 2+ directives (access, hooks, vectorize, draft, i18n,
			// soft_delete, schedule) are recognised but skipped in Phase 1.
			// TODO(phase-2): parse access, vectorize, draft, i18n, soft_delete.
			// TODO(phase-3): parse hooks, schedule.
		}
	}
	return col, nil
}

func toFields(node *yaml.Node) ([]FieldDef, error) {
	entries, err := mappingEntries(node)
	if err != nil {
		return nil, err
	}
	fields := make([]FieldDef, 0, len(entries))
	for _, e := range entries {
		f := FieldDef{Name: e.Key}
		switch e.Val.Kind {
		case yaml.ScalarNode:
			// Shorthand: `title: string`
			f.Type = FieldType(e.Val.Value)
		case yaml.MappingNode:
			// Full form: `title: { type: string, required: true, ... }`
			var rf rawField
			if err := e.Val.Decode(&rf); err != nil {
				return nil, fmt.Errorf("%s: %w", e.Key, err)
			}
			f.Type = FieldType(rf.Type)
			f.Required = rf.Required
			f.Default = rf.Default
			f.Unique = rf.Unique
			f.Min = rf.Min
			f.Max = rf.Max
			f.Pattern = rf.Pattern
			f.Values = rf.Values
			f.Label = rf.Label
			f.Hint = rf.Hint
		default:
			return nil, fmt.Errorf("%s: expected a type or a field definition, got %s", e.Key, kindName(e.Val.Kind))
		}
		fields = append(fields, f)
	}
	return fields, nil
}

func toIndexes(node *yaml.Node) ([]Index, error) {
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("expected a list, got %s", kindName(node.Kind))
	}
	indexes := make([]Index, 0, len(node.Content))
	for _, item := range node.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			// Single-column index: `status`
			indexes = append(indexes, Index{Columns: []string{item.Value}})
		case yaml.SequenceNode:
			// Composite index: `[category_id, status]`
			var cols []string
			if err := item.Decode(&cols); err != nil {
				return nil, err
			}
			indexes = append(indexes, Index{Columns: cols})
		default:
			return nil, fmt.Errorf("index entry must be a field name or a list, got %s", kindName(item.Kind))
		}
	}
	return indexes, nil
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.ScalarNode:
		return "scalar"
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "list"
	case yaml.DocumentNode:
		return "document"
	case yaml.AliasNode:
		return "alias"
	default:
		return "empty"
	}
}
