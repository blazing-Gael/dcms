package gateway

import (
	"net/http"
	"testing"
)

func TestProxyHeaderAuthenticator(t *testing.T) {
	a := NewProxyHeaderAuthenticator("X-Auth-User", "X-Auth-Roles", "")

	// A verified identity header → an authenticated principal with parsed roles.
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Auth-User", "user-123")
	r.Header.Set("X-Auth-Roles", "admin, editor ,, viewer")
	p, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !p.Authenticated || p.ID != "user-123" {
		t.Fatalf("principal = %+v, want authenticated user-123", p)
	}
	// Roles are split on the separator, trimmed, and empties dropped.
	if got := p.Roles; len(got) != 3 || got[0] != "admin" || got[1] != "editor" || got[2] != "viewer" {
		t.Fatalf("roles = %v, want [admin editor viewer]", got)
	}

	// No identity header → anonymous, not an error (so public routes still work
	// and access rules make the deny decision).
	r2, _ := http.NewRequest(http.MethodGet, "/", nil)
	p2, err := a.Authenticate(r2)
	if err != nil || p2.Authenticated || p2.ID != "" {
		t.Fatalf("absent header should be anonymous, got %+v err=%v", p2, err)
	}
}

func TestProxyHeaderAuthenticator_NoRolesHeader(t *testing.T) {
	a := NewProxyHeaderAuthenticator("X-Auth-User", "", "")
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Auth-User", "svc")
	r.Header.Set("X-Auth-Roles", "admin") // ignored: no roles header configured
	p, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !p.Authenticated || p.ID != "svc" || len(p.Roles) != 0 {
		t.Fatalf("principal = %+v, want authenticated svc with no roles", p)
	}
}

// It satisfies the Authenticator seam.
var _ Authenticator = NewProxyHeaderAuthenticator("X-Auth-User", "", "")
