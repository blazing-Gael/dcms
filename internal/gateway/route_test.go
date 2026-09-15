package gateway_test

import (
	"net/http"
	"testing"
)

// The `route` metadata and meta.site_url (issue #10) are served on /__schema, the
// contract a consumer (admin panel preview link, SSG) reads to build page URLs.
func TestRoute_ExposedOnSchema(t *testing.T) {
	srv, _ := newServerWith(t, `
version: "1"
meta:
  site_url: https://golpo.example
collections:
  stories:
    route: /golpo/{slug}
    fields:
      slug:  { type: string, unique: true }
      title: { type: string, required: true }
`)
	_, body := do(t, http.MethodGet, srv.URL+"/__schema", "")
	meta, _ := body["meta"].(map[string]any)
	if meta["site_url"] != "https://golpo.example" {
		t.Errorf("site_url missing from /__schema: %v", meta)
	}
	cols, _ := body["collections"].([]any)
	var storiesRoute any
	for _, c := range cols {
		if m, ok := c.(map[string]any); ok && m["name"] == "stories" {
			storiesRoute = m["route"]
		}
	}
	if storiesRoute != "/golpo/{slug}" {
		t.Errorf("stories route on /__schema = %v, want /golpo/{slug}", storiesRoute)
	}
}
