package schema

import (
	"strings"
	"testing"
)

func TestRevisions_DirectiveShapes(t *testing.T) {
	// Boolean shorthand: revisions on, unbounded retention.
	def, err := Parse([]byte("version: \"1\"\ncollections:\n  docs:\n    revisions: true\n    fields:\n      b: { type: string }\n"))
	if err != nil {
		t.Fatalf("bool form: %v", err)
	}
	if c := findCollection(def, "docs"); !c.Revisions || c.RevisionsMax != 0 {
		t.Fatalf("bool form: Revisions=%v Max=%d, want true/0", c.Revisions, c.RevisionsMax)
	}

	// Mapping form: revisions on, capped.
	def, err = Parse([]byte("version: \"1\"\ncollections:\n  docs:\n    revisions: { max: 50 }\n    fields:\n      b: { type: string }\n"))
	if err != nil {
		t.Fatalf("map form: %v", err)
	}
	if c := findCollection(def, "docs"); !c.Revisions || c.RevisionsMax != 50 {
		t.Fatalf("map form: Revisions=%v Max=%d, want true/50", c.Revisions, c.RevisionsMax)
	}
}

func TestRevisions_DirectiveErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			name: "unknown option",
			src:  "version: \"1\"\ncollections:\n  docs:\n    revisions: { mx: 5 }\n    fields:\n      b: { type: string }\n",
			want: "unknown revisions option",
		},
		{
			name: "negative max",
			src:  "version: \"1\"\ncollections:\n  docs:\n    revisions: { max: -1 }\n    fields:\n      b: { type: string }\n",
			want: "must be zero (unbounded) or positive",
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

func findCollection(def *SchemaDefinition, name string) *CollectionDef {
	for i := range def.Collections {
		if def.Collections[i].Name == name {
			return &def.Collections[i]
		}
	}
	return nil
}
