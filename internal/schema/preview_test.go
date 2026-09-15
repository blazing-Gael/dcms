package schema

import (
	"strings"
	"testing"
)

func TestPreview_ParsesRule(t *testing.T) {
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
      preview: { any: [editor, owner] }
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
	rule, ok := stories.PreviewRule()
	if !ok || rule.Kind != RuleAny || len(rule.Any) != 2 {
		t.Fatalf("preview rule not parsed: %#v (ok=%v)", rule, ok)
	}
	// A collection with no preview rule reports absent (token-only, not a default).
	other := CollectionDef{Name: "x"}
	if _, ok := other.PreviewRule(); ok {
		t.Error("undeclared preview should report absent")
	}
}

func TestPreview_ValidationErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			name: "public in preview",
			src: `
version: "1"
collections:
  stories:
    publishing: true
    fields:
      title: { type: string }
    access:
      preview: public
`,
			want: "`public` is not allowed",
		},
		{
			name: "public nested in preview composite",
			src: `
version: "1"
auth:
  roles:
    admin: { label: Admin }
collections:
  stories:
    publishing: true
    fields:
      title: { type: string }
    access:
      preview: { any: [admin, public] }
`,
			want: "`public` is not allowed",
		},
		{
			name: "preview without a lifecycle",
			src: `
version: "1"
collections:
  stories:
    fields:
      title: { type: string }
    access:
      preview: owner
`,
			want: "publishing or soft_delete",
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
