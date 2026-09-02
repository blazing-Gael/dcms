package gateway_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// captureNotifier records notifications instead of sending them, so a test can
// read the reset link back.
type captureNotifier struct {
	mu    sync.Mutex
	calls int
	last  gateway.Notification
}

func (c *captureNotifier) Notify(_ context.Context, msg gateway.Notification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.last = msg
	return nil
}

func (c *captureNotifier) tokenFromLink(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	_, tok, ok := strings.Cut(c.last.Link, "token=")
	if !ok {
		t.Fatalf("no token in reset link %q", c.last.Link)
	}
	return tok
}

func TestPasswordReset_HappyPath(t *testing.T) {
	cap := &captureNotifier{}
	base, db := newAccountsServer(t, gateway.Options{Notifier: cap, ResetLinkBase: "https://site/reset"})
	seedUser(t, db, "u@x.com", "oldpassword", "author")
	live := login(t, base, "u@x.com", "oldpassword")

	// Forgot → 200 and a delivered link.
	if st, _ := do(t, http.MethodPost, base+"/auth/forgot", `{"email":"u@x.com"}`); st != http.StatusOK {
		t.Fatalf("forgot should be 200, got %d", st)
	}
	if cap.calls != 1 {
		t.Fatalf("a reset link should have been delivered once, got %d", cap.calls)
	}
	token := cap.tokenFromLink(t)

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
	cap := &captureNotifier{}
	base, _ := newAccountsServer(t, gateway.Options{Notifier: cap})

	// Forgot for an unknown email → 200, but nothing is sent (no oracle).
	if st, _ := do(t, http.MethodPost, base+"/auth/forgot", `{"email":"nobody@x.com"}`); st != http.StatusOK {
		t.Fatalf("forgot for unknown email should still be 200, got %d", st)
	}
	if cap.calls != 0 {
		t.Fatalf("no notification should be sent for an unknown email, got %d", cap.calls)
	}

	// A garbage reset token is a flat 400.
	if st, _ := do(t, http.MethodPost, base+"/auth/reset", `{"token":"not-a-real-token","new":"whateverpass"}`); st != http.StatusBadRequest {
		t.Fatalf("invalid reset token should be 400, got %d", st)
	}
}
