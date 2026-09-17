package gateway_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// clearNotifications empties the outbox so a follow-up assertion sees only the
// rows enqueued after it.
func clearNotifications(t *testing.T, db store.Adapter) {
	t.Helper()
	for _, n := range queuedNotifications(t, db) {
		id, _ := n["id"].(string)
		if err := db.Delete(context.Background(), schema.NotificationsCollection, id); err != nil {
			t.Fatalf("clear notification %s: %v", id, err)
		}
	}
}

// otpCodeFromQueue reads the single queued login-OTP notification and returns its
// code. Delivery is asynchronous, so the test inspects the outbox row.
func otpCodeFromQueue(t *testing.T, db store.Adapter) string {
	t.Helper()
	ns := queuedNotifications(t, db)
	if len(ns) != 1 {
		t.Fatalf("expected exactly 1 queued notification, got %d", len(ns))
	}
	if kind, _ := ns[0][schema.NotificationKind].(string); kind != "login_otp" {
		t.Fatalf("queued notification kind = %q, want login_otp", kind)
	}
	code, _ := ns[0][schema.NotificationCode].(string)
	if len(code) != 6 {
		t.Fatalf("otp code = %q, want 6 digits", code)
	}
	return code
}

// requestOTP performs POST /auth/otp/request and asserts the generic 204.
func requestOTP(t *testing.T, base, email string) {
	t.Helper()
	if st, _ := do(t, http.MethodPost, base+"/auth/otp/request", `{"email":"`+email+`"}`); st != http.StatusNoContent {
		t.Fatalf("otp request should be 204, got %d", st)
	}
}

func TestOTPLogin_HappyPath(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{OTPLogin: &gateway.OTPLoginOptions{}})
	seedUser(t, db, "u@x.com", "irrelevant-pw", "author")

	requestOTP(t, base, "u@x.com")
	code := otpCodeFromQueue(t, db)

	// Verify with the code → 200 and a working session.
	st, body := do(t, http.MethodPost, base+"/auth/otp/verify", `{"email":"u@x.com","code":"`+code+`"}`)
	if st != http.StatusOK {
		t.Fatalf("otp verify should be 200, got %d %v", st, body)
	}
	tok, _ := body["token"].(string)
	if tok == "" || !tokenValid(t, base, tok) {
		t.Fatalf("otp verify should issue a working session token, got %q", tok)
	}

	// Single-use: the same code can't be redeemed again.
	if st, _ := do(t, http.MethodPost, base+"/auth/otp/verify", `{"email":"u@x.com","code":"`+code+`"}`); st != http.StatusUnauthorized {
		t.Fatalf("reused otp code should be 401, got %d", st)
	}
}

func TestOTPLogin_EnumerationSafe(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{OTPLogin: &gateway.OTPLoginOptions{}})

	// A request for an unknown email is still a generic 204 with nothing queued.
	requestOTP(t, base, "nobody@x.com")
	if n := len(queuedNotifications(t, db)); n != 0 {
		t.Fatalf("no notification should be queued for an unknown email, got %d", n)
	}

	// A wrong code for a non-existent account is the same flat 401 as any miss.
	if st, _ := do(t, http.MethodPost, base+"/auth/otp/verify", `{"email":"nobody@x.com","code":"123456"}`); st != http.StatusUnauthorized {
		t.Fatalf("verify for unknown email should be 401, got %d", st)
	}
}

func TestOTPLogin_WrongCodeBurnsAfterCap(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{OTPLogin: &gateway.OTPLoginOptions{MaxAttempts: 3}})
	seedUser(t, db, "u@x.com", "irrelevant-pw", "author")

	requestOTP(t, base, "u@x.com")
	realCode := otpCodeFromQueue(t, db)

	// Exhaust the 3-guess budget with wrong codes.
	for range 3 {
		if st, _ := do(t, http.MethodPost, base+"/auth/otp/verify", `{"email":"u@x.com","code":"000000"}`); st != http.StatusUnauthorized {
			t.Fatalf("wrong code should be 401, got %d", st)
		}
	}
	// The code is now burned — even the correct one fails.
	if st, _ := do(t, http.MethodPost, base+"/auth/otp/verify", `{"email":"u@x.com","code":"`+realCode+`"}`); st != http.StatusUnauthorized {
		t.Fatalf("correct code after the attempt cap should be 401 (burned), got %d", st)
	}
}

func TestOTPLogin_NewRequestInvalidatesOld(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{OTPLogin: &gateway.OTPLoginOptions{}})
	seedUser(t, db, "u@x.com", "irrelevant-pw", "author")

	requestOTP(t, base, "u@x.com")
	first := otpCodeFromQueue(t, db)

	// A second request mints a new code and invalidates the first. Clear the outbox
	// row so the queue holds only the new one.
	clearNotifications(t, db)
	requestOTP(t, base, "u@x.com")
	second := otpCodeFromQueue(t, db)

	if first == second {
		t.Skip("the two random codes collided (1-in-10^6); rerun")
	}
	if st, _ := do(t, http.MethodPost, base+"/auth/otp/verify", `{"email":"u@x.com","code":"`+first+`"}`); st != http.StatusUnauthorized {
		t.Fatalf("superseded code should be 401, got %d", st)
	}
	if st, _ := do(t, http.MethodPost, base+"/auth/otp/verify", `{"email":"u@x.com","code":"`+second+`"}`); st != http.StatusOK {
		t.Fatalf("newest code should log in, got %d", st)
	}
}

func TestOTPLogin_DisabledNotMounted(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{}) // OTP off
	seedUser(t, db, "u@x.com", "irrelevant-pw", "author")

	if st, _ := do(t, http.MethodPost, base+"/auth/otp/request", `{"email":"u@x.com"}`); st != http.StatusNotFound {
		t.Fatalf("otp request with OTP disabled should be 404, got %d", st)
	}
	if st, _ := do(t, http.MethodPost, base+"/auth/otp/verify", `{"email":"u@x.com","code":"123456"}`); st != http.StatusNotFound {
		t.Fatalf("otp verify with OTP disabled should be 404, got %d", st)
	}
}
