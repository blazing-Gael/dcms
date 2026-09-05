package gateway_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/store"
)

// owner_field access rules (issue #7): ownership by a named relation, not just
// created_by. The canonical case is a row a service writes FOR a user — the user
// must read/manage it even though created_by is the service.
const ownerFieldSchema = `
version: "1"
auth:
  roles:
    admin: { label: Administrator }
    service: { label: Service }
  session:
    ttl: 1h
collections:
  subscriptions:
    fields:
      user: { type: relation, target: _users, required: true }
      level: { type: string }
    access:
      read: { owner_field: user }
      create: [service, admin]
      update:
        any: [admin, { owner_field: user }]
      delete: [admin]
`

// ownerFieldEdgeSchema pairs a publicly-readable collection with a field-level
// owner_field rule and a NULLABLE owner relation, so tests can exercise field
// masking, delete, and the empty-owner fail-closed edge.
const ownerFieldEdgeSchema = `
version: "1"
auth:
  roles:
    admin: { label: Administrator }
  session:
    ttl: 1h
collections:
  docs:
    fields:
      owner:  { type: relation, target: _users }
      title:  { type: string }
      secret: { type: string, access: { read: { owner_field: owner } } }
    access:
      read:   public
      create: [admin]
      update: { any: [admin, { owner_field: owner }] }
      delete: { any: [admin, { owner_field: owner }] }
`

// seedUserID creates a user and returns their _users id, so a test can set a
// relation to it (seedUser alone doesn't surface the id).
func seedUserID(t *testing.T, db store.Adapter, email, password string, roles ...string) string {
	t.Helper()
	rec, err := gateway.CreateUser(context.Background(), db, email, password, roles)
	if err != nil {
		t.Fatalf("CreateUser %s: %v", email, err)
	}
	id, _ := rec["id"].(string)
	if id == "" {
		t.Fatalf("CreateUser %s: no id", email)
	}
	return id
}

func TestOwnerField_ServiceWritesUserReads(t *testing.T) {
	srv, db := newServerWith(t, ownerFieldSchema)
	seedUser(t, db, "svc@x.com", "pw-service-123", "service")
	aliceID := seedUserID(t, db, "alice@x.com", "pw-alice-1234")
	seedUser(t, db, "bob@x.com", "pw-bob-123456")

	svcTok := login(t, srv.URL, "svc@x.com", "pw-service-123")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-1234")
	bob := login(t, srv.URL, "bob@x.com", "pw-bob-123456")

	// The service creates an entitlement FOR Alice: created_by is the service,
	// but the row belongs to Alice via `user`.
	st, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/subscriptions", svcTok,
		`{"user":"`+aliceID+`","level":"pro"}`)
	if st != http.StatusCreated {
		t.Fatalf("service create: got %d, want 201 (%v)", st, body)
	}
	aliceSub := recordID(t, body)

	// Alice sees her own entitlement on the list; Bob sees none.
	_, aList := doAs(t, http.MethodGet, srv.URL+"/api/v1/subscriptions", alice, "")
	if rows, _ := aList["data"].([]any); len(rows) != 1 {
		t.Fatalf("alice list: want 1 own row, got %v", aList)
	}
	_, bList := doAs(t, http.MethodGet, srv.URL+"/api/v1/subscriptions", bob, "")
	if rows, _ := bList["data"].([]any); len(rows) != 0 {
		t.Fatalf("bob list: want 0 rows, got %v", bList)
	}

	// Single read: Alice can fetch hers (200); Bob cannot (404, no existence leak).
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/subscriptions/"+aliceSub, alice, ""); st != http.StatusOK {
		t.Fatalf("alice read own: got %d, want 200", st)
	}
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/subscriptions/"+aliceSub, bob, ""); st != http.StatusNotFound {
		t.Fatalf("bob read alice's: got %d, want 404", st)
	}
}

func TestOwnerField_UpdateByRelationOwner(t *testing.T) {
	srv, db := newServerWith(t, ownerFieldSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-12345", "admin")
	seedUserID(t, db, "svc@x.com", "pw-service-123", "service")
	aliceID := seedUserID(t, db, "alice@x.com", "pw-alice-1234")
	seedUser(t, db, "bob@x.com", "pw-bob-123456")

	svcTok := login(t, srv.URL, "svc@x.com", "pw-service-123")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-12345")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-1234")
	bob := login(t, srv.URL, "bob@x.com", "pw-bob-123456")

	_, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/subscriptions", svcTok,
		`{"user":"`+aliceID+`","level":"pro"}`)
	sub := recordID(t, body)

	// The relation owner (Alice) may update, though she never created the row.
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/subscriptions/"+sub, alice, `{"level":"plus"}`); st != http.StatusOK {
		t.Fatalf("alice (relation owner) update: got %d, want 200", st)
	}
	// A non-owner, non-admin is forbidden.
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/subscriptions/"+sub, bob, `{"level":"hijack"}`); st != http.StatusForbidden {
		t.Fatalf("bob update alice's sub: got %d, want 403", st)
	}
	// An admin bypasses owner scope entirely.
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/subscriptions/"+sub, boss, `{"level":"admin-set"}`); st != http.StatusOK {
		t.Fatalf("admin update: got %d, want 200", st)
	}
	// The service (not admin, not the relation owner) cannot update under this
	// rule — it only holds create; its own creation doesn't grant ownership.
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/subscriptions/"+sub, svcTok, `{"level":"svc"}`); st != http.StatusForbidden {
		t.Fatalf("service update: got %d, want 403", st)
	}
}

func TestOwnerField_AnonymousDenied(t *testing.T) {
	srv, _ := newServerWith(t, ownerFieldSchema)
	// read is {owner_field: user} with no public/authenticated branch → anonymous
	// can never match a relation to their identity → 401.
	if st, _ := do(t, http.MethodGet, srv.URL+"/api/v1/subscriptions", ""); st != http.StatusUnauthorized {
		t.Fatalf("anon list: got %d, want 401", st)
	}
}

func TestOwnerField_DeleteByRelationOwner(t *testing.T) {
	srv, db := newServerWith(t, ownerFieldEdgeSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-12345", "admin")
	aliceID := seedUserID(t, db, "alice@x.com", "pw-alice-1234")
	seedUser(t, db, "bob@x.com", "pw-bob-123456")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-12345")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-1234")
	bob := login(t, srv.URL, "bob@x.com", "pw-bob-123456")

	mk := func() string {
		_, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/docs", boss,
			`{"owner":"`+aliceID+`","title":"t","secret":"s"}`)
		return recordID(t, body)
	}

	// A non-owner, non-admin cannot delete.
	if st, _ := doAs(t, http.MethodDelete, srv.URL+"/api/v1/docs/"+mk(), bob, ""); st != http.StatusForbidden {
		t.Fatalf("bob delete alice's doc: got %d, want 403", st)
	}
	// The relation owner can.
	if st, _ := doAs(t, http.MethodDelete, srv.URL+"/api/v1/docs/"+mk(), alice, ""); st != http.StatusNoContent {
		t.Fatalf("alice delete own doc: got %d, want 204", st)
	}
}

// TestOwnerField_FieldMaskedByRelation exercises owner_field at the FIELD level:
// the collection is publicly readable but the `secret` field is scoped to the
// row's owner relation, so a non-owner sees the record minus the field.
func TestOwnerField_FieldMaskedByRelation(t *testing.T) {
	srv, db := newServerWith(t, ownerFieldEdgeSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-12345", "admin")
	aliceID := seedUserID(t, db, "alice@x.com", "pw-alice-1234")
	seedUser(t, db, "bob@x.com", "pw-bob-123456")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-12345")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-1234")
	bob := login(t, srv.URL, "bob@x.com", "pw-bob-123456")

	_, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/docs", boss,
		`{"owner":"`+aliceID+`","title":"t","secret":"top"}`)
	id := recordID(t, body)

	// The owner sees the field; a different user does not; an anonymous reader
	// does not — the record is still returned (public read), just without secret.
	if _, b := doAs(t, http.MethodGet, srv.URL+"/api/v1/docs/"+id, alice, ""); dataObj(t, b)["secret"] != "top" {
		t.Errorf("owner should see secret, got %v", dataObj(t, b)["secret"])
	}
	if _, b := doAs(t, http.MethodGet, srv.URL+"/api/v1/docs/"+id, bob, ""); dataObj(t, b)["secret"] != nil {
		t.Errorf("non-owner should not see secret, got %v", dataObj(t, b)["secret"])
	}
	if _, b := do(t, http.MethodGet, srv.URL+"/api/v1/docs/"+id, ""); dataObj(t, b)["secret"] != nil {
		t.Errorf("anonymous should not see secret, got %v", dataObj(t, b)["secret"])
	}
}

// TestOwnerField_NullOwnerFailsClosed is the security-critical edge: a row whose
// owner relation is empty must never match any principal's owner scope — neither
// the masked field nor an owner-scoped write may leak through a null owner.
func TestOwnerField_NullOwnerFailsClosed(t *testing.T) {
	srv, db := newServerWith(t, ownerFieldEdgeSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-12345", "admin")
	seedUser(t, db, "alice@x.com", "pw-alice-1234")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-12345")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-1234")

	// Admin creates a doc with NO owner set (the relation is nullable).
	_, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/docs", boss, `{"title":"orphan","secret":"top"}`)
	id := recordID(t, body)

	// The owner-scoped field is masked for everyone but the admin — an empty owner
	// matches no one, so it fails closed rather than becoming world-readable.
	if _, b := doAs(t, http.MethodGet, srv.URL+"/api/v1/docs/"+id, alice, ""); dataObj(t, b)["secret"] != nil {
		t.Errorf("empty owner must not expose secret, got %v", dataObj(t, b)["secret"])
	}
	// An owner-scoped write against a null-owner row is refused (403), not allowed.
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/docs/"+id, alice, `{"title":"x"}`); st != http.StatusForbidden {
		t.Fatalf("owner-scoped update on null-owner row: got %d, want 403", st)
	}
	// The admin branch of the composite still works.
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/docs/"+id, boss, `{"title":"x"}`); st != http.StatusOK {
		t.Fatalf("admin update: got %d, want 200", st)
	}
}
