package gateway_test

import (
	"net/http"
	"strings"
	"testing"
)

const hardeningSchema = `
version: "1"
collections:
  widgets:
    fields:
      name: { type: string, required: true }
`

func TestReadHardening_ExpandBudget(t *testing.T) {
	def, db := newDB(t, hardeningSchema)
	srv := mount(t, def, db)
	api := srv.URL + "/api/v1/widgets"
	do(t, http.MethodPost, api, `{"name":"a"}`)

	// Too many expand fields → 422.
	many := make([]string, 25)
	for i := range many {
		many[i] = "f"
	}
	if st, _ := do(t, http.MethodGet, api+"?expand="+strings.Join(many, ","), ""); st != http.StatusUnprocessableEntity {
		t.Fatalf("over-budget expand (too many fields) should be 422, got %d", st)
	}
	// Too deep a path → 422.
	if st, _ := do(t, http.MethodGet, api+"?expand=a.b.c.d.e.f", ""); st != http.StatusUnprocessableEntity {
		t.Fatalf("over-deep expand path should be 422, got %d", st)
	}
}

func TestReadHardening_CountOptOut(t *testing.T) {
	def, db := newDB(t, hardeningSchema)
	srv := mount(t, def, db)
	api := srv.URL + "/api/v1/widgets"
	do(t, http.MethodPost, api, `{"name":"a"}`)
	do(t, http.MethodPost, api, `{"name":"b"}`)

	// Default: meta.total present.
	_, b := do(t, http.MethodGet, api, "")
	meta, _ := b["meta"].(map[string]any)
	if _, ok := meta["total"]; !ok {
		t.Fatalf("default list should report meta.total, got %v", meta)
	}
	// ?count=false: total omitted (no COUNT scan).
	_, b = do(t, http.MethodGet, api+"?count=false", "")
	meta, _ = b["meta"].(map[string]any)
	if _, ok := meta["total"]; ok {
		t.Fatalf("?count=false should omit meta.total, got %v", meta)
	}
	// A bad count value is a 422.
	if st, _ := do(t, http.MethodGet, api+"?count=maybe", ""); st != http.StatusUnprocessableEntity {
		t.Fatalf("invalid count should be 422, got %d", st)
	}
}

func TestReadHardening_ETagConditionalGet(t *testing.T) {
	def, db := newDB(t, hardeningSchema)
	srv := mount(t, def, db)
	api := srv.URL + "/api/v1/widgets"
	_, cb := do(t, http.MethodPost, api, `{"name":"a"}`)
	id := dataObj(t, cb)["id"].(string)

	// A GET carries an ETag.
	req, _ := http.NewRequest(http.MethodGet, api+"/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("GET response should carry an ETag")
	}

	// Re-requesting with If-None-Match → 304, no body.
	req, _ = http.NewRequest(http.MethodGet, api+"/"+id, nil)
	req.Header.Set("If-None-Match", etag)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("matching If-None-Match should yield 304, got %d", resp.StatusCode)
	}

	// After a change, the ETag differs and a conditional GET returns 200 again.
	do(t, http.MethodPatch, api+"/"+id, `{"name":"b"}`)
	req, _ = http.NewRequest(http.MethodGet, api+"/"+id, nil)
	req.Header.Set("If-None-Match", etag)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET after change: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stale If-None-Match should yield 200, got %d", resp.StatusCode)
	}
}
