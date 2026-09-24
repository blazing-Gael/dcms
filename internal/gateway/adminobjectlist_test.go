package gateway_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
)

// Admin panel 2C: object_list editor (server-side parse + render). The JS add/remove
// is exercised by posting the indexed field names a browser would submit.

const adminObjectListSchema = `
version: "1"
auth:
  roles:
    admin: { label: Admin }
collections:
  pages:
    access:
      read:   authenticated
      create: authenticated
      update: authenticated
      delete: authenticated
    fields:
      title: { type: string, required: true }
      sections:
        type: object_list
        of:
          heading: { type: string, required: true }
          body:    { type: text }
`

func newAdminObjectListServer(t *testing.T) (string, store.Adapter) {
	t.Helper()
	def, err := schema.Parse([]byte(adminObjectListSchema))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	db, err := sqlite.New(sqlite.Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, meta := range def.CollectionMetas() {
		plan, err := db.Diff(ctx, meta)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if err := db.Migrate(ctx, plan); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	srv := httptest.NewServer(gateway.New(def, db, nil, gateway.Options{
		Authenticator: gateway.NewSessionAuthenticator(db),
	}).Handler())
	t.Cleanup(srv.Close)
	return srv.URL, db
}

func sectionsOf(t *testing.T, db store.Adapter, id string) []map[string]any {
	t.Helper()
	rec, err := db.FindOne(context.Background(), "pages", id)
	if err != nil {
		t.Fatalf("find page: %v", err)
	}
	// The raw store returns a json column as a string; decode it the way the gateway's
	// CoerceResponse would before rendering.
	var arr []any
	switch v := rec["sections"].(type) {
	case []any:
		arr = v
	case string:
		_ = json.Unmarshal([]byte(v), &arr)
	case []byte:
		_ = json.Unmarshal(v, &arr)
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func TestAdmin2C_ObjectListEditor(t *testing.T) {
	base, db := newAdminObjectListServer(t)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	c := jarClient(t)
	adminLogin(t, c, base, "admin@x.com", "correcthorse")

	// Create a page with two sections (the indexed names a browser submits after
	// the script adds a second row).
	tok := csrfToken(t, c, base)
	resp, _ := c.PostForm(base+"/__admin/c/pages", url.Values{
		"title":               {"Home"},
		"__ol_sections":       {"1"},
		"sections[0].heading": {"Intro"},
		"sections[0].body":    {"Welcome"},
		"sections[1].heading": {"Details"},
		"sections[1].body":    {"More"},
		"csrf":                {tok},
	})
	resp.Body.Close()
	page, _ := db.Find(context.Background(), store.Query{Collection: "pages", SkipCount: true})
	id, _ := page.Data[0]["id"].(string)

	secs := sectionsOf(t, db, id)
	if len(secs) != 2 || secs[0]["heading"] != "Intro" || secs[1]["heading"] != "Details" {
		t.Fatalf("two sections should persist in order, got %#v", secs)
	}

	// The edit form renders the existing rows and a template + Add control.
	_, form := getBody(t, c, base+"/__admin/c/pages/"+id)
	if !strings.Contains(form, `value="Intro"`) || !strings.Contains(form, "data-ol-template") || !strings.Contains(form, "data-ol-add") {
		t.Fatalf("edit form should show rows + an add control:\n%s", excerpt(form, "sections"))
	}
	if !strings.Contains(form, "__IDX__") {
		t.Fatal("template row should carry the __IDX__ placeholder for the script")
	}

	// Edit: drop the second row (submit only index 0). A removed row's fields simply
	// aren't submitted, so the array shrinks.
	tok = csrfToken(t, c, base)
	resp, _ = c.PostForm(base+"/__admin/c/pages/"+id, url.Values{
		"title":               {"Home"},
		"__ol_sections":       {"1"},
		"sections[0].heading": {"Intro edited"},
		"sections[0].body":    {"Welcome"},
		"csrf":                {tok},
	})
	resp.Body.Close()
	secs = sectionsOf(t, db, id)
	if len(secs) != 1 || secs[0]["heading"] != "Intro edited" {
		t.Fatalf("after dropping a row, one edited section should remain, got %#v", secs)
	}

	// A blank row (all inner fields empty) is ignored, and a required inner field is
	// still enforced.
	tok = csrfToken(t, c, base)
	resp, _ = c.PostForm(base+"/__admin/c/pages/"+id, url.Values{
		"title":               {"Home"},
		"__ol_sections":       {"1"},
		"sections[0].heading": {"Only one"},
		"sections[1].heading": {""}, // blank row → dropped, not a validation error
		"sections[1].body":    {""},
		"csrf":                {tok},
	})
	resp.Body.Close()
	if secs = sectionsOf(t, db, id); len(secs) != 1 {
		t.Fatalf("a fully-blank row should be dropped, got %#v", secs)
	}
}
