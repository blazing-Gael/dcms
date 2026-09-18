package dcms

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/engine"
	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/store"
)

// newTestApp writes a throwaway schema, builds an App pointed at a temp DB, and
// migrates it — the library path as an embedder would use it, minus the blocking
// Serve (we exercise the handler directly so the test needs no port).
func newTestApp(t *testing.T) (*App, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	schemaPath := dir + "/schema.yaml"
	if err := os.WriteFile(schemaPath, []byte(`version: "1"
collections:
  notes:
    access:
      read:   public
      create: public
    fields:
      title: { type: string, required: true }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	app, err := New(Options{SchemaPath: schemaPath, DBPath: dir + "/test.db", AutoMigrate: true, Dev: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	if err := engine.Apply(context.Background(), app.db, app.def); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return app, nil
}

// handler builds the real gateway handler for the app (what Serve mounts), so a
// test can drive hooks and routes over HTTP without binding a port.
func (a *App) handler(t *testing.T) http.Handler {
	t.Helper()
	opts, _, err := a.gatewayOptions(context.Background())
	if err != nil {
		t.Fatalf("gatewayOptions: %v", err)
	}
	return gateway.New(a.def, a.db, a.logger, opts).Handler()
}

func TestApp_HookFiresThroughLibraryPath(t *testing.T) {
	app, _ := newTestApp(t)

	var ran bool
	app.On("notes", BeforeCreate, func(ctx context.Context, hc HookContext, rec Record) (Record, error) {
		ran = true
		rec["title"] = rec["title"].(string) + "!" // prove mutation flows through
		return rec, nil
	})

	srv := httptest.NewServer(app.handler(t))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/api/v1/notes", "application/json", strings.NewReader(`{"title":"hi"}`))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	data, _ := body["data"].(map[string]any)
	if !ran {
		t.Fatal("BeforeCreate hook did not run through the library path")
	}
	if data["title"] != "hi!" {
		t.Fatalf("hook mutation not applied: %v", data["title"])
	}
}

func TestApp_CustomRoute(t *testing.T) {
	app, _ := newTestApp(t)

	app.Route("GET", "/ping", func(req *Request) {
		// Principal is resolvable (anonymous here) and Store is usable.
		_, _ = req.W.Write([]byte("pong"))
	})
	app.Route("POST", "/echo", func(req *Request) {
		b, _ := io.ReadAll(req.R.Body)
		req.W.WriteHeader(http.StatusAccepted)
		_, _ = req.W.Write(b)
	})

	srv := httptest.NewServer(app.handler(t))
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/ping")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "pong" {
		t.Fatalf("GET /ping = %d %q", resp.StatusCode, b)
	}

	resp2, _ := http.Post(srv.URL+"/echo", "text/plain", strings.NewReader("hello"))
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted || string(b2) != "hello" {
		t.Fatalf("POST /echo = %d %q", resp2.StatusCode, b2)
	}
}

func TestApp_CustomRouteStoreAccess(t *testing.T) {
	app, _ := newTestApp(t)

	// A custom route that reads through its Store handle — proves the embedder can
	// query DCMS data from a bespoke endpoint.
	app.Route("GET", "/count", func(req *Request) {
		page, err := req.Store.Find(req.R.Context(), store.Query{Collection: "notes", SkipCount: true})
		if err != nil {
			req.W.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = req.W.Write([]byte(strconv.Itoa(len(page.Data))))
	})

	srv := httptest.NewServer(app.handler(t))
	defer srv.Close()

	// seed one note, then the custom route should count it
	http.Post(srv.URL+"/api/v1/notes", "application/json", strings.NewReader(`{"title":"a"}`))
	resp, _ := http.Get(srv.URL + "/count")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "1" {
		t.Fatalf("/count = %q, want 1", b)
	}
}

func TestApp_MissingExplicitConfigIsError(t *testing.T) {
	if _, err := New(Options{ConfigPath: "does-not-exist.yaml", ConfigRequired: true}); err == nil {
		t.Fatal("an explicit missing config should error")
	}
}
