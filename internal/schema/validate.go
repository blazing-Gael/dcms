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
	"_roles": true, "_audit": true, "_jobs": true, "_media": true,
	"_revisions": true,
}

// reservedFields are engine-managed columns; a schema must not declare them.
// They are added automatically during compilation (see translate.go). The
// leading-underscore lifecycle columns are added only when a collection opts into
// the matching directive (ADR-0012), but are reserved unconditionally.
var reservedFields = map[string]bool{
	"id": true, "created_at": true, "updated_at": true,
	"created_by": true, "updated_by": true,
	LifecycleStatus: true, LifecyclePublishedAt: true, LifecycleDeletedAt: true,
}

// phase1Types are the field types implemented in Phase 1.
var phase1Types = map[FieldType]bool{
	TypeString: true, TypeText: true, TypeNumber: true, TypeInteger: true,
	TypeBoolean: true, TypeDate: true, TypeDateTime: true, TypeEnum: true, TypeJSON: true,
}

// deferredTypes maps recognised-but-unimplemented field types to the phase that
// will implement them, so we can give a precise error instead of "unknown type".
var deferredTypes = map[FieldType]string{
	"i18n":  "2",
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

	// All declared collection names, for relation target resolution (a target may
	// reference a collection declared later in the file, or the collection itself).
	allCols := make(map[string]bool, len(s.Collections))
	for _, col := range s.Collections {
		allCols[col.Name] = true
	}

	// Declared roles (ADR-0016), validated once and reused when checking that every
	// role named in an access rule actually exists.
	roleSet := make(map[string]bool, len(s.Auth.Roles))
	for i, r := range s.Auth.Roles {
		switch {
		case r.Name == "":
			add("auth.roles[%d]: role name is empty", i)
		case !nameRe.MatchString(r.Name):
			add("auth.roles.%s: invalid role name (must be lowercase snake_case, starting with a letter)", r.Name)
		case roleSet[r.Name]:
			add("auth.roles.%s: duplicate role", r.Name)
		}
		roleSet[r.Name] = true
	}
	if p := s.Auth.Provider; p != "" && p != "local" && p != "oidc" && p != "both" {
		add("auth.provider: %q is not one of local, oidc, both", p)
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

		// Column names available to indexes: declared fields + engine columns
		// (including the lifecycle columns this collection opts into).
		known := map[string]bool{"id": true, "created_at": true, "updated_at": true, "created_by": true, "updated_by": true}
		if col.Publishing {
			known[LifecycleStatus] = true
			known[LifecyclePublishedAt] = true
		}
		if col.SoftDelete {
			known[LifecycleDeletedAt] = true
		}

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
			case f.Type == TypeRelation:
				// Relation specifics validated just below.
			case f.Type == TypeFile:
				// A file is sugar for a relation to _media, finalized after
				// validation. Specifics validated just below.
			case f.Type == TypeRichText:
				// Structured content (ADR-0014). Its per-field allowlists are
				// validated just below.
			default:
				if phase, ok := deferredTypes[f.Type]; ok {
					add("%s: type %q is not supported until phase %s", fpath, f.Type, phase)
				} else {
					add("%s: unknown field type %q", fpath, f.Type)
				}
			}

			// Relation constraints. A belongs-to (many:false) becomes a string FK
			// column; a many-to-many (many:true) is backed by an engine-managed
			// join table (see translate.go).
			if f.Type == TypeRelation {
				switch {
				case f.Target == "":
					add("%s: relation requires a 'target' collection", fpath)
				case !allCols[f.Target]:
					add("%s: relation target %q is not a declared collection", fpath, f.Target)
				}
			}

			// A file field's target is the implicit _media collection.
			if f.Type == TypeFile && f.Target != "" {
				add("%s: a file field targets the built-in media library implicitly; do not set 'target'", fpath)
			}

			// on_delete applies to belongs-to relations and single file fields. A
			// many-to-many join table always cascades its own link rows
			// (engine-managed, not tunable).
			if (f.Type == TypeRelation || f.Type == TypeFile) && f.OnDelete != "" {
				canonical, ok := normalizeOnDelete(f.OnDelete)
				switch {
				case !ok:
					add("%s: invalid on_delete %q (want restrict, cascade, or set null)", fpath, f.OnDelete)
				case f.Many:
					add("%s: on_delete is not valid on a many-to-many relation (join links always cascade)", fpath)
				case canonical == "set null" && f.Required:
					add("%s: on_delete: set null requires the relation to be nullable (remove 'required')", fpath)
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

			// Rich content allowlists (ADR-0014): marks/blocks must be known.
			if f.Type == TypeRichText {
				for _, msg := range f.validateRichTextConfig() {
					add("%s: %s", fpath, msg)
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

		// Access rules (ADR-0016): every role named in a rule must be declared in
		// auth.roles, so a typo fails fast at compile time instead of silently
		// locking everyone out at request time.
		if col.Access != nil {
			for action, rule := range map[AccessAction]*Rule{
				ActionRead: col.Access.Read, ActionCreate: col.Access.Create,
				ActionUpdate: col.Access.Update, ActionDelete: col.Access.Delete,
			} {
				if rule == nil || rule.Kind != RuleRoles {
					continue
				}
				for _, role := range rule.Roles {
					if !roleSet[role] {
						add("%s.access.%s: role %q is not declared in auth.roles", cpath, action, role)
					}
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
