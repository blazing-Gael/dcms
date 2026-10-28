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

// Field-level access (ADR-0016 M2): read masks a field out of the response for an
// unauthorized reader; write silently drops a field an unauthorized writer sends.
const fieldAccessSchema = `
version: "1"
auth:
  roles:
    admin: { label: Administrator }
  session:
    ttl: 1h
collections:
  profiles:
    fields:
      name: { type: string, required: true }
      salary:
        type: number
        access:
          read:  [admin]
          write: [admin]
      bio:
        type: string
        access:
          write: owner
    access:
      read:   public
      create: authenticated
      update: authenticated
`

// newServerWith builds a live, auth-enabled server for an arbitrary schema.
func newServerWith(t *testing.T, src string) (*httptest.Server, store.Adapter) {
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

	srv := httptest.NewServer(gateway.New(def, db, nil, gateway.Options{
		Authenticator: gateway.NewSessionAuthenticator(db),
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, db
}

func dataField(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data object in %v", body)
	}
	return data
}

func TestFieldAccess_ReadMasking(t *testing.T) {
	srv, db := newServerWith(t, fieldAccessSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")
	seedUser(t, db, "worker@x.com", "pw-worker-12", "")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-1234")
	worker := login(t, srv.URL, "worker@x.com", "pw-worker-12")

	// Admin creates a profile with a salary (admin may write salary).
	_, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/profiles", boss,
		`{"name":"Alice","salary":120000}`)
	id := recordID(t, body)
	if got := dataField(t, body)["salary"]; got == nil {
		t.Fatalf("admin create should echo salary, got %v", body)
	}

	// Admin GET sees salary.
	_, body = doAs(t, http.MethodGet, srv.URL+"/api/v1/profiles/"+id, boss, "")
	if _, ok := dataField(t, body)["salary"]; !ok {
		t.Fatalf("admin read: salary should be present, got %v", body)
	}

	// Non-admin GET has salary masked out, but still gets the record + name.
	st, body := doAs(t, http.MethodGet, srv.URL+"/api/v1/profiles/"+id, worker, "")
	if st != http.StatusOK {
		t.Fatalf("worker read: got %d, want 200", st)
	}
	d := dataField(t, body)
	if _, leaked := d["salary"]; leaked {
		t.Fatalf("worker read: salary should be masked, got %v", d)
	}
	if d["name"] != "Alice" {
		t.Fatalf("worker read: name should survive masking, got %v", d)
	}

	// Anonymous GET (public read) is masked too.
	st, body = do(t, http.MethodGet, srv.URL+"/api/v1/profiles/"+id, "")
	if st != http.StatusOK {
		t.Fatalf("anon read: got %d, want 200", st)
	}
	if _, leaked := dataField(t, body)["salary"]; leaked {
		t.Fatalf("anon read: salary should be masked, got %v", body)
	}
}

func TestFieldAccess_ListMasking(t *testing.T) {
	srv, db := newServerWith(t, fieldAccessSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")
	seedUser(t, db, "worker@x.com", "pw-worker-12", "")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-1234")
	worker := login(t, srv.URL, "worker@x.com", "pw-worker-12")

	doAs(t, http.MethodPost, srv.URL+"/api/v1/profiles", boss, `{"name":"Alice","salary":100}`)
	doAs(t, http.MethodPost, srv.URL+"/api/v1/profiles", boss, `{"name":"Bob","salary":200}`)

	_, body := doAs(t, http.MethodGet, srv.URL+"/api/v1/profiles", worker, "")
	rows, _ := body["data"].([]any)
	if len(rows) != 2 {
		t.Fatalf("worker list: want 2 rows, got %v", body)
	}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		if _, leaked := row["salary"]; leaked {
			t.Fatalf("worker list: salary should be masked in every row, got %v", row)
		}
	}
}

// TestFieldAccess_NoFilterSortOracle pins GHSA-x4q6-jjx6-rwxm: a caller who can't
// read a masked field must not be able to recover its value by filtering or
// sorting on it. Both are rejected as an unknown field (never "forbidden", so the
// field's existence isn't confirmed), while an admin who *can* read it may still
// filter/sort, and a projection of a masked field stays masked rather than leaking.
func TestFieldAccess_NoFilterSortOracle(t *testing.T) {
	srv, db := newServerWith(t, fieldAccessSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")
	seedUser(t, db, "worker@x.com", "pw-worker-12", "")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-1234")
	worker := login(t, srv.URL, "worker@x.com", "pw-worker-12")

	doAs(t, http.MethodPost, srv.URL+"/api/v1/profiles", boss, `{"name":"Alice","salary":100}`)
	doAs(t, http.MethodPost, srv.URL+"/api/v1/profiles", boss, `{"name":"Bob","salary":200}`)

	base := srv.URL + "/api/v1/profiles"

	// Worker (and anonymous) cannot filter or sort on the masked salary field.
	for _, tc := range []struct{ name, token string }{{"worker", worker}, {"anonymous", ""}} {
		for _, q := range []string{"?filter[salary][eq]=100", "?filter[salary][gt]=150", "?sort=salary", "?sort=-salary"} {
			st, _ := doAs(t, http.MethodGet, base+q, tc.token, "")
			if st != http.StatusUnprocessableEntity {
				t.Errorf("%s GET %s: got %d, want 422 (masked field must look unknown)", tc.name, q, st)
			}
		}
		// A projection of the masked field still returns it masked, not rejected.
		st, body := doAs(t, http.MethodGet, base+"?fields=id,salary", tc.token, "")
		if st != http.StatusOK {
			t.Errorf("%s fields=id,salary: got %d, want 200", tc.name, st)
			continue
		}
		rows, _ := body["data"].([]any)
		for _, r := range rows {
			if row, _ := r.(map[string]any); row != nil {
				if _, leaked := row["salary"]; leaked {
					t.Errorf("%s projection leaked salary: %v", tc.name, row)
				}
			}
		}
	}

	// The admin, who can read salary, may filter and sort on it.
	if st, body := doAs(t, http.MethodGet, base+"?filter[salary][eq]=100", boss, ""); st != http.StatusOK {
		t.Fatalf("admin filter on salary: got %d, want 200 (%v)", st, body)
	}
	if st, _ := doAs(t, http.MethodGet, base+"?sort=-salary", boss, ""); st != http.StatusOK {
		t.Fatalf("admin sort on salary: got %d, want 200", st)
	}

	// A non-masked field stays filterable by everyone.
	if st, _ := doAs(t, http.MethodGet, base+"?filter[name][eq]=Alice", worker, ""); st != http.StatusOK {
		t.Fatalf("worker filter on unmasked name: got %d, want 200", st)
	}
}

func TestFieldAccess_WriteStripping_RoleRule(t *testing.T) {
	srv, db := newServerWith(t, fieldAccessSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")
	seedUser(t, db, "worker@x.com", "pw-worker-12", "")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-1234")
	worker := login(t, srv.URL, "worker@x.com", "pw-worker-12")

	// Non-admin creates with a salary → the value is dropped, not rejected (201).
	st, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/profiles", worker,
		`{"name":"Carol","salary":999999}`)
	if st != http.StatusCreated {
		t.Fatalf("worker create: got %d, want 201 (%v)", st, body)
	}
	id := recordID(t, body)

	// Read back as admin: salary was never stored.
	_, body = doAs(t, http.MethodGet, srv.URL+"/api/v1/profiles/"+id, boss, "")
	if got := dataField(t, body)["salary"]; got != nil {
		t.Fatalf("worker's salary should have been dropped, admin sees %v", got)
	}

	// Admin can update the salary.
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/profiles/"+id, boss,
		`{"salary":55000}`); st != http.StatusOK {
		t.Fatalf("admin update salary: got %d, want 200", st)
	}
	// Non-admin PATCH of salary is dropped, leaving the admin's value intact.
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/profiles/"+id, worker,
		`{"salary":1}`); st != http.StatusOK {
		t.Fatalf("worker update: got %d, want 200", st)
	}
	_, body = doAs(t, http.MethodGet, srv.URL+"/api/v1/profiles/"+id, boss, "")
	if got := dataField(t, body)["salary"]; got != float64(55000) {
		t.Fatalf("salary should still be 55000 after worker's stripped write, got %v", got)
	}
}

func TestFieldAccess_WriteStripping_OwnerRule(t *testing.T) {
	srv, db := newServerWith(t, fieldAccessSchema)
	seedUser(t, db, "u1@x.com", "pw-u1-1234567", "")
	seedUser(t, db, "u2@x.com", "pw-u2-1234567", "")
	u1 := login(t, srv.URL, "u1@x.com", "pw-u1-1234567")
	u2 := login(t, srv.URL, "u2@x.com", "pw-u2-1234567")

	// bio write is owner-scoped. On create it collapses to authenticated, so u1's
	// bio is stored.
	_, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/profiles", u1,
		`{"name":"Dave","bio":"original"}`)
	id := recordID(t, body)
	if dataField(t, body)["bio"] != "original" {
		t.Fatalf("owner create should keep bio, got %v", body)
	}

	// u2 (not the owner) PATCHes bio → dropped, value unchanged.
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/profiles/"+id, u2,
		`{"bio":"hijacked"}`); st != http.StatusOK {
		t.Fatalf("non-owner bio update: got %d, want 200", st)
	}
	_, body = doAs(t, http.MethodGet, srv.URL+"/api/v1/profiles/"+id, u1, "")
	if dataField(t, body)["bio"] != "original" {
		t.Fatalf("non-owner bio write should be dropped, got %v", dataField(t, body)["bio"])
	}

	// The owner can update bio.
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/profiles/"+id, u1,
		`{"bio":"updated"}`); st != http.StatusOK {
		t.Fatalf("owner bio update: got %d, want 200", st)
	}
	_, body = doAs(t, http.MethodGet, srv.URL+"/api/v1/profiles/"+id, u1, "")
	if dataField(t, body)["bio"] != "updated" {
		t.Fatalf("owner bio write should apply, got %v", dataField(t, body)["bio"])
	}
}

// Round-tripping a masked record must not 4xx: a non-admin GETs a profile (salary
// masked out), then PATCHes the whole body back. The absent salary is untouched
// and the write succeeds.
func TestFieldAccess_MaskedRoundTripSucceeds(t *testing.T) {
	srv, db := newServerWith(t, fieldAccessSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")
	seedUser(t, db, "worker@x.com", "pw-worker-12", "")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-1234")
	worker := login(t, srv.URL, "worker@x.com", "pw-worker-12")

	_, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/profiles", boss,
		`{"name":"Erin","salary":77000}`)
	id := recordID(t, body)

	// worker reads (salary masked) then PATCHes name only.
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/profiles/"+id, worker,
		`{"name":"Erin Renamed"}`); st != http.StatusOK {
		t.Fatalf("worker round-trip update: got %d, want 200", st)
	}
	// admin confirms salary survived untouched.
	_, body = doAs(t, http.MethodGet, srv.URL+"/api/v1/profiles/"+id, boss, "")
	d := dataField(t, body)
	if d["salary"] != float64(77000) {
		t.Fatalf("salary should be untouched, got %v", d["salary"])
	}
	if d["name"] != "Erin Renamed" {
		t.Fatalf("name should be updated, got %v", d["name"])
	}
}
