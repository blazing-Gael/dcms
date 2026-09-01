package schema

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExampleSchemasCompile parses and validates every shipped example schema.
// The examples are documentation people copy from, so a stale or malformed one
// is a real defect — this keeps them honest as the schema language grows.
func TestExampleSchemasCompile(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.schema.yaml"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no example schemas found")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			_, err = Parse(src) // Parse validates internally
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
		})
	}
}

// TestFarmlyAccessWiring pins the access policy the farmly example demonstrates.
// It is the reference schema for the auth model (ADR-0016): if a rename or a
// parser change silently drops a rule, a public storefront collection could go
// private — or worse, a PII collection could go public.
func TestFarmlyAccessWiring(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "examples", "farmly.schema.yaml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	def, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, role := range []string{"admin", "vendor", "customer"} {
		if !def.HasRole(role) {
			t.Errorf("role %q not declared", role)
		}
	}
	if def.Auth.Provider != "local" {
		t.Errorf("auth.provider = %q, want local", def.Auth.Provider)
	}
	if def.Auth.Session.TTL != "168h" {
		t.Errorf("auth.session.ttl = %q, want 168h", def.Auth.Session.TTL)
	}

	byName := make(map[string]CollectionDef, len(def.Collections))
	for _, c := range def.Collections {
		byName[c.Name] = c
	}

	cases := []struct {
		collection string
		action     AccessAction
		want       Rule
	}{
		{"products", ActionRead, Rule{Kind: RulePublic}},
		{"products", ActionCreate, Rule{Kind: RuleRoles, Roles: []string{"admin", "vendor"}}},
		{"products", ActionDelete, Rule{Kind: RuleRoles, Roles: []string{"admin"}}},
		{"customers", ActionRead, Rule{Kind: RuleRoles, Roles: []string{"admin"}}},
		{"customers", ActionCreate, Rule{Kind: RulePublic}},
		{"orders", ActionRead, Rule{Kind: RuleAny}},
		{"orders", ActionCreate, Rule{Kind: RuleAuthenticated}},
		{"reviews", ActionCreate, Rule{Kind: RuleAuthenticated}},
		// order_items declares no separate update rule beyond admin; stories
		// mirrors products. Spot-check the engine default still applies where
		// nothing is declared.
		{"order_items", ActionUpdate, Rule{Kind: RuleRoles, Roles: []string{"admin"}}},
	}
	for _, tc := range cases {
		col, ok := byName[tc.collection]
		if !ok {
			t.Fatalf("collection %q missing", tc.collection)
		}
		got := col.AccessRule(tc.action)
		if got.Kind != tc.want.Kind || !equalStrings(got.Roles, tc.want.Roles) {
			t.Errorf("%s.%s = %+v, want %+v", tc.collection, tc.action, got, tc.want)
		}
	}

	// Composite rules (ADR-0016): orders.read and customers.update are both
	// `any: [admin, owner]` — admins see/edit everything, customers are scoped to
	// their own rows. A regression to a bare `owner` here would hide records from
	// admins again (the exact gap composite rules closed).
	for _, tc := range []struct {
		collection string
		action     AccessAction
	}{
		{"orders", ActionRead},
		{"customers", ActionUpdate},
	} {
		got := byName[tc.collection].AccessRule(tc.action)
		if got.Kind != RuleAny || len(got.Any) != 2 {
			t.Fatalf("%s.%s: want composite of 2, got %+v", tc.collection, tc.action, got)
		}
		if got.Any[0].Kind != RuleRoles || !equalStrings(got.Any[0].Roles, []string{"admin"}) {
			t.Errorf("%s.%s any[0]: want roles[admin], got %+v", tc.collection, tc.action, got.Any[0])
		}
		if got.Any[1].Kind != RuleOwner {
			t.Errorf("%s.%s any[1]: want owner, got %+v", tc.collection, tc.action, got.Any[1])
		}
	}

	// Field-level access (ADR-0016 M2): the reference schema demonstrates a read
	// mask (orders.payment_reference → admin) and two write masks (reviews.status
	// and reviews.is_verified_purchase → admin). A dropped field rule here would
	// leak a payment id or let a customer self-approve their own review.
	field := func(collection, name string) FieldDef {
		t.Helper()
		for _, f := range byName[collection].Fields {
			if f.Name == name {
				return f
			}
		}
		t.Fatalf("%s.%s missing", collection, name)
		return FieldDef{}
	}
	if got := field("orders", "payment_reference").ReadRule(); got.Kind != RuleRoles || !equalStrings(got.Roles, []string{"admin"}) {
		t.Errorf("orders.payment_reference read rule = %+v, want roles[admin]", got)
	}
	if got := field("reviews", "status").WriteRule(); got.Kind != RuleRoles || !equalStrings(got.Roles, []string{"admin"}) {
		t.Errorf("reviews.status write rule = %+v, want roles[admin]", got)
	}
	if got := field("reviews", "is_verified_purchase").WriteRule(); got.Kind != RuleRoles || !equalStrings(got.Roles, []string{"admin"}) {
		t.Errorf("reviews.is_verified_purchase write rule = %+v, want roles[admin]", got)
	}
	if !byName["reviews"].HasFieldAccess() {
		t.Error("reviews should report HasFieldAccess")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
