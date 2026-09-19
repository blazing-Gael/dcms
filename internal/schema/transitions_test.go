package schema

import (
	"strings"
	"testing"
)

const transitionSchema = `
version: "1"
auth:
  roles:
    admin:  { label: Admin }
    editor: { label: Editor }
collections:
  stories:
    fields:
      title: { type: string, required: true }
      review:
        type: enum
        values: [writing, submitted, changes_requested, approved]
        access:
          write:
            rules:
              - who: owner
                from: [writing, changes_requested]
                to:   [writing, submitted]
              - who: [admin, editor]
`

func TestTransitions_Parse(t *testing.T) {
	def, err := Parse([]byte(transitionSchema))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var review *FieldDef
	for i := range findCollection(def, "stories").Fields {
		if f := &findCollection(def, "stories").Fields[i]; f.Name == "review" {
			review = f
		}
	}
	if review == nil || review.Access == nil {
		t.Fatal("review field or access missing")
	}
	trs := review.Access.WriteTransitions
	if len(trs) != 2 {
		t.Fatalf("got %d transition rules, want 2", len(trs))
	}
	if trs[0].Who.Kind != RuleOwner {
		t.Fatalf("rule 0 who = %v, want owner", trs[0].Who.Kind)
	}
	if len(trs[0].From) != 2 || trs[0].To[1] != "submitted" {
		t.Fatalf("rule 0 from/to parsed wrong: %+v", trs[0])
	}
	if trs[1].Who.Kind != RuleRoles || len(trs[1].From) != 0 || len(trs[1].To) != 0 {
		t.Fatalf("rule 1 should be a roles rule with any transition: %+v", trs[1])
	}
	if review.Access.Write != nil {
		t.Fatal("Write should be nil when WriteTransitions is set")
	}
}

func TestTransitions_ValidateErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			name: "not an enum",
			src: `version: "1"
collections:
  s:
    fields:
      x:
        type: string
        access: { write: { rules: [ { who: authenticated, to: [a] } ] } }`,
			want: "only valid on an enum field",
		},
		{
			name: "undeclared role in who",
			src: `version: "1"
auth: { roles: { admin: { label: A } } }
collections:
  s:
    fields:
      x:
        type: enum
        values: [a, b]
        access: { write: { rules: [ { who: [ghost], from: [a], to: [b] } ] } }`,
			want: `role "ghost" is not declared`,
		},
		{
			name: "undeclared to value",
			src: `version: "1"
collections:
  s:
    fields:
      x:
        type: enum
        values: [a, b]
        access: { write: { rules: [ { who: authenticated, to: [z] } ] } }`,
			want: `"z" is not a declared value`,
		},
		{
			name: "missing who",
			src: `version: "1"
collections:
  s:
    fields:
      x:
        type: enum
        values: [a, b]
        access: { write: { rules: [ { from: [a], to: [b] } ] } }`,
			want: "requires `who`",
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
