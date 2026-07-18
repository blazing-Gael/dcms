package schema

import (
	"strings"
	"testing"
)

func TestAccess_ParsesRuleForms(t *testing.T) {
	def, err := Parse([]byte(`
version: "1"
auth:
  roles:
    admin: { label: Administrator }
    editor: { label: Editor }
collections:
  posts:
    fields:
      title: string
    access:
      read: public
      create: [admin, editor]
      update: owner
      delete: authenticated
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var posts *CollectionDef
	for i := range def.Collections {
		if def.Collections[i].Name == "posts" {
			posts = &def.Collections[i]
		}
	}
	if posts == nil || posts.Access == nil {
		t.Fatal("posts.access not parsed")
	}
	if got := posts.AccessRule(ActionRead); got.Kind != RulePublic {
		t.Errorf("read: %+v", got)
	}
	if got := posts.AccessRule(ActionCreate); got.Kind != RuleRoles || len(got.Roles) != 2 {
		t.Errorf("create: %+v", got)
	}
	if got := posts.AccessRule(ActionUpdate); got.Kind != RuleOwner {
		t.Errorf("update: %+v", got)
	}
	if got := posts.AccessRule(ActionDelete); got.Kind != RuleAuthenticated {
		t.Errorf("delete: %+v", got)
	}
}

func TestAccess_DefaultPolicyWhenOmitted(t *testing.T) {
	def, err := Parse([]byte(`
version: "1"
collections:
  posts:
    fields:
      title: string
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	posts := def.Collections[0]
	if got := posts.AccessRule(ActionRead); got.Kind != RulePublic {
		t.Errorf("default read should be public, got %+v", got)
	}
	for _, a := range []AccessAction{ActionCreate, ActionUpdate, ActionDelete} {
		if got := posts.AccessRule(a); got.Kind != RuleAuthenticated {
			t.Errorf("default %s should be authenticated, got %+v", a, got)
		}
	}
}

func TestAccess_UndeclaredRoleIsError(t *testing.T) {
	_, err := Parse([]byte(`
version: "1"
auth:
  roles:
    admin: { label: Administrator }
collections:
  posts:
    fields:
      title: string
    access:
      create: [admin, ghost]
`))
	if err == nil {
		t.Fatal("expected an error for the undeclared role 'ghost'")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should name the offending role, got: %v", err)
	}
}

func TestAccess_UnknownActionIsError(t *testing.T) {
	_, err := Parse([]byte(`
version: "1"
collections:
  posts:
    fields:
      title: string
    access:
      writes: public
`))
	if err == nil || !strings.Contains(err.Error(), "unknown access action") {
		t.Fatalf("expected unknown-action error, got: %v", err)
	}
}

func TestAuth_IdentityCollectionsInjected(t *testing.T) {
	def, err := Parse([]byte(`
version: "1"
collections:
  posts:
    fields:
      title: string
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]bool{}
	for _, c := range def.Collections {
		got[c.Name] = true
	}
	if !got[UsersCollection] || !got[SessionsCollection] {
		t.Fatalf("identity collections not injected: %v", got)
	}
}
