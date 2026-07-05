package codegen

import (
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/schema"
)

// fixture builds a small schema exercising the type mapping and optionality
// rules: a required-no-default field, a required-with-default field, an
// optional field, and an enum.
func fixture(t *testing.T) *schema.SchemaDefinition {
	t.Helper()
	src := []byte(`
version: "1"
collections:
  products:
    fields:
      title:  { type: string, required: true }
      stock:  { type: integer, required: true, default: 0 }
      note:   { type: text }
      status: { type: enum, values: [draft, active], default: draft }
`)
	def, err := schema.Parse(src)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return def
}

func TestTypeScript_Interfaces(t *testing.T) {
	out, err := TypeScript(fixture(t))
	if err != nil {
		t.Fatalf("TypeScript: %v", err)
	}

	wants := []string{
		"export interface Products {",
		"export interface CreateProducts {",
		"export interface UpdateProducts {",
		"readonly id: string;",          // engine-managed, readonly
		"readonly created_at: string;",  // audit column present + readonly
		"readonly created_by?: string;", // audit actor optional
		"status: 'draft' | 'active';",   // enum → string-literal union
		"export function createClient(", // runtime client emitted
		"products: Object.assign(resource<Products, CreateProducts, UpdateProducts>(cfg, \"products\")",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("generated output missing %q", w)
		}
	}
}

func TestTypeScript_Lifecycle(t *testing.T) {
	src := []byte(`
version: "1"
collections:
  articles:
    publishing: true
    soft_delete: true
    fields:
      title: { type: string, required: true }
`)
	def, err := schema.Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := TypeScript(def)
	if err != nil {
		t.Fatalf("TypeScript: %v", err)
	}
	wants := []string{
		"readonly _status: string;",             // managed field in the record type
		"readonly _published_at?: string;",       // nullable managed field, optional
		"readonly _deleted_at?: string;",         // soft-delete marker
		"& Publishable<Articles>",                // client field gains publish methods
		"& SoftDeletable<Articles>",              // ...and restore/purge
		"publishable<Articles>(cfg, \"articles\")",
		"softDeletable<Articles>(cfg, \"articles\")",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("generated output missing %q", w)
		}
	}
}

func TestTypeScript_CreateOptionality(t *testing.T) {
	out, err := TypeScript(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	create := section(t, out, "export interface CreateProducts {")

	// required + no default → required in Create
	if !strings.Contains(create, "title: string;") || strings.Contains(create, "title?:") {
		t.Errorf("title should be required in CreateProducts:\n%s", create)
	}
	// required BUT has a default → optional in Create (server fills it)
	if !strings.Contains(create, "stock?: number;") {
		t.Errorf("stock should be optional in CreateProducts (has default):\n%s", create)
	}
	// enum with default → optional
	if !strings.Contains(create, "status?:") {
		t.Errorf("status should be optional in CreateProducts (has default):\n%s", create)
	}
	// client-supplied id is always optional
	if !strings.Contains(create, "id?: string;") {
		t.Errorf("id should be optional in CreateProducts:\n%s", create)
	}
}

func TestTypeScript_UpdateAllOptional(t *testing.T) {
	out, err := TypeScript(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	update := section(t, out, "export interface UpdateProducts {")
	for _, f := range []string{"title", "stock", "note", "status"} {
		if !strings.Contains(update, f+"?:") {
			t.Errorf("%s should be optional in UpdateProducts:\n%s", f, update)
		}
	}
	// PATCH takes the id from the URL, so it must not appear in the body type.
	if strings.Contains(update, "id?:") || strings.Contains(update, "id:") {
		t.Errorf("UpdateProducts must not contain id:\n%s", update)
	}
}

func TestTypeScript_StampsContractVersion(t *testing.T) {
	def := fixture(t)
	out, err := TypeScript(def)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "CONTRACT_VERSION = \""+def.ContractVersion()+"\"") {
		t.Errorf("output not stamped with contract version %q", def.ContractVersion())
	}
}

func TestTypeScript_RelationTypes(t *testing.T) {
	def, err := schema.Parse([]byte(`
version: "1"
collections:
  users:
    fields:
      name: { type: string, required: true }
  tags:
    fields:
      name: { type: string, required: true }
  posts:
    fields:
      title:  { type: string, required: true }
      author: { type: relation, target: users, required: true }
      tags:   { type: relation, target: tags, many: true }
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := TypeScript(def)
	if err != nil {
		t.Fatalf("TypeScript: %v", err)
	}

	record := section(t, out, "export interface Posts {")
	create := section(t, out, "export interface CreatePosts {")

	// Response: belongs-to is the id OR the expanded object; m2m is a list of
	// expanded objects (present only when expanded, hence optional).
	if !strings.Contains(record, "author: string | Users;") {
		t.Errorf("author in Posts should be 'string | Users':\n%s", record)
	}
	if !strings.Contains(record, "tags?: Tags[];") {
		t.Errorf("tags in Posts should be 'Tags[]' and optional:\n%s", record)
	}

	// Input: belongs-to takes an id or an inline record; m2m takes a list of
	// ids and/or inline records (nested writes).
	if !strings.Contains(create, "author: string | CreateUsers;") || strings.Contains(create, "author?:") {
		t.Errorf("author in CreatePosts should be required 'string | CreateUsers':\n%s", create)
	}
	if !strings.Contains(create, "tags?: (string | CreateTags)[];") {
		t.Errorf("tags in CreatePosts should be optional '(string | CreateTags)[]':\n%s", create)
	}
}

// section returns the substring from a header line up to the closing "}" so a
// test can assert about one interface without matching another's fields.
func section(t *testing.T, out, header string) string {
	t.Helper()
	i := strings.Index(out, header)
	if i < 0 {
		t.Fatalf("header %q not found", header)
	}
	rest := out[i:]
	j := strings.Index(rest, "\n}")
	if j < 0 {
		t.Fatalf("no closing brace after %q", header)
	}
	return rest[:j]
}
