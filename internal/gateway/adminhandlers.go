package gateway

import (
	"context"
	"errors"
	"fmt"
	"html/template"
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
	// Refuse at login when an admin.roles allowlist is set and this account isn't on
	// it — a self-registered writer never gets a panel session (they keep their app
	// session elsewhere; this only declines the panel login).
	if !s.panelRolesAllowed(rolesOf(user)) {
		s.credFailures.reset(userID)
		s.renderAdmin(w, r, "login", &adminPage{Title: "Sign in", Error: "This account isn't permitted to use the admin panel."})
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
	Lifecycle  bool // collection has a publishing/soft-delete state → show a Status column
	SoftDelete bool // primary row action is Trash (reversible), not hard Delete
}

type adminRow struct {
	ID     string
	Cells  []string
	Status adminBadge
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
	hasLifecycle := cd.Publishing || cd.SoftDelete
	rows := make([]adminRow, 0, len(page.Data))
	for _, rec := range page.Data {
		cd.CoerceResponse(rec)
		id, _ := rec["id"].(string)
		cells := make([]string, len(cols))
		for i, col := range cols {
			cells[i] = adminDisplay(rec[col])
		}
		row := adminRow{ID: id, Cells: cells}
		if hasLifecycle {
			row.Status = adminRowStatus(cd, rec)
		}
		rows = append(rows, row)
	}
	note := ""
	if page.Total > adminListLimit {
		note = fmt.Sprintf("Showing the newest %d of %d.", adminListLimit, page.Total)
	}
	s.renderAdmin(w, r, "list", &adminPage{
		Title: cd.Name, Flash: r.URL.Query().Get("flash"),
		Data: adminListData{
			Collection: cd.Name, NewHref: adminBasePath + "/c/" + cd.Name + "/new",
			Columns: cols, Rows: rows, Note: note,
			Lifecycle: hasLifecycle, SoftDelete: cd.SoftDelete,
		},
	})
}

// ── forms ─────────────────────────────────────────────────────────────────────

type adminField struct {
	Name, Label, Widget, InputType, Value, Help string
	Options                                     []adminOption
	Required                                    bool
	Preview                                     template.HTML // current media, for the file widget
}

type adminFormData struct {
	Collection, Title, Action, RecordID string
	Version                             string // current _version, for the If-Match hidden field
	Fields                              []adminField
	ObjectLists                         []adminObjectListData
	ReadOnly                            []adminReadField
	Meta                                []adminKV
	Lifecycle                           *adminLifecycle
	Revised                             bool
	Multipart                           bool // form has a file widget → multipart enctype
}

// adminHasFileWidget reports whether a collection renders an inline file widget, so
// the form is submitted as multipart/form-data.
func (s *Server) adminHasFileWidget(cd schema.CollectionDef) bool {
	for _, f := range cd.Fields {
		if s.adminMediaEditable(f) {
			return true
		}
	}
	return false
}

type adminKV struct{ K, V string }

func (s *Server) adminNewForm(w http.ResponseWriter, r *http.Request) {
	cd, ok := s.adminCollection(w, r)
	if !ok {
		return
	}
	s.renderAdmin(w, r, "form", &adminPage{Title: "New " + cd.Name, Data: adminFormData{
		Collection: cd.Name, Title: "New " + cd.Name, Action: adminBasePath + "/c/" + cd.Name,
		Fields: s.adminFields(r, cd, nil, nil, true), ObjectLists: s.adminObjectLists(r, cd, nil, nil),
		Multipart: s.adminHasFileWidget(cd),
	}})
}

func (s *Server) adminEditForm(w http.ResponseWriter, r *http.Request) {
	cd, ok := s.adminCollection(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	s.renderAdminEdit(w, r, cd, id, r.URL.Query().Get("flash"), "")
}

// renderAdminEdit renders the edit page for a record, loading it fresh. `flash` and
// `errMsg` are optional banners (errMsg is used for the optimistic-concurrency
// conflict re-render, which must show the *current* stored values).
func (s *Server) renderAdminEdit(w http.ResponseWriter, r *http.Request, cd schema.CollectionDef, id, flash, errMsg string) {
	rec, err := s.db.FindOne(r.Context(), cd.Name, id)
	if err != nil {
		s.adminError(w, r, "Record not found.")
		return
	}
	cd.CoerceResponse(rec)
	data := adminFormData{
		Collection: cd.Name, Title: "Edit " + cd.Name, Action: adminBasePath + "/c/" + cd.Name + "/" + id,
		RecordID: id, Fields: s.adminFields(r, cd, rec, nil, false), ObjectLists: s.adminObjectLists(r, cd, rec, nil),
		ReadOnly: s.adminReadOnlyFields(r, cd, rec),
		Meta:     adminMeta(rec), Lifecycle: s.adminLifecycleFor(r, cd, rec), Revised: s.revised(cd.Name),
		Multipart: s.adminHasFileWidget(cd),
	}
	if s.versioned(cd.Name) {
		data.Version = strconv.FormatInt(recordVersion(rec), 10)
	}
	s.renderAdmin(w, r, "form", &adminPage{Title: "Edit " + cd.Name, Flash: flash, Error: errMsg, Data: data})
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
	if msg := s.adminApplyFiles(r, cd, data); msg != "" {
		s.adminReRenderForm(w, r, cd, "", data, msg)
		return
	}
	if errMsg, _ := s.adminWrite(r, cd, "", data, true, nil); errMsg != "" {
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
	if msg := s.adminApplyFiles(r, cd, data); msg != "" {
		s.renderAdminEdit(w, r, cd, id, "", msg)
		return
	}
	expect := s.adminExpectedVersion(cd, r)
	errMsg, conflict := s.adminWrite(r, cd, id, data, false, expect)
	if conflict {
		// Someone saved a newer version between load and submit — show the current
		// record so the admin can reapply, never silently overwrite (#26).
		s.renderAdminEdit(w, r, cd, id, "", "Someone else changed this record while you were editing. Here's the latest — please reapply your change.")
		return
	}
	if errMsg != "" {
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
// revisions + events). On a versioned collection it honors the optimistic-
// concurrency precondition (expect, from the form's _version) so a stale save is
// refused rather than clobbering a newer edit (#26). Returns a human message on
// failure ("" on success) and whether the failure was a version conflict.
func (s *Server) adminWrite(r *http.Request, cd schema.CollectionDef, id string, data store.Record, isCreate bool, expect *int64) (string, bool) {
	if err := s.stripUnwritableFields(r.Context(), cd.Name, id, data, isCreate); err != nil {
		var fe *fieldForbiddenError
		if errors.As(err, &fe) {
			return "You may not set '" + fe.field + "' to that value.", false
		}
		return "Write was rejected.", false
	}
	var errs schema.FieldErrors
	if isCreate {
		errs = cd.ValidateCreate(data)
	} else {
		errs = cd.ValidateUpdate(data)
	}
	if errs != nil {
		return "Please fix: " + adminErrsSummary(errs), false
	}
	cd.EncodeDecimals(data)
	if err := s.checkReferences(r.Context(), s.db, cd.Name, data); err != nil {
		return "Invalid reference: " + err.Error(), false
	}
	op := "update"
	if isCreate {
		op = "create"
	}
	_, err := s.writeWithLinks(r.Context(), cd.Name, op, data, func(ctx context.Context, db store.DB, base store.Record) (store.Record, error) {
		if isCreate {
			return db.Create(ctx, store.WriteInput{Collection: cd.Name, Data: base})
		}
		if e := s.applyVersion(ctx, db, cd.Name, base, expect); e != nil {
			return nil, e
		}
		return db.Update(ctx, store.WriteInput{Collection: cd.Name, Data: base})
	})
	if errors.Is(err, errVersionConflict) {
		return "This record was changed by someone else.", true
	}
	if err != nil {
		return "Could not save the record.", false
	}
	return "", false
}

// adminExpectedVersion reads the If-Match precondition the edit form carried back in
// its hidden _version field. Nil for a non-versioned collection or an absent value.
func (s *Server) adminExpectedVersion(cd schema.CollectionDef, r *http.Request) *int64 {
	if !s.versioned(cd.Name) {
		return nil
	}
	raw := strings.TrimSpace(r.FormValue(schema.ConcurrencyVersion))
	if raw == "" {
		return nil
	}
	if v, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return &v
	}
	return nil
}

func (s *Server) adminReRenderForm(w http.ResponseWriter, r *http.Request, cd schema.CollectionDef, id string, submitted store.Record, errMsg string) {
	title, action := "New "+cd.Name, adminBasePath+"/c/"+cd.Name
	isCreate := id == ""
	var stored store.Record
	if !isCreate {
		title, action = "Edit "+cd.Name, adminBasePath+"/c/"+cd.Name+"/"+id
		// Reload the stored row so #27 transition options are computed from the real
		// current value, not the rejected submission.
		if rec, err := s.db.FindOne(r.Context(), cd.Name, id); err == nil {
			cd.CoerceResponse(rec)
			stored = rec
		}
	}
	data := adminFormData{
		Collection: cd.Name, Title: title, Action: action, RecordID: id,
		Fields:      s.adminFields(r, cd, stored, submitted, isCreate),
		ObjectLists: s.adminObjectLists(r, cd, stored, submitted),
		Multipart:   s.adminHasFileWidget(cd),
	}
	if !isCreate && s.versioned(cd.Name) {
		// Preserve the version the form was loaded at, so the resubmit still carries a
		// precondition rather than silently dropping it.
		data.Version = strings.TrimSpace(r.FormValue(schema.ConcurrencyVersion))
		data.ReadOnly = s.adminReadOnlyFields(r, cd, stored)
	}
	s.renderAdmin(w, r, "form", &adminPage{Title: title, Error: errMsg, Data: data})
}

// ── field / value helpers ─────────────────────────────────────────────────────

// adminEditable reports whether a field is edited via a supported form widget.
// Relations (belongs-to selects and m2m checklists) are handled in 2B; file,
// richtext, object_list and json editors are deferred to 2C.
func adminEditable(f schema.FieldDef) bool {
	switch f.Type {
	case schema.TypeString, schema.TypeText, schema.TypeNumber, schema.TypeInteger,
		schema.TypeDecimal, schema.TypeBoolean, schema.TypeDate, schema.TypeDateTime,
		schema.TypeEnum, schema.TypeRelation:
		return true
	default:
		return false // file, richtext, object_list, json → phase 2C
	}
}

// adminFields builds the form widgets. `rec` is the stored record (nil on create),
// used for current values, m2m selection, and #27 transition gating; `submitted`
// (a re-render after an error) supplies the values the user just typed so nothing
// is lost. Enum transition options are always computed from the stored value.
func (s *Server) adminFields(r *http.Request, cd schema.CollectionDef, rec, submitted store.Record, isCreate bool) []adminField {
	p := principalFromContext(r.Context())
	recID, _ := rec["id"].(string)
	shownVal := func(name string) string {
		if submitted != nil {
			return adminRawString(submitted[name])
		}
		return adminDisplay(rec[name])
	}
	var out []adminField
	for _, f := range cd.Fields {
		if !adminEditable(f) && !s.adminMediaEditable(f) {
			continue
		}
		af := adminField{Name: f.Name, Label: adminLabel(f), Value: shownVal(f.Name), Required: f.Required, Widget: "input", InputType: "text"}
		if s.adminMediaEditable(f) {
			// File widget: current media preview + upload + library picker + clear.
			cur := shownVal(f.Name)
			af.Widget = "file"
			if cur != "" {
				af.Preview = s.renderMediaField(r, cur)
			}
			af.Options = s.adminMediaLibrary(r, cur)
			out = append(out, af)
			continue
		}
		switch f.Type {
		case schema.TypeText:
			af.Widget = "textarea"
		case schema.TypeBoolean:
			af.Widget = "checkbox"
		case schema.TypeEnum:
			af.Widget = "select"
			af.Options = markSelected(s.adminEnumOptions(f, p, rec, isCreate), shownVal(f.Name))
		case schema.TypeNumber, schema.TypeInteger:
			af.InputType = "number"
		case schema.TypeDate:
			af.InputType = "date"
		case schema.TypeRelation:
			if f.Many {
				// m2m checklist: current selection from the resubmitted form, else the join table.
				sel := adminSelectedFromSubmitted(submitted, f.Name)
				if submitted == nil {
					sel = s.adminM2MSelected(r, cd.Name, f.Name, recID)
				}
				af.Widget, af.Value = "multiselect", ""
				af.Options = s.adminRelationOptions(r, f.Target, sel)
				af.Help = "Pick any that apply"
			} else {
				// belongs-to: a select of human labels, not a raw id.
				sel := map[string]bool{}
				if v := strings.TrimSpace(shownVal(f.Name)); v != "" {
					sel[v] = true
				}
				af.Widget, af.Value = "select", ""
				af.Options = s.adminRelationOptions(r, f.Target, sel)
			}
		}
		out = append(out, af)
	}
	return out
}

// markSelected re-points an option list's selection at `val` (the value the user
// just submitted), so an error re-render preselects their choice, not the stored one.
func markSelected(opts []adminOption, val string) []adminOption {
	if val == "" {
		return opts
	}
	for i := range opts {
		opts[i].Selected = opts[i].Value == val
	}
	return opts
}

// adminSelectedFromSubmitted reads a resubmitted m2m field ([]any of ids) back into
// a selected-set, so a validation error doesn't lose the user's picks.
func adminSelectedFromSubmitted(submitted store.Record, name string) map[string]bool {
	out := map[string]bool{}
	if submitted == nil {
		return out
	}
	if arr, ok := submitted[name].([]any); ok {
		for _, e := range arr {
			if id, ok := e.(string); ok && id != "" {
				out[id] = true
			}
		}
	}
	return out
}

// adminParseForm reads the submitted editable fields into a typed record. File
// fields are left for adminApplyFiles (which can surface upload errors); a multipart
// submission is parsed here so both its text fields and its file parts are available.
func (s *Server) adminParseForm(cd schema.CollectionDef, r *http.Request, isCreate bool) store.Record {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		_ = r.ParseMultipartForm(16 << 20) // in-memory threshold; larger parts spill to temp files
	} else {
		_ = r.ParseForm() // populate r.Form so multi-valued m2m checklists are readable
	}
	data := store.Record{}
	for _, f := range cd.Fields {
		if !adminEditable(f) {
			continue
		}
		// A many-to-many field arrives as repeated values; always include it (even
		// empty) so unchecking every box clears the links.
		if f.Type == schema.TypeRelation && f.Many {
			ids := make([]any, 0, len(r.Form[f.Name]))
			for _, v := range r.Form[f.Name] {
				if v = strings.TrimSpace(v); v != "" {
					ids = append(ids, v)
				}
			}
			data[f.Name] = ids
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
	s.adminParseObjectLists(cd, r, data)
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
	return s.adminCanRule(r, collection, id, s.collections[collection].AccessRule(action))
}

// adminCanRule authorizes a record write against an explicit rule (an update/delete
// default, or the collection's `publish` rule for a lifecycle transition), without
// writing any HTTP response — the admin renders HTML on its own paths.
func (s *Server) adminCanRule(r *http.Request, collection, id string, rule schema.Rule) bool {
	if !s.authEnabled() {
		return true
	}
	p := principalFromContext(r.Context())
	switch d, field := evalRule(rule, p); d {
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
