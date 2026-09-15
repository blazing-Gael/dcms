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

const introSchema = `
version: "1"
auth:
  roles:
    admin: { label: Administrator }
  session:
    ttl: 1h
collections:
  posts:
    fields:
      title: { type: string, required: true }
`

func introServer(t *testing.T, mode string) (string, string) {
	t.Helper()
	def, err := schema.Parse([]byte(introSchema))
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
		plan, _ := db.Diff(ctx, meta)
		if err := db.Migrate(ctx, plan); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	srv := httptest.NewServer(gateway.New(def, db, nil, gateway.Options{
		Authenticator: gateway.NewSessionAuthenticator(db),
		Introspection: mode,
	}).Handler())
	t.Cleanup(srv.Close)
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")
	return srv.URL, login(t, srv.URL, "boss@x.com", "pw-boss-1234")
}

func TestIntrospection_Public(t *testing.T) {
	url, _ := introServer(t, "") // default = public
	for _, p := range []string{"/__schema", "/__openapi", "/__docs"} {
		resp, err := http.Get(url + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("public %s: got %d, want 200", p, resp.StatusCode)
		}
		// Always noindex, even when public.
		if got := resp.Header.Get("X-Robots-Tag"); got == "" {
			t.Errorf("%s missing X-Robots-Tag noindex header", p)
		}
	}
	// Probes are never gated and carry no such restriction concern.
	if resp, _ := http.Get(url + "/__health"); resp != nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("/__health should be 200, got %d", resp.StatusCode)
		}
	}
}

func TestIntrospection_Admin(t *testing.T) {
	url, admin := introServer(t, "admin")
	// Anonymous is refused.
	if st, _ := do(t, http.MethodGet, url+"/__schema", ""); st != http.StatusUnauthorized {
		t.Errorf("anon /__schema under admin mode: got %d, want 401", st)
	}
	// An admin passes.
	if st, _ := doAs(t, http.MethodGet, url+"/__schema", admin, ""); st != http.StatusOK {
		t.Errorf("admin /__schema: got %d, want 200", st)
	}
}

func TestIntrospection_Off(t *testing.T) {
	url, admin := introServer(t, "off")
	// 404 even for an admin — the route is not addressable.
	for _, tok := range []string{"", admin} {
		if st, _ := doAs(t, http.MethodGet, url+"/__openapi", tok, ""); st != http.StatusNotFound {
			t.Errorf("/__openapi under off mode (tok=%q): got %d, want 404", tok, st)
		}
	}
	// Probes still work.
	if resp, _ := http.Get(url + "/__ready"); resp != nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("/__ready should not be gated, got %d", resp.StatusCode)
		}
	}
}
