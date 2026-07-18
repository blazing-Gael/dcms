package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
)

const authSchema = `
version: "1"
auth:
  roles:
    admin:  { label: Administrator }
    author: { label: Author }
  session:
    ttl: 1h
collections:
  articles:
    fields:
      title: { type: string, required: true }
    access:
      read:   public
      create: [admin, author]
      update: owner
      delete: [admin]
  notes:
    fields:
      body: string
    access:
      read:   owner
      create: authenticated
`

// newAuthServer builds a live server with the session authenticator wired on, so
// the access: rules are actually enforced. It returns the server and the store so
// a test can seed users directly (there is no users API in M1).
func newAuthServer(t *testing.T) (*httptest.Server, store.Adapter) {
	t.Helper()
	def, err := schema.Parse([]byte(authSchema))
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
	return srv, db
}

func seedUser(t *testing.T, db store.Adapter, email, password string, roles ...string) {
	t.Helper()
	if _, err := gateway.CreateUser(context.Background(), db, email, password, roles); err != nil {
		t.Fatalf("CreateUser %s: %v", email, err)
	}
}

// login authenticates and returns the opaque session token.
func login(t *testing.T, base, email, password string) string {
	t.Helper()
	st, body := do(t, http.MethodPost, base+"/auth/login",
		`{"email":"`+email+`","password":"`+password+`"}`)
	if st != http.StatusOK {
		t.Fatalf("login %s: status %d (%v)", email, st, body)
	}
	tok, _ := body["token"].(string)
	if tok == "" {
		t.Fatalf("login %s: no token in %v", email, body)
	}
	return tok
}

// doAs performs a request with a bearer token and returns status + decoded body.
func doAs(t *testing.T, method, url, token, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func recordID(t *testing.T, body map[string]any) string {
	t.Helper()
	data, _ := body["data"].(map[string]any)
	id, _ := data["id"].(string)
	if id == "" {
		t.Fatalf("no data.id in %v", body)
	}
	return id
}

func TestAuth_AnonymousWriteRejected(t *testing.T) {
	srv, _ := newAuthServer(t)
	// A default-authenticated write with no identity is 401 (create rule needs a role).
	st, _ := do(t, http.MethodPost, srv.URL+"/api/v1/articles", `{"title":"hi"}`)
	if st != http.StatusUnauthorized {
		t.Fatalf("anonymous create: got %d, want 401", st)
	}
}

func TestAuth_PublicReadAllowed(t *testing.T) {
	srv, _ := newAuthServer(t)
	// articles.read is public → anonymous list works.
	st, _ := do(t, http.MethodGet, srv.URL+"/api/v1/articles", "")
	if st != http.StatusOK {
		t.Fatalf("anonymous read: got %d, want 200", st)
	}
}

func TestAuth_LoginBadCredentials(t *testing.T) {
	srv, db := newAuthServer(t)
	seedUser(t, db, "a@x.com", "correct-horse", "author")
	st, _ := do(t, http.MethodPost, srv.URL+"/auth/login", `{"email":"a@x.com","password":"wrong"}`)
	if st != http.StatusUnauthorized {
		t.Fatalf("bad password: got %d, want 401", st)
	}
	st, _ = do(t, http.MethodPost, srv.URL+"/auth/login", `{"email":"nobody@x.com","password":"x"}`)
	if st != http.StatusUnauthorized {
		t.Fatalf("unknown email: got %d, want 401", st)
	}
}

func TestAuth_RoleGatedCreate(t *testing.T) {
	srv, db := newAuthServer(t)
	seedUser(t, db, "author@x.com", "pw-author-123", "author")
	seedUser(t, db, "reader@x.com", "pw-reader-123") // no roles

	authorTok := login(t, srv.URL, "author@x.com", "pw-author-123")
	readerTok := login(t, srv.URL, "reader@x.com", "pw-reader-123")

	// author holds a listed role → 201.
	if st, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/articles", authorTok, `{"title":"by author"}`); st != http.StatusCreated {
		t.Fatalf("author create: got %d (%v), want 201", st, body)
	}
	// authenticated but role-less → 403 (not 401: they ARE authenticated).
	if st, _ := doAs(t, http.MethodPost, srv.URL+"/api/v1/articles", readerTok, `{"title":"by reader"}`); st != http.StatusForbidden {
		t.Fatalf("reader create: got %d, want 403", st)
	}
}

func TestAuth_OwnerUpdateAndDelete(t *testing.T) {
	srv, db := newAuthServer(t)
	seedUser(t, db, "a1@x.com", "pw-a1-123456", "author")
	seedUser(t, db, "a2@x.com", "pw-a2-123456", "author")
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")

	a1 := login(t, srv.URL, "a1@x.com", "pw-a1-123456")
	a2 := login(t, srv.URL, "a2@x.com", "pw-a2-123456")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-1234")

	_, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/articles", a1, `{"title":"a1 owns this"}`)
	id := recordID(t, body)

	// update is owner-scoped: the non-owner author is 403, the owner is 200.
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/articles/"+id, a2, `{"title":"hijack"}`); st != http.StatusForbidden {
		t.Fatalf("non-owner update: got %d, want 403", st)
	}
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/articles/"+id, a1, `{"title":"owner edit"}`); st != http.StatusOK {
		t.Fatalf("owner update: got %d, want 200", st)
	}
	// delete needs admin: the author (even the owner) is 403, admin is 204.
	if st, _ := doAs(t, http.MethodDelete, srv.URL+"/api/v1/articles/"+id, a1, ""); st != http.StatusForbidden {
		t.Fatalf("author delete: got %d, want 403", st)
	}
	if st, _ := doAs(t, http.MethodDelete, srv.URL+"/api/v1/articles/"+id, boss, ""); st != http.StatusNoContent {
		t.Fatalf("admin delete: got %d, want 204", st)
	}
}

func TestAuth_OwnerReadScoping(t *testing.T) {
	srv, db := newAuthServer(t)
	seedUser(t, db, "u1@x.com", "pw-u1-1234567")
	seedUser(t, db, "u2@x.com", "pw-u2-1234567")
	u1 := login(t, srv.URL, "u1@x.com", "pw-u1-1234567")
	u2 := login(t, srv.URL, "u2@x.com", "pw-u2-1234567")

	// notes.create is authenticated; notes.read is owner.
	_, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/notes", u1, `{"body":"u1 secret"}`)
	noteID := recordID(t, body)

	// u2's list is scoped to u2's own rows → u1's note is absent.
	st, list := doAs(t, http.MethodGet, srv.URL+"/api/v1/notes", u2, "")
	if st != http.StatusOK {
		t.Fatalf("u2 list: got %d, want 200", st)
	}
	if data, _ := list["data"].([]any); len(data) != 0 {
		t.Fatalf("u2 should see none of u1's notes, got %v", data)
	}
	// u2 fetching u1's note by id is 404 (owner boundary must not leak existence).
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/notes/"+noteID, u2, ""); st != http.StatusNotFound {
		t.Fatalf("u2 get u1 note: got %d, want 404", st)
	}
	// u1 sees their own note.
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/notes/"+noteID, u1, ""); st != http.StatusOK {
		t.Fatalf("u1 get own note: got %d, want 200", st)
	}
}

func TestAuth_MeAndLogout(t *testing.T) {
	srv, db := newAuthServer(t)
	seedUser(t, db, "me@x.com", "pw-me-1234567", "author")
	tok := login(t, srv.URL, "me@x.com", "pw-me-1234567")

	st, body := doAs(t, http.MethodGet, srv.URL+"/auth/me", tok, "")
	if st != http.StatusOK {
		t.Fatalf("me: got %d, want 200", st)
	}
	data, _ := body["data"].(map[string]any)
	if data["email"] != "me@x.com" {
		t.Fatalf("me email: %v", data["email"])
	}
	if _, leaked := data["password_hash"]; leaked {
		t.Fatalf("password_hash leaked in /auth/me: %v", data)
	}

	// logout revokes the session → the same token is now anonymous.
	if st, _ := doAs(t, http.MethodPost, srv.URL+"/auth/logout", tok, ""); st != http.StatusNoContent {
		t.Fatalf("logout: got %d, want 204", st)
	}
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/auth/me", tok, ""); st != http.StatusUnauthorized {
		t.Fatalf("me after logout: got %d, want 401", st)
	}
}

func TestAuth_LoginResponseHasNoHash(t *testing.T) {
	srv, db := newAuthServer(t)
	seedUser(t, db, "u@x.com", "pw-u-12345678", "admin")
	_, body := do(t, http.MethodPost, srv.URL+"/auth/login", `{"email":"u@x.com","password":"pw-u-12345678"}`)
	user, _ := body["user"].(map[string]any)
	if user == nil {
		t.Fatalf("login response missing user: %v", body)
	}
	if _, leaked := user["password_hash"]; leaked {
		t.Fatalf("password_hash leaked in login response: %v", user)
	}
	roles, _ := user["roles"].([]any)
	if len(roles) != 1 || roles[0] != "admin" {
		t.Fatalf("login user roles: %v", user["roles"])
	}
}
