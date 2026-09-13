package gateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
)

// The shipped proxy_header authenticator (issue #9) drives access: enforcement:
// the roles it reads from the configured header gate writes exactly as any other
// principal's roles do. This exercises gateway.NewProxyHeaderAuthenticator itself,
// not a test double.
func TestProxyHeaderAuthenticator_DrivesAuthorization(t *testing.T) {
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
		Authenticator: gateway.NewProxyHeaderAuthenticator("X-Auth-User", "X-Auth-Roles", ""),
	}).Handler())
	t.Cleanup(srv.Close)

	// No identity header → anonymous → create refused.
	if st, _ := do(t, http.MethodPost, srv.URL+"/api/v1/articles", `{"title":"x"}`); st != http.StatusUnauthorized {
		t.Fatalf("anonymous create: got %d, want 401", st)
	}

	// Proxy asserts an identity with the author role → create allowed.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/articles", newReader(`{"title":"x"}`))
	req.Header.Set("X-Auth-User", "proxied-7")
	req.Header.Set("X-Auth-Roles", "author")
	req.Header.Set("Content-Type", "application/json")
	st, body := doReq(t, req)
	if st != http.StatusCreated {
		t.Fatalf("proxied author create: got %d (%v), want 201", st, body)
	}
	id := recordID(t, body)

	// author is not admin → delete (admin-only) forbidden.
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/articles/"+id, nil)
	req.Header.Set("X-Auth-User", "proxied-7")
	req.Header.Set("X-Auth-Roles", "author")
	if st, _ := doReq(t, req); st != http.StatusForbidden {
		t.Fatalf("proxied non-admin delete: got %d, want 403", st)
	}

	// Same identity with the admin role in the header → delete allowed.
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/articles/"+id, nil)
	req.Header.Set("X-Auth-User", "proxied-7")
	req.Header.Set("X-Auth-Roles", "admin")
	if st, _ := doReq(t, req); st != http.StatusNoContent {
		t.Fatalf("proxied admin delete: got %d, want 204", st)
	}
}
