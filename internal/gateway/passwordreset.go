package gateway

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

const defaultResetTokenTTL = time.Hour

// notifier returns the configured Notifier, or a dev-log notifier so reset works
// out of the box without a mailer (the link is printed to the console).
func (s *Server) notifier() Notifier {
	if s.opts.Notifier != nil {
		return s.opts.Notifier
	}
	return logNotifier{logger: s.logger}
}

func (s *Server) resetTokenTTL() time.Duration {
	if s.opts.ResetTokenTTL > 0 {
		return s.opts.ResetTokenTTL
	}
	return defaultResetTokenTTL
}

// resetBases returns the allowlist of reset frontend URLs: the multi-frontend list
// when set (issue #32), else the single ResetLinkBase, else empty (dev: bare token).
func (s *Server) resetBases() []string {
	if len(s.opts.ResetLinkBases) > 0 {
		return s.opts.ResetLinkBases
	}
	if s.opts.ResetLinkBase != "" {
		return []string{s.opts.ResetLinkBase}
	}
	return nil
}

// resetLink builds the link a user follows to reset. It sends them to returnTo when
// that EXACTLY matches an allowlisted base (issue #32) — otherwise to the first
// allowlisted base, never to an arbitrary URL, so a forged return_to can't turn the
// reset email into an open redirect. With no base configured the "link" is the bare
// token (dev, where the operator reads it off the log).
func (s *Server) resetLink(rawToken, returnTo string) string {
	bases := s.resetBases()
	if len(bases) == 0 {
		return rawToken
	}
	base := bases[0]
	if returnTo != "" && slices.Contains(bases, returnTo) {
		base = returnTo
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "token=" + rawToken
}

// handleForgotPassword starts a password reset. It ALWAYS returns 200 with a
// generic body — whether or not the email exists — so it can't be used to
// enumerate accounts. A token is minted and delivered only for a real, active
// user (ADR-0019).
func (s *Server) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	data, err := decodeBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	email, _ := data[schema.UserEmail].(string)
	// return_to lets a multi-frontend instance send the user back to the client they
	// started from (issue #32); it is validated against the allowlist in resetLink.
	returnTo, _ := data["return_to"].(string)
	if email != "" {
		if user, uerr := s.findUserByEmail(r.Context(), email); uerr == nil && user != nil && !userDisabled(user) {
			// Only within the shared account-email budget (issue #50): forgot and OTP
			// draw on one per-recipient/daily/instance budget, so a /auth/forgot loop
			// can't drain the mail quota (and take down every real reset) or bomb an
			// inbox. Over budget ⇒ still 200, just no mail.
			if s.allowAccountMail(canonEmail(email), "password_reset") {
				s.issueResetToken(r.Context(), user, returnTo)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// issueResetToken mints a single-use reset token, stores its hash, and delivers
// the link. Failures are logged, never surfaced — the caller still returns 200.
// The recipient is the user's stored (validated, normalized) email, so no
// client-supplied string reaches the mailer.
func (s *Server) issueResetToken(ctx context.Context, user store.Record, returnTo string) {
	email, _ := user[schema.UserEmail].(string)
	raw, err := newSessionToken()
	if err != nil {
		s.logger.Error("reset token generation failed", "err", err)
		return
	}
	_, err = s.db.Create(ctx, store.WriteInput{Collection: schema.AuthTokensCollection, Data: store.Record{
		schema.AuthTokenHash:      hashToken(raw),
		schema.AuthTokenUserID:    user["id"],
		schema.AuthTokenPurpose:   schema.AuthTokenPurposeReset,
		schema.AuthTokenExpiresAt: nowUTC().Add(s.resetTokenTTL()),
	}})
	if err != nil {
		s.logger.Error("reset token store failed", "err", err)
		return
	}
	// Enqueue for durable, off-request-path delivery (ADR-0021 phase 3). A slow or
	// down mailer no longer blocks the response or drops the email.
	if err := s.enqueueNotification(ctx, Notification{To: email, Kind: "password_reset", Link: s.resetLink(raw, returnTo)}); err != nil {
		s.logger.Error("reset notification enqueue failed", "err", err)
	}
}

// handleResetPassword consumes a reset token and sets a new password. A bad,
// expired, or already-used token is a flat 400 (no oracle). On success the token
// is deleted (single-use) and ALL of the user's sessions are revoked — a reset
// assumes the account may be compromised.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	data, err := decodeBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	token, _ := data["token"].(string)
	next, _ := data["new"].(string)
	if token == "" || next == "" {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "token and new password are required"})
		return
	}
	row, err := s.findAuthToken(r.Context(), hashToken(token), schema.AuthTokenPurposeReset)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	if row == nil || pastRFC3339(row[schema.AuthTokenExpiresAt]) {
		writeError(w, http.StatusBadRequest, apiError{Code: "INVALID_TOKEN", Message: "the reset link is invalid or has expired"})
		return
	}
	if msg := s.validatePassword(next); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: msg})
		return
	}
	userID, _ := row[schema.AuthTokenUserID].(string)
	if err := s.setUserPassword(r.Context(), userID, next); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	// Single-use: drop the token, then revoke every session for the account.
	if id, ok := row["id"].(string); ok {
		_ = s.db.Delete(r.Context(), schema.AuthTokensCollection, id)
	}
	if err := s.revokeUserSessions(r.Context(), userID, ""); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// findAuthToken returns the _auth_tokens row for a token hash + purpose, or nil.
func (s *Server) findAuthToken(ctx context.Context, tokenHash, purpose string) (store.Record, error) {
	page, err := s.db.Find(ctx, store.Query{
		Collection: schema.AuthTokensCollection,
		Filters: []store.Filter{
			{Field: schema.AuthTokenHash, Operator: store.Eq, Value: tokenHash},
			{Field: schema.AuthTokenPurpose, Operator: store.Eq, Value: purpose},
		},
		Limit:     1,
		SkipCount: true,
	})
	if err != nil || len(page.Data) == 0 {
		return nil, err
	}
	return page.Data[0], nil
}

// sweepExpiredAuthTokens deletes reset/verify tokens past their TTL in one
// statement (RFC3339 lexical order matches time order).
func (s *Server) sweepExpiredAuthTokens(ctx context.Context) (int64, error) {
	return s.db.RawExec(ctx,
		`DELETE FROM `+schema.AuthTokensCollection+` WHERE `+schema.AuthTokenExpiresAt+` < $1`,
		nowUTC().Format(time.RFC3339))
}

// pastRFC3339 reports whether an RFC3339 datetime value is in the past (or is
// missing/unparseable, treated as expired).
func pastRFC3339(v any) bool {
	s, _ := v.(string)
	if s == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return true
	}
	return !t.After(nowUTC())
}
