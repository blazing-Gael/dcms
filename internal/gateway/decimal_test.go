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

const decimalSchema = `
version: "1"
collections:
  orders:
    fields:
      label:
        type: string
        required: true
      total:
        type: decimal
      weight:
        type: decimal
        scale: 3
`

// newSchemaServer builds a live server from an arbitrary schema (the shared
// newTestServer is pinned to testSchema).
func newSchemaServer(t *testing.T, src string) *httptest.Server {
	t.Helper()
	def, err := schema.Parse([]byte(src))
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

func TestDecimalWireRoundTrip(t *testing.T) {
	srv := newSchemaServer(t, decimalSchema)
	base := srv.URL + "/api/v1/orders"

	// Create with exact decimal strings, at both the default and a custom scale.
	status, body := do(t, http.MethodPost, base, `{"label":"a","total":"12.50","weight":"0.001"}`)
	if status != http.StatusCreated {
		t.Fatalf("create status: got %d, want 201 (%v)", status, body)
	}
	rec := dataObj(t, body)
	if rec["total"] != "12.50" {
		t.Fatalf("total round-trip: got %#v, want \"12.50\"", rec["total"])
	}
	if rec["weight"] != "0.001" {
		t.Fatalf("weight round-trip: got %#v, want \"0.001\"", rec["weight"])
	}
	id, _ := rec["id"].(string)

	// A value the float type would corrupt survives exactly, at the full scale.
	status, body = do(t, http.MethodPatch, base+"/"+id, `{"total":"0.30"}`)
	if status != http.StatusOK {
		t.Fatalf("update status: got %d, want 200 (%v)", status, body)
	}
	if dataObj(t, body)["total"] != "0.30" {
		t.Fatalf("exact decimal not preserved: %#v", dataObj(t, body)["total"])
	}

	// Read back is still the exact string.
	_, body = do(t, http.MethodGet, base+"/"+id, "")
	if dataObj(t, body)["total"] != "0.30" {
		t.Fatalf("get total: %#v", dataObj(t, body)["total"])
	}
}

func TestDecimalRejectsJSONNumber(t *testing.T) {
	srv := newSchemaServer(t, decimalSchema)
	base := srv.URL + "/api/v1/orders"

	// A bare JSON number is already a lossy float; the API must refuse it.
	status, body := do(t, http.MethodPost, base, `{"label":"a","total":12.5}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("json number: got %d, want 422 (%v)", status, body)
	}
	// So is more precision than the scale allows — never silently rounded.
	status, _ = do(t, http.MethodPost, base, `{"label":"a","total":"12.505"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("excess precision: got %d, want 422", status)
	}
}

func TestDecimalRangeFilterIsNumeric(t *testing.T) {
	srv := newSchemaServer(t, decimalSchema)
	base := srv.URL + "/api/v1/orders"

	// Seed three prices whose lexical order differs from numeric order — the exact
	// case a TEXT column would sort wrong ("9.90" > "12.50" as text).
	for _, p := range []string{"9.90", "12.50", "100.00"} {
		status, body := do(t, http.MethodPost, base, `{"label":"x","total":"`+p+`"}`)
		if status != http.StatusCreated {
			t.Fatalf("seed %s: got %d (%v)", p, status, body)
		}
	}

	// gte 10.00 must match 12.50 and 100.00 but not 9.90 — a numeric comparison.
	status, body := do(t, http.MethodGet, base+"?filter[total][gte]=10.00&sort=total", "")
	if status != http.StatusOK {
		t.Fatalf("filter status: got %d (%v)", status, body)
	}
	arr, _ := body["data"].([]any)
	if len(arr) != 2 {
		t.Fatalf("gte 10.00: got %d rows, want 2 (%v)", len(arr), body)
	}
	first := arr[0].(map[string]any)
	last := arr[1].(map[string]any)
	if first["total"] != "12.50" || last["total"] != "100.00" {
		t.Fatalf("numeric sort wrong: %#v then %#v", first["total"], last["total"])
	}

	// A filter value that isn't a decimal is a 422, not a silently dropped filter.
	status, _ = do(t, http.MethodGet, base+"?filter[total][gte]=abc", "")
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("bad filter: got %d, want 422", status)
	}
}
