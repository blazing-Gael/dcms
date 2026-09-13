package gateway_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// A long-lived API token (issue #8) authenticates a machine caller as a
// first-class principal with its own roles, over the same bearer header, so every
// access: rule applies unchanged. Revocation and expiry make it stop working.
func TestAPIToken_DrivesAuthorization(t *testing.T) {
	srv, db := newAuthServer(t)
	ctx := context.Background()
	base := srv.URL + "/api/v1"

	// Mint a token with the author role (authSchema: articles create = [admin, author]).
	raw, rec, err := gateway.CreateAPIToken(ctx, db, "ci", []string{"author"}, time.Time{})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	tokenID, _ := rec["id"].(string)

	post := func(token string) int {
		req, _ := http.NewRequest(http.MethodPost, base+"/articles", newReader(`{"title":"built"}`))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		st, _ := doReq(t, req)
		return st
	}

	// The token's author role satisfies the create rule.
	if st := post(raw); st != http.StatusCreated {
		t.Fatalf("create with API token: got %d, want 201", st)
	}
	// A bogus token with the right prefix is anonymous → 401.
	if st := post("dcms_pat_deadbeef"); st != http.StatusUnauthorized {
		t.Fatalf("create with bogus token: got %d, want 401", st)
	}
	// No token at all → 401.
	if st := post(""); st != http.StatusUnauthorized {
		t.Fatalf("anonymous create: got %d, want 401", st)
	}

	// Revoked → stops working immediately.
	if err := gateway.RevokeAPIToken(ctx, db, tokenID); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if st := post(raw); st != http.StatusUnauthorized {
		t.Fatalf("create with revoked token: got %d, want 401", st)
	}
}

// An expired token does not authenticate.
func TestAPIToken_ExpiredIsAnonymous(t *testing.T) {
	srv, db := newAuthServer(t)
	ctx := context.Background()

	raw, _, err := gateway.CreateAPIToken(ctx, db, "stale", []string{"author"}, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/articles", newReader(`{"title":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+raw)
	if st, _ := doReq(t, req); st != http.StatusUnauthorized {
		t.Fatalf("create with expired token: got %d, want 401", st)
	}
}

// The stored token's writes are attributed to the token itself (its row id),
// distinguishing machine writes from a human's (issue #8).
func TestAPIToken_StampsItselfAsActor(t *testing.T) {
	srv, db := newAuthServer(t)
	ctx := context.Background()

	raw, rec, err := gateway.CreateAPIToken(ctx, db, "writer", []string{"author"}, time.Time{})
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	tokenID, _ := rec["id"].(string)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/articles", newReader(`{"title":"built"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+raw)
	st, body := doReq(t, req)
	if st != http.StatusCreated {
		t.Fatalf("create: got %d, want 201", st)
	}
	data, _ := body["data"].(map[string]any)
	if data["created_by"] != tokenID {
		t.Fatalf("created_by = %v, want the token id %q", data["created_by"], tokenID)
	}
}
