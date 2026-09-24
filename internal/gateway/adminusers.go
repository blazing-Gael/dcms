package gateway

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Admin panel — user management (ADR-0035, phase 2 core). Full CRUD over accounts,
// role assignment, status, password reset and log-out-everywhere, reusing the same
// guarded primitives as the JSON admin-users API (ADR-0019) — including the
// last-active-admin guard — so the panel can't lock the instance out of itself.
// Admin-role gated.

type adminUserRow struct{ ID, Email, Name, Roles, Status string }

// adminUsersData is the users page: the account rows plus an ops readout of today's
// account-mail usage against the instance ceiling (#50), since the mail cap is what
// silently stops password resets when it trips.
type adminUsersData struct {
	Rows     []adminUserRow
	MailUsed int
	MailCap  int // 0 ⇒ no instance ceiling configured
}

type adminRoleChoice struct {
	Name, Label string
	Checked     bool
}

type adminUserFormData struct {
	ID, Email, Name, Status, Title, Action string
	IsNew                                  bool
	Roles                                  []adminRoleChoice
	Locked                                 bool   // credential lockout active (#50)
	LockedFor                              string // human duration remaining, when locked
}

func (s *Server) adminUsersList(w http.ResponseWriter, r *http.Request) {
	page, err := s.db.Find(r.Context(), store.Query{Collection: schema.UsersCollection, Limit: adminListLimit, Sort: "-created_at"})
	if err != nil {
		s.adminError(w, r, "Could not load users.")
		return
	}
	rows := make([]adminUserRow, 0, len(page.Data))
	for _, u := range page.Data {
		id, _ := u["id"].(string)
		email, _ := u[schema.UserEmail].(string)
		name, _ := u[schema.UserName].(string)
		status, _ := u[schema.UserStatus].(string)
		if status == "" {
			status = schema.UserStatusActive
		}
		rows = append(rows, adminUserRow{ID: id, Email: email, Name: name, Roles: joinComma(rolesOf(u)), Status: status})
	}
	data := adminUsersData{Rows: rows}
	if s.accountMail != nil {
		ms := s.accountMail.stats()
		data.MailUsed, data.MailCap = ms.SentToday, ms.InstanceCap
	}
	s.renderAdmin(w, r, "users", &adminPage{Title: "Users", Flash: r.URL.Query().Get("flash"), Data: data})
}

func (s *Server) adminUserNewForm(w http.ResponseWriter, r *http.Request) {
	s.renderAdmin(w, r, "user_form", &adminPage{Title: "New user", Data: adminUserFormData{
		Title: "New user", Action: adminBasePath + "/users", IsNew: true, Status: schema.UserStatusActive,
		Roles: s.adminRoleChoices(nil),
	}})
}

func (s *Server) adminUserCreate(w http.ResponseWriter, r *http.Request) {
	if !s.adminCSRFValid(r) {
		s.adminForbidden(w, r)
		return
	}
	_ = r.ParseForm()
	email, ok := normalizeEmail(r.FormValue("email"))
	password := r.FormValue("password")
	name := strings.TrimSpace(r.FormValue("name"))
	roles := r.Form["roles"]
	if err := s.adminUserFormError(ok, password, roles); err != "" {
		s.adminUserFormRerender(w, r, adminUserFormData{Title: "New user", Action: adminBasePath + "/users", IsNew: true, Email: r.FormValue("email"), Name: name, Status: schema.UserStatusActive, Roles: s.adminRoleChoices(roles)}, err)
		return
	}
	user, err := CreateUser(r.Context(), s.db, email, password, roles)
	if err == ErrUserExists {
		s.adminUserFormRerender(w, r, adminUserFormData{Title: "New user", Action: adminBasePath + "/users", IsNew: true, Email: r.FormValue("email"), Name: name, Status: schema.UserStatusActive, Roles: s.adminRoleChoices(roles)}, "A user with that email already exists.")
		return
	}
	if err != nil {
		s.adminError(w, r, "Could not create the user.")
		return
	}
	if name != "" {
		_, _ = s.db.Update(r.Context(), store.WriteInput{Collection: schema.UsersCollection,
			Data: store.Record{"id": user["id"], schema.UserName: name}})
	}
	http.Redirect(w, r, adminBasePath+"/users?flash=User+created", http.StatusSeeOther)
}

func (s *Server) adminUserEditForm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	u, err := s.db.FindOne(r.Context(), schema.UsersCollection, id)
	if err != nil {
		s.adminError(w, r, "User not found.")
		return
	}
	email, _ := u[schema.UserEmail].(string)
	name, _ := u[schema.UserName].(string)
	status, _ := u[schema.UserStatus].(string)
	if status == "" {
		status = schema.UserStatusActive
	}
	locked, lockedFor := s.adminUserLock(id)
	s.renderAdmin(w, r, "user_form", &adminPage{Title: "Edit user", Flash: r.URL.Query().Get("flash"), Data: adminUserFormData{
		ID: id, Email: email, Name: name, Status: status, Title: "Edit " + email,
		Action: adminBasePath + "/users/" + id, Roles: s.adminRoleChoices(rolesOf(u)),
		Locked: locked, LockedFor: lockedFor,
	}})
}

func (s *Server) adminUserUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.adminCSRFValid(r) {
		s.adminForbidden(w, r)
		return
	}
	id := chi.URLParam(r, "id")
	_ = r.ParseForm()
	name := strings.TrimSpace(r.FormValue("name"))
	status := r.FormValue("status")
	roles := r.Form["roles"]
	if msg := s.validateRoles(roles); msg != "" {
		s.adminError(w, r, msg)
		return
	}
	if status != schema.UserStatusActive && status != schema.UserStatusDisabled {
		status = schema.UserStatusActive
	}
	// Last-active-admin guard: refuse an edit that removes the final admin.
	target, err := s.db.FindOne(r.Context(), schema.UsersCollection, id)
	if err != nil {
		s.adminError(w, r, "User not found.")
		return
	}
	wasActiveAdmin := s.hasAdminRole(rolesOf(target)) && !userDisabled(target)
	stillActiveAdmin := s.hasAdminRole(roles) && status != schema.UserStatusDisabled
	if wasActiveAdmin && !stillActiveAdmin {
		if orphan, oerr := s.wouldOrphanAdmin(r.Context(), true); oerr == nil && orphan {
			s.adminError(w, r, "This is the last active admin — you can't remove its admin role or disable it.")
			return
		}
	}
	rj, _ := jsonList(roles)
	if _, err := s.db.Update(r.Context(), store.WriteInput{Collection: schema.UsersCollection, Data: store.Record{
		"id": id, schema.UserName: name, schema.UserStatus: status, schema.UserRoles: rj,
	}}); err != nil {
		s.adminError(w, r, "Could not update the user.")
		return
	}
	http.Redirect(w, r, adminBasePath+"/users/"+id+"?flash=Saved", http.StatusSeeOther)
}

func (s *Server) adminUserResetPassword(w http.ResponseWriter, r *http.Request) {
	if !s.adminCSRFValid(r) {
		s.adminForbidden(w, r)
		return
	}
	id := chi.URLParam(r, "id")
	password := r.FormValue("password")
	if msg := s.validatePassword(password); msg != "" {
		s.adminError(w, r, msg)
		return
	}
	if err := s.setUserPassword(r.Context(), id, password); err != nil {
		s.adminError(w, r, "Could not set the password.")
		return
	}
	_ = s.revokeUserSessions(r.Context(), id, "") // a reset invalidates existing sessions
	http.Redirect(w, r, adminBasePath+"/users/"+id+"?flash=Password+reset+and+sessions+revoked", http.StatusSeeOther)
}

func (s *Server) adminUserLogoutAll(w http.ResponseWriter, r *http.Request) {
	if !s.adminCSRFValid(r) {
		s.adminForbidden(w, r)
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.revokeUserSessions(r.Context(), id, ""); err != nil {
		s.adminError(w, r, "Could not revoke sessions.")
		return
	}
	http.Redirect(w, r, adminBasePath+"/users/"+id+"?flash=Signed+out+everywhere", http.StatusSeeOther)
}

func (s *Server) adminUserDelete(w http.ResponseWriter, r *http.Request) {
	if !s.adminCSRFValid(r) {
		s.adminForbidden(w, r)
		return
	}
	id := chi.URLParam(r, "id")
	target, err := s.db.FindOne(r.Context(), schema.UsersCollection, id)
	if err != nil {
		s.adminError(w, r, "User not found.")
		return
	}
	if s.hasAdminRole(rolesOf(target)) && !userDisabled(target) {
		if orphan, oerr := s.wouldOrphanAdmin(r.Context(), true); oerr == nil && orphan {
			s.adminError(w, r, "This is the last active admin — you can't delete it.")
			return
		}
	}
	if err := s.db.Delete(r.Context(), schema.UsersCollection, id); err != nil {
		s.adminError(w, r, "Could not delete the user.")
		return
	}
	http.Redirect(w, r, adminBasePath+"/users?flash=User+deleted", http.StatusSeeOther)
}

// adminRoleChoices returns the declared roles with the given set pre-checked.
func (s *Server) adminRoleChoices(selected []string) []adminRoleChoice {
	sel := make(map[string]bool, len(selected))
	for _, r := range selected {
		sel[r] = true
	}
	var out []adminRoleChoice
	for _, rd := range s.schema.Auth.Roles {
		label := rd.Label
		if label == "" {
			label = rd.Name
		}
		out = append(out, adminRoleChoice{Name: rd.Name, Label: label, Checked: sel[rd.Name]})
	}
	return out
}

func (s *Server) adminUserFormError(emailOK bool, password string, roles []string) string {
	if !emailOK {
		return "A valid email is required."
	}
	if msg := s.validatePassword(password); msg != "" {
		return msg
	}
	return s.validateRoles(roles)
}

func (s *Server) adminUserFormRerender(w http.ResponseWriter, r *http.Request, data adminUserFormData, errMsg string) {
	s.renderAdmin(w, r, "user_form", &adminPage{Title: data.Title, Error: errMsg, Data: data})
}

// adminUserUnlock clears a #50 credential lockout for one account, so an admin can
// rescue a locked-out editor (who has no other recourse — a locked account also
// can't reset its password). Keyed by user id, matching the login failure path.
func (s *Server) adminUserUnlock(w http.ResponseWriter, r *http.Request) {
	if !s.adminCSRFValid(r) {
		s.adminForbidden(w, r)
		return
	}
	id := chi.URLParam(r, "id")
	if s.credFailures != nil {
		s.credFailures.reset(id)
	}
	http.Redirect(w, r, adminBasePath+"/users/"+id+"?flash=Sign-in+unlocked", http.StatusSeeOther)
}

// adminUserLock reports whether an account is currently locked out by the #50
// credential-failure budget, and a short human duration remaining.
func (s *Server) adminUserLock(id string) (bool, string) {
	if s.credFailures == nil {
		return false, ""
	}
	locked, d := s.credFailures.locked(id)
	if !locked {
		return false, ""
	}
	return true, d.Round(time.Minute).String()
}
