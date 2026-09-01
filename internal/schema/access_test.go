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

func TestFieldAccess_ParsesReadAndWrite(t *testing.T) {
	def, err := Parse([]byte(`
version: "1"
auth:
  roles:
    admin: { label: Administrator }
collections:
  profiles:
    fields:
      name: string
      salary:
        type: number
        access:
          read:  [admin]
          write: [admin]
      bio:
        type: string
        access:
          write: owner
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var profiles *CollectionDef
	for i := range def.Collections {
		if def.Collections[i].Name == "profiles" {
			profiles = &def.Collections[i]
		}
	}
	if profiles == nil {
		t.Fatal("profiles collection missing")
	}
	if !profiles.HasFieldAccess() {
		t.Fatal("HasFieldAccess should be true")
	}
	byName := map[string]FieldDef{}
	for _, f := range profiles.Fields {
		byName[f.Name] = f
	}
	// name: no access → both directions default public.
	if got := byName["name"].ReadRule(); got.Kind != RulePublic {
		t.Errorf("name read: %+v", got)
	}
	if got := byName["name"].WriteRule(); got.Kind != RulePublic {
		t.Errorf("name write: %+v", got)
	}
	// salary: role-gated both ways.
	if got := byName["salary"].ReadRule(); got.Kind != RuleRoles || len(got.Roles) != 1 || got.Roles[0] != "admin" {
		t.Errorf("salary read: %+v", got)
	}
	if got := byName["salary"].WriteRule(); got.Kind != RuleRoles {
		t.Errorf("salary write: %+v", got)
	}
	// bio: write owner, read undeclared → public.
	if got := byName["bio"].ReadRule(); got.Kind != RulePublic {
		t.Errorf("bio read should default public: %+v", got)
	}
	if got := byName["bio"].WriteRule(); got.Kind != RuleOwner {
		t.Errorf("bio write: %+v", got)
	}
}

func TestFieldAccess_HasFieldAccessFalseWhenNoneDeclared(t *testing.T) {
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
	if def.Collections[0].HasFieldAccess() {
		t.Fatal("HasFieldAccess should be false with no field rules")
	}
}

func TestFieldAccess_UndeclaredRoleIsError(t *testing.T) {
	_, err := Parse([]byte(`
version: "1"
auth:
  roles:
    admin: { label: Administrator }
collections:
  profiles:
    fields:
      salary:
        type: number
        access:
          read: [admin, ghost]
`))
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected undeclared-role error naming 'ghost', got: %v", err)
	}
}

func TestFieldAccess_UnknownKeyIsError(t *testing.T) {
	_, err := Parse([]byte(`
version: "1"
collections:
  profiles:
    fields:
      salary:
        type: number
        access:
          create: public
`))
	if err == nil || !strings.Contains(err.Error(), "unknown field access key") {
		t.Fatalf("expected unknown-field-access-key error, got: %v", err)
	}
}

func TestAccess_ParsesCompositeRule(t *testing.T) {
	def, err := Parse([]byte(`
version: "1"
auth:
  roles:
    admin: { label: Administrator }
collections:
  orders:
    fields:
      total: number
    access:
      read:
        any: [admin, owner]
      update:
        any: [admin, owner]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var orders *CollectionDef
	for i := range def.Collections {
		if def.Collections[i].Name == "orders" {
			orders = &def.Collections[i]
		}
	}
	if orders == nil {
		t.Fatal("orders collection not parsed")
	}
	read := orders.AccessRule(ActionRead)
	if read.Kind != RuleAny || len(read.Any) != 2 {
		t.Fatalf("read: expected composite of 2, got %+v", read)
	}
	if read.Any[0].Kind != RuleRoles || len(read.Any[0].Roles) != 1 || read.Any[0].Roles[0] != "admin" {
		t.Errorf("read.any[0]: expected role admin, got %+v", read.Any[0])
	}
	if read.Any[1].Kind != RuleOwner {
		t.Errorf("read.any[1]: expected owner, got %+v", read.Any[1])
	}
	if !read.MentionsOwner() {
		t.Error("composite [admin, owner] should MentionsOwner")
	}
}

func TestAccess_CompositeUndeclaredRoleIsError(t *testing.T) {
	_, err := Parse([]byte(`
version: "1"
auth:
  roles:
    admin: { label: Administrator }
collections:
  orders:
    fields:
      total: number
    access:
      read:
        any: [admin, ghost, owner]
`))
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("expected error naming undeclared role 'ghost', got: %v", err)
	}
}

func TestAccess_CompositeNeedsTwoRules(t *testing.T) {
	_, err := Parse([]byte(`
version: "1"
auth:
  roles:
    admin: { label: Administrator }
collections:
  orders:
    fields:
      total: number
    access:
      read:
        any: [admin]
`))
	if err == nil || !strings.Contains(err.Error(), "at least two") {
		t.Fatalf("expected 'at least two rules' error, got: %v", err)
	}
}

func TestAccess_CompositeUnknownKeyIsError(t *testing.T) {
	_, err := Parse([]byte(`
version: "1"
auth:
  roles:
    admin: { label: Administrator }
collections:
  orders:
    fields:
      total: number
    access:
      read:
        all: [admin, owner]
`))
	if err == nil || !strings.Contains(err.Error(), "any") {
		t.Fatalf("expected composite-key error mentioning 'any', got: %v", err)
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
