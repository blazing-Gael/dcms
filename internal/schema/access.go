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
)

// Rule is a single access rule. For RuleRoles, Roles lists the accepted role
// names; for the other kinds Roles is empty.
type Rule struct {
	Kind  RuleKind `json:"kind"`
	Roles []string `json:"roles,omitempty"`
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
// names. This mirrors the field shorthand (scalar-or-mapping) style.
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
	default:
		return nil, fmt.Errorf("expected a keyword, a role name, or a list of roles, got %s", kindName(node.Kind))
	}
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
