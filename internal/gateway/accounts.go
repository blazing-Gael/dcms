package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// canonEmail is the lookup form of an email: trimmed and lower-cased. Used on the
// read path so a login/reset for "User@X.com" matches a stored "user@x.com".
func canonEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// normalizeEmail validates and canonicalizes an email for the WRITE path. It
// rejects anything net/mail can't parse as a bare address — which also blocks
// header-injection payloads (embedded CR/LF) before they can reach the mailer.
func normalizeEmail(raw string) (string, bool) {
	addr, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil || addr.Name != "" { // require a bare address, not "Name <a@b>"
		return "", false
	}
	return strings.ToLower(addr.Address), true
}

// stringList coerces a decoded JSON value (roles, typically []any of strings)
// into a []string, dropping non-strings.
func stringList(v any) []string {
	switch xs := v.(type) {
	case []string:
		return xs
	case []any:
		out := make([]string, 0, len(xs))
		for _, e := range xs {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// jsonList marshals a []string to a JSON array string (the roles column shape).
func jsonList(roles []string) (string, error) {
	if roles == nil {
		roles = []string{}
	}
	b, err := json.Marshal(roles)
	return string(b), err
}

const (
	defaultPasswordMinLength = 8
	// bcryptMaxBytes is bcrypt's hard input limit — it silently truncates beyond
	// 72 bytes, so a longer passphrase would be weaker than it reads. We reject.
	bcryptMaxBytes = 72
)

// ── policy / role helpers (ADR-0019) ─────────────────────────────────────────

func (s *Server) passwordMinLength() int {
	if s.opts.PasswordMinLength > 0 {
		return s.opts.PasswordMinLength
	}
	return defaultPasswordMinLength
}

// validatePassword returns a client-facing message if pw violates policy, else "".
func (s *Server) validatePassword(pw string) string {
	if len(pw) < s.passwordMinLength() {
		return fmt.Sprintf("password must be at least %d characters", s.passwordMinLength())
	}
	if len(pw) > bcryptMaxBytes {
		return "password must be at most 72 bytes"
	}
	return ""
}

// adminRoles returns the roles permitted to administer users, defaulting to admin.
func (s *Server) adminRoles() []string {
	if len(s.opts.AdminRoles) > 0 {
		return s.opts.AdminRoles
	}
	return []string{"admin"}
}

func (s *Server) isAdmin(p principal) bool {
	if !p.Authenticated {
		return false
	}
	return s.hasAdminRole(p.Roles)
}

// hasAdminRole reports whether any of roles is an admin role.
func (s *Server) hasAdminRole(roles []string) bool {
	for _, r := range roles {
		for _, ar := range s.adminRoles() {
			if r == ar {
				return true
			}
		}
	}
	return false
}

// activeAdminCount counts non-disabled users holding an admin role. It runs only
// on admin-mutating operations (never the hot path). Roles are stored as a JSON
// array of quoted names, so a LIKE on the quoted token matches precisely.
func (s *Server) activeAdminCount(ctx context.Context) (int, error) {
	admins := s.adminRoles()
	conds := make([]string, 0, len(admins))
	args := make([]any, 0, len(admins))
	for i, ar := range admins {
		conds = append(conds, fmt.Sprintf("%s LIKE $%d", schema.UserRoles, i+1))
		args = append(args, `%"`+ar+`"%`)
	}
	q := `SELECT COUNT(*) AS n FROM ` + schema.UsersCollection +
		` WHERE (` + strings.Join(conds, " OR ") + `)` +
		` AND (` + schema.UserStatus + ` IS NULL OR ` + schema.UserStatus + ` != '` + schema.UserStatusDisabled + `')`
	rows, err := s.db.RawQuery(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return idemInt(rows[0]["n"], 0), nil
}

// wouldOrphanAdmin reports whether an operation that leaves the target no longer
// an active admin would remove the last one — the invariant "there is always at
// least one active admin" (ADR-0019, reliability).
func (s *Server) wouldOrphanAdmin(ctx context.Context, currentlyActiveAdmin bool) (bool, error) {
	if !currentlyActiveAdmin {
		return false, nil // demoting a non-admin can't orphan admin access
	}
	n, err := s.activeAdminCount(ctx)
	if err != nil {
		return false, err
	}
	return n <= 1, nil
}

// requireAdmin gates an admin endpoint: 401 if anonymous, 403 if not an admin.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (principal, bool) {
	p := principalFromContext(r.Context())
	if !p.Authenticated {
		writeError(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: "authentication required"})
		return p, false
	}
	if !s.isAdmin(p) {
		writeError(w, http.StatusForbidden, apiError{Code: "FORBIDDEN", Message: "admin role required"})
		return p, false
	}
	return p, true
}

// validateRoles returns a message if any role is not a declared role, else "".
func (s *Server) validateRoles(roles []string) string {
	for _, role := range roles {
		if !s.schema.HasRole(role) {
			return "unknown role: " + role
		}
	}
	return ""
}

// userDisabled reports whether a _users row is suspended. An empty/absent status
// is active (the status column is additive; ADR-0019).
func userDisabled(user store.Record) bool {
	st, _ := user[schema.UserStatus].(string)
	return st == schema.UserStatusDisabled
}

// revokeUserSessions deletes a user's sessions, optionally keeping the session
// whose token hashes to exceptHash (used to keep the current device on a password
// change). Immediate because sessions are opaque DB rows (ADR-0016).
func (s *Server) revokeUserSessions(ctx context.Context, userID, exceptHash string) error {
	if exceptHash != "" {
		_, err := s.db.RawExec(ctx,
			`DELETE FROM `+schema.SessionsCollection+` WHERE `+schema.SessionUserID+` = $1 AND `+schema.SessionTokenHash+` != $2`,
			userID, exceptHash)
		return err
	}
	_, err := s.db.RawExec(ctx,
		`DELETE FROM `+schema.SessionsCollection+` WHERE `+schema.SessionUserID+` = $1`, userID)
	return err
}

// setUserPassword hashes and stores a new password for a user.
func (s *Server) setUserPassword(ctx context.Context, userID, plain string) error {
	hash, err := hashPassword(plain)
	if err != nil {
		return err
	}
	_, err = s.db.Update(ctx, store.WriteInput{Collection: schema.UsersCollection, Data: store.Record{
		"id":                     userID,
		schema.UserPasswordHash: hash,
	}})
	return err
}

// ── self-service (/auth) ─────────────────────────────────────────────────────

// handleRegister creates a new local account when self-registration is enabled.
// It is enumeration-safe: a taken email returns the same generic success as a
// new one, and it never auto-logs-in (the client logs in separately), so the
// response shape can't reveal whether the email already existed.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.registrationEnabled() {
		writeError(w, http.StatusForbidden, apiError{Code: "FORBIDDEN", Message: "self-registration is disabled"})
		return
	}
	data, err := decodeBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	rawEmail, _ := data[schema.UserEmail].(string)
	password, _ := data["password"].(string)
	name, _ := data[schema.UserName].(string)
	email, ok := normalizeEmail(rawEmail)
	if !ok || password == "" {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "a valid email and a password are required"})
		return
	}
	if msg := s.validatePassword(password); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: msg})
		return
	}

	// Create the account. A taken email is swallowed so the response is uniform.
	user, err := CreateUser(r.Context(), s.db, email, password, s.opts.Registration.DefaultRoles)
	switch {
	case err == nil:
		// Stamp name (if any) and an explicit active status on the new row.
		_, _ = s.db.Update(r.Context(), store.WriteInput{Collection: schema.UsersCollection, Data: store.Record{
			"id":              user["id"],
			schema.UserName:   name,
			schema.UserStatus: schema.UserStatusActive,
		}})
	case err == ErrUserExists:
		// Enumeration-safe: fall through to the same success response.
	default:
		writeStoreError(w, s.logger, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) registrationEnabled() bool { return s.opts.Registration != nil }

// handlePasswordChange lets an authenticated user change their own password. It
// verifies the current password and then revokes the user's *other* sessions.
func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !p.Authenticated {
		writeError(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: "authentication required"})
		return
	}
	data, err := decodeBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	current, _ := data["current"].(string)
	next, _ := data["new"].(string)
	if current == "" || next == "" {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "current and new passwords are required"})
		return
	}
	user, err := s.db.FindOne(r.Context(), schema.UsersCollection, p.ID)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	if hash, _ := user[schema.UserPasswordHash].(string); !checkPassword(hash, current) {
		writeError(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: "current password is incorrect"})
		return
	}
	if msg := s.validatePassword(next); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: msg})
		return
	}
	if err := s.setUserPassword(r.Context(), p.ID, next); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	// Keep the current device, log out the rest.
	keep := ""
	if tok := sessionTokenFromRequest(r); tok != "" {
		keep = hashToken(tok)
	}
	if err := s.revokeUserSessions(r.Context(), p.ID, keep); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleLogoutAll revokes all of the authenticated caller's sessions and clears
// the cookie.
func (s *Server) handleLogoutAll(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !p.Authenticated {
		writeError(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: "authentication required"})
		return
	}
	if err := s.revokeUserSessions(r.Context(), p.ID, ""); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// ── admin (/admin/users) ─────────────────────────────────────────────────────

func (s *Server) handleAdminUserList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	page, err := s.db.Find(r.Context(), store.Query{Collection: schema.UsersCollection, Limit: 100})
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	users := make([]map[string]any, 0, len(page.Data))
	for _, u := range page.Data {
		users = append(users, publicUser(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": users, "meta": map[string]any{"total": page.Total}})
}

func (s *Server) handleAdminUserCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	data, err := decodeBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	rawEmail, _ := data[schema.UserEmail].(string)
	password, _ := data["password"].(string)
	name, _ := data[schema.UserName].(string)
	roles := stringList(data[schema.UserRoles])
	email, ok := normalizeEmail(rawEmail)
	if !ok || password == "" {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "a valid email and a password are required"})
		return
	}
	if msg := s.validatePassword(password); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: msg})
		return
	}
	if msg := s.validateRoles(roles); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: msg})
		return
	}
	user, err := CreateUser(r.Context(), s.db, email, password, roles)
	if err == ErrUserExists {
		writeError(w, http.StatusConflict, apiError{Code: "CONFLICT", Message: "a user with that email already exists"})
		return
	}
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	if name != "" {
		user, _ = s.db.Update(r.Context(), store.WriteInput{Collection: schema.UsersCollection,
			Data: store.Record{"id": user["id"], schema.UserName: name, schema.UserStatus: schema.UserStatusActive}})
	}
	writeJSON(w, http.StatusCreated, map[string]any{"data": publicUser(user)})
}

func (s *Server) handleAdminUserGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	user, err := s.db.FindOne(r.Context(), schema.UsersCollection, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": publicUser(user)})
}

// handleAdminUserUpdate edits name/roles/status, and (optionally) resets the
// password — in which case the target's sessions are revoked.
func (s *Server) handleAdminUserUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	id := chi.URLParam(r, "id")
	data, err := decodeBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	// Load the target first so the last-admin guard can compare before/after.
	target, err := s.db.FindOne(r.Context(), schema.UsersCollection, id)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	postRoles := rolesOf(target)
	postDisabled := userDisabled(target)

	upd := store.Record{"id": id}
	if name, ok := data[schema.UserName].(string); ok {
		upd[schema.UserName] = name
	}
	if _, ok := data[schema.UserRoles]; ok {
		roles := stringList(data[schema.UserRoles])
		if msg := s.validateRoles(roles); msg != "" {
			writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: msg})
			return
		}
		rj, _ := jsonList(roles)
		upd[schema.UserRoles] = rj
		postRoles = roles
	}
	if status, ok := data[schema.UserStatus].(string); ok {
		if status != schema.UserStatusActive && status != schema.UserStatusDisabled {
			writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "status must be active or disabled"})
			return
		}
		upd[schema.UserStatus] = status
		postDisabled = status == schema.UserStatusDisabled
	}

	// Guard: refuse an edit that would strip admin from the last active admin.
	wasActiveAdmin := s.hasAdminRole(rolesOf(target)) && !userDisabled(target)
	stillActiveAdmin := s.hasAdminRole(postRoles) && !postDisabled
	if wasActiveAdmin && !stillActiveAdmin {
		if orphan, oerr := s.wouldOrphanAdmin(r.Context(), true); oerr != nil {
			writeStoreError(w, s.logger, r, oerr)
			return
		} else if orphan {
			writeError(w, http.StatusConflict, apiError{Code: "LAST_ADMIN", Message: "cannot remove the last remaining active admin"})
			return
		}
	}

	resetPassword, _ := data["password"].(string)
	if resetPassword != "" {
		if msg := s.validatePassword(resetPassword); msg != "" {
			writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: msg})
			return
		}
		hash, herr := hashPassword(resetPassword)
		if herr != nil {
			writeStoreError(w, s.logger, r, herr)
			return
		}
		upd[schema.UserPasswordHash] = hash
	}

	user, err := s.db.Update(r.Context(), store.WriteInput{Collection: schema.UsersCollection, Data: upd})
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	// A disabled user or a password reset should not keep live sessions.
	if resetPassword != "" || userDisabled(user) {
		if err := s.revokeUserSessions(r.Context(), id, ""); err != nil {
			writeStoreError(w, s.logger, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": publicUser(user)})
}

func (s *Server) handleAdminUserDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	id := chi.URLParam(r, "id")
	// Refuse to delete the last active admin (would leave no one to administer).
	target, err := s.db.FindOne(r.Context(), schema.UsersCollection, id)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	wasActiveAdmin := s.hasAdminRole(rolesOf(target)) && !userDisabled(target)
	if orphan, oerr := s.wouldOrphanAdmin(r.Context(), wasActiveAdmin); oerr != nil {
		writeStoreError(w, s.logger, r, oerr)
		return
	} else if orphan {
		writeError(w, http.StatusConflict, apiError{Code: "LAST_ADMIN", Message: "cannot delete the last remaining active admin"})
		return
	}
	if err := s.db.Delete(r.Context(), schema.UsersCollection, id); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent) // _sessions cascade on user delete
}

func (s *Server) handleAdminUserLogoutAll(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if err := s.revokeUserSessions(r.Context(), chi.URLParam(r, "id"), ""); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
