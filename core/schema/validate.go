package schema

import (
	"fmt"
	"regexp"
)

// nameRe is the allowed shape for collection and field names: lowercase
// snake_case, starting with a letter. It matches the storage layer's identifier
// allowlist, so anything that validates here is safe to splice into SQL.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// reservedCollections are names DCMS uses for internal endpoints/tables.
var reservedCollections = map[string]bool{
	"_schema": true, "_dashboards": true, "_users": true,
	"_roles": true, "_audit": true, "_jobs": true,
}

// reservedFields are engine-managed columns; a schema must not declare them.
// They are added automatically during compilation (see translate.go).
var reservedFields = map[string]bool{
	"id": true, "created_at": true, "updated_at": true,
	"created_by": true, "updated_by": true,
}

// phase1Types are the field types implemented in Phase 1.
var phase1Types = map[FieldType]bool{
	TypeString: true, TypeText: true, TypeNumber: true, TypeInteger: true,
	TypeBoolean: true, TypeDate: true, TypeDateTime: true, TypeEnum: true, TypeJSON: true,
}

// deferredTypes maps recognised-but-unimplemented field types to the phase that
// will implement them, so we can give a precise error instead of "unknown type".
var deferredTypes = map[FieldType]string{
	"relation": "2", "i18n": "2",
	"media": "3", "geo": "3", "computed": "3",
}

// Validate checks the schema against the structural rules in SCHEMA_SPEC.md and
// returns a *ValidationError listing every problem found (it does not stop at the
// first). Returns nil when the schema is valid.
func (s *SchemaDefinition) Validate() error {
	var issues []string
	add := func(format string, args ...any) { issues = append(issues, fmt.Sprintf(format, args...)) }

	if s.Version == "" {
		add("version: required (use \"1\")")
	}
	if len(s.Collections) == 0 {
		add("collections: at least one collection is required")
	}

	seenCol := make(map[string]bool)
	for _, col := range s.Collections {
		cpath := "collections." + col.Name

		switch {
		case reservedCollections[col.Name]:
			add("%s: %q is a reserved collection name", cpath, col.Name)
		case !nameRe.MatchString(col.Name):
			add("%s: invalid name (must be lowercase snake_case, starting with a letter)", cpath)
		}
		if seenCol[col.Name] {
			add("%s: duplicate collection name", cpath)
		}
		seenCol[col.Name] = true

		// Column names available to indexes: declared fields + engine columns.
		known := map[string]bool{"id": true, "created_at": true, "updated_at": true, "created_by": true, "updated_by": true}

		seenField := make(map[string]bool)
		for _, f := range col.Fields {
			fpath := cpath + ".fields." + f.Name

			switch {
			case !nameRe.MatchString(f.Name):
				add("%s: invalid field name (must be lowercase snake_case, starting with a letter)", fpath)
			case reservedFields[f.Name]:
				add("%s: %q is reserved and added automatically", fpath, f.Name)
			}
			if seenField[f.Name] {
				add("%s: duplicate field name", fpath)
			}
			seenField[f.Name] = true
			known[f.Name] = true

			// Field type.
			switch {
			case f.Type == "":
				add("%s: missing type", fpath)
			case phase1Types[f.Type]:
				// ok
			default:
				if phase, ok := deferredTypes[f.Type]; ok {
					add("%s: type %q is not supported until phase %s", fpath, f.Type, phase)
				} else {
					add("%s: unknown field type %q", fpath, f.Type)
				}
			}

			// Enum values.
			if f.Type == TypeEnum {
				if len(f.Values) == 0 {
					add("%s: enum requires a non-empty values list", fpath)
				}
				seenVal := make(map[string]bool)
				for _, v := range f.Values {
					if seenVal[v] {
						add("%s: enum values contains duplicate %q", fpath, v)
					}
					seenVal[v] = true
				}
			}

			// A pattern must be a compilable regex (so validation never fails at
			// request time on a broken schema).
			if f.Pattern != "" {
				if _, err := regexp.Compile(f.Pattern); err != nil {
					add("%s: invalid pattern: %v", fpath, err)
				}
			}
		}

		// Index columns must reference real columns.
		for i, idx := range col.Indexes {
			if len(idx.Columns) == 0 {
				add("%s.indexes[%d]: index has no columns", cpath, i)
				continue
			}
			for _, c := range idx.Columns {
				if !known[c] {
					add("%s.indexes[%d]: column %q is not a field of this collection", cpath, i, c)
				}
			}
		}
	}

	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}
