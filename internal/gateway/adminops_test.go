package gateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
)

// Admin panel ops hardening: role allowlist for the whole panel, If-Match on panel
// updates, unlock a locked account (#50), read-only richtext rendering, strict CSP.

const adminOpsSchema = `
version: "1"
auth:
  roles:
    admin:  { label: Admin }
    editor: { label: Editor }
collections:
  notes:
    concurrency: true
    access:
      read:   authenticated
      create: authenticated
      update: authenticated
      delete: authenticated
    fields:
      title: { type: string, required: true }
      body:  { type: richtext }
`

func newAdminOpsServer(t *testing.T, adminRoles []string) (string, store.Adapter) {
	t.Helper()
	def, err := schema.Parse([]byte(adminOpsSchema))
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
	opts := gateway.Options{Authenticator: gateway.NewSessionAuthenticator(db)}
	if adminRoles != nil {
		opts.Admin = &gateway.AdminOptions{Enabled: true, Roles: adminRoles}
	}
	srv := httptest.NewServer(gateway.New(def, db, nil, opts).Handler())
	t.Cleanup(srv.Close)
	return srv.URL, db
}

// postLogin submits the login form and returns status + body (following redirects).
func postLogin(t *testing.T, c *http.Client, base, email, password string) (int, string) {
	t.Helper()
	getBody(t, c, base+"/__admin/login")
	tok := csrfToken(t, c, base)
	resp, err := c.PostForm(base+"/__admin/login", url.Values{"email": {email}, "password": {password}, "csrf": {tok}})
	if err != nil {
		t.Fatalf("login post: %v", err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

func TestAdminPanel_RoleAllowlist(t *testing.T) {
	base, db := newAdminOpsServer(t, []string{"admin"})
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	seedUser(t, db, "writer@x.com", "correcthorse", "editor") // not on the allowlist

	// A writer is refused at login and never gets into the panel.
	cw := jarClient(t)
	_, body := postLogin(t, cw, base, "writer@x.com", "correcthorse")
	if !strings.Contains(body, "permitted to use the admin panel") {
		t.Fatalf("writer login should be refused by the allowlist; body:\n%s", body[:min(300, len(body))])
	}
	if st, b := getBody(t, cw, base+"/__admin/"); st != http.StatusOK || !strings.Contains(b, "Sign in") {
		t.Fatalf("refused writer should land back on login, got %d", st)
	}

	// An admin gets in.
	ca := jarClient(t)
	postLogin(t, ca, base, "admin@x.com", "correcthorse")
	if st, b := getBody(t, ca, base+"/__admin/"); st != http.StatusOK || !strings.Contains(b, "Overview") {
		t.Fatalf("admin should reach the overview, got %d", st)
	}
}

func TestAdminPanel_IfMatchGuardsConcurrentEdit(t *testing.T) {
	base, db := newAdminOpsServer(t, nil)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	c := jarClient(t)
	adminLogin(t, c, base, "admin@x.com", "correcthorse")

	// Create a note through the panel.
	tok := csrfToken(t, c, base)
	resp, _ := c.PostForm(base+"/__admin/c/notes", url.Values{"title": {"v0"}, "csrf": {tok}})
	resp.Body.Close()
	page, _ := db.Find(context.Background(), store.Query{Collection: "notes", SkipCount: true})
	id, _ := page.Data[0]["id"].(string)
	loaded := adminIntVersion(t, db, id) // the version the "form" was loaded at

	// First save at the loaded version succeeds (bumps the version).
	tok = csrfToken(t, c, base)
	resp, _ = c.PostForm(base+"/__admin/c/notes/"+id, url.Values{
		"title": {"first"}, "_version": {itoa(loaded)}, "csrf": {tok},
	})
	resp.Body.Close()

	// A second save at the *stale* version must be refused, not overwrite "first".
	tok = csrfToken(t, c, base)
	resp, err := c.PostForm(base+"/__admin/c/notes/"+id, url.Values{
		"title": {"second"}, "_version": {itoa(loaded)}, "csrf": {tok},
	})
	if err != nil {
		t.Fatalf("stale update: %v", err)
	}
	body := readAll(resp)
	if !strings.Contains(strings.ToLower(body), "someone else changed this record") {
		t.Fatalf("stale save should warn about a concurrent change; body:\n%s", body[:min(300, len(body))])
	}
	rec, _ := db.FindOne(context.Background(), "notes", id)
	if rec["title"] != "first" {
		t.Fatalf("stale save must not clobber; title = %v, want 'first'", rec["title"])
	}
}

func TestAdminPanel_UnlockAccount(t *testing.T) {
	base, db := newAdminOpsServer(t, nil)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	seedUser(t, db, "ed@x.com", "correcthorse", "editor")
	victim, _ := s0FindUser(t, db, "ed@x.com")
	vid, _ := victim["id"].(string)

	// Lock the editor out with a burst of wrong passwords (past the #50 threshold).
	cbad := jarClient(t)
	for range 12 {
		postLogin(t, cbad, base, "ed@x.com", "wrong-pass")
	}
	// Even the correct password is now refused (account locked).
	if _, body := postLogin(t, jarClient(t), base, "ed@x.com", "correcthorse"); !strings.Contains(body, "Invalid email or password") {
		t.Fatal("locked account should be refused even with the right password")
	}

	// An admin unlocks the account.
	ca := jarClient(t)
	adminLogin(t, ca, base, "admin@x.com", "correcthorse")
	tok := csrfToken(t, ca, base)
	resp, err := ca.PostForm(base+"/__admin/users/"+vid+"/unlock", url.Values{"csrf": {tok}})
	if err != nil {
		t.Fatalf("unlock: %v", err)
	}
	resp.Body.Close()

	// The editor can sign in again.
	if st, _ := getBody(t, jarClient(t), base+"/__admin/login"); st != http.StatusOK {
		t.Fatalf("login page should render, got %d", st)
	}
	cv := jarClient(t)
	adminLogin(t, cv, base, "ed@x.com", "correcthorse")
	if st, _ := getBody(t, cv, base+"/__admin/"); st != http.StatusOK {
		t.Fatalf("unlocked editor should sign in, got %d", st)
	}
}

func TestAdminPanel_ReadOnlyRichText(t *testing.T) {
	base, db := newAdminOpsServer(t, nil)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	// Seed a note with a richtext body directly (richtext isn't a form widget yet).
	body := `[{"_type":"block","style":"normal","children":[{"_type":"span","text":"Hello moderators","marks":["strong"]}]}]`
	rec, err := db.Create(context.Background(), store.WriteInput{
		Collection: "notes", Data: store.Record{"title": "Story", "body": body},
	})
	if err != nil {
		t.Fatalf("seed note: %v", err)
	}
	id, _ := rec["id"].(string)

	c := jarClient(t)
	adminLogin(t, c, base, "admin@x.com", "correcthorse")
	st, page := getBody(t, c, base+"/__admin/c/notes/"+id)
	if st != http.StatusOK {
		t.Fatalf("edit form status %d", st)
	}
	if !strings.Contains(page, "read-only") || !strings.Contains(page, "<strong>Hello moderators</strong>") {
		t.Fatalf("richtext should render read-only with formatting; body:\n%s", excerpt(page, "moderators"))
	}
}

func TestAdminPanel_SecurityHeaders(t *testing.T) {
	base, db := newAdminOpsServer(t, nil)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	c := jarClient(t)
	resp, err := c.Get(base + "/__admin/login")
	if err != nil {
		t.Fatalf("GET login: %v", err)
	}
	defer resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("strict CSP missing; got %q", csp)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("nosniff header missing")
	}
	if !strings.Contains(resp.Header.Get("X-Robots-Tag"), "noindex") {
		t.Fatal("noindex header missing")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func adminIntVersion(t *testing.T, db store.Adapter, id string) int64 {
	t.Helper()
	rec, err := db.FindOne(context.Background(), "notes", id)
	if err != nil {
		t.Fatalf("find note: %v", err)
	}
	switch v := rec[schema.ConcurrencyVersion].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	default:
		return 0
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
