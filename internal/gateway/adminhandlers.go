package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Admin panel request handlers (ADR-0035, phase 1): server-rendered login, a
// system overview, and per-collection list + create/edit/delete. Every write reuses
// the same authorized pipeline as the API — the admin is a privileged client, not a
// backdoor.

const adminListLimit = 100

// ── auth ──────────────────────────────────────────────────────────────────────

func (s *Server) adminLoginForm(w http.ResponseWriter, r *http.Request) {
	if principalFromContext(r.Context()).Authenticated {
		http.Redirect(w, r, adminBasePath+"/", http.StatusSeeOther)
		return
	}
	s.renderAdmin(w, r, "login", &adminPage{Title: "Sign in"})
}

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if !s.adminCSRFValid(r) {
		s.renderAdmin(w, r, "login", &adminPage{Title: "Sign in", Error: "Your session expired — please try again."})
		return
	}
	email, password := r.FormValue("email"), r.FormValue("password")
	user, err := s.findUserByEmail(r.Context(), email)
	if err != nil {
		s.renderAdmin(w, r, "login", &adminPage{Title: "Sign in", Error: "Something went wrong. Please try again."})
		return
	}
	userID, _ := user["id"].(string)
	if userID != "" {
		if locked, _ := s.credFailures.locked(userID); locked {
			s.renderAdmin(w, r, "login", &adminPage{Title: "Sign in", Error: "Invalid email or password."})
			return
		}
	}
	hash, _ := user[schema.UserPasswordHash].(string)
	if user == nil || hash == "" || !checkPassword(hash, password) {
		if userID != "" {
			s.credFailures.fail(userID)
		}
		s.renderAdmin(w, r, "login", &adminPage{Title: "Sign in", Error: "Invalid email or password."})
		return
	}
	if userDisabled(user) {
		s.renderAdmin(w, r, "login", &adminPage{Title: "Sign in", Error: "Invalid email or password."})
		return
	}
	s.credFailures.reset(userID)
	token, expiresAt, err := s.issueSession(r.Context(), userID, rolesOf(user))
	if err != nil {
		s.renderAdmin(w, r, "login", &adminPage{Title: "Sign in", Error: "Could not start a session. Please try again."})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.requestIsSecure(r), Expires: expiresAt,
	})
	http.Redirect(w, r, adminBasePath+"/", http.StatusSeeOther)
}

func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	if s.adminCSRFValid(r) {
		if tok := sessionTokenFromRequest(r); tok != "" {
			if sess, err := s.findSessionByHash(r.Context(), hashToken(tok)); err == nil && sess != nil {
				if id, ok := sess["id"].(string); ok {
					_ = s.db.Delete(r.Context(), schema.SessionsCollection, id)
				}
			}
		}
		clearSessionCookie(w)
	}
	http.Redirect(w, r, adminBasePath+"/login", http.StatusSeeOther)
}

// ── overview ──────────────────────────────────────────────────────────────────

type adminStat struct {
	Name  string
	Count int64
	Href  string
}

func (s *Server) adminOverview(w http.ResponseWriter, r *http.Request) {
	var stats []adminStat
	for _, c := range s.schema.Collections {
		if !s.routableCollection(c.Name) {
			continue
		}
		if _, ok := s.adminReadFilters(r, c.Name); !ok {
			continue // caller can't read this collection at all
		}
		page, err := s.db.Find(r.Context(), store.Query{Collection: c.Name, Limit: 1})
		count := int64(-1)
		if err == nil {
			count = page.Total
		}
		stats = append(stats, adminStat{Name: c.Name, Count: count, Href: adminBasePath + "/c/" + c.Name})
	}
	s.renderAdmin(w, r, "overview", &adminPage{Title: "Overview", Flash: r.URL.Query().Get("flash"), Data: stats})
}

// ── list ──────────────────────────────────────────────────────────────────────

type adminListData struct {
	Collection string
	NewHref    string
	Columns    []string
	Rows       []adminRow
	Note       string
}

type adminRow struct {
	ID    string
	Cells []string
}

func (s *Server) adminList(w http.ResponseWriter, r *http.Request) {
	cd, ok := s.adminCollection(w, r)
	if !ok {
		return
	}
	filters, ok := s.adminReadFilters(r, cd.Name)
	if !ok {
		s.adminForbidden(w, r)
		return
	}
	cols := adminColumns(cd)
	page, err := s.db.Find(r.Context(), store.Query{
		Collection: cd.Name, Filters: filters, Limit: adminListLimit, Sort: "-created_at",
	})
	if err != nil {
		s.adminError(w, r, "Could not load records.")
		return
	}
	rows := make([]adminRow, 0, len(page.Data))
	for _, rec := range page.Data {
		cd.CoerceResponse(rec)
		id, _ := rec["id"].(string)
		cells := make([]string, len(cols))
		for i, col := range cols {
			cells[i] = adminDisplay(rec[col])
		}
		rows = append(rows, adminRow{ID: id, Cells: cells})
	}
	note := ""
	if page.Total > adminListLimit {
		note = fmt.Sprintf("Showing the newest %d of %d.", adminListLimit, page.Total)
	}
	s.renderAdmin(w, r, "list", &adminPage{
		Title: cd.Name, Flash: r.URL.Query().Get("flash"),
		Data: adminListData{Collection: cd.Name, NewHref: adminBasePath + "/c/" + cd.Name + "/new", Columns: cols, Rows: rows, Note: note},
	})
}

// ── forms ─────────────────────────────────────────────────────────────────────

type adminField struct {
	Name, Label, Widget, InputType, Value string
	Options                               []string
	Required                              bool
}

type adminFormData struct {
	Collection, Title, Action string
	Fields                    []adminField
	Meta                      []adminKV
}

type adminKV struct{ K, V string }

func (s *Server) adminNewForm(w http.ResponseWriter, r *http.Request) {
	cd, ok := s.adminCollection(w, r)
	if !ok {
		return
	}
	s.renderAdmin(w, r, "form", &adminPage{Title: "New " + cd.Name, Data: adminFormData{
		Collection: cd.Name, Title: "New " + cd.Name, Action: adminBasePath + "/c/" + cd.Name,
		Fields: s.adminFields(cd, nil, nil),
	}})
}

func (s *Server) adminEditForm(w http.ResponseWriter, r *http.Request) {
	cd, ok := s.adminCollection(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	rec, err := s.db.FindOne(r.Context(), cd.Name, id)
	if err != nil {
		s.adminError(w, r, "Record not found.")
		return
	}
	cd.CoerceResponse(rec)
	s.renderAdmin(w, r, "form", &adminPage{Title: "Edit " + cd.Name, Data: adminFormData{
		Collection: cd.Name, Title: "Edit " + cd.Name, Action: adminBasePath + "/c/" + cd.Name + "/" + id,
		Fields: s.adminFields(cd, rec, nil), Meta: adminMeta(rec),
	}})
}

func (s *Server) adminCreate(w http.ResponseWriter, r *http.Request) {
	cd, ok := s.adminCollection(w, r)
	if !ok {
		return
	}
	if !s.adminCSRFValid(r) {
		s.adminForbidden(w, r)
		return
	}
	if !s.adminCan(r, cd.Name, schema.ActionCreate) {
		s.adminForbidden(w, r)
		return
	}
	data := s.adminParseForm(cd, r, true)
	if errMsg := s.adminWrite(r, cd, "", data, true); errMsg != "" {
		s.adminReRenderForm(w, r, cd, "", data, errMsg)
		return
	}
	http.Redirect(w, r, adminBasePath+"/c/"+cd.Name+"?flash=Created", http.StatusSeeOther)
}

func (s *Server) adminUpdate(w http.ResponseWriter, r *http.Request) {
	cd, ok := s.adminCollection(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !s.adminCSRFValid(r) {
		s.adminForbidden(w, r)
		return
	}
	if !s.adminCanRecord(r, cd.Name, id, schema.ActionUpdate) {
		s.adminForbidden(w, r)
		return
	}
	data := s.adminParseForm(cd, r, false)
	data["id"] = id
	if errMsg := s.adminWrite(r, cd, id, data, false); errMsg != "" {
		s.adminReRenderForm(w, r, cd, id, data, errMsg)
		return
	}
	http.Redirect(w, r, adminBasePath+"/c/"+cd.Name+"?flash=Saved", http.StatusSeeOther)
}

func (s *Server) adminDelete(w http.ResponseWriter, r *http.Request) {
	cd, ok := s.adminCollection(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !s.adminCSRFValid(r) {
		s.adminForbidden(w, r)
		return
	}
	if !s.adminCanRecord(r, cd.Name, id, schema.ActionDelete) {
		s.adminForbidden(w, r)
		return
	}
	if err := s.deleteRecord(r.Context(), cd.Name, id, nil); err != nil {
		s.adminError(w, r, "Could not delete: "+err.Error())
		return
	}
	http.Redirect(w, r, adminBasePath+"/c/"+cd.Name+"?flash=Deleted", http.StatusSeeOther)
}

// adminWrite runs the shared authorized write pipeline (field rules + #27
// transitions, validation, decimals, reference checks, then the write with hooks +
// revisions + events). It returns a human message on failure, "" on success.
func (s *Server) adminWrite(r *http.Request, cd schema.CollectionDef, id string, data store.Record, isCreate bool) string {
	if err := s.stripUnwritableFields(r.Context(), cd.Name, id, data, isCreate); err != nil {
		var fe *fieldForbiddenError
		if errors.As(err, &fe) {
			return "You may not set '" + fe.field + "' to that value."
		}
		return "Write was rejected."
	}
	var errs schema.FieldErrors
	if isCreate {
		errs = cd.ValidateCreate(data)
	} else {
		errs = cd.ValidateUpdate(data)
	}
	if errs != nil {
		return "Please fix: " + adminErrsSummary(errs)
	}
	cd.EncodeDecimals(data)
	if err := s.checkReferences(r.Context(), s.db, cd.Name, data); err != nil {
		return "Invalid reference: " + err.Error()
	}
	op := "update"
	if isCreate {
		op = "create"
	}
	_, err := s.writeWithLinks(r.Context(), cd.Name, op, data, func(ctx context.Context, db store.DB, base store.Record) (store.Record, error) {
		if isCreate {
			return db.Create(ctx, store.WriteInput{Collection: cd.Name, Data: base})
		}
		return db.Update(ctx, store.WriteInput{Collection: cd.Name, Data: base})
	})
	if err != nil {
		return "Could not save the record."
	}
	return ""
}

func (s *Server) adminReRenderForm(w http.ResponseWriter, r *http.Request, cd schema.CollectionDef, id string, submitted store.Record, errMsg string) {
	title, action := "New "+cd.Name, adminBasePath+"/c/"+cd.Name
	if id != "" {
		title, action = "Edit "+cd.Name, adminBasePath+"/c/"+cd.Name+"/"+id
	}
	s.renderAdmin(w, r, "form", &adminPage{Title: title, Error: errMsg, Data: adminFormData{
		Collection: cd.Name, Title: title, Action: action, Fields: s.adminFields(cd, nil, submitted),
	}})
}

// ── field / value helpers ─────────────────────────────────────────────────────

// adminEditable reports whether a field is edited via a phase-1 form widget.
func adminEditable(f schema.FieldDef) bool {
	switch f.Type {
	case schema.TypeString, schema.TypeText, schema.TypeNumber, schema.TypeInteger,
		schema.TypeDecimal, schema.TypeBoolean, schema.TypeDate, schema.TypeDateTime,
		schema.TypeEnum, schema.TypeRelation:
		return f.Type != schema.TypeRelation || !f.Many // belongs-to only (id text); m2m is phase 2
	default:
		return false // file, richtext, object_list, json → phase 2
	}
}

// adminFields builds the form widgets. `rec` supplies values on edit; `submitted`
// (a re-render after an error) takes precedence so the user doesn't lose input.
func (s *Server) adminFields(cd schema.CollectionDef, rec, submitted store.Record) []adminField {
	var out []adminField
	for _, f := range cd.Fields {
		if !adminEditable(f) {
			continue
		}
		val := ""
		if submitted != nil {
			val = adminRawString(submitted[f.Name])
		} else if rec != nil {
			val = adminDisplay(rec[f.Name])
		}
		af := adminField{Name: f.Name, Label: adminLabel(f), Value: val, Required: f.Required, Widget: "input", InputType: "text"}
		switch f.Type {
		case schema.TypeText:
			af.Widget = "textarea"
		case schema.TypeBoolean:
			af.Widget = "checkbox"
		case schema.TypeEnum:
			af.Widget, af.Options = "select", f.Values
		case schema.TypeNumber, schema.TypeInteger:
			af.InputType = "number"
		case schema.TypeDate:
			af.InputType = "date"
		}
		out = append(out, af)
	}
	return out
}

// adminParseForm reads the submitted editable fields into a typed record.
func (s *Server) adminParseForm(cd schema.CollectionDef, r *http.Request, isCreate bool) store.Record {
	data := store.Record{}
	for _, f := range cd.Fields {
		if !adminEditable(f) {
			continue
		}
		raw := strings.TrimSpace(r.FormValue(f.Name))
		switch f.Type {
		case schema.TypeBoolean:
			data[f.Name] = raw == "true" // an unchecked box submits nothing → false
		case schema.TypeNumber, schema.TypeInteger:
			if raw == "" {
				continue
			}
			if n, err := strconv.ParseFloat(raw, 64); err == nil {
				data[f.Name] = n
			} else {
				data[f.Name] = raw // let validation report the bad number
			}
		default:
			if raw == "" {
				continue // omit empties (defaults / null); a cleared field is left untouched on update
			}
			data[f.Name] = raw
		}
	}
	return data
}

func adminColumns(cd schema.CollectionDef) []string {
	cols := []string{}
	for _, f := range cd.Fields {
		switch f.Type {
		case schema.TypeString, schema.TypeText, schema.TypeNumber, schema.TypeInteger,
			schema.TypeDecimal, schema.TypeBoolean, schema.TypeDate, schema.TypeDateTime, schema.TypeEnum:
			cols = append(cols, f.Name)
		}
		if len(cols) >= 5 {
			break
		}
	}
	return cols
}

func adminMeta(rec store.Record) []adminKV {
	var m []adminKV
	for _, k := range []string{"id", "created_at", "updated_at", "created_by", "updated_by"} {
		if v := adminDisplay(rec[k]); v != "" {
			m = append(m, adminKV{K: k, V: v})
		}
	}
	return m
}

func adminLabel(f schema.FieldDef) string {
	if f.Label != "" {
		return f.Label
	}
	return strings.ToUpper(f.Name[:1]) + strings.ReplaceAll(f.Name[1:], "_", " ")
}

// adminDisplay renders a value for a table cell or a prefilled field, truncated.
func adminDisplay(v any) string {
	s := adminRawString(v)
	if len([]rune(s)) > 80 {
		return string([]rune(s)[:80]) + "…"
	}
	return s
}

func adminRawString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	case time.Time:
		return t.Format(time.RFC3339)
	default:
		return fmt.Sprint(t)
	}
}

func adminErrsSummary(errs schema.FieldErrors) string {
	parts := make([]string, 0, len(errs))
	for k, v := range errs {
		parts = append(parts, k+" "+v)
	}
	return strings.Join(parts, "; ")
}

// ── authorization (no response writing — the admin renders HTML) ───────────────

func (s *Server) adminCan(r *http.Request, collection string, action schema.AccessAction) bool {
	if !s.authEnabled() {
		return true
	}
	p := principalFromContext(r.Context())
	d, _ := evalRule(s.collections[collection].AccessRule(action), p)
	return d == allow || d == ownerScope
}

func (s *Server) adminCanRecord(r *http.Request, collection, id string, action schema.AccessAction) bool {
	if !s.authEnabled() {
		return true
	}
	p := principalFromContext(r.Context())
	switch d, field := evalRule(s.collections[collection].AccessRule(action), p); d {
	case allow:
		return true
	case ownerScope:
		rec, err := s.db.FindOne(r.Context(), collection, id)
		if err != nil {
			return false
		}
		owner, _ := rec[field].(string)
		return owner != "" && owner == p.ID
	default:
		return false
	}
}

func (s *Server) adminReadFilters(r *http.Request, collection string) ([]store.Filter, bool) {
	if !s.authEnabled() {
		return nil, true
	}
	p := principalFromContext(r.Context())
	switch d, field := evalRule(s.collections[collection].AccessRule(schema.ActionRead), p); d {
	case allow:
		return nil, true
	case ownerScope:
		return []store.Filter{{Field: field, Operator: store.Eq, Value: p.ID}}, true
	default:
		return nil, false
	}
}

// ── small response helpers ────────────────────────────────────────────────────

// adminCollection resolves the {collection} param to a routable collection, or
// renders a 404 and returns false.
func (s *Server) adminCollection(w http.ResponseWriter, r *http.Request) (schema.CollectionDef, bool) {
	name := chi.URLParam(r, "collection")
	if !s.routableCollection(name) {
		w.WriteHeader(http.StatusNotFound)
		s.renderAdmin(w, r, "overview", &adminPage{Title: "Not found", Error: "No such collection: " + name, Data: []adminStat{}})
		return schema.CollectionDef{}, false
	}
	return s.collections[name], true
}

func (s *Server) adminForbidden(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusForbidden)
	s.renderAdmin(w, r, "overview", &adminPage{Title: "Forbidden", Error: "You don't have permission to do that.", Data: []adminStat{}})
}

func (s *Server) adminError(w http.ResponseWriter, r *http.Request, msg string) {
	s.renderAdmin(w, r, "overview", &adminPage{Title: "Error", Error: msg, Data: []adminStat{}})
}
