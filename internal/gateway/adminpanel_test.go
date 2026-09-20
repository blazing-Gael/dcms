package gateway_test

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
)

const adminPanelSchema = `
version: "1"
auth:
  roles:
    admin:  { label: Admin }
    editor: { label: Editor }
collections:
  posts:
    events: true
    access:
      read:   authenticated
      create: authenticated
      update: authenticated
      delete: authenticated
    fields:
      title:  { type: string, required: true }
      body:   { type: text }
      status: { type: enum, values: [draft, live], default: draft }
`

func newAdminPanelServer(t *testing.T) (string, store.Adapter) {
	t.Helper()
	def, err := schema.Parse([]byte(adminPanelSchema))
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

// jarClient follows redirects and carries cookies, like a browser.
func jarClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{Jar: jar}
}

// csrfToken reads the double-submit CSRF cookie the panel set (its value is the
// token to echo in the form).
func csrfToken(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	u, _ := url.Parse(base + "/__admin/")
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == "dcms_admin_csrf" {
			return ck.Value
		}
	}
	t.Fatal("no CSRF cookie set")
	return ""
}

func getBody(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
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

func TestAdminPanel_LoginAndCRUD(t *testing.T) {
	base, db := newAdminPanelServer(t)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	c := jarClient(t)

	// Unauthenticated → redirected to the login page.
	st, body := getBody(t, c, base+"/__admin/")
	if st != http.StatusOK || !strings.Contains(body, "Sign in") {
		t.Fatalf("unauth root should land on login, got %d", st)
	}
	// Static assets serve.
	if st, _ := getBody(t, c, base+"/__admin/static/admin.css"); st != http.StatusOK {
		t.Fatalf("admin.css should serve, got %d", st)
	}

	// Log in (double-submit CSRF: echo the cookie value).
	token := csrfToken(t, c, base)
	resp, err := c.PostForm(base+"/__admin/login", url.Values{
		"email": {"admin@x.com"}, "password": {"correcthorse"}, "csrf": {token},
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()

	// Overview now renders and shows the posts collection.
	st, body = getBody(t, c, base+"/__admin/")
	if st != http.StatusOK || !strings.Contains(body, "Overview") || !strings.Contains(strings.ToLower(body), "posts") {
		t.Fatalf("overview after login: %d, body missing posts", st)
	}

	// Create a post through the panel.
	token = csrfToken(t, c, base)
	resp, err = c.PostForm(base+"/__admin/c/posts", url.Values{
		"title": {"Hello Admin"}, "body": {"first post"}, "status": {"draft"}, "csrf": {token},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	resp.Body.Close()

	// It persisted and shows in the list.
	page, _ := db.Find(context.Background(), store.Query{Collection: "posts", SkipCount: true})
	if len(page.Data) != 1 || page.Data[0]["title"] != "Hello Admin" {
		t.Fatalf("post not created via admin: %#v", page.Data)
	}
	if st, body := getBody(t, c, base+"/__admin/c/posts"); st != http.StatusOK || !strings.Contains(body, "Hello Admin") {
		t.Fatalf("list should show the new post, got %d", st)
	}
}

// login is a small helper that authenticates the jar client as email/password.
func adminLogin(t *testing.T, c *http.Client, base, email, password string) {
	t.Helper()
	getBody(t, c, base+"/__admin/login")
	tok := csrfToken(t, c, base)
	resp, err := c.PostForm(base+"/__admin/login", url.Values{"email": {email}, "password": {password}, "csrf": {tok}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
}

func TestAdminPanel_SystemViewsAndSchema(t *testing.T) {
	base, db := newAdminPanelServer(t)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	c := jarClient(t)
	adminLogin(t, c, base, "admin@x.com", "correcthorse")

	// The admin sees the System nav (users/events) and can open the views.
	st, body := getBody(t, c, base+"/__admin/")
	if !strings.Contains(body, "System") || !strings.Contains(body, "Data model") {
		t.Fatalf("admin overview should show the System nav (got status %d)", st)
	}
	for _, v := range []string{"events", "users", "sessions", "notifications", "webhooks"} {
		if st, _ := getBody(t, c, base+"/__admin/system/"+v); st != http.StatusOK {
			t.Fatalf("system view %s should be 200, got %d", v, st)
		}
	}
	// Users view lists the admin's own account.
	if st, body := getBody(t, c, base+"/__admin/system/users"); st != http.StatusOK || !strings.Contains(body, "admin@x.com") {
		t.Fatalf("users view should list admin@x.com, got %d", st)
	}
	// Data model page renders the posts collection.
	if st, body := getBody(t, c, base+"/__admin/schema"); st != http.StatusOK || !strings.Contains(strings.ToLower(body), "posts") {
		t.Fatalf("schema page should show posts, got %d", st)
	}
}

func TestAdminPanel_SystemViewsAdminOnly(t *testing.T) {
	base, db := newAdminPanelServer(t)
	seedUser(t, db, "ed@x.com", "correcthorse", "editor") // not admin
	c := jarClient(t)
	adminLogin(t, c, base, "ed@x.com", "correcthorse")

	// A non-admin does not see the System nav and is forbidden from its views.
	if _, body := getBody(t, c, base+"/__admin/"); strings.Contains(body, "/__admin/system/") {
		t.Fatal("non-admin should not see the System nav")
	}
	if st, _ := getBody(t, c, base+"/__admin/system/users"); st != http.StatusForbidden {
		t.Fatalf("non-admin system view should be 403, got %d", st)
	}
	if st, _ := getBody(t, c, base+"/__admin/schema"); st != http.StatusForbidden {
		t.Fatalf("non-admin schema page should be 403, got %d", st)
	}
}

func TestAdminPanel_CSRFRequired(t *testing.T) {
	base, db := newAdminPanelServer(t)
	seedUser(t, db, "admin@x.com", "correcthorse", "admin")
	c := jarClient(t)

	// Log in first.
	getBody(t, c, base+"/__admin/login")
	tok := csrfToken(t, c, base)
	resp, _ := c.PostForm(base+"/__admin/login", url.Values{"email": {"admin@x.com"}, "password": {"correcthorse"}, "csrf": {tok}})
	resp.Body.Close()

	// A create with a wrong CSRF token is refused, and nothing is written.
	resp, err := c.PostForm(base+"/__admin/c/posts", url.Values{"title": {"Nope"}, "csrf": {"wrong"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad CSRF should be 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	page, _ := db.Find(context.Background(), store.Query{Collection: "posts", SkipCount: true})
	if len(page.Data) != 0 {
		t.Fatalf("CSRF-rejected create must not persist, found %d", len(page.Data))
	}
}
