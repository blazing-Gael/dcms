package schema

import (
	"strings"
	"testing"
)

const validSchema = `
version: "1"
meta:
  name: shop
  base_url: /api/v1
collections:
  products:
    fields:
      title: string
      slug:
        type: string
        required: true
        unique: true
        max: 200
      price:
        type: number
        required: true
      status:
        type: enum
        values: [draft, active, archived]
        default: draft
    timestamps: true
    indexes:
      - status
      - [status, price]
    # Phase 2+ directives below must be ignored, not error:
    access:
      read: public
    vectorize: [title]
`

func TestParse_ShorthandAndFullForm(t *testing.T) {
	def, err := Parse([]byte(validSchema))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if def.Version != "1" || def.Meta.Name != "shop" || def.Meta.BaseURL != "/api/v1" {
		t.Fatalf("meta not parsed: %#v", def.Meta)
	}
	if len(def.Collections) != 1 {
		t.Fatalf("collections: got %d, want 1", len(def.Collections))
	}
	col := def.Collections[0]
	if col.Name != "products" || len(col.Fields) != 4 {
		t.Fatalf("collection: %#v", col)
	}

	// Field order preserved; shorthand and full form both parsed.
	if col.Fields[0].Name != "title" || col.Fields[0].Type != TypeString {
		t.Fatalf("shorthand field wrong: %#v", col.Fields[0])
	}
	slug := col.Fields[1]
	if slug.Name != "slug" || !slug.Required || !slug.Unique {
		t.Fatalf("full-form field wrong: %#v", slug)
	}
	if slug.Max == nil || *slug.Max != 200 {
		t.Fatalf("max not parsed as number: %#v", slug.Max)
	}
	status := col.Fields[3]
	if status.Type != TypeEnum || len(status.Values) != 3 || status.Default != "draft" {
		t.Fatalf("enum field wrong: %#v", status)
	}
}

func TestParse_IndexesSingleAndComposite(t *testing.T) {
	def, err := Parse([]byte(validSchema))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	idx := def.Collections[0].Indexes
	if len(idx) != 2 {
		t.Fatalf("indexes: got %d, want 2", len(idx))
	}
	if len(idx[0].Columns) != 1 || idx[0].Columns[0] != "status" {
		t.Fatalf("single index wrong: %#v", idx[0])
	}
	if len(idx[1].Columns) != 2 {
		t.Fatalf("composite index wrong: %#v", idx[1])
	}
}

// parseExpectIssue parses a schema expected to fail validation and asserts the
// error message contains substr.
func parseExpectIssue(t *testing.T, src, substr string) {
	t.Helper()
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatalf("expected validation error containing %q, got nil", substr)
	}
	if !strings.Contains(err.Error(), substr) {
		t.Fatalf("error %q does not contain %q", err.Error(), substr)
	}
}

func TestValidate_Failures(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			name: "reserved field name",
			src: `version: "1"
collections:
  products:
    fields:
      id: string`,
			want: "reserved",
		},
		{
			name: "empty enum",
			src: `version: "1"
collections:
  products:
    fields:
      status:
        type: enum
        values: []`,
			want: "non-empty values",
		},
		{
			name: "duplicate enum value",
			src: `version: "1"
collections:
  products:
    fields:
      status:
        type: enum
        values: [active, active]`,
			want: "duplicate",
		},
		{
			name: "unknown type",
			src: `version: "1"
collections:
  products:
    fields:
      x: { type: wibble }`,
			want: "unknown field type",
		},
		{
			name: "deferred type",
			src: `version: "1"
collections:
  products:
    fields:
      cat: { type: relation }`,
			want: "not supported until phase 2",
		},
		{
			name: "bad collection name",
			src: `version: "1"
collections:
  Products:
    fields:
      title: string`,
			want: "invalid name",
		},
		{
			name: "reserved collection",
			src: `version: "1"
collections:
  _audit:
    fields:
      title: string`,
			want: "reserved collection name",
		},
		{
			name: "index references missing field",
			src: `version: "1"
collections:
  products:
    fields:
      title: string
    indexes: [nope]`,
			want: `column "nope" is not a field`,
		},
		{
			name: "missing version",
			src: `collections:
  products:
    fields:
      title: string`,
			want: "version: required",
		},
		{
			name: "no collections",
			src:  `version: "1"`,
			want: "at least one collection",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parseExpectIssue(t, tc.src, tc.want)
		})
	}
}

func TestToCollectionMeta_AddsIDAndAuditColumns(t *testing.T) {
	def, err := Parse([]byte(validSchema))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	meta := def.Collections[0].ToCollectionMeta()

	// id first, then the 4 declared fields, then 4 audit columns = 9 total.
	if len(meta.Columns) != 9 {
		t.Fatalf("columns: got %d, want 9: %#v", len(meta.Columns), meta.Columns)
	}
	if meta.Columns[0].Name != "id" || meta.Columns[0].Nullable {
		t.Fatalf("id column wrong: %#v", meta.Columns[0])
	}
	want := map[string]bool{"created_at": false, "updated_at": false, "created_by": false, "updated_by": false}
	for _, c := range meta.Columns {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("audit column %q missing", name)
		}
	}

	// unique slug → a unique index; plus the two declared indexes = 3.
	if len(meta.Indexes) != 3 {
		t.Fatalf("indexes: got %d, want 3: %#v", len(meta.Indexes), meta.Indexes)
	}
	var sawUniqueSlug bool
	for _, idx := range meta.Indexes {
		if idx.Unique && len(idx.Columns) == 1 && idx.Columns[0] == "slug" {
			sawUniqueSlug = true
		}
	}
	if !sawUniqueSlug {
		t.Fatalf("expected unique index on slug: %#v", meta.Indexes)
	}
}
