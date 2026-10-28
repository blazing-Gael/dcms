package gateway

import (
	"net/http"
	"net/netip"
	"testing"
)

func TestProxyHeaderAuthenticator(t *testing.T) {
	a := NewProxyHeaderAuthenticator("X-Auth-User", "X-Auth-Roles", "", nil)

	// A verified identity header → an authenticated principal with parsed roles.
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:5000" // the auth proxy
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
	r2.RemoteAddr = "127.0.0.1:5000"
	p2, err := a.Authenticate(r2)
	if err != nil || p2.Authenticated || p2.ID != "" {
		t.Fatalf("absent header should be anonymous, got %+v err=%v", p2, err)
	}
}

func TestProxyHeaderAuthenticator_NoRolesHeader(t *testing.T) {
	a := NewProxyHeaderAuthenticator("X-Auth-User", "", "", nil)
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.2:5000"
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

// Identity headers are believed only from a trusted proxy: a caller reaching the
// port directly (a public peer) who sets X-Auth-User stays anonymous, and an
// operator-configured range replaces the default.
func TestProxyHeaderAuthenticator_UntrustedPeerIgnored(t *testing.T) {
	impersonate := func(a Authenticator, peer string) principal {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = peer
		r.Header.Set("X-Auth-User", "admin-id")
		r.Header.Set("X-Auth-Roles", "admin")
		p, err := a.Authenticate(r)
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		return p
	}

	def := NewProxyHeaderAuthenticator("X-Auth-User", "X-Auth-Roles", "", nil)
	if p := impersonate(def, "203.0.113.9:4444"); p.Authenticated {
		t.Fatalf("header from a public peer must be ignored, got %+v", p)
	}
	if p := impersonate(def, "garbage"); p.Authenticated {
		t.Fatalf("header from an unparseable peer must be ignored, got %+v", p)
	}

	// Auth proxy on a public address: the operator lists it, and only it.
	custom := NewProxyHeaderAuthenticator("X-Auth-User", "X-Auth-Roles", "", []netip.Prefix{netip.MustParsePrefix("198.51.100.7/32")})
	if p := impersonate(custom, "198.51.100.7:4444"); !p.Authenticated {
		t.Fatal("listed proxy should be believed")
	}
	if p := impersonate(custom, "127.0.0.1:4444"); p.Authenticated {
		t.Fatal("a configured list replaces the default; loopback is no longer trusted")
	}
}

// It satisfies the Authenticator seam.
var _ Authenticator = NewProxyHeaderAuthenticator("X-Auth-User", "", "", nil)
