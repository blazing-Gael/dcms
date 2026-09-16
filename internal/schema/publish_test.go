package schema

import (
	"strings"
	"testing"
)

func TestPublish_ParsesRule(t *testing.T) {
	def, err := Parse([]byte(`
version: "1"
auth:
  roles:
    editor: { label: Editor }
collections:
  stories:
    publishing: true
    fields:
      title: { type: string, required: true }
    access:
      update:  { any: [editor, owner] }
      publish: [editor]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var stories *CollectionDef
	for i := range def.Collections {
		if def.Collections[i].Name == "stories" {
			stories = &def.Collections[i]
		}
	}
	rule, ok := stories.PublishRule()
	if !ok || rule.Kind != RuleRoles || len(rule.Roles) != 1 || rule.Roles[0] != "editor" {
		t.Fatalf("publish rule not parsed: %#v (ok=%v)", rule, ok)
	}
	// A collection with no publish rule reports absent — transitions fall back to
	// the update rule (today's behaviour), not a default.
	other := CollectionDef{Name: "x"}
	if _, ok := other.PublishRule(); ok {
		t.Error("undeclared publish should report absent")
	}
}

func TestPublish_ValidationErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			name: "public in publish",
			src: `
version: "1"
collections:
  stories:
    publishing: true
    fields:
      title: { type: string }
    access:
      publish: public
`,
			want: "`public` is not allowed",
		},
		{
			name: "public nested in publish composite",
			src: `
version: "1"
auth:
  roles:
    editor: { label: Editor }
collections:
  stories:
    publishing: true
    fields:
      title: { type: string }
    access:
      publish: { any: [editor, public] }
`,
			want: "`public` is not allowed",
		},
		{
			name: "publish without publishing",
			src: `
version: "1"
auth:
  roles:
    editor: { label: Editor }
collections:
  stories:
    fields:
      title: { type: string }
    access:
      publish: [editor]
`,
			want: "only valid on a collection with publishing",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.src))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected an error mentioning %q, got %v", c.want, err)
			}
		})
	}
}
