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

// loginTokenRow returns the single outstanding login-purpose _auth_tokens row.
func loginTokenRow(t *testing.T, db store.Adapter) store.Record {
	t.Helper()
	page, err := db.Find(context.Background(), store.Query{
		Collection: schema.AuthTokensCollection,
		Filters:    []store.Filter{{Field: schema.AuthTokenPurpose, Operator: store.Eq, Value: schema.AuthTokenPurposeLogin}},
		SkipCount:  true,
	})
	if err != nil {
		t.Fatalf("read login tokens: %v", err)
	}
	if len(page.Data) != 1 {
		t.Fatalf("expected exactly 1 login token, got %d", len(page.Data))
	}
	return page.Data[0]
}

func TestOTPLogin_ExpiredCodeRejected(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{OTPLogin: &gateway.OTPLoginOptions{}})
	seedUser(t, db, "u@x.com", "irrelevant-pw", "author")

	requestOTP(t, base, "u@x.com")
	code := otpCodeFromQueue(t, db)

	// Force the code past its TTL, then verify — the short-TTL defense must reject it.
	row := loginTokenRow(t, db)
	if _, err := db.Update(context.Background(), store.WriteInput{Collection: schema.AuthTokensCollection, Data: store.Record{
		"id":                      row["id"],
		schema.AuthTokenExpiresAt: "2000-01-01T00:00:00Z",
	}}); err != nil {
		t.Fatalf("age the token: %v", err)
	}
	if st, _ := do(t, http.MethodPost, base+"/auth/otp/verify", `{"email":"u@x.com","code":"`+code+`"}`); st != http.StatusUnauthorized {
		t.Fatalf("expired code should be 401, got %d", st)
	}
}

func TestOTPLogin_DisabledUserRejected(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{OTPLogin: &gateway.OTPLoginOptions{}})
	id := seedUserID(t, db, "u@x.com", "irrelevant-pw", "author")

	requestOTP(t, base, "u@x.com")
	code := otpCodeFromQueue(t, db)

	// Suspend the account after the code was issued: a valid code must not let a
	// disabled user in (mirrors the password-login guard).
	if _, err := db.Update(context.Background(), store.WriteInput{Collection: schema.UsersCollection, Data: store.Record{
		"id":              id,
		schema.UserStatus: schema.UserStatusDisabled,
	}}); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if st, _ := do(t, http.MethodPost, base+"/auth/otp/verify", `{"email":"u@x.com","code":"`+code+`"}`); st != http.StatusUnauthorized {
		t.Fatalf("disabled user with a valid code should be 401, got %d", st)
	}
}

func TestOTPLogin_CodeIsUserBound(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{OTPLogin: &gateway.OTPLoginOptions{}})
	seedUser(t, db, "a@x.com", "irrelevant-pw", "author")
	seedUser(t, db, "b@x.com", "irrelevant-pw", "author")

	// Only A has an outstanding code.
	requestOTP(t, base, "a@x.com")
	code := otpCodeFromQueue(t, db)

	// B cannot use A's code — the stored hash binds the user id.
	if st, _ := do(t, http.MethodPost, base+"/auth/otp/verify", `{"email":"b@x.com","code":"`+code+`"}`); st != http.StatusUnauthorized {
		t.Fatalf("A's code used as B should be 401, got %d", st)
	}
	// And A's own code still works (B's failed attempt didn't consume it).
	if st, _ := do(t, http.MethodPost, base+"/auth/otp/verify", `{"email":"a@x.com","code":"`+code+`"}`); st != http.StatusOK {
		t.Fatalf("A's code for A should be 200, got %d", st)
	}
}

func TestOTPLogin_RequestThrottledPerEmail(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{OTPLogin: &gateway.OTPLoginOptions{}})
	seedUser(t, db, "u@x.com", "irrelevant-pw", "author")

	// The per-email limiter admits a small burst then throttles. Fire more requests
	// than the burst back-to-back; throttled ones are still generic 204s but mint
	// and queue nothing, so the outbox holds at most the burst count — never one per
	// request. This is what stops the endpoint bombing an inbox.
	for range 6 {
		if st, _ := do(t, http.MethodPost, base+"/auth/otp/request", `{"email":"u@x.com"}`); st != http.StatusNoContent {
			t.Fatalf("otp request should be 204 even when throttled, got %d", st)
		}
	}
	if n := len(queuedNotifications(t, db)); n == 0 || n >= 6 {
		t.Fatalf("per-email throttle: %d codes queued from 6 requests, want a small burst (>0, <6)", n)
	}
}

func TestOTPLogin_VerifyRequiresBothFields(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{OTPLogin: &gateway.OTPLoginOptions{}})
	seedUser(t, db, "u@x.com", "irrelevant-pw", "author")

	for _, body := range []string{`{"email":"u@x.com"}`, `{"code":"123456"}`, `{}`} {
		if st, _ := do(t, http.MethodPost, base+"/auth/otp/verify", body); st != http.StatusUnprocessableEntity {
			t.Fatalf("verify %s should be 422, got %d", body, st)
		}
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
