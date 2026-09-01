package gateway_test

import (
	"net/http"
	"testing"
)

// Composite access rules (ADR-0016): a single rule can OR gates that resolve
// differently per caller. `any: [admin, owner]` grants admins full access while
// narrowing everyone else to the rows they created — the case a bare `owner`
// rule could not express (it also hid records from admins).
const compositeSchema = `
version: "1"
auth:
  roles:
    admin: { label: Administrator }
  session:
    ttl: 1h
collections:
  orders:
    fields:
      note: { type: string }
    access:
      read:
        any: [admin, owner]
      create: authenticated
      update:
        any: [admin, owner]
      delete:
        any: [admin, owner]
`

func TestComposite_AdminSeesAllOwnerSeesOwn(t *testing.T) {
	srv, db := newServerWith(t, compositeSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")
	seedUser(t, db, "alice@x.com", "pw-alice-123", "")
	seedUser(t, db, "bob@x.com", "pw-bob-12345", "")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-1234")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-123")
	bob := login(t, srv.URL, "bob@x.com", "pw-bob-12345")

	// Alice and Bob each create an order.
	_, aBody := doAs(t, http.MethodPost, srv.URL+"/api/v1/orders", alice, `{"note":"alice-order"}`)
	aliceOrder := recordID(t, aBody)
	_, bBody := doAs(t, http.MethodPost, srv.URL+"/api/v1/orders", bob, `{"note":"bob-order"}`)
	bobOrder := recordID(t, bBody)

	// Owner scope on list: Alice sees only her row; the admin sees both.
	_, body := doAs(t, http.MethodGet, srv.URL+"/api/v1/orders", alice, "")
	if rows, _ := body["data"].([]any); len(rows) != 1 {
		t.Fatalf("alice list: want 1 own row, got %v", body)
	}
	_, body = doAs(t, http.MethodGet, srv.URL+"/api/v1/orders", boss, "")
	if rows, _ := body["data"].([]any); len(rows) != 2 {
		t.Fatalf("admin list: want all 2 rows, got %v", body)
	}

	// Single read: Alice cannot see Bob's order (404, no existence leak); the
	// admin can; Bob sees his own.
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/orders/"+bobOrder, alice, ""); st != http.StatusNotFound {
		t.Fatalf("alice read bob's order: got %d, want 404", st)
	}
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/orders/"+bobOrder, boss, ""); st != http.StatusOK {
		t.Fatalf("admin read bob's order: got %d, want 200", st)
	}
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/orders/"+aliceOrder, alice, ""); st != http.StatusOK {
		t.Fatalf("alice read own order: got %d, want 200", st)
	}
}

func TestComposite_WriteAdminBypassesOwner(t *testing.T) {
	srv, db := newServerWith(t, compositeSchema)
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin")
	seedUser(t, db, "alice@x.com", "pw-alice-123", "")
	seedUser(t, db, "bob@x.com", "pw-bob-12345", "")
	boss := login(t, srv.URL, "boss@x.com", "pw-boss-1234")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-123")
	bob := login(t, srv.URL, "bob@x.com", "pw-bob-12345")

	_, aBody := doAs(t, http.MethodPost, srv.URL+"/api/v1/orders", alice, `{"note":"alice-order"}`)
	aliceOrder := recordID(t, aBody)

	// The owner may update her own row.
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/orders/"+aliceOrder, alice, `{"note":"edited"}`); st != http.StatusOK {
		t.Fatalf("alice update own order: got %d, want 200", st)
	}
	// A non-owner, non-admin is forbidden (403 — the record exists for the owner,
	// but authorizeRecordWrite loaded it and found a created_by mismatch).
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/orders/"+aliceOrder, bob, `{"note":"hijack"}`); st != http.StatusForbidden {
		t.Fatalf("bob update alice's order: got %d, want 403", st)
	}
	// The admin bypasses owner scope entirely (allow branch — no record load).
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/orders/"+aliceOrder, boss, `{"note":"admin-edit"}`); st != http.StatusOK {
		t.Fatalf("admin update alice's order: got %d, want 200", st)
	}
}

func TestComposite_AnonymousDeniedOnAllPrivateRule(t *testing.T) {
	srv, db := newServerWith(t, compositeSchema)
	seedUser(t, db, "alice@x.com", "pw-alice-123", "")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-123")
	doAs(t, http.MethodPost, srv.URL+"/api/v1/orders", alice, `{"note":"alice-order"}`)

	// `any: [admin, owner]` contains no public/authenticated gate, so an
	// anonymous caller satisfies neither branch → 401 on the list endpoint.
	if st, _ := do(t, http.MethodGet, srv.URL+"/api/v1/orders", ""); st != http.StatusUnauthorized {
		t.Fatalf("anon list of admin-or-owner collection: got %d, want 401", st)
	}
}
