package gateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
)

// Admin panel 2B: relation pickers, lifecycle actions, #27 transition-aware enum
// selects, and revision restore — the editing widgets that make a handoff panel
// usable by a non-technical client.

const adminWidgetsSchema = `
version: "1"
auth:
  roles:
    admin:  { label: Admin }
    editor: { label: Editor }
collections:
  categories:
    fields:
      name: { type: string, required: true }
  articles:
    publishing: true
    soft_delete: true
    revisions: true
    access:
      read:   authenticated
      create: authenticated
      update: authenticated
      delete: authenticated
    fields:
      title:    { type: string, required: true }
      category: { type: relation, target: categories }
      tags:     { type: relation, target: categories, many: true }
      stage:
        type: enum
        values: [draft, in_review, approved]
        default: draft
        access:
          write:
            rules:
              - { who: authenticated, from: [draft], to: [in_review] }
              - who: [admin]
                to: [approved]
`

func newAdminWidgetsServer(t *testing.T) (string, store.Adapter) {
	t.Helper()
	def, err := schema.Parse([]byte(adminWidgetsSchema))
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

// seedCategory inserts a category directly and returns its id.
func seedCategory(t *testing.T, db store.Adapter, name string) string {
	t.Helper()
	rec, err := db.Create(context.Background(), store.WriteInput{
		Collection: "categories", Data: store.Record{"name": name},
	})
	if err != nil {
		t.Fatalf("seed category %q: %v", name, err)
	}
	id, _ := rec["id"].(string)
	if id == "" {
		t.Fatalf("seed category %q: no id", name)
	}
	return id
}

func TestAdmin2B_RelationPickerShowsLabels(t *testing.T) {
	base, db := newAdminWidgetsServer(t)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	dessertsID := seedCategory(t, db, "Desserts")
	c := jarClient(t)
	adminLogin(t, c, base, "admin@x.com", "correcthorse")

	// Create an article pointing at the category.
	tok := csrfToken(t, c, base)
	resp, err := c.PostForm(base+"/__admin/c/articles", url.Values{
		"title": {"Cake"}, "category": {dessertsID}, "csrf": {tok},
	})
	if err != nil {
		t.Fatalf("create article: %v", err)
	}
	resp.Body.Close()

	page, _ := db.Find(context.Background(), store.Query{Collection: "articles", SkipCount: true})
	if len(page.Data) != 1 || page.Data[0]["category"] != dessertsID {
		t.Fatalf("article category not stored as the picked id: %#v", page.Data)
	}
	id, _ := page.Data[0]["id"].(string)

	// The edit form's category select shows the human label, with the id selected —
	// never a bare UUID as the visible text.
	st, body := getBody(t, c, base+"/__admin/c/articles/"+id)
	if st != http.StatusOK {
		t.Fatalf("edit form status %d", st)
	}
	if !strings.Contains(body, `value="`+dessertsID+`" selected>Desserts<`) {
		t.Fatalf("category select should show the selected label 'Desserts'; body:\n%s", excerpt(body, "category"))
	}
}

func TestAdmin2B_M2MChecklist(t *testing.T) {
	base, db := newAdminWidgetsServer(t)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	a := seedCategory(t, db, "Alpha")
	b := seedCategory(t, db, "Beta")
	c := jarClient(t)
	adminLogin(t, c, base, "admin@x.com", "correcthorse")

	tok := csrfToken(t, c, base)
	resp, err := c.PostForm(base+"/__admin/c/articles", url.Values{
		"title": {"Tagged"}, "tags": {a, b}, "csrf": {tok},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp.Body.Close()

	page, _ := db.Find(context.Background(), store.Query{Collection: "articles", SkipCount: true})
	id, _ := page.Data[0]["id"].(string)

	// Both links exist in the join table.
	links, _ := db.Find(context.Background(), store.Query{
		Collection: schema.JoinTableName("articles", "tags"),
		Filters:    []store.Filter{{Field: "source_id", Operator: store.Eq, Value: id}},
		SkipCount:  true,
	})
	if len(links.Data) != 2 {
		t.Fatalf("want 2 m2m links, got %d", len(links.Data))
	}

	// The edit form pre-checks both tags.
	_, body := getBody(t, c, base+"/__admin/c/articles/"+id)
	if strings.Count(body, "checked>") < 2 {
		t.Fatalf("both tag checkboxes should be checked; body:\n%s", excerpt(body, "tags"))
	}
}

func TestAdmin2B_LifecyclePublishTrashRestore(t *testing.T) {
	base, db := newAdminWidgetsServer(t)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	c := jarClient(t)
	adminLogin(t, c, base, "admin@x.com", "correcthorse")

	tok := csrfToken(t, c, base)
	resp, _ := c.PostForm(base+"/__admin/c/articles", url.Values{"title": {"Draft one"}, "csrf": {tok}})
	resp.Body.Close()
	page, _ := db.Find(context.Background(), store.Query{Collection: "articles", SkipCount: true})
	id, _ := page.Data[0]["id"].(string)

	// A fresh record is a draft, and the list says so in plain language.
	if _, body := getBody(t, c, base+"/__admin/c/articles"); !strings.Contains(body, "badge-draft") {
		t.Fatal("list should show a Draft badge")
	}

	// Publish → live.
	postForm(t, c, base, "/__admin/c/articles/"+id+"/publish")
	rec, _ := db.FindOne(context.Background(), "articles", id)
	if rec[schema.LifecycleStatus] != schema.StatusPublished || !isFilled(rec[schema.LifecyclePublishedAt]) {
		t.Fatalf("publish did not set live status/published_at: %#v", rec)
	}
	if _, body := getBody(t, c, base+"/__admin/c/articles/"+id); !strings.Contains(body, "Live on your website") {
		t.Fatal("edit page should explain the record is live")
	}

	// Trash (soft delete) → reversible.
	postForm(t, c, base, "/__admin/c/articles/"+id+"/trash")
	rec, _ = db.FindOne(context.Background(), "articles", id)
	if !isFilled(rec[schema.LifecycleDeletedAt]) {
		t.Fatal("trash should set _deleted_at")
	}

	// Restore → back.
	postForm(t, c, base, "/__admin/c/articles/"+id+"/restore")
	rec, _ = db.FindOne(context.Background(), "articles", id)
	if isFilled(rec[schema.LifecycleDeletedAt]) {
		t.Fatal("restore should clear _deleted_at")
	}
}

func TestAdmin2B_TransitionAwareEnum(t *testing.T) {
	base, db := newAdminWidgetsServer(t)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	seedUser(t, db, "ed@x.com", "correcthorse", "editor")

	// Admin creates a draft article.
	ca := jarClient(t)
	adminLogin(t, ca, base, "admin@x.com", "correcthorse")
	tok := csrfToken(t, ca, base)
	resp, _ := ca.PostForm(base+"/__admin/c/articles", url.Values{"title": {"Flow"}, "csrf": {tok}})
	resp.Body.Close()
	page, _ := db.Find(context.Background(), store.Query{Collection: "articles", SkipCount: true})
	id, _ := page.Data[0]["id"].(string)

	// The editor may move draft→in_review but not →approved: the select must offer
	// in_review and hide approved, so the panel can never lead them into a 403.
	ce := jarClient(t)
	adminLogin(t, ce, base, "ed@x.com", "correcthorse")
	_, edBody := getBody(t, ce, base+"/__admin/c/articles/"+id)
	if !strings.Contains(edBody, `value="in_review"`) {
		t.Fatal("editor should see the in_review option")
	}
	if strings.Contains(edBody, `value="approved"`) {
		t.Fatal("editor must NOT see the approved option (admin-only transition)")
	}

	// The admin, by contrast, may approve.
	_, adBody := getBody(t, ca, base+"/__admin/c/articles/"+id)
	if !strings.Contains(adBody, `value="approved"`) {
		t.Fatal("admin should see the approved option")
	}
}

func TestAdmin2B_RevisionRestore(t *testing.T) {
	base, db := newAdminWidgetsServer(t)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	c := jarClient(t)
	adminLogin(t, c, base, "admin@x.com", "correcthorse")

	tok := csrfToken(t, c, base)
	resp, _ := c.PostForm(base+"/__admin/c/articles", url.Values{"title": {"Version A"}, "csrf": {tok}})
	resp.Body.Close()
	page, _ := db.Find(context.Background(), store.Query{Collection: "articles", SkipCount: true})
	id, _ := page.Data[0]["id"].(string)

	// Edit the title.
	tok = csrfToken(t, c, base)
	resp, _ = c.PostForm(base+"/__admin/c/articles/"+id, url.Values{"title": {"Version B"}, "csrf": {tok}})
	resp.Body.Close()

	// History lists both versions.
	st, body := getBody(t, c, base+"/__admin/c/articles/"+id+"/history")
	if st != http.StatusOK || !strings.Contains(body, "Restore") {
		t.Fatalf("history page missing (status %d)", st)
	}

	// Restore version 1 → the title reverts.
	postForm(t, c, base, "/__admin/c/articles/"+id+"/history/1/restore")
	rec, _ := db.FindOne(context.Background(), "articles", id)
	if rec["title"] != "Version A" {
		t.Fatalf("restore should revert the title to 'Version A', got %v", rec["title"])
	}
}

// ── small test helpers ─────────────────────────────────────────────────────────

// postForm submits a CSRF-protected POST to a panel path with no extra fields.
func postForm(t *testing.T, c *http.Client, base, path string) {
	t.Helper()
	tok := csrfToken(t, c, base)
	resp, err := c.PostForm(base+path, url.Values{"csrf": {tok}})
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	resp.Body.Close()
}

func isFilled(v any) bool {
	s, ok := v.(string)
	return ok && s != ""
}

// excerpt returns a window of body around the first occurrence of marker, for
// readable failure output.
func excerpt(body, marker string) string {
	i := strings.Index(body, marker)
	if i < 0 {
		return body[:min(400, len(body))]
	}
	start := max(0, i-80)
	end := min(len(body), i+320)
	return body[start:end]
}
