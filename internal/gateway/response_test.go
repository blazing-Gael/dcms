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

const gadgetSchema = `
version: "1"
collections:
  gadgets:
    fields:
      name:   { type: string, required: true }
      active: { type: boolean }
      status: { type: enum, values: [live, paused], default: live }
`

// newDB parses a schema and returns it alongside a migrated in-memory store.
func newDB(t *testing.T, src string) (*schema.SchemaDefinition, store.Adapter) {
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
	return def, db
}

func mount(t *testing.T, def *schema.SchemaDefinition, db store.Adapter, opts ...gateway.Options) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(gateway.New(def, db, nil, opts...).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// A boolean field is stored as 0/1 in SQLite; the response must restore it to a
// real JSON boolean so the wire matches the schema/OpenAPI/SDK.
func TestResponseCoercion_BooleanIsJSONBool(t *testing.T) {
	def, db := newDB(t, gadgetSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1/gadgets"

	st, body := do(t, http.MethodPost, base, `{"name":"widget","active":true}`)
	if st != http.StatusCreated {
		t.Fatalf("create: %d %v", st, body)
	}
	rec := dataObj(t, body)
	if rec["active"] != true { // JSON true → Go bool; a leaked 0/1 would be float64
		t.Fatalf("active should be JSON boolean true, got %#v (%T)", rec["active"], rec["active"])
	}
	id := rec["id"].(string)

	// Same on read-back, and false must round-trip too.
	if st, body = do(t, http.MethodPatch, base+"/"+id, `{"active":false}`); st != http.StatusOK {
		t.Fatalf("update: %d %v", st, body)
	}
	if dataObj(t, body)["active"] != false {
		t.Fatalf("active should be JSON boolean false, got %#v", dataObj(t, body)["active"])
	}
	st, body = do(t, http.MethodGet, base+"/"+id, "")
	if st != http.StatusOK || dataObj(t, body)["active"] != false {
		t.Fatalf("get active: %d %#v", st, body)
	}
}

// Strict response validation must gate on the option: the SAME corrupt row is
// passed through untouched when off, but rejected with a 500 when on.
func TestResponseValidation_StrictModeGatesDrift(t *testing.T) {
	def, db := newDB(t, gadgetSchema)
	ctx := context.Background()

	// Seed a valid row, then corrupt its enum behind the store's back — exactly
	// the kind of drift request validation can't prevent.
	rec, err := db.Create(ctx, store.WriteInput{Collection: "gadgets", Data: store.Record{"name": "y", "status": "live"}})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	id := rec["id"].(string)
	if _, err := db.RawExec(ctx, `UPDATE gadgets SET status = $1 WHERE id = $2`, "banana", id); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}

	// Validation OFF (default): nonconforming data passes through as 200.
	off := mount(t, def, db)
	st, body := do(t, http.MethodGet, off.URL+"/api/v1/gadgets/"+id, "")
	if st != http.StatusOK {
		t.Fatalf("validate-off: got %d, want 200 (%v)", st, body)
	}
	if dataObj(t, body)["status"] != "banana" {
		t.Fatalf("validate-off should pass corrupt value through, got %#v", dataObj(t, body)["status"])
	}

	// Validation ON: the contract violation becomes a 500, not leaked data.
	on := mount(t, def, db, gateway.Options{ValidateResponses: true})
	st, body = do(t, http.MethodGet, on.URL+"/api/v1/gadgets/"+id, "")
	if st != http.StatusInternalServerError {
		t.Fatalf("validate-on: got %d, want 500 (%v)", st, body)
	}
}
