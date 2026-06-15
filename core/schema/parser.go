package schema

import "errors"

// ErrNotImplemented is returned by stubs that are filled in during Phase 1.
var ErrNotImplemented = errors.New("schema: not implemented")

// ValidationError lists schema validation failures with their field paths,
// e.g. "collections.products.fields.category".
type ValidationError struct {
	Issues []string
}

func (e *ValidationError) Error() string {
	msg := "schema validation failed:"
	for _, issue := range e.Issues {
		msg += "\n  " + issue
	}
	return msg
}

// Parse reads YAML bytes into a SchemaDefinition.
//
// TODO(phase-1): parse with gopkg.in/yaml.v3, map Phase 1 field types and the
// fields/timestamps/indexes directives, and run Validate. Phase 2+ directives
// are recognised but skipped.
func Parse(src []byte) (*SchemaDefinition, error) {
	return nil, ErrNotImplemented
}

// Validate checks structural rules (naming, enum values, reserved names, …)
// and returns a *ValidationError listing every problem found.
//
// TODO(phase-1): implement the validation rules from SCHEMA_SPEC.md.
func (s *SchemaDefinition) Validate() error {
	return ErrNotImplemented
}
