package schema

import (
	"strings"
	"testing"
)

const objectListSchema = `
version: "1"
collections:
  pages:
    fields:
      key: { type: string, unique: true }
      capabilities:
        type: object_list
        max: 4
        of:
          title: { type: string, required: true }
          body:  { type: text }
          image: { type: file }
`

func TestObjectList_ParsesShape(t *testing.T) {
	def, err := Parse([]byte(objectListSchema))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c := findCollection(def, "pages")
	var caps *FieldDef
	for i := range c.Fields {
		if c.Fields[i].Name == "capabilities" {
			caps = &c.Fields[i]
		}
	}
	if caps == nil {
		t.Fatal("capabilities field missing")
	}
	if caps.Type != TypeObjectList {
		t.Fatalf("type = %q, want object_list", caps.Type)
	}
	if caps.Max == nil || *caps.Max != 4 {
		t.Fatalf("max = %v, want 4", caps.Max)
	}
	if len(caps.Of) != 3 {
		t.Fatalf("of has %d fields, want 3", len(caps.Of))
	}
	if caps.Of[0].Name != "title" || !caps.Of[0].Required {
		t.Fatalf("of[0] = %+v, want required title", caps.Of[0])
	}
	// The inner `file` field is finalized to a _media relation, like a top-level one.
	if img := caps.Of[2]; img.Type != TypeRelation || img.Target != MediaCollection {
		t.Fatalf("inner file field = %q/%q, want relation → %s", img.Type, img.Target, MediaCollection)
	}
}

func TestObjectList_StoredAsJSONColumn(t *testing.T) {
	def, err := Parse([]byte(objectListSchema))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, meta := range def.CollectionMetas() {
		if meta.Name != "pages" {
			continue
		}
		for _, col := range meta.Columns {
			if col.Name == "capabilities" {
				if col.Type != string(TypeJSON) {
					t.Fatalf("capabilities column type = %q, want json", col.Type)
				}
				return
			}
		}
		t.Fatal("capabilities column missing")
	}
	t.Fatal("pages collection missing")
}

func TestObjectList_ValidateErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			name: "empty of",
			src:  "version: \"1\"\ncollections:\n  p:\n    fields:\n      x: { type: object_list, of: {} }\n",
			want: "requires a non-empty 'of'",
		},
		{
			name: "nested object_list",
			src:  "version: \"1\"\ncollections:\n  p:\n    fields:\n      x:\n        type: object_list\n        of:\n          y: { type: object_list, of: { z: { type: string } } }\n",
			want: "may not contain another object_list",
		},
		{
			name: "inner richtext",
			src:  "version: \"1\"\ncollections:\n  p:\n    fields:\n      x:\n        type: object_list\n        of:\n          y: { type: richtext }\n",
			want: "may not contain richtext",
		},
		{
			name: "inner m2m relation",
			src:  "version: \"1\"\ncollections:\n  p:\n    fields:\n      x:\n        type: object_list\n        of:\n          y: { type: relation, target: p, many: true }\n",
			want: "may not hold a many-to-many relation",
		},
		{
			name: "inner unknown target",
			src:  "version: \"1\"\ncollections:\n  p:\n    fields:\n      x:\n        type: object_list\n        of:\n          y: { type: relation, target: nope }\n",
			want: "is not a declared collection",
		},
		{
			name: "reserved _key inner name",
			src:  "version: \"1\"\ncollections:\n  p:\n    fields:\n      x:\n        type: object_list\n        of:\n          _key: { type: string }\n",
			want: "is reserved",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Parse([]byte(c.src)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error mentioning %q, got %v", c.want, err)
			}
		})
	}
}

func TestObjectList_ValidatesElementValues(t *testing.T) {
	def, err := Parse([]byte(objectListSchema))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	c := findCollection(def, "pages")

	// A valid list, including an optional _key per element.
	ok := map[string]any{"capabilities": []any{
		map[string]any{"_key": "a", "title": "One"},
		map[string]any{"title": "Two", "body": "text"},
	}}
	if errs := c.ValidateCreate(ok); errs != nil {
		t.Fatalf("valid object_list rejected: %v", errs)
	}

	// A missing required inner field is reported against the list.
	bad := map[string]any{"capabilities": []any{map[string]any{"body": "no title"}}}
	if errs := c.ValidateCreate(bad); errs == nil || !strings.Contains(errs["capabilities"], "title is required") {
		t.Fatalf("missing inner required not caught: %v", errs)
	}

	// Over the max length is rejected.
	over := map[string]any{"capabilities": []any{
		map[string]any{"title": "1"}, map[string]any{"title": "2"},
		map[string]any{"title": "3"}, map[string]any{"title": "4"},
		map[string]any{"title": "5"},
	}}
	if errs := c.ValidateCreate(over); errs == nil || !strings.Contains(errs["capabilities"], "at most 4") {
		t.Fatalf("max length not enforced: %v", errs)
	}

	// An unknown inner field is rejected.
	unknown := map[string]any{"capabilities": []any{map[string]any{"title": "ok", "nope": 1}}}
	if errs := c.ValidateCreate(unknown); errs == nil || !strings.Contains(errs["capabilities"], "nope") {
		t.Fatalf("unknown inner field not caught: %v", errs)
	}

	// A non-array value is rejected.
	notlist := map[string]any{"capabilities": "oops"}
	if errs := c.ValidateCreate(notlist); errs == nil || !strings.Contains(errs["capabilities"], "list of objects") {
		t.Fatalf("non-array not caught: %v", errs)
	}
}
