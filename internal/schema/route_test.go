package schema

import (
	"strings"
	"testing"
)

func TestRoute_ParsesAndExposesPlaceholders(t *testing.T) {
	def, err := Parse([]byte(`
version: "1"
meta:
  site_url: https://golpo.example
collections:
  stories:
    route: /golpo/{slug}
    fields:
      slug: { type: string, unique: true }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if def.Meta.SiteURL != "https://golpo.example" {
		t.Errorf("site_url = %q", def.Meta.SiteURL)
	}
	var stories *CollectionDef
	for i := range def.Collections {
		if def.Collections[i].Name == "stories" {
			stories = &def.Collections[i]
		}
	}
	if stories.Route != "/golpo/{slug}" {
		t.Fatalf("route = %q", stories.Route)
	}
	if got := RoutePlaceholders(stories.Route); len(got) != 1 || got[0] != "slug" {
		t.Errorf("placeholders = %v, want [slug]", got)
	}
}

func TestRoute_AllowsEngineColumns(t *testing.T) {
	// id / created_by etc. are valid placeholders (they are real columns).
	if _, err := Parse([]byte(`
version: "1"
collections:
  posts:
    route: /p/{id}
    fields:
      title: { type: string }
`)); err != nil {
		t.Fatalf("route on id should be allowed: %v", err)
	}
}

func TestRoute_ValidationErrors(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{
			name: "unknown field",
			src: `
version: "1"
collections:
  stories:
    route: /golpo/{nope}
    fields:
      slug: { type: string }
`,
			want: "`{nope}` is not a field",
		},
		{
			name: "missing leading slash",
			src: `
version: "1"
collections:
  stories:
    route: golpo/{slug}
    fields:
      slug: { type: string }
`,
			want: "must start with '/'",
		},
		{
			name: "date format not yet supported",
			src: `
version: "1"
collections:
  stories:
    publishing: true
    route: /{published_at:2006}/{slug}
    fields:
      slug: { type: string }
`,
			want: "not supported yet",
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
