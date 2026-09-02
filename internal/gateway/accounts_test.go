package gateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
)

// newAccountsServer builds an auth-enabled server with the given account options
// merged in, and returns the server URL and the store (to seed users).
func newAccountsServer(t *testing.T, opts gateway.Options) (string, store.Adapter) {
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
	opts.Authenticator = gateway.NewSessionAuthenticator(db)
	srv := httptest.NewServer(gateway.New(def, db, nil, opts).Handler())
	t.Cleanup(srv.Close)
	return srv.URL, db
}

// tokenValid reports whether a session token still authenticates (GET /auth/me).
func tokenValid(t *testing.T, base, token string) bool {
	t.Helper()
	st, _ := doAs(t, http.MethodGet, base+"/auth/me", token, "")
	return st == http.StatusOK
}

func TestAccounts_RegistrationFlow(t *testing.T) {
	base, _ := newAccountsServer(t, gateway.Options{
		Registration: &gateway.RegistrationOptions{DefaultRoles: []string{"author"}},
	})

	// Register a new account → generic success.
	st, body := do(t, http.MethodPost, base+"/auth/register", `{"email":"a@x.com","password":"correcthorse","name":"Ada"}`)
	if st != http.StatusOK {
		t.Fatalf("register: %d %v", st, body)
	}
	// The account can now log in, and carries its default role.
	tok := login(t, base, "a@x.com", "correcthorse")
	st, me := doAs(t, http.MethodGet, base+"/auth/me", tok, "")
	if st != http.StatusOK {
		t.Fatalf("me: %d %v", st, me)
	}
	roles, _ := dataObj(t, me)["roles"].([]any)
	if len(roles) != 1 || roles[0] != "author" {
		t.Fatalf("registered user should have default role author, got %v", dataObj(t, me))
	}

	// A taken email returns the SAME success (enumeration-safe) and creates nothing new.
	if st, _ := do(t, http.MethodPost, base+"/auth/register", `{"email":"a@x.com","password":"different-pass"}`); st != http.StatusOK {
		t.Fatalf("re-register should be a generic 200, got %d", st)
	}
	// The original password still works (the second register didn't overwrite it).
	login(t, base, "a@x.com", "correcthorse")
}

func TestAccounts_RegistrationDisabledAndWeakPassword(t *testing.T) {
	// Disabled by default (no Registration option).
	base, _ := newAccountsServer(t, gateway.Options{})
	if st, _ := do(t, http.MethodPost, base+"/auth/register", `{"email":"a@x.com","password":"correcthorse"}`); st != http.StatusForbidden {
		t.Fatalf("registration should be 403 when disabled, got %d", st)
	}

	// Enabled but a too-short password is a 422.
	base2, _ := newAccountsServer(t, gateway.Options{Registration: &gateway.RegistrationOptions{}})
	if st, _ := do(t, http.MethodPost, base2+"/auth/register", `{"email":"a@x.com","password":"short"}`); st != http.StatusUnprocessableEntity {
		t.Fatalf("weak password should be 422, got %d", st)
	}
}

func TestAccounts_PasswordChangeRevokesOtherSessions(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{})
	seedUser(t, db, "u@x.com", "oldpassword", "author")

	other := login(t, base, "u@x.com", "oldpassword") // a second device
	current := login(t, base, "u@x.com", "oldpassword")

	st, body := doAs(t, http.MethodPost, base+"/auth/password", current, `{"current":"oldpassword","new":"newpassword1"}`)
	if st != http.StatusOK {
		t.Fatalf("password change: %d %v", st, body)
	}
	// Current device stays; the other device is revoked.
	if !tokenValid(t, base, current) {
		t.Fatal("the changing device's session should survive")
	}
	if tokenValid(t, base, other) {
		t.Fatal("other sessions should be revoked on password change")
	}
	// New password works, old one doesn't.
	login(t, base, "u@x.com", "newpassword1")
	if st, _ := do(t, http.MethodPost, base+"/auth/login", `{"email":"u@x.com","password":"oldpassword"}`); st != http.StatusUnauthorized {
		t.Fatalf("old password should no longer work, got %d", st)
	}

	// A wrong current password is a 401.
	if st, _ := doAs(t, http.MethodPost, base+"/auth/password", current, `{"current":"WRONG","new":"anotherpass1"}`); st != http.StatusUnauthorized {
		t.Fatalf("wrong current password should be 401, got %d", st)
	}
}

func TestAccounts_LogoutAll(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{})
	seedUser(t, db, "u@x.com", "password12", "author")
	a := login(t, base, "u@x.com", "password12")
	b := login(t, base, "u@x.com", "password12")

	if st, _ := doAs(t, http.MethodPost, base+"/auth/logout-all", a, ""); st != http.StatusNoContent {
		t.Fatalf("logout-all should be 204, got %d", st)
	}
	if tokenValid(t, base, a) || tokenValid(t, base, b) {
		t.Fatal("logout-all should revoke every session for the user")
	}
}

func TestAccounts_AdminUsersAPI(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{})
	seedUser(t, db, "admin@x.com", "adminpass1", "admin")
	seedUser(t, db, "author@x.com", "authorpass", "author")
	adminTok := login(t, base, "admin@x.com", "adminpass1")
	authorTok := login(t, base, "author@x.com", "authorpass")

	// A non-admin is forbidden.
	if st, _ := doAs(t, http.MethodGet, base+"/admin/users", authorTok, ""); st != http.StatusForbidden {
		t.Fatalf("non-admin listing should be 403, got %d", st)
	}
	// An anonymous caller is unauthorized.
	if st, _ := do(t, http.MethodGet, base+"/admin/users", ""); st != http.StatusUnauthorized {
		t.Fatalf("anonymous listing should be 401, got %d", st)
	}

	// Admin creates a user.
	st, body := doAs(t, http.MethodPost, base+"/admin/users", adminTok,
		`{"email":"new@x.com","password":"newpass123","roles":["author"],"name":"New"}`)
	if st != http.StatusCreated {
		t.Fatalf("admin create: %d %v", st, body)
	}
	newID, _ := body["data"].(map[string]any)["id"].(string)

	// An unknown role is rejected.
	if st, _ := doAs(t, http.MethodPost, base+"/admin/users", adminTok,
		`{"email":"bad@x.com","password":"newpass123","roles":["wizard"]}`); st != http.StatusUnprocessableEntity {
		t.Fatalf("unknown role should be 422, got %d", st)
	}

	// Disable the new user; they can no longer log in.
	if st, _ := doAs(t, http.MethodPatch, base+"/admin/users/"+newID, adminTok, `{"status":"disabled"}`); st != http.StatusOK {
		t.Fatalf("disable user: %d", st)
	}
	if st, _ := do(t, http.MethodPost, base+"/auth/login", `{"email":"new@x.com","password":"newpass123"}`); st != http.StatusUnauthorized {
		t.Fatalf("disabled user login should be 401, got %d", st)
	}

	// Admin force-logout revokes an author's live session.
	if st, _ := doAs(t, http.MethodPost, base+"/admin/users/"+userID(t, base, adminTok, "author@x.com")+"/logout-all", adminTok, ""); st != http.StatusNoContent {
		t.Fatalf("admin logout-all should be 204, got %d", st)
	}
	if tokenValid(t, base, authorTok) {
		t.Fatal("admin force-logout should have revoked the author's session")
	}

	// Delete the created user.
	if st, _ := doAs(t, http.MethodDelete, base+"/admin/users/"+newID, adminTok, ""); st != http.StatusNoContent {
		t.Fatalf("admin delete: %d", st)
	}
}

// A user with no local password (as an external/OIDC account will be) can never
// authenticate through the local login path — any password is a flat 401.
func TestAccounts_PasswordlessAccountCannotLoginLocally(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{})
	// Seed an account with NO password_hash, straight into the store.
	if _, err := db.Create(context.Background(), store.WriteInput{
		Collection: "_users",
		Data:       store.Record{"email": "ext@x.com", "roles": "[]"},
	}); err != nil {
		t.Fatalf("seed passwordless user: %v", err)
	}
	// (An empty password is a 422 field error before lookup; any real guess is 401.)
	for _, pw := range []string{"anything", "guess123", "x' OR '1'='1' --"} {
		if st, _ := do(t, http.MethodPost, base+"/auth/login", `{"email":"ext@x.com","password":"`+pw+`"}`); st != http.StatusUnauthorized {
			t.Fatalf("passwordless local login (pw=%q) should be 401, got %d", pw, st)
		}
	}
}

// Emails are case-insensitive: registering a case variant is a duplicate (not a
// second account), and login works regardless of the case typed.
func TestAccounts_EmailIsCaseInsensitive(t *testing.T) {
	base, _ := newAccountsServer(t, gateway.Options{Registration: &gateway.RegistrationOptions{}})

	if st, _ := do(t, http.MethodPost, base+"/auth/register", `{"email":"Casey@X.com","password":"correcthorse"}`); st != http.StatusOK {
		t.Fatalf("register: %d", st)
	}
	// Login with different casing still works.
	login(t, base, "casey@x.com", "correcthorse")
	login(t, base, "CASEY@X.COM", "correcthorse")

	// A garbage / header-injection email is rejected.
	if st, _ := do(t, http.MethodPost, base+"/auth/register", "{\"email\":\"a@x.com\\r\\nBcc: evil@x.com\",\"password\":\"correcthorse\"}"); st != http.StatusUnprocessableEntity {
		t.Fatalf("malformed email should be 422, got %d", st)
	}
}

// The last active admin cannot be demoted, disabled, or deleted — there must
// always be someone who can administer.
func TestAccounts_LastAdminIsProtected(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{})
	seedUser(t, db, "admin@x.com", "adminpass1", "admin")
	adminTok := login(t, base, "admin@x.com", "adminpass1")
	adminID := userID(t, base, adminTok, "admin@x.com")

	// Demote / disable / delete the only admin → 409.
	if st, _ := doAs(t, http.MethodPatch, base+"/admin/users/"+adminID, adminTok, `{"roles":[]}`); st != http.StatusConflict {
		t.Fatalf("demoting the last admin should be 409, got %d", st)
	}
	if st, _ := doAs(t, http.MethodPatch, base+"/admin/users/"+adminID, adminTok, `{"status":"disabled"}`); st != http.StatusConflict {
		t.Fatalf("disabling the last admin should be 409, got %d", st)
	}
	if st, _ := doAs(t, http.MethodDelete, base+"/admin/users/"+adminID, adminTok, ""); st != http.StatusConflict {
		t.Fatalf("deleting the last admin should be 409, got %d", st)
	}

	// With a second admin present, demoting the first is allowed.
	if st, _ := doAs(t, http.MethodPost, base+"/admin/users", adminTok,
		`{"email":"admin2@x.com","password":"adminpass2","roles":["admin"]}`); st != http.StatusCreated {
		t.Fatalf("create second admin: %d", st)
	}
	if st, _ := doAs(t, http.MethodPatch, base+"/admin/users/"+adminID, adminTok, `{"roles":[]}`); st != http.StatusOK {
		t.Fatalf("demoting one of two admins should be 200, got %d", st)
	}
}

// userID looks up a user's id via the admin list (small test helper).
func userID(t *testing.T, base, adminTok, email string) string {
	t.Helper()
	_, body := doAs(t, http.MethodGet, base+"/admin/users", adminTok, "")
	list, _ := body["data"].([]any)
	for _, u := range list {
		m, _ := u.(map[string]any)
		if m["email"] == email {
			id, _ := m["id"].(string)
			return id
		}
	}
	t.Fatalf("user %s not found in admin list", email)
	return ""
}
