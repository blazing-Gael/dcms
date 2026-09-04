package gateway_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// queuedNotifications returns the rows sitting in the durable outbox. Since
// delivery is now asynchronous (ADR-0021 phase 3), a test reads the enqueued
// notification rather than intercepting a synchronous send.
func queuedNotifications(t *testing.T, db store.Adapter) []store.Record {
	t.Helper()
	page, err := db.Find(context.Background(), store.Query{
		Collection: schema.NotificationsCollection,
		SkipCount:  true,
	})
	if err != nil {
		t.Fatalf("read notifications: %v", err)
	}
	return page.Data
}

// resetTokenFromQueue reads the single queued reset link and extracts its token.
func resetTokenFromQueue(t *testing.T, db store.Adapter) string {
	t.Helper()
	ns := queuedNotifications(t, db)
	if len(ns) != 1 {
		t.Fatalf("expected exactly 1 queued notification, got %d", len(ns))
	}
	link, _ := ns[0][schema.NotificationLink].(string)
	_, tok, ok := strings.Cut(link, "token=")
	if !ok {
		t.Fatalf("no token in queued reset link %q", link)
	}
	return tok
}

func TestPasswordReset_HappyPath(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{ResetLinkBase: "https://site/reset"})
	seedUser(t, db, "u@x.com", "oldpassword", "author")
	live := login(t, base, "u@x.com", "oldpassword")

	// Forgot → 200 and a durably-queued link.
	if st, _ := do(t, http.MethodPost, base+"/auth/forgot", `{"email":"u@x.com"}`); st != http.StatusOK {
		t.Fatalf("forgot should be 200, got %d", st)
	}
	token := resetTokenFromQueue(t, db)

	// Reset with the token → 200.
	if st, body := do(t, http.MethodPost, base+"/auth/reset", `{"token":"`+token+`","new":"brandnewpass"}`); st != http.StatusOK {
		t.Fatalf("reset should be 200, got %d %v", st, body)
	}
	// Old sessions are revoked; new password works, old doesn't.
	if tokenValid(t, base, live) {
		t.Fatal("reset should revoke all existing sessions")
	}
	login(t, base, "u@x.com", "brandnewpass")
	if st, _ := do(t, http.MethodPost, base+"/auth/login", `{"email":"u@x.com","password":"oldpassword"}`); st != http.StatusUnauthorized {
		t.Fatalf("old password should be rejected, got %d", st)
	}
	// The token is single-use: a second reset with it fails.
	if st, _ := do(t, http.MethodPost, base+"/auth/reset", `{"token":"`+token+`","new":"yetanotherpw"}`); st != http.StatusBadRequest {
		t.Fatalf("reused reset token should be 400, got %d", st)
	}
}

func TestPasswordReset_EnumerationSafeAndBadToken(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{})

	// Forgot for an unknown email → 200, but nothing is enqueued (no oracle).
	if st, _ := do(t, http.MethodPost, base+"/auth/forgot", `{"email":"nobody@x.com"}`); st != http.StatusOK {
		t.Fatalf("forgot for unknown email should still be 200, got %d", st)
	}
	if n := len(queuedNotifications(t, db)); n != 0 {
		t.Fatalf("no notification should be queued for an unknown email, got %d", n)
	}

	// A garbage reset token is a flat 400.
	if st, _ := do(t, http.MethodPost, base+"/auth/reset", `{"token":"not-a-real-token","new":"whateverpass"}`); st != http.StatusBadRequest {
		t.Fatalf("invalid reset token should be 400, got %d", st)
	}
}
