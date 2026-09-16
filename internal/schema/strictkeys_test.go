package schema

import (
	"strings"
	"testing"
)

// Strict directive/key handling (issue #35): a mistyped directive must fail loudly
// with a did-you-mean, an unknown top-level or meta key must fail, and a reserved
// (not-yet-implemented) directive must load but surface a warning.

func TestParse_UnknownDirectiveErrors(t *testing.T) {
	cases := []struct{ name, src, wantSubstr string }{
		{
			name: "typo of a real directive",
			src: `
version: "1"
collections:
  posts:
    fields:
      title: { type: string }
    publising: true
`,
			wantSubstr: "did you mean `publishing`?",
		},
		{
			name: "typo of soft_delete",
			src: `
version: "1"
collections:
  posts:
    fields:
      title: { type: string }
    soft_delet: true
`,
			wantSubstr: "did you mean `soft_delete`?",
		},
		{
			name: "wholly unknown directive (no suggestion)",
			src: `
version: "1"
collections:
  posts:
    fields:
      title: { type: string }
    sparkles: true
`,
			wantSubstr: `unknown directive "sparkles"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.src))
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantSubstr) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.wantSubstr)
			}
		})
	}
}

func TestParse_UnknownTopLevelAndMetaKeysError(t *testing.T) {
	cases := map[string]string{
		"top-level": `
version: "1"
brand:
  name: x
collections:
  posts:
    fields:
      title: { type: string }
`,
		"meta": `
version: "1"
meta:
  naem: myapp
collections:
  posts:
    fields:
      title: { type: string }
`,
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(src)); err == nil {
				t.Fatal("expected an unknown-key error, got nil")
			}
		})
	}
}

func TestParse_ReservedDirectiveWarnsNotErrors(t *testing.T) {
	def, err := Parse([]byte(`
version: "1"
collections:
  posts:
    fields:
      title: { type: string }
    vectorize: [title]
`))
	if err != nil {
		t.Fatalf("a reserved directive should load, not error: %v", err)
	}
	if len(def.Warnings) != 1 || !strings.Contains(def.Warnings[0], "vectorize") {
		t.Fatalf("want one warning mentioning vectorize, got %#v", def.Warnings)
	}
	if !strings.Contains(def.Warnings[0], "not implemented yet") {
		t.Errorf("warning should say it is not implemented yet: %q", def.Warnings[0])
	}
}

func TestParse_CleanSchemaHasNoWarnings(t *testing.T) {
	def, err := Parse([]byte(`
version: "1"
collections:
  posts:
    fields:
      title: { type: string }
    timestamps: true
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(def.Warnings) != 0 {
		t.Errorf("clean schema produced warnings: %#v", def.Warnings)
	}
}
