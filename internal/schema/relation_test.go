package schema

import (
	"strings"
	"testing"
)

const relationSchema = `
version: "1"
collections:
  users:
    fields:
      name: { type: string, required: true }
  posts:
    fields:
      title:  { type: string, required: true }
      author: { type: relation, target: users, required: true }
`

func TestRelation_ParsesTargetAndBecomesFKColumn(t *testing.T) {
	def, err := Parse([]byte(relationSchema))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	posts := def.Collections[1]
	author := posts.Fields[1]
	if author.Type != TypeRelation || author.Target != "users" || author.Many {
		t.Fatalf("author field parsed wrong: %#v", author)
	}

	// belongs-to compiles to a string FK column (holds the target id).
	meta := posts.ToCollectionMeta()
	found := false
	for _, c := range meta.Columns {
		if c.Name == "author" {
			found = true
			if c.Type != string(TypeString) {
				t.Errorf("author column type = %q, want string (FK stores the id)", c.Type)
			}
			if c.Nullable {
				t.Errorf("author is required, column should be NOT NULL")
			}
		}
	}
	if !found {
		t.Fatal("belongs-to relation did not produce a column")
	}
}

func TestRelation_ValidationErrors(t *testing.T) {
	cases := map[string]struct {
		src  string
		want string
	}{
		"missing target": {
			`version: "1"
collections:
  posts:
    fields:
      author: { type: relation }`,
			"requires a 'target'",
		},
		"unknown target": {
			`version: "1"
collections:
  posts:
    fields:
      author: { type: relation, target: ghosts }`,
			"not a declared collection",
		},
		"m2m unknown target": {
			`version: "1"
collections:
  posts:
    fields:
      tags: { type: relation, target: ghosts, many: true }`,
			"not a declared collection",
		},
		"bad on_delete": {
			`version: "1"
collections:
  users:
    fields:
      name: { type: string }
  posts:
    fields:
      author: { type: relation, target: users, on_delete: explode }`,
			"invalid on_delete",
		},
		"set null on required": {
			`version: "1"
collections:
  users:
    fields:
      name: { type: string }
  posts:
    fields:
      author: { type: relation, target: users, required: true, on_delete: set null }`,
			"requires the relation to be nullable",
		},
		"on_delete on m2m": {
			`version: "1"
collections:
  tags:
    fields:
      name: { type: string }
  posts:
    fields:
      tags: { type: relation, target: tags, many: true, on_delete: cascade }`,
			"not valid on a many-to-many",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src))
			if err == nil {
				t.Fatalf("expected a validation error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestRelation_ManyToManyBuildsJoinTable(t *testing.T) {
	def, err := Parse([]byte(`
version: "1"
collections:
  tags:
    fields:
      name: { type: string, required: true }
  posts:
    fields:
      title: { type: string, required: true }
      tags:  { type: relation, target: tags, many: true }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// The m2m field must NOT become a column on posts.
	posts := def.Collections[1].ToCollectionMeta()
	for _, c := range posts.Columns {
		if c.Name == "tags" {
			t.Fatalf("m2m field must not be a column on the base table")
		}
	}

	// A join table posts_tags must appear in the migration metas, with the
	// endpoint columns, audit columns, and a unique link index.
	metas := def.CollectionMetas()
	found := false
	for _, m := range metas {
		if m.Name != "posts_tags" {
			continue
		}
		found = true
		cols := map[string]bool{}
		for _, c := range m.Columns {
			cols[c.Name] = true
		}
		for _, want := range []string{"id", "source_id", "target_id", "created_at", "updated_at", "created_by", "updated_by"} {
			if !cols[want] {
				t.Errorf("join table missing column %q", want)
			}
		}
		hasUnique := false
		for _, idx := range m.Indexes {
			if idx.Unique && len(idx.Columns) == 2 && idx.Columns[0] == "source_id" && idx.Columns[1] == "target_id" {
				hasUnique = true
			}
		}
		if !hasUnique {
			t.Errorf("join table missing unique(source_id,target_id) index")
		}
	}
	if !found {
		t.Fatal("no posts_tags join table generated")
	}
}

func TestRelation_OnDeletePolicyOnColumn(t *testing.T) {
	def, err := Parse([]byte(`
version: "1"
collections:
  users:
    fields:
      name: { type: string }
  posts:
    fields:
      author: { type: relation, target: users, on_delete: cascade }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	posts := def.Collections[1].ToCollectionMeta()
	found := false
	for _, c := range posts.Columns {
		if c.Name == "author" {
			found = true
			if c.References != "users" {
				t.Errorf("author.References = %q, want users", c.References)
			}
			if c.OnDelete != "cascade" {
				t.Errorf("author.OnDelete = %q, want cascade", c.OnDelete)
			}
		}
	}
	if !found {
		t.Fatal("author column missing")
	}
}

func TestRelation_SelfReferenceIsAllowed(t *testing.T) {
	// A category that points at its own collection (parent) must validate.
	_, err := Parse([]byte(`
version: "1"
collections:
  categories:
    fields:
      name:   { type: string, required: true }
      parent: { type: relation, target: categories }
`))
	if err != nil {
		t.Fatalf("self-relation should be valid: %v", err)
	}
}
