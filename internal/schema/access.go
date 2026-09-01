package schema

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// AccessAction is one of the four operations an access rule can gate. Read
// covers both list and get-one (the gateway applies the record-scoped variant
// for `owner`); the write actions map one-to-one to the CRUD/transition routes.
type AccessAction string

const (
	ActionRead   AccessAction = "read"
	ActionCreate AccessAction = "create"
	ActionUpdate AccessAction = "update"
	ActionDelete AccessAction = "delete"
)

// RuleKind is the parsed shape of a single access rule value (ADR-0016).
type RuleKind string

const (
	RulePublic        RuleKind = "public"        // no authentication required
	RuleAuthenticated RuleKind = "authenticated" // any valid principal
	RuleRoles         RuleKind = "roles"         // principal holds one of Roles
	RuleOwner         RuleKind = "owner"         // principal is the record's created_by
	RuleAny           RuleKind = "any"           // satisfied if ANY sub-rule is (logical OR)
)

// Rule is a single access rule. For RuleRoles, Roles lists the accepted role
// names; for RuleAny, Any lists the OR-combined sub-rules (e.g. `[admin] OR
// owner`); for the other kinds both are empty.
//
// A composite (RuleAny) lets one rule mix gates that resolve differently per
// caller: `any: [admin, owner]` grants admins full access while narrowing
// everyone else to the rows they own. The evaluator ORs the sub-decisions
// (allow beats owner-scope beats deny), so the enforcement layer needs no new
// cases — an admin resolves to plain allow, an owner to owner-scope.
type Rule struct {
	Kind  RuleKind `json:"kind"`
	Roles []string `json:"roles,omitempty"`
	Any   []Rule   `json:"any,omitempty"`
}

// roleNames returns every role name referenced anywhere in the rule tree, so
// validation can check them against the declared roles regardless of nesting.
func (r Rule) roleNames() []string {
	if r.Kind == RuleRoles {
		return r.Roles
	}
	var out []string
	for _, sub := range r.Any {
		out = append(out, sub.roleNames()...)
	}
	return out
}

// MentionsOwner reports whether the rule tree contains an `owner` gate. Callers
// use it to decide whether an owner comparison (and thus the record it needs)
// can come into play — a composite hides its `owner` sub-rule behind RuleAny.
func (r Rule) MentionsOwner() bool {
	if r.Kind == RuleOwner {
		return true
	}
	for _, sub := range r.Any {
		if sub.MentionsOwner() {
			return true
		}
	}
	return false
}

// AccessRules is a collection's per-operation authorization policy. A nil field
// means the action was not declared and the gateway's default policy applies
// (reads → public, writes → authenticated; see ADR-0016 §4).
type AccessRules struct {
	Read   *Rule `json:"read,omitempty"`
	Create *Rule `json:"create,omitempty"`
	Update *Rule `json:"update,omitempty"`
	Delete *Rule `json:"delete,omitempty"`
}

// defaultRule is the effective policy for an action with no explicit rule.
func defaultRule(action AccessAction) Rule {
	if action == ActionRead {
		return Rule{Kind: RulePublic}
	}
	return Rule{Kind: RuleAuthenticated}
}

// AccessRule returns the effective rule for an action: the declared rule if the
// collection set one, otherwise the engine default. This is the single place the
// gateway and the contract generators agree on policy.
func (c CollectionDef) AccessRule(action AccessAction) Rule {
	if c.Access != nil {
		var r *Rule
		switch action {
		case ActionRead:
			r = c.Access.Read
		case ActionCreate:
			r = c.Access.Create
		case ActionUpdate:
			r = c.Access.Update
		case ActionDelete:
			r = c.Access.Delete
		}
		if r != nil {
			return *r
		}
	}
	return defaultRule(action)
}

// FieldAccess is a field's per-direction policy (ADR-0016 milestone 2). A nil
// direction means unrestricted: the collection-level rule already gated the
// operation, and the field adds nothing on top.
//
// Read is a *masking* rule, not a gate — an unauthorized reader still gets the
// record, just without the field. Write is likewise a filter: an unauthorized
// writer's value for the field is dropped, not rejected, so a client that
// round-trips a record it fetched does not fail on a field it never saw.
type FieldAccess struct {
	Read  *Rule `json:"read,omitempty"`
	Write *Rule `json:"write,omitempty"`
}

// ReadRule returns the effective read rule for a field (public when undeclared).
func (f FieldDef) ReadRule() Rule {
	if f.Access != nil && f.Access.Read != nil {
		return *f.Access.Read
	}
	return Rule{Kind: RulePublic}
}

// WriteRule returns the effective write rule for a field (public when
// undeclared — the collection's create/update rule is the real gate).
func (f FieldDef) WriteRule() Rule {
	if f.Access != nil && f.Access.Write != nil {
		return *f.Access.Write
	}
	return Rule{Kind: RulePublic}
}

// HasFieldAccess reports whether any field on the collection declares a rule.
// The gateway uses this to skip field masking entirely on the common case.
func (c CollectionDef) HasFieldAccess() bool {
	for _, f := range c.Fields {
		if f.Access != nil && (f.Access.Read != nil || f.Access.Write != nil) {
			return true
		}
	}
	return false
}

// parseFieldAccess decodes a field's `access:` mapping. Only read/write are
// valid keys — the CRUD actions belong to the collection, and silently accepting
// `create:` here would suggest a gate that does not exist.
func parseFieldAccess(node *yaml.Node) (*FieldAccess, error) {
	entries, err := mappingEntries(node)
	if err != nil {
		return nil, err
	}
	fa := &FieldAccess{}
	for _, e := range entries {
		rule, err := parseRule(e.Val)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Key, err)
		}
		switch e.Key {
		case "read":
			fa.Read = rule
		case "write":
			fa.Write = rule
		default:
			return nil, fmt.Errorf("unknown field access key %q (want read or write)", e.Key)
		}
	}
	return fa, nil
}

// AuthConfig is the top-level `auth:` block. M1 uses Provider (defaulting to
// "local"), the declared Roles, and Session. The jwt:/oidc: sub-blocks reserved
// in SCHEMA_SPEC are for the external-identity milestone and are ignored here.
type AuthConfig struct {
	Provider string        `json:"provider,omitempty"` // local | oidc | both (default local)
	Roles    []RoleDef     `json:"roles,omitempty"`
	Session  SessionConfig `json:"session,omitempty"`
}

// RoleDef declares a role name and its admin-UI label.
type RoleDef struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
}

// SessionConfig tunes opaque local sessions (ADR-0016). TTL is a Go duration
// string (e.g. "168h"); empty uses the engine default. Parsed at runtime, not
// schema-compile time, so a bad value is a config error, not a schema error.
type SessionConfig struct {
	TTL string `json:"ttl,omitempty"`
}

// HasRole reports whether name is a declared role.
func (s *SchemaDefinition) HasRole(name string) bool {
	for _, r := range s.Auth.Roles {
		if r.Name == name {
			return true
		}
	}
	return false
}

// ── parsing ──────────────────────────────────────────────────────────────────

// parseAccess decodes a collection's `access:` mapping into AccessRules. Unknown
// action keys are a hard error (a typo like `writes:` must not silently open a
// collection). Each value is parsed by parseRule.
func parseAccess(node *yaml.Node) (*AccessRules, error) {
	entries, err := mappingEntries(node)
	if err != nil {
		return nil, err
	}
	ar := &AccessRules{}
	for _, e := range entries {
		rule, err := parseRule(e.Val)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Key, err)
		}
		switch AccessAction(e.Key) {
		case ActionRead:
			ar.Read = rule
		case ActionCreate:
			ar.Create = rule
		case ActionUpdate:
			ar.Update = rule
		case ActionDelete:
			ar.Delete = rule
		default:
			return nil, fmt.Errorf("unknown access action %q (want read, create, update, or delete)", e.Key)
		}
	}
	return ar, nil
}

// parseRule decodes one access rule value. A scalar is a keyword
// (public/authenticated/owner) or a single role name; a list is a set of role
// names; a mapping with an `any:` key is a composite OR of sub-rules. This
// mirrors the field shorthand (scalar-or-mapping) style.
func parseRule(node *yaml.Node) (*Rule, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		switch RuleKind(node.Value) {
		case RulePublic:
			return &Rule{Kind: RulePublic}, nil
		case RuleAuthenticated:
			return &Rule{Kind: RuleAuthenticated}, nil
		case RuleOwner:
			return &Rule{Kind: RuleOwner}, nil
		default:
			// A bare scalar that isn't a keyword is a single role name.
			if node.Value == "" {
				return nil, fmt.Errorf("empty access rule")
			}
			return &Rule{Kind: RuleRoles, Roles: []string{node.Value}}, nil
		}
	case yaml.SequenceNode:
		var roles []string
		if err := node.Decode(&roles); err != nil {
			return nil, fmt.Errorf("expected a list of role names: %w", err)
		}
		if len(roles) == 0 {
			return nil, fmt.Errorf("empty role list")
		}
		return &Rule{Kind: RuleRoles, Roles: roles}, nil
	case yaml.MappingNode:
		return parseCompositeRule(node)
	default:
		return nil, fmt.Errorf("expected a keyword, a role name, a list of roles, or an {any: [...]} composite, got %s", kindName(node.Kind))
	}
}

// parseCompositeRule decodes a mapping-form rule. The only supported key is
// `any:` (logical OR); its value is a sequence of rules, each parsed recursively
// so a sub-rule may itself be a keyword, a role, a role list, or a nested
// composite. An `all:` (AND) counterpart is intentionally not offered yet — no
// use case has needed it, and adding it later is additive.
func parseCompositeRule(node *yaml.Node) (*Rule, error) {
	entries, err := mappingEntries(node)
	if err != nil {
		return nil, err
	}
	if len(entries) != 1 || entries[0].Key != "any" {
		return nil, fmt.Errorf("a composite access rule takes exactly one key, `any`")
	}
	seq := entries[0].Val
	if seq.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("any: expected a list of rules, got %s", kindName(seq.Kind))
	}
	subs := make([]Rule, 0, len(seq.Content))
	for _, item := range seq.Content {
		sub, err := parseRule(item)
		if err != nil {
			return nil, fmt.Errorf("any: %w", err)
		}
		subs = append(subs, *sub)
	}
	if len(subs) < 2 {
		return nil, fmt.Errorf("any: needs at least two rules to combine")
	}
	return &Rule{Kind: RuleAny, Any: subs}, nil
}

// parseAuth decodes the top-level `auth:` block. provider/session are scalars;
// roles is an ordered mapping (label per role). jwt/oidc are recognised but
// skipped in M1 (external-identity milestone).
func parseAuth(node *yaml.Node) (AuthConfig, error) {
	var cfg AuthConfig
	entries, err := mappingEntries(node)
	if err != nil {
		return cfg, err
	}
	for _, e := range entries {
		switch e.Key {
		case "provider":
			cfg.Provider = e.Val.Value
		case "roles":
			roles, err := parseRoles(e.Val)
			if err != nil {
				return cfg, fmt.Errorf("roles: %w", err)
			}
			cfg.Roles = roles
		case "session":
			if err := e.Val.Decode(&cfg.Session); err != nil {
				return cfg, fmt.Errorf("session: %w", err)
			}
		default:
			// jwt / oidc — recognised, deferred to the external-identity milestone.
		}
	}
	return cfg, nil
}

// parseRoles decodes the `roles:` mapping, preserving document order so codegen
// stays deterministic. Each value may be null/scalar or a mapping with `label`.
func parseRoles(node *yaml.Node) ([]RoleDef, error) {
	entries, err := mappingEntries(node)
	if err != nil {
		return nil, err
	}
	roles := make([]RoleDef, 0, len(entries))
	for _, e := range entries {
		rd := RoleDef{Name: e.Key}
		if e.Val.Kind == yaml.MappingNode {
			var body struct {
				Label string `yaml:"label"`
			}
			if err := e.Val.Decode(&body); err != nil {
				return nil, fmt.Errorf("%s: %w", e.Key, err)
			}
			rd.Label = body.Label
		}
		roles = append(roles, rd)
	}
	return roles, nil
}
