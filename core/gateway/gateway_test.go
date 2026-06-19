package gateway_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/core/gateway"
	"github.com/blazing-Gael/dcms/core/schema"
	"github.com/blazing-Gael/dcms/core/store/sqlite"
)

const testSchema = `
version: "1"
collections:
  products:
    fields:
      title:
        type: string
        required: true
      slug:
        type: string
        unique: true
      price:
        type: number
      stock:
        type: integer
      status:
        type: enum
        values: [draft, active]
        default: draft
    indexes: [status]
`

// newTestServer builds a live HTTP server backed by an in-memory DB whose tables
// are migrated from testSchema.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	def, err := schema.Parse([]byte(testSchema))
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

	srv := httptest.NewServer(gateway.New(def, db, nil).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// do performs a request and returns status + decoded JSON body (or nil for 204).
func do(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
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
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s %s: %v", method, url, err)
	}
	return resp.StatusCode, out
}

func dataObj(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	d, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected object data, got %#v", body)
	}
	return d
}

func TestCRUDRoundTrip(t *testing.T) {
	srv := newTestServer(t)
	base := srv.URL + "/api/v1/products"

	// Create.
	status, body := do(t, http.MethodPost, base, `{"title":"Honey","slug":"honey","price":12.5}`)
	if status != http.StatusCreated {
		t.Fatalf("create status: got %d, want 201 (%v)", status, body)
	}
	rec := dataObj(t, body)
	id, _ := rec["id"].(string)
	if id == "" {
		t.Fatalf("create did not return an id: %#v", rec)
	}
	if rec["status"] != "draft" {
		t.Fatalf("enum default not applied: %#v", rec["status"])
	}

	// Get one.
	status, body = do(t, http.MethodGet, base+"/"+id, "")
	if status != http.StatusOK {
		t.Fatalf("get status: got %d, want 200", status)
	}
	if dataObj(t, body)["title"] != "Honey" {
		t.Fatalf("get title wrong: %#v", body)
	}

	// Update (PATCH).
	status, body = do(t, http.MethodPatch, base+"/"+id, `{"price":9.99}`)
	if status != http.StatusOK {
		t.Fatalf("update status: got %d, want 200 (%v)", status, body)
	}
	if dataObj(t, body)["price"] != 9.99 {
		t.Fatalf("update price wrong: %#v", body)
	}

	// List.
	status, body = do(t, http.MethodGet, base, "")
	if status != http.StatusOK {
		t.Fatalf("list status: got %d, want 200", status)
	}
	arr, ok := body["data"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("list data: %#v", body)
	}
	meta, _ := body["meta"].(map[string]any)
	if meta["total"].(float64) != 1 {
		t.Fatalf("list total: %#v", meta)
	}

	// Delete → 204, then 404.
	status, _ = do(t, http.MethodDelete, base+"/"+id, "")
	if status != http.StatusNoContent {
		t.Fatalf("delete status: got %d, want 204", status)
	}
	status, body = do(t, http.MethodGet, base+"/"+id, "")
	if status != http.StatusNotFound {
		t.Fatalf("get after delete: got %d, want 404 (%v)", status, body)
	}
}

func TestList_FilterAndSort(t *testing.T) {
	srv := newTestServer(t)
	base := srv.URL + "/api/v1/products"

	for _, b := range []string{
		`{"title":"A","price":30,"stock":5,"status":"active"}`,
		`{"title":"B","price":10,"stock":5,"status":"active"}`,
		`{"title":"C","price":20,"stock":0,"status":"draft"}`,
	} {
		if st, body := do(t, http.MethodPost, base, b); st != http.StatusCreated {
			t.Fatalf("seed create: %d %v", st, body)
		}
	}

	// filter[stock]=5 sorted by -price → A(30), B(10).
	status, body := do(t, http.MethodGet, base+"?filter[stock]=5&sort=-price", "")
	if status != http.StatusOK {
		t.Fatalf("status: %d", status)
	}
	arr := body["data"].([]any)
	if len(arr) != 2 {
		t.Fatalf("filtered len: got %d, want 2 (%v)", len(arr), body)
	}
	first := arr[0].(map[string]any)
	if first["title"] != "A" {
		t.Fatalf("sort order wrong, first = %#v", first["title"])
	}
}

func TestErrors(t *testing.T) {
	srv := newTestServer(t)
	base := srv.URL + "/api/v1/products"

	// Unknown collection → 404.
	if st, _ := do(t, http.MethodGet, srv.URL+"/api/v1/ghosts", ""); st != http.StatusNotFound {
		t.Fatalf("unknown collection: got %d, want 404", st)
	}

	// Duplicate unique slug → 409 CONFLICT.
	if st, body := do(t, http.MethodPost, base, `{"title":"X","slug":"dup"}`); st != http.StatusCreated {
		t.Fatalf("first create: %d %v", st, body)
	}
	st, body := do(t, http.MethodPost, base, `{"title":"Y","slug":"dup"}`)
	if st != http.StatusConflict {
		t.Fatalf("duplicate: got %d, want 409 (%v)", st, body)
	}
	if errObj, _ := body["error"].(map[string]any); errObj["code"] != "CONFLICT" {
		t.Fatalf("error code: %#v", body)
	}

	// Bad limit → 422 VALIDATION_ERROR.
	st, body = do(t, http.MethodGet, base+"?limit=abc", "")
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("bad limit: got %d, want 422 (%v)", st, body)
	}

	// Unknown filter field → 422.
	st, _ = do(t, http.MethodGet, base+"?filter[nope]=1", "")
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("unknown filter field: got %d, want 422", st)
	}

	// Malformed JSON body → 422.
	st, _ = do(t, http.MethodPost, base, `{not json`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("bad json: got %d, want 422", st)
	}

	// Missing required field → 422 with field-level message.
	st, body = do(t, http.MethodPost, base, `{"slug":"x"}`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("missing required: got %d, want 422 (%v)", st, body)
	}
	errObj, _ := body["error"].(map[string]any)
	fields, _ := errObj["fields"].(map[string]any)
	if fields["title"] == nil {
		t.Fatalf("expected field error for title: %v", body)
	}

	// Invalid enum value → 422.
	st, _ = do(t, http.MethodPost, base, `{"title":"X","slug":"y","price":1,"status":"banana"}`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("invalid enum: got %d, want 422", st)
	}

	// Wrong type (string for number) → 422.
	st, _ = do(t, http.MethodPost, base, `{"title":"X","slug":"z","price":"lots"}`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("wrong type: got %d, want 422", st)
	}
}

func TestProbesAndSchema(t *testing.T) {
	srv := newTestServer(t)

	if st, body := do(t, http.MethodGet, srv.URL+"/__health", ""); st != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("health: %d %v", st, body)
	}
	if st, body := do(t, http.MethodGet, srv.URL+"/__ready", ""); st != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("ready: %d %v", st, body)
	}

	st, body := do(t, http.MethodGet, srv.URL+"/__schema", "")
	if st != http.StatusOK {
		t.Fatalf("schema status: %d", st)
	}
	if body["version"] != "1" {
		t.Fatalf("schema version: %#v", body["version"])
	}
	cols, ok := body["collections"].([]any)
	if !ok || len(cols) != 1 {
		t.Fatalf("schema collections: %#v", body["collections"])
	}
}

func TestOpenAPIEndpoint(t *testing.T) {
	srv := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/__openapi", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /__openapi: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if etag := resp.Header.Get("ETag"); etag == "" {
		t.Fatal("expected an ETag (contract hash) header")
	}

	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi version: %v", doc["openapi"])
	}
	paths, _ := doc["paths"].(map[string]any)
	if _, ok := paths["/api/v1/products"]; !ok {
		t.Fatalf("missing products path in served spec: %v", paths)
	}
}

func TestDocsEndpoint(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get(srv.URL + "/__docs")
	if err != nil {
		t.Fatalf("GET /__docs: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type: got %q, want text/html", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	if !strings.Contains(body, "/__openapi") {
		t.Fatalf("docs page should reference /__openapi: %s", body)
	}
}
