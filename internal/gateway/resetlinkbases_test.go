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

const resetBasesSchema = `
version: "1"
collections:
  notes:
    fields:
      body: { type: string }
`

// forgotResetLink builds a fresh server with the given reset-base allowlist, runs
// POST /auth/forgot for a seeded user with the given return_to, and returns the
// reset link that was enqueued for delivery (read straight from the outbox).
func forgotResetLink(t *testing.T, bases []string, returnTo string) string {
	t.Helper()
	def, db := newDB(t, resetBasesSchema)
	srv := mount(t, def, db, gateway.Options{ResetLinkBases: bases})
	seedUser(t, db, "a@x.com", "pw-user-1234")

	body := `{"email":"a@x.com"}`
	if returnTo != "" {
		body = `{"email":"a@x.com","return_to":"` + returnTo + `"}`
	}
	if st, _ := do(t, http.MethodPost, srv.URL+"/auth/forgot", body); st != http.StatusOK {
		t.Fatalf("forgot: got %d, want 200", st)
	}
	page, err := db.Find(context.Background(), store.Query{Collection: schema.NotificationsCollection, SkipCount: true})
	if err != nil {
		t.Fatalf("read notifications: %v", err)
	}
	if len(page.Data) != 1 {
		t.Fatalf("want exactly one queued reset notification, got %d", len(page.Data))
	}
	link, _ := page.Data[0][schema.NotificationLink].(string)
	return link
}

func TestResetLinkBases_ReturnToAllowlisted(t *testing.T) {
	bases := []string{"https://app.example/restore", "https://write.example/reset"}

	// A return_to that exactly matches an allowlisted base is honoured.
	if link := forgotResetLink(t, bases, "https://write.example/reset"); !strings.HasPrefix(link, "https://write.example/reset?token=") {
		t.Fatalf("matching return_to: link = %q, want the write.example base", link)
	}

	// A forged return_to (not in the allowlist) falls back to the first base — never
	// an open redirect to the attacker's URL.
	if link := forgotResetLink(t, bases, "https://evil.example/steal"); !strings.HasPrefix(link, "https://app.example/restore?token=") {
		t.Fatalf("forged return_to must fall back to the first base, got %q", link)
	}

	// No return_to → the first base (the default frontend).
	if link := forgotResetLink(t, bases, ""); !strings.HasPrefix(link, "https://app.example/restore?token=") {
		t.Fatalf("no return_to: link = %q, want the first base", link)
	}
}

func TestResetLinkBases_SingleBaseStillWorks(t *testing.T) {
	// The single-frontend shorthand (ResetLinkBase, no list) is unchanged, and a
	// return_to can't escape it.
	def, db := newDB(t, resetBasesSchema)
	srv := mount(t, def, db, gateway.Options{ResetLinkBase: "https://only.example/reset"})
	seedUser(t, db, "a@x.com", "pw-user-1234")
	if st, _ := do(t, http.MethodPost, srv.URL+"/auth/forgot", `{"email":"a@x.com","return_to":"https://evil.example"}`); st != http.StatusOK {
		t.Fatalf("forgot: %d", st)
	}
	page, _ := db.Find(context.Background(), store.Query{Collection: schema.NotificationsCollection, SkipCount: true})
	link, _ := page.Data[0][schema.NotificationLink].(string)
	if !strings.HasPrefix(link, "https://only.example/reset?token=") {
		t.Fatalf("single base with forged return_to: got %q", link)
	}
}
