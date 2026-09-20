package gateway

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Admin panel — system views (ADR-0035, phase 2A): read-only windows over the
// engine-managed collections the public API hides (the change feed, users,
// sessions, webhook deliveries, scheduled publishes, notifications) plus a
// read-only data-model view. Admin-role-gated, since these expose every user's
// data. A couple of safe recovery actions (revoke a session, retry a dead-lettered
// webhook) are the only writes.

// adminSystemView describes one read-only browser over a system collection. Columns
// is a deliberate allowlist — secrets (password/token hashes, reset links, OTP
// codes) are never listed.
type adminSystemView struct {
	Key, Title, Coll, Sort string
	Columns                []string
	Action                 string // "", "revoke" or "retry" — an optional per-row action
}

func adminSystemViews() []adminSystemView {
	return []adminSystemView{
		{Key: "events", Title: "Change events", Coll: schema.EventsCollection, Sort: "-created_at",
			Columns: []string{schema.EventCollection, schema.EventRecordID, schema.EventType, schema.EventFromStatus, schema.EventToStatus, "created_at", "created_by"}},
		{Key: "sessions", Title: "Sessions", Coll: schema.SessionsCollection, Sort: "-created_at",
			Columns: []string{schema.SessionUserID, schema.SessionExpiresAt, "created_at"}, Action: "revoke"},
		{Key: "webhooks", Title: "Webhook deliveries", Coll: schema.WebhookDeliveriesCollection, Sort: "-created_at",
			Columns: []string{schema.WebhookDeliveryEndpoint, schema.WebhookDeliveryStatus, schema.WebhookDeliveryAttempts, schema.WebhookDeliveryNextAt, schema.WebhookDeliveryLastError, schema.WebhookDeliveryDelivered}, Action: "retry"},
		{Key: "scheduled", Title: "Scheduled publishes", Coll: schema.ScheduledPublishesCollection, Sort: "due_at",
			Columns: []string{schema.ScheduledCollection, schema.ScheduledRecordID, schema.ScheduledDueAt}},
		{Key: "notifications", Title: "Notifications", Coll: schema.NotificationsCollection, Sort: "-created_at",
			Columns: []string{schema.NotificationTo, schema.NotificationKind, schema.NotificationStatus, schema.NotificationAttempts, schema.NotificationNextAt, schema.NotificationLastError}},
	}
}

func adminSystemViewByKey(key string) (adminSystemView, bool) {
	for _, v := range adminSystemViews() {
		if v.Key == key {
			return v, true
		}
	}
	return adminSystemView{}, false
}

// adminSystemNav returns the system views whose backing collection actually exists
// (an engine collection is only present when its feature is in the schema).
func (s *Server) adminSystemNav() []adminNavItem {
	var items []adminNavItem
	for _, v := range adminSystemViews() {
		if _, ok := s.collections[v.Coll]; ok {
			items = append(items, adminNavItem{Name: v.Key, Label: v.Title})
		}
	}
	return items
}

// adminRequireAdminRole gates the System section on the admin role — these views
// expose data across all users, unlike the per-principal collection browser.
func (s *Server) adminRequireAdminRole(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authEnabled() && !s.isAdmin(principalFromContext(r.Context())) {
			s.adminForbidden(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type adminSystemData struct {
	Key, Title, Action string
	Columns            []string
	Rows               []adminRow
	Note               string
}

func (s *Server) adminSystemList(w http.ResponseWriter, r *http.Request) {
	view, ok := adminSystemViewByKey(chi.URLParam(r, "view"))
	if !ok {
		s.adminError(w, r, "Unknown system view.")
		return
	}
	data := adminSystemData{Key: view.Key, Title: view.Title, Action: view.Action, Columns: view.Columns}
	cd, exists := s.collections[view.Coll]
	if !exists {
		data.Note = "This subsystem isn't enabled for this instance."
		s.renderAdmin(w, r, "system", &adminPage{Title: view.Title, Flash: r.URL.Query().Get("flash"), Data: data})
		return
	}
	page, err := s.db.Find(r.Context(), store.Query{Collection: view.Coll, Limit: adminListLimit, Sort: view.Sort})
	if err != nil {
		s.adminError(w, r, "Could not load "+view.Title+".")
		return
	}
	for _, rec := range page.Data {
		cd.CoerceResponse(rec)
		id, _ := rec["id"].(string)
		cells := make([]string, len(view.Columns))
		for i, col := range view.Columns {
			cells[i] = adminDisplay(rec[col])
		}
		data.Rows = append(data.Rows, adminRow{ID: id, Cells: cells})
	}
	if page.Total > adminListLimit {
		data.Note = "Showing the newest " + strconv.Itoa(adminListLimit) + " of " + strconv.Itoa(int(page.Total)) + "."
	}
	s.renderAdmin(w, r, "system", &adminPage{Title: view.Title, Flash: r.URL.Query().Get("flash"), Data: data})
}

// adminRevokeSession deletes one session row (a safe recovery action).
func (s *Server) adminRevokeSession(w http.ResponseWriter, r *http.Request) {
	if !s.adminCSRFValid(r) {
		s.adminForbidden(w, r)
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.db.Delete(r.Context(), schema.SessionsCollection, id); err != nil {
		s.adminError(w, r, "Could not revoke the session.")
		return
	}
	http.Redirect(w, r, adminBasePath+"/system/sessions?flash=Session+revoked", http.StatusSeeOther)
}

// adminRetryWebhook re-arms a failed or dead delivery — status back to pending, due
// now — unless it already succeeded (mirrors the API retry endpoint).
func (s *Server) adminRetryWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.adminCSRFValid(r) {
		s.adminForbidden(w, r)
		return
	}
	id := chi.URLParam(r, "id")
	d, err := s.db.FindOne(r.Context(), schema.WebhookDeliveriesCollection, id)
	if err != nil || d == nil {
		s.adminError(w, r, "Delivery not found.")
		return
	}
	if status, _ := d[schema.WebhookDeliveryStatus].(string); status == schema.WebhookDelivered {
		http.Redirect(w, r, adminBasePath+"/system/webhooks?flash=Already+delivered", http.StatusSeeOther)
		return
	}
	if _, err := s.db.Update(r.Context(), store.WriteInput{Collection: schema.WebhookDeliveriesCollection, Data: store.Record{
		"id":                         id,
		schema.WebhookDeliveryStatus: schema.WebhookPending,
		schema.WebhookDeliveryNextAt: nowUTC().Format(time.RFC3339),
	}}); err != nil {
		s.adminError(w, r, "Could not retry the delivery.")
		return
	}
	http.Redirect(w, r, adminBasePath+"/system/webhooks?flash=Retry+queued", http.StatusSeeOther)
}

// ── data-model visualization ──────────────────────────────────────────────────

type adminSchemaData struct {
	Collections []adminSchemaColl
}

type adminSchemaColl struct {
	Name   string
	Flags  string
	Fields []adminSchemaField
}

type adminSchemaField struct {
	Name, Type, Attrs string
}

func (s *Server) adminSchema(w http.ResponseWriter, r *http.Request) {
	var colls []adminSchemaColl
	for _, c := range s.schema.Collections {
		if !s.routableCollection(c.Name) {
			continue
		}
		sc := adminSchemaColl{Name: c.Name, Flags: adminCollFlags(c)}
		for _, f := range c.Fields {
			sc.Fields = append(sc.Fields, adminSchemaField{Name: f.Name, Type: adminFieldType(f), Attrs: adminFieldAttrs(f)})
		}
		colls = append(colls, sc)
	}
	s.renderAdmin(w, r, "schema", &adminPage{Title: "Data model", Data: adminSchemaData{Collections: colls}})
}

func adminCollFlags(c schema.CollectionDef) string {
	var f []string
	if c.Publishing {
		f = append(f, "publishing")
	}
	if c.SoftDelete {
		f = append(f, "soft-delete")
	}
	if c.Revisions {
		f = append(f, "revisions")
	}
	if c.Events {
		f = append(f, "events")
	}
	if c.Concurrency {
		f = append(f, "concurrency")
	}
	return joinComma(f)
}

func adminFieldType(f schema.FieldDef) string {
	if f.Type == schema.TypeRelation {
		if f.Many {
			return "relation[] → " + f.Target
		}
		return "relation → " + f.Target
	}
	return string(f.Type)
}

func adminFieldAttrs(f schema.FieldDef) string {
	var a []string
	if f.Required {
		a = append(a, "required")
	}
	if f.Unique {
		a = append(a, "unique")
	}
	if len(f.Values) > 0 {
		a = append(a, "["+joinComma(f.Values)+"]")
	}
	if f.Access != nil {
		a = append(a, "access")
	}
	return joinComma(a)
}

func joinComma(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}
