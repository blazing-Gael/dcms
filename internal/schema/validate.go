package schema

import (
	"fmt"
	"regexp"
	"strings"
)

// nameRe is the allowed shape for collection and field names: lowercase
// snake_case, starting with a letter. It matches the storage layer's identifier
// allowlist, so anything that validates here is safe to splice into SQL.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// routePlaceholderRe matches a {field} placeholder in a collection route (#10).
var routePlaceholderRe = regexp.MustCompile(`\{([^}]*)\}`)

// RoutePlaceholders returns the field names referenced by a route's {field}
// placeholders, in order. Exported so consumers (an admin panel, an SSG) can
// interpolate a route from a record without re-parsing it.
func RoutePlaceholders(route string) []string {
	m := routePlaceholderRe.FindAllStringSubmatch(route, -1)
	out := make([]string, 0, len(m))
	for _, g := range m {
		out = append(out, g[1])
	}
	return out
}

// reservedCollections are names DCMS uses for internal endpoints/tables.
var reservedCollections = map[string]bool{
	"_schema": true, "_dashboards": true, "_users": true,
	"_roles": true, "_audit": true, "_jobs": true, "_media": true,
	"_revisions": true,
}

// referenceableEngineCollections are engine-managed collections a user schema may
// point a relation at (by name) even though it cannot declare them. _users is the
// identity table, so a collection can model "belongs to a user" (an author byline,
// an owner, an assignee) with real referential integrity and use it in an
// `owner_field` access rule (issue #7). Expansion of a _users target is already
// redacted at the serialization choke point (redactSecrets), so no password hash
// leaks through the relation.
var referenceableEngineCollections = map[string]bool{
	UsersCollection: true,
}

// reservedFields are engine-managed columns; a schema must not declare them.
// They are added automatically during compilation (see translate.go). The
// leading-underscore lifecycle columns are added only when a collection opts into
// the matching directive (ADR-0012), but are reserved unconditionally.
var reservedFields = map[string]bool{
	"id": true, "created_at": true, "updated_at": true,
	"created_by": true, "updated_by": true,
	LifecycleStatus: true, LifecyclePublishedAt: true, LifecycleDeletedAt: true,
	ConcurrencyVersion: true,
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
	// A per-role session TTL keyed by an undeclared role would silently never apply
	// (issue #33) — the exact silent-failure class strict keys guards against — so a
	// typo is a compile error.
	for role := range s.Auth.Session.Roles {
		if !roleSet[role] {
			add("auth.session.roles.%s: role %q is not declared in auth.roles", role, role)
		}
	}

	seenCol := make(map[string]bool)
	for _, col := range s.Collections {
		cpath := "collections." + col.Name

		switch {
		case col.Name == MediaCollection:
			// _media is engine-managed, but a schema may name it to attach an
			// `access:` block — the only way to make the media library anything
			// other than the default public-read (ADR-0011/0016). Its shape and
			// behaviour stay the engine's, so every other directive is refused
			// rather than silently dropped (ADR-0001).
			//
			// A YAML `_media` is already checked by PRESENCE at parse time
			// (toCollection), so `timestamps: false` / `fields: {}` are rejected
			// there before decode. This is the value-based backstop for a
			// directly-constructed CollectionDef (the parser is bypassed): it lists
			// every directive except Name and Access, and a new one added to
			// CollectionDef must be added here too — TestMedia_RejectsEveryNonAccessDirective
			// reflects over the struct to fail the build if one isn't.
			if len(col.Fields) > 0 || len(col.Indexes) > 0 || col.Timestamps ||
				col.Publishing || col.SoftDelete || col.Revisions || col.RevisionsMax != 0 || col.Events || col.Concurrency || col.Route != "" {
				add("%s: the media library is engine-managed — it accepts an `access:` block only", cpath)
			}
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

			// Field-level access roles must be declared too (ADR-0016 M2).
			if f.Access != nil {
				for dir, rule := range map[string]*Rule{"read": f.Access.Read, "write": f.Access.Write} {
					if rule == nil {
						continue
					}
					for _, role := range rule.roleNames() {
						if !roleSet[role] {
							add("%s.access.%s: role %q is not declared in auth.roles", fpath, dir, role)
						}
					}
					for _, msg := range validateOwnerFields(*rule, col) {
						add("%s.access.%s: %s", fpath, dir, msg)
					}
				}
				// Value-scoped write rules (issue #27): only on an enum field; each
				// rule's roles must be declared and its from/to values must be
				// declared enum values.
				if len(f.Access.WriteTransitions) > 0 {
					if f.Type != TypeEnum {
						add("%s.access.write: value-scoped write rules (from/to) are only valid on an enum field", fpath)
					}
					allowed := make(map[string]bool, len(f.Values))
					for _, v := range f.Values {
						allowed[v] = true
					}
					for i, tr := range f.Access.WriteTransitions {
						rpath := fmt.Sprintf("%s.access.write.rules[%d]", fpath, i)
						for _, role := range tr.Who.roleNames() {
							if !roleSet[role] {
								add("%s.who: role %q is not declared in auth.roles", rpath, role)
							}
						}
						for _, msg := range validateOwnerFields(tr.Who, col) {
							add("%s.who: %s", rpath, msg)
						}
						if f.Type == TypeEnum {
							for _, v := range tr.From {
								if !allowed[v] {
									add("%s.from: %q is not a declared value of this enum", rpath, v)
								}
							}
							for _, v := range tr.To {
								if !allowed[v] {
									add("%s.to: %q is not a declared value of this enum", rpath, v)
								}
							}
						}
					}
				}
			}

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
			case f.Type == TypeObjectList:
				// A repeatable group of fields (issue #6). Its element shape is
				// validated just below.
			case f.Type == TypeDecimal:
				// Exact fixed-point (ADR-0017). Scale + default validated just below.
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
				case !allCols[f.Target] && !referenceableEngineCollections[f.Target]:
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

			// Decimal scale + default (ADR-0017). Scale is bounded so int64 minor
			// units keep a large whole-number range; a default is a decimal string
			// that must parse to the declared scale (it becomes the column's integer
			// SQL DEFAULT at migration time).
			if f.Type == TypeDecimal {
				scale := f.DecimalScale()
				if scale < 0 || scale > MaxDecimalScale {
					add("%s: scale must be between 0 and %d", fpath, MaxDecimalScale)
				} else if f.Default != nil {
					s, ok := f.Default.(string)
					if !ok {
						add("%s: default must be a decimal string, e.g. \"12.50\"", fpath)
					} else if _, err := ParseDecimal(s, scale); err != nil {
						add("%s: default %v", fpath, err)
					}
				}
			}

			// Rich content allowlists (ADR-0014): marks/blocks must be known.
			if f.Type == TypeRichText {
				for _, msg := range f.validateRichTextConfig() {
					add("%s: %s", fpath, msg)
				}
			}

			// Object-list element shape (issue #6). One level deep: an element is an
			// ordinary field set, but may not itself nest an object_list or richtext,
			// nor hold a many-to-many relation (a group of small records shouldn't own
			// a join table). Inner relation/file targets are resolved like any other.
			if f.Type == TypeObjectList {
				if len(f.Of) == 0 {
					add("%s: object_list requires a non-empty 'of' shape", fpath)
				}
				if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
					add("%s: min (%s) may not exceed max (%s)", fpath, trimNum(*f.Min), trimNum(*f.Max))
				}
				seenInner := make(map[string]bool)
				for _, inner := range f.Of {
					ipath := fpath + ".of." + inner.Name
					if !nameRe.MatchString(inner.Name) {
						add("%s: invalid field name (must be lowercase snake_case, starting with a letter)", ipath)
					}
					if inner.Name == objectListKey {
						add("%s: %q is reserved (added automatically to each element)", ipath, objectListKey)
					}
					if seenInner[inner.Name] {
						add("%s: duplicate field name", ipath)
					}
					seenInner[inner.Name] = true

					switch {
					case inner.Type == "":
						add("%s: missing type", ipath)
					case inner.Type == TypeObjectList:
						add("%s: an object_list may not contain another object_list (nesting is one level deep)", ipath)
					case inner.Type == TypeRichText:
						add("%s: an object_list element may not contain richtext", ipath)
					case (inner.Type == TypeRelation || inner.Type == TypeFile) && inner.Many:
						// A many relation is a join table; an element stored as JSON has
						// none, so a gallery/m2m inside an element is not representable.
						add("%s: an object_list element may not hold a many-to-many relation (a %q with many: true)", ipath, inner.Type)
					case inner.Type == TypeRelation:
						switch {
						case inner.Target == "":
							add("%s: relation requires a 'target' collection", ipath)
						case !allCols[inner.Target] && !referenceableEngineCollections[inner.Target]:
							add("%s: relation target %q is not a declared collection", ipath, inner.Target)
						}
					case inner.Type == TypeFile:
						if inner.Target != "" {
							add("%s: a file field targets the built-in media library implicitly; do not set 'target'", ipath)
						}
					case inner.Type == TypeEnum:
						if len(inner.Values) == 0 {
							add("%s: enum requires a non-empty values list", ipath)
						}
					case phase1Types[inner.Type], inner.Type == TypeDecimal:
						// ok
					default:
						add("%s: unsupported type %q inside an object_list", ipath, inner.Type)
					}
					if inner.Pattern != "" {
						if _, err := regexp.Compile(inner.Pattern); err != nil {
							add("%s: invalid pattern: %v", ipath, err)
						}
					}
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
				ActionPreview: col.Access.Preview,
				ActionPublish: col.Access.Publish,
			} {
				if rule == nil {
					continue
				}
				for _, role := range rule.roleNames() {
					if !roleSet[role] {
						add("%s.access.%s: role %q is not declared in auth.roles", cpath, action, role)
					}
				}
				for _, msg := range validateOwnerFields(*rule, col) {
					add("%s.access.%s: %s", cpath, action, msg)
				}
				// `inherit` (issue #30) is meaningful only as the _media read rule.
				if rule.mentionsInherit() && !(col.Name == MediaCollection && action == ActionRead && rule.Kind == RuleInherit) {
					add("%s.access.%s: `inherit` is only valid as the read rule of the _media library", cpath, action)
				}
			}
			// preview (ADR-0023) gates hidden lifecycle states, so it needs hidden
			// states to gate, and a `public` preview would show every draft to
			// everyone — defeating the read-vs-preview split.
			if p := col.Access.Preview; p != nil {
				if !col.Publishing && !col.SoftDelete {
					add("%s.access.preview: only valid on a collection with publishing or soft_delete (no hidden states to gate otherwise)", cpath)
				}
				if p.mentionsPublic() {
					add("%s.access.preview: `public` is not allowed — a public preview would expose every hidden record to everyone", cpath)
				}
			}
			// publish (#23) gates publish/unpublish/archive — it needs the
			// publishing state machine to gate, and a `public` publish rule (anyone
			// may go live) defeats the point.
			if pub := col.Access.Publish; pub != nil {
				if !col.Publishing {
					add("%s.access.publish: only valid on a collection with publishing (no go-live transitions to gate otherwise)", cpath)
				}
				if pub.mentionsPublic() {
					add("%s.access.publish: `public` is not allowed — anyone could publish", cpath)
				}
			}
		}

		// Revision retention (issue #25): a negative cap is meaningless, and a cap
		// without revisions has nothing to prune (a typo worth catching).
		if col.RevisionsMax < 0 {
			add("%s.revisions.max: must be zero (unbounded) or positive", cpath)
		}
		if col.RevisionsMax > 0 && !col.Revisions {
			add("%s.revisions.max: only valid when revisions are enabled", cpath)
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

		// route (#10): a path template whose {field} placeholders must name real
		// columns, so a consumer can always interpolate it from a record. Date /
		// format placeholders (`{published_at:2006}`) are a deliberate later add.
		if col.Route != "" {
			if !strings.HasPrefix(col.Route, "/") {
				add("%s.route: must start with '/'", cpath)
			}
			for _, ph := range RoutePlaceholders(col.Route) {
				switch {
				case strings.ContainsRune(ph, ':'):
					add("%s.route: date/format placeholders (`{%s}`) are not supported yet", cpath, ph)
				case ph == "":
					add("%s.route: empty `{}` placeholder", cpath)
				case !known[ph]:
					add("%s.route: `{%s}` is not a field of this collection", cpath, ph)
				}
			}
		}
	}

	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}
