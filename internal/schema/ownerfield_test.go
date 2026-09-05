package schema

import (
	"strings"
	"testing"
)

// owner_field (issue #7): an access rule can scope to a named relation instead of
// created_by, so a row owned by someone who didn't create it (an entitlement
// written by a service, a byline pointing at an author) is still reachable.

func TestOwnerField_ParsesScalarAndComposite(t *testing.T) {
	def, err := Parse([]byte(`
version: "1"
auth:
  roles:
    admin: { label: Administrator }
    service: { label: Service }
collections:
  subscriptions:
    fields:
      user: { type: relation, target: _users }
      level: { type: string }
    access:
      read: { owner_field: user }
      create: [service, admin]
      update:
        any: [admin, { owner_field: user }]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var subs *CollectionDef
	for i := range def.Collections {
		if def.Collections[i].Name == "subscriptions" {
			subs = &def.Collections[i]
		}
	}
	if subs == nil {
		t.Fatal("subscriptions not parsed")
	}
	if got := subs.AccessRule(ActionRead); got.Kind != RuleOwnerField || got.Field != "user" {
		t.Errorf("read: %+v", got)
	}
	upd := subs.AccessRule(ActionUpdate)
	if upd.Kind != RuleAny || len(upd.Any) != 2 {
		t.Fatalf("update composite: %+v", upd)
	}
	if upd.Any[1].Kind != RuleOwnerField || upd.Any[1].Field != "user" {
		t.Errorf("update owner_field branch: %+v", upd.Any[1])
	}
	if !upd.MentionsOwner() {
		t.Error("a composite containing owner_field should MentionsOwner")
	}
}

// A relation may point at the engine's _users table so ownership models a real
// user with referential integrity.
func TestOwnerField_RelationToUsersAllowed(t *testing.T) {
	if _, err := Parse([]byte(`
version: "1"
collections:
  entitlements:
    fields:
      user: { type: relation, target: _users }
`)); err != nil {
		t.Fatalf("relation to _users should be allowed: %v", err)
	}
}

func TestOwnerField_ValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "missing field",
			src: `
version: "1"
collections:
  subs:
    fields:
      level: { type: string }
    access:
      read: { owner_field: user }
`,
			want: `owner_field "user" is not a field`,
		},
		{
			name: "not a relation",
			src: `
version: "1"
collections:
  subs:
    fields:
      user: { type: string }
    access:
      read: { owner_field: user }
`,
			want: `must be a relation`,
		},
		{
			name: "many-to-many",
			src: `
version: "1"
collections:
  subs:
    fields:
      user: { type: relation, target: _users, many: true }
    access:
      read: { owner_field: user }
`,
			want: `cannot be a many-to-many`,
		},
		{
			name: "two owner scopes",
			src: `
version: "1"
collections:
  subs:
    fields:
      user: { type: relation, target: _users }
    access:
      read:
        any: [owner, { owner_field: user }]
`,
			want: `at most one owner scope`,
		},
		{
			name: "field-level owner_field bad target",
			src: `
version: "1"
collections:
  subs:
    fields:
      user: { type: relation, target: _users }
      secret: { type: string, access: { read: { owner_field: nope } } }
`,
			want: `owner_field "nope" is not a field`,
		},
		{
			name: "empty owner_field",
			src: `
version: "1"
collections:
  subs:
    fields:
      user: { type: relation, target: _users }
    access:
      read: { owner_field: "" }
`,
			want: `expected a field name`,
		},
		{
			name: "unknown mapping key",
			src: `
version: "1"
collections:
  subs:
    fields:
      user: { type: relation, target: _users }
    access:
      read: { owner: user }
`,
			want: `want ` + "`any` or `owner_field`",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.want)
			}
		})
	}
}
