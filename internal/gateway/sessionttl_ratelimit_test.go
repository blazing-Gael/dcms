package gateway_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
)

// ── #33: per-role session TTL ────────────────────────────────────────────────

const perRoleTTLSchema = `
version: "1"
auth:
  roles:
    admin:  { label: Administrator }
    editor: { label: Editor }
  session:
    ttl: 720h                 # 30 days for ordinary users
    roles:
      admin: 1h               # an admin's session is short-lived
      editor: 72h
collections:
  notes:
    fields:
      body: { type: string, required: true }
`

func loginExpiry(t *testing.T, base, email, password string) time.Time {
	t.Helper()
	st, body := do(t, http.MethodPost, base+"/auth/login",
		`{"email":"`+email+`","password":"`+password+`"}`)
	if st != http.StatusOK {
		t.Fatalf("login %s: %d (%v)", email, st, body)
	}
	raw, _ := body["expires_at"].(string)
	exp, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("parse expires_at %q: %v", raw, err)
	}
	return exp
}

func TestSessionTTL_PerRoleShortestWins(t *testing.T) {
	def, db := newDB(t, perRoleTTLSchema)
	srv := mount(t, def, db, gateway.Options{})
	base := srv.URL

	seedUser(t, db, "reader@x.com", "pw-reader-123")                 // no role → base 720h
	seedUser(t, db, "ed@x.com", "pw-editor-123", "editor")           // 72h
	seedUser(t, db, "boss@x.com", "pw-boss-1234", "admin", "editor") // admin(1h) & editor(72h) → 1h

	now := time.Now().UTC()
	reader := loginExpiry(t, base, "reader@x.com", "pw-reader-123").Sub(now)
	editor := loginExpiry(t, base, "ed@x.com", "pw-editor-123").Sub(now)
	boss := loginExpiry(t, base, "boss@x.com", "pw-boss-1234").Sub(now)

	if reader < 700*time.Hour {
		t.Errorf("reader TTL = %s, want ~720h (base)", reader)
	}
	if editor < 60*time.Hour || editor > 80*time.Hour {
		t.Errorf("editor TTL = %s, want ~72h", editor)
	}
	// A user with both admin(1h) and editor(72h) gets the shortest matching role.
	if boss > 2*time.Hour {
		t.Errorf("multi-role TTL = %s, want ~1h (shortest matching role wins)", boss)
	}
}

func TestSessionTTL_UndeclaredRoleIsSchemaError(t *testing.T) {
	_, err := schema.Parse([]byte(`
version: "1"
auth:
  roles:
    admin: { label: Administrator }
  session:
    ttl: 720h
    roles:
      editr: 12h              # typo — not a declared role
collections:
  notes:
    fields:
      body: { type: string }
`))
	if err == nil || !strings.Contains(err.Error(), "not declared in auth.roles") {
		t.Fatalf("expected an undeclared-role error, got %v", err)
	}
}

// ── #34: anonymous writes get their own rate-limit tier ──────────────────────

const anonWriteSchema = `
version: "1"
auth:
  roles:
    admin: { label: Administrator }
collections:
  signups:
    fields:
      email: { type: string, required: true }
    access:
      read: public
      create: public          # a public signup form
      update: [admin]
      delete: [admin]
`

func TestRateLimit_AnonWritesTierIsolated(t *testing.T) {
	def, db := newDB(t, anonWriteSchema)
	// A tight anon-write tier, but a generous API tier — so an authenticated caller
	// and reads are never throttled by the anonymous-write budget.
	srv := mount(t, def, db, gateway.Options{
		Authenticator: gateway.NewSessionAuthenticator(db),
		RateLimit: &gateway.RateLimitOptions{
			AnonWritePerMinute: 2, AnonWriteBurst: 2,
			APIPerMinute: 100000, APIBurst: 10000,
			AuthPerMinute: 100000, AuthBurst: 10000,
		},
	})
	base := srv.URL + "/api/v1"

	// Anonymous writes: the burst of 2 passes, then the 3rd is throttled.
	st1, _ := do(t, http.MethodPost, base+"/signups", `{"email":"a@x.com"}`)
	st2, _ := do(t, http.MethodPost, base+"/signups", `{"email":"b@x.com"}`)
	st3, body := do(t, http.MethodPost, base+"/signups", `{"email":"c@x.com"}`)
	if st1 != http.StatusCreated || st2 != http.StatusCreated {
		t.Fatalf("first two anon writes should pass: %d, %d", st1, st2)
	}
	if st3 != http.StatusTooManyRequests {
		t.Fatalf("third anon write should be 429, got %d (%v)", st3, body)
	}

	// Reads are NOT on the anon-write tier — still fine after the write budget is spent.
	if st, _ := do(t, http.MethodGet, base+"/signups", ""); st != http.StatusOK {
		t.Fatalf("anonymous read should not be throttled by the anon-write tier, got %d", st)
	}

	// An authenticated write uses the (generous) API tier, not the anon-write tier.
	seedUser(t, db, "admin@x.com", "pw-admin-1234", "admin")
	tok := login(t, srv.URL, "admin@x.com", "pw-admin-1234")
	for i := 0; i < 5; i++ {
		if st, _ := doAs(t, http.MethodPost, base+"/signups", tok, `{"email":"admin@x.com"}`); st != http.StatusCreated {
			t.Fatalf("authenticated write %d should pass the anon-write cap, got %d", i, st)
		}
	}
}
