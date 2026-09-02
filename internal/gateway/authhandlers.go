package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Local authentication endpoints (ADR-0016), mounted at /auth outside the
// collection API. Login exchanges credentials for an opaque session token;
// logout revokes it; me reports the current principal.

// loginRequest is the POST /auth/login body.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handleLogin verifies credentials and issues an opaque session. It never reveals
// whether the failure was an unknown email or a wrong password — both are a flat
// 401, so the endpoint can't be used to enumerate accounts.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "request body must be a JSON object"})
		return
	}
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "email and password are required"})
		return
	}

	user, err := s.findUserByEmail(r.Context(), req.Email)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	hash, _ := user[schema.UserPasswordHash].(string)
	if user == nil || !checkPassword(hash, req.Password) {
		writeError(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: "invalid email or password"})
		return
	}

	token, expiresAt, err := s.issueSession(r.Context(), user["id"].(string))
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

// handleLogout revokes the request's session (if any) and clears the cookie. It
// is idempotent — logging out without a session is a no-op 204.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if tok := sessionTokenFromRequest(r); tok != "" {
		if sess, err := s.findSessionByHash(r.Context(), hashToken(tok)); err == nil && sess != nil {
			if id, ok := sess["id"].(string); ok {
				_ = s.db.Delete(r.Context(), schema.SessionsCollection, id)
			}
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleMe returns the current principal. It is the client's way to discover who
// it is authenticated as (and to check its roles).
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := principalFromContext(r.Context())
	if !p.authenticated {
		writeError(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: "authentication required"})
		return
	}
	out := map[string]any{"id": p.id, "roles": p.roles}
	// Enrich with email/name when the user row is still readable.
	if user, err := s.db.FindOne(r.Context(), schema.UsersCollection, p.id); err == nil {
		if email, ok := user[schema.UserEmail].(string); ok {
			out["email"] = email
		}
		if name, ok := user[schema.UserName].(string); ok && name != "" {
			out["name"] = name
		}
	}
	writeData(w, http.StatusOK, out)
}

// issueSession creates a session row for userID and returns the raw opaque token
// (shown to the client once) and its expiry. Only the token's hash is stored.
func (s *Server) issueSession(ctx context.Context, userID string) (string, time.Time, error) {
	token, err := newSessionToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt := time.Now().UTC().Add(s.sessionTTL())
	_, err = s.db.Create(ctx, store.WriteInput{
		Collection: schema.SessionsCollection,
		Data: store.Record{
			schema.SessionTokenHash: hashToken(token),
			schema.SessionUserID:    userID,
			schema.SessionExpiresAt: expiresAt,
		},
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

// sessionTTL is the configured session lifetime, or the engine default.
func (s *Server) sessionTTL() time.Duration {
	if raw := s.schema.Auth.Session.TTL; raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return defaultSessionTTL
}

// findUserByEmail returns the _users row for an email, or nil if none exists.
func (s *Server) findUserByEmail(ctx context.Context, email string) (store.Record, error) {
	page, err := s.db.Find(ctx, store.Query{
		Collection: schema.UsersCollection,
		Filters:    []store.Filter{{Field: schema.UserEmail, Operator: store.Eq, Value: email}},
		Limit:      1,
		SkipCount:  true,
	})
	if err != nil {
		return nil, err
	}
	if len(page.Data) == 0 {
		return nil, nil
	}
	return page.Data[0], nil
}

// findSessionByHash returns the _sessions row for a token hash, or nil.
func (s *Server) findSessionByHash(ctx context.Context, tokenHash string) (store.Record, error) {
	page, err := s.db.Find(ctx, store.Query{
		Collection: schema.SessionsCollection,
		Filters:    []store.Filter{{Field: schema.SessionTokenHash, Operator: store.Eq, Value: tokenHash}},
		Limit:      1,
		SkipCount:  true,
	})
	if err != nil {
		return nil, err
	}
	if len(page.Data) == 0 {
		return nil, nil
	}
	return page.Data[0], nil
}

// publicUser projects a _users row to the fields safe to return to a client —
// never the password hash.
func publicUser(user store.Record) map[string]any {
	return map[string]any{
		"id":    user["id"],
		"email": user[schema.UserEmail],
		"name":  user[schema.UserName],
		"roles": rolesOf(user),
	}
}
