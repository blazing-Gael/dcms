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
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
	"github.com/blazing-Gael/dcms/pkg/auth"
)

// doReq sends a fully-formed request (so a test can set arbitrary headers) and
// returns status + decoded body.
func doReq(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func newReader(s string) io.Reader { return strings.NewReader(s) }

// headerAuthenticator is a bring-your-own Authenticator built ENTIRELY from the
// public pkg/auth — no gateway internals. It stands in for any external identity
// source (OIDC, a reverse proxy, Appwrite): it verifies a request however it
// likes and hands DCMS a Principal. Here "verification" is a trusted header,
// which is all the test needs to prove the seam (issue #1 / ADR-0020) works
// out-of-tree.
type headerAuthenticator struct{}

func (headerAuthenticator) Authenticate(r *http.Request) (auth.Principal, error) {
	id := r.Header.Get("X-Test-User")
	if id == "" {
		return auth.Principal{}, nil // anonymous, not an error
	}
	p := auth.Principal{ID: id, Authenticated: true, Claims: map[string]string{"src": "header"}}
	if r.Header.Get("X-Test-Admin") == "1" {
		p.Roles = []string{"admin"}
	} else {
		p.Roles = []string{"author"}
	}
	return p, nil
}

// TestExternalAuthenticator_DrivesAuthorization proves a host can satisfy the
// public Authenticator interface and have its verified Principal flow through the
// gateway's access: enforcement — the whole point of promoting the seam.
func TestExternalAuthenticator_DrivesAuthorization(t *testing.T) {
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
		Authenticator: headerAuthenticator{}, // bring-your-own, no session store
	}).Handler())
	t.Cleanup(srv.Close)

	// Anonymous create is refused (create needs a role).
	if st, _ := do(t, http.MethodPost, srv.URL+"/api/v1/articles", `{"title":"x"}`); st != http.StatusUnauthorized {
		t.Fatalf("anonymous create: got %d, want 401", st)
	}

	// An externally-authenticated author (via header) may create — the Principal
	// the host produced satisfied the [admin, author] create rule.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/articles", newReader(`{"title":"x"}`))
	req.Header.Set("X-Test-User", "ext-42")
	req.Header.Set("Content-Type", "application/json")
	st, body := doReq(t, req)
	if st != http.StatusCreated {
		t.Fatalf("external author create: got %d (%v), want 201", st, body)
	}

	// Delete needs role admin: the same external user without the admin header is
	// forbidden, proving roles from the external Principal are enforced.
	id := recordID(t, body)
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/articles/"+id, nil)
	req.Header.Set("X-Test-User", "ext-42")
	if st, _ := doReq(t, req); st != http.StatusForbidden {
		t.Fatalf("external non-admin delete: got %d, want 403", st)
	}

	// With the admin header the delete succeeds.
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/articles/"+id, nil)
	req.Header.Set("X-Test-User", "ext-42")
	req.Header.Set("X-Test-Admin", "1")
	if st, _ := doReq(t, req); st != http.StatusNoContent {
		t.Fatalf("external admin delete: got %d, want 204", st)
	}
}
