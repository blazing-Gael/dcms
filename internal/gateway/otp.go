package gateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Passwordless email-OTP login (issue #11, ADR-0029). A user requests a short
// numeric code by email and exchanges it for a session — the same opaque session
// a password login issues. It reuses the _auth_tokens table (a new `login`
// purpose) and the durable notification outbox. It is opt-in (Options.OTPLogin);
// when off the endpoints are not mounted.
//
// The security model for a low-entropy 6-digit code rests on four things: a short
// TTL, single use, a per-code wrong-guess cap (so the 10^6 space can't be walked),
// and invalidation of any outstanding code when a new one is requested — plus a
// per-recipient request throttle so the endpoint can't bomb an inbox.

func (s *Server) otpTTL() time.Duration {
	if s.opts.OTPLogin != nil && s.opts.OTPLogin.TTL > 0 {
		return s.opts.OTPLogin.TTL
	}
	return defaultOTPTTL
}

func (s *Server) otpMaxAttempts() int {
	if s.opts.OTPLogin != nil && s.opts.OTPLogin.MaxAttempts > 0 {
		return s.opts.OTPLogin.MaxAttempts
	}
	return defaultOTPMaxAttempts
}

// handleOTPRequest starts an OTP login. Like password reset it ALWAYS returns 204,
// whether or not the email maps to an active account, so it can't be used to
// enumerate users. A code is minted and emailed only for a real, active user.
func (s *Server) handleOTPRequest(w http.ResponseWriter, r *http.Request) {
	data, err := decodeBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	email, _ := data[schema.UserEmail].(string)
	if email == "" {
		// Nothing to act on, but stay generic — a missing email is not an oracle.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Throttle per recipient (on top of the per-IP auth tier) so the endpoint can't
	// bomb one inbox from many IPs. A throttled request is still a generic 204 — it
	// reveals nothing — it simply skips minting and sending.
	if s.otpEmailLimiter != nil {
		if ok, _ := s.otpEmailLimiter.Allow("otp:" + canonEmail(email)); !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if user, uerr := s.findUserByEmail(r.Context(), email); uerr == nil && user != nil && !userDisabled(user) {
		s.issueOTPCode(r.Context(), user)
	}
	w.WriteHeader(http.StatusNoContent)
}

// issueOTPCode invalidates any outstanding login code for the user, mints a fresh
// one, stores only its hash, and enqueues the email. Failures are logged, never
// surfaced — the caller still returns 204. The recipient is the user's stored,
// normalized email, so no client-supplied string reaches the mailer.
func (s *Server) issueOTPCode(ctx context.Context, user store.Record) {
	userID, _ := user["id"].(string)
	if userID == "" {
		return
	}
	// Single outstanding code per user: drop earlier ones so a new request always
	// invalidates the old (issue #11) and the unique token_hash can't collide.
	if _, err := s.db.RawExec(ctx,
		`DELETE FROM `+schema.AuthTokensCollection+` WHERE `+schema.AuthTokenUserID+` = $1 AND `+schema.AuthTokenPurpose+` = $2`,
		userID, schema.AuthTokenPurposeLogin); err != nil {
		s.logger.Error("otp: clearing prior codes failed", "err", err)
		return
	}
	code, err := generateOTPCode()
	if err != nil {
		s.logger.Error("otp: code generation failed", "err", err)
		return
	}
	_, err = s.db.Create(ctx, store.WriteInput{Collection: schema.AuthTokensCollection, Data: store.Record{
		schema.AuthTokenHash:      otpHash(userID, code),
		schema.AuthTokenUserID:    userID,
		schema.AuthTokenPurpose:   schema.AuthTokenPurposeLogin,
		schema.AuthTokenExpiresAt: nowUTC().Add(s.otpTTL()),
		schema.AuthTokenAttempts:  0,
	}})
	if err != nil {
		s.logger.Error("otp: code store failed", "err", err)
		return
	}
	email, _ := user[schema.UserEmail].(string)
	if err := s.enqueueNotification(ctx, Notification{To: email, Kind: "login_otp", Code: code}); err != nil {
		s.logger.Error("otp: notification enqueue failed", "err", err)
	}
}

// handleOTPVerify exchanges {email, code} for a session, identical to a password
// login on success. Every failure — unknown email, no outstanding code, expired,
// too many wrong guesses, or a wrong code — is the same flat 401, so the endpoint
// leaks nothing. A correct code is single-use: it is deleted before the session is
// issued.
func (s *Server) handleOTPVerify(w http.ResponseWriter, r *http.Request) {
	data, err := decodeBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	email, _ := data[schema.UserEmail].(string)
	code, _ := data["code"].(string)
	if email == "" || code == "" {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "email and code are required"})
		return
	}

	user, err := s.findUserByEmail(r.Context(), email)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	if user == nil || userDisabled(user) {
		otpUnauthorized(w)
		return
	}
	userID, _ := user["id"].(string)

	row, err := s.findLoginToken(r.Context(), userID)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	if row == nil || pastRFC3339(row[schema.AuthTokenExpiresAt]) {
		otpUnauthorized(w)
		return
	}
	tokenID, _ := row["id"].(string)

	// A code that has already burned through its attempt budget is dead — remove it
	// and fail, so a walk of the 10^6 space can't continue past the cap.
	if intOf(row[schema.AuthTokenAttempts]) >= s.otpMaxAttempts() {
		_ = s.db.Delete(r.Context(), schema.AuthTokensCollection, tokenID)
		otpUnauthorized(w)
		return
	}

	stored, _ := row[schema.AuthTokenHash].(string)
	if subtle.ConstantTimeCompare([]byte(stored), []byte(otpHash(userID, code))) != 1 {
		// Wrong guess: count it, and burn the code once the cap is reached.
		attempts := intOf(row[schema.AuthTokenAttempts]) + 1
		if attempts >= s.otpMaxAttempts() {
			_ = s.db.Delete(r.Context(), schema.AuthTokensCollection, tokenID)
		} else if _, uerr := s.db.Update(r.Context(), store.WriteInput{Collection: schema.AuthTokensCollection, Data: store.Record{
			"id":                     tokenID,
			schema.AuthTokenAttempts: attempts,
		}}); uerr != nil {
			s.logger.Warn("otp: attempt increment failed", "err", uerr)
		}
		otpUnauthorized(w)
		return
	}

	// Correct: single-use, so delete before issuing the session.
	if derr := s.db.Delete(r.Context(), schema.AuthTokensCollection, tokenID); derr != nil {
		writeStoreError(w, s.logger, r, derr)
		return
	}
	token, expiresAt, err := s.issueSession(r.Context(), userID, rolesOf(user))
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.requestIsSecure(r),
		Expires:  expiresAt,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_at": expiresAt.Format(time.RFC3339),
		"user":       publicUser(user),
	})
}

// otpUnauthorized is the single generic failure for every OTP verify miss.
func otpUnauthorized(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: "invalid or expired code"})
}

// findLoginToken returns the user's outstanding login-purpose token, or nil. At
// most one exists (issueOTPCode clears prior ones); lookup is by user + purpose,
// not by hash, so a wrong code still finds the row and its attempt counter.
func (s *Server) findLoginToken(ctx context.Context, userID string) (store.Record, error) {
	page, err := s.db.Find(ctx, store.Query{
		Collection: schema.AuthTokensCollection,
		Filters: []store.Filter{
			{Field: schema.AuthTokenUserID, Operator: store.Eq, Value: userID},
			{Field: schema.AuthTokenPurpose, Operator: store.Eq, Value: schema.AuthTokenPurposeLogin},
		},
		Limit:     1,
		SkipCount: true,
	})
	if err != nil || len(page.Data) == 0 {
		return nil, err
	}
	return page.Data[0], nil
}

// otpHash binds the code to the user before hashing, so two users who happen to
// draw the same code get different stored hashes (the token_hash column is
// unique), and a leaked hash is useless without the user id.
func otpHash(userID, code string) string { return hashToken(userID + ":" + code) }

// generateOTPCode returns a uniformly-random zero-padded numeric code.
func generateOTPCode() (string, error) {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(otpCodeDigits), nil)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%0*d", otpCodeDigits, n), nil
}
