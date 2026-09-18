package gateway_test

import (
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// A run of failed logins locks the account across the shared credential budget
// (issue #50): after the threshold even the correct password is refused, and a
// different account is unaffected.
func TestLogin_AccountLockoutAfterFailures(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{})
	seedUser(t, db, "u@x.com", "correcthorse", "author")
	seedUser(t, db, "v@x.com", "correcthorse", "author")

	for range 10 {
		if st, _ := do(t, http.MethodPost, base+"/auth/login", `{"email":"u@x.com","password":"wrong"}`); st != http.StatusUnauthorized {
			t.Fatalf("wrong password should be 401, got %d", st)
		}
	}
	// The account is now locked — the correct password is refused too.
	if st, _ := do(t, http.MethodPost, base+"/auth/login", `{"email":"u@x.com","password":"correcthorse"}`); st != http.StatusUnauthorized {
		t.Fatalf("locked account should refuse the correct password (401), got %d", st)
	}
	// A different account is not affected.
	if st, _ := do(t, http.MethodPost, base+"/auth/login", `{"email":"v@x.com","password":"correcthorse"}`); st != http.StatusOK {
		t.Fatalf("an unrelated account should still log in (200), got %d", st)
	}
}

// OTP verify honors the same per-account lock as password login (one shared
// budget): once the account is locked by failed logins, even a valid OTP code is
// refused — proving the lock is shared, not per-endpoint.
func TestOTP_HonorsSharedCredentialLock(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{OTPLogin: &gateway.OTPLoginOptions{}})
	seedUser(t, db, "u@x.com", "correcthorse", "author")

	// Mint a real code first (within the mail budget) and capture it.
	if st, _ := do(t, http.MethodPost, base+"/auth/otp/request", `{"email":"u@x.com"}`); st != http.StatusNoContent {
		t.Fatalf("otp request: %d", st)
	}
	code := otpCodeFromQueue(t, db)

	// Lock the account via failed password logins.
	for range 10 {
		do(t, http.MethodPost, base+"/auth/login", `{"email":"u@x.com","password":"wrong"}`)
	}

	// The valid code would normally log in; under the shared lock it is refused.
	if st, _ := do(t, http.MethodPost, base+"/auth/otp/verify", `{"email":"u@x.com","code":"`+code+`"}`); st != http.StatusUnauthorized {
		t.Fatalf("a valid code on a locked account should be 401, got %d", st)
	}
}

// Rapid password-reset requests are capped by the shared account-email budget: the
// endpoint still answers 200 every time, but only the budgeted number of mails is
// actually enqueued — so a /auth/forgot loop can't drain the mail quota.
func TestForgot_MailSuppressedByCap(t *testing.T) {
	base, db := newAccountsServer(t, gateway.Options{ResetLinkBase: "https://site/reset"})
	seedUser(t, db, "u@x.com", "correcthorse", "author")

	for range 5 {
		if st, _ := do(t, http.MethodPost, base+"/auth/forgot", `{"email":"u@x.com"}`); st != http.StatusOK {
			t.Fatalf("forgot should always be 200, got %d", st)
		}
	}
	// The generic 200 hides it, but the mail budget suppressed the overflow: rapid
	// requests are held to the short-window burst (2), not one mail each.
	if n := len(queuedNotifications(t, db)); n == 0 || n > 2 {
		t.Fatalf("rapid forgots should be capped to the burst, got %d queued", n)
	}
}
