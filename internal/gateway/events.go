package gateway

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// emitsEvents reports whether a collection opts into the change log (ADR-0021).
func (s *Server) emitsEvents(collection string) bool {
	return s.collections[collection].Events
}

// needsWriteTx reports whether a write to this collection must run in a
// transaction to capture its side effects atomically — a revision snapshot
// (ADR-0013) and/or a change-log event (ADR-0021). The fast paths that skip the
// transaction consult this so event-emitting collections are never missed.
func (s *Server) needsWriteTx(collection string) bool {
	return s.revised(collection) || s.emitsEvents(collection)
}

// captureWrite records the durable side effects of a write, using db (which must
// be the write's own transaction so they commit together): a full-snapshot
// revision when the collection is revisioned, and a change-log event when it
// emits events. It is the single place both are captured, so every write path
// gets the same behaviour by calling it in place of captureRevision.
func (s *Server) captureWrite(ctx context.Context, db store.DB, collection string, rec store.Record, operation string) error {
	return s.captureWriteFrom(ctx, db, collection, rec, operation, "")
}

// captureWriteFrom is captureWrite with the record's prior lifecycle status
// (empty when unknown or not a transition). Only the transition seam threads it
// through, so the event records both ends of a status change.
func (s *Server) captureWriteFrom(ctx context.Context, db store.DB, collection string, rec store.Record, operation, fromStatus string) error {
	if s.revised(collection) {
		if err := s.captureRevision(ctx, db, collection, rec, operation); err != nil {
			return err
		}
	}
	if s.emitsEvents(collection) {
		if err := s.captureEvent(ctx, db, collection, rec, operation, fromStatus); err != nil {
			return err
		}
	}
	return nil
}

// captureEvent appends one row to the _events log for a write, in the given
// transaction. The event type is derived from the operation; for a lifecycle
// transition both the destination status and (when known) the prior status are
// recorded, so a consumer sees the full transition. Audit columns supply the
// timestamp and actor, so no separate fields are written.
func (s *Server) captureEvent(ctx context.Context, db store.DB, collection string, rec store.Record, operation, fromStatus string) error {
	id, _ := rec["id"].(string)
	if id == "" {
		return nil
	}
	to := ""
	if isStatusOperation(operation) {
		to, _ = rec[schema.LifecycleStatus].(string)
	}
	return s.captureEventRow(ctx, db, collection, id, eventForOperation(s, collection, operation), fromStatus, to)
}

// captureEventRow appends one _events row with an explicit event type. It is the
// low-level insert both the operation-derived path and the hard-delete path use.
func (s *Server) captureEventRow(ctx context.Context, db store.DB, collection, recordID, event, fromStatus, toStatus string) error {
	data := store.Record{
		schema.EventCollection: collection,
		schema.EventRecordID:   recordID,
		schema.EventType:       event,
	}
	if fromStatus != "" {
		data[schema.EventFromStatus] = fromStatus
	}
	if toStatus != "" {
		data[schema.EventToStatus] = toStatus
	}
	_, err := db.Create(ctx, store.WriteInput{Collection: schema.EventsCollection, Data: data})
	return err
}

// deleteRecord hard-deletes a record. When the collection emits events, the
// delete and its `deleted` event commit together in one transaction; otherwise it
// is a plain delete. A referential-integrity error propagates unchanged, so the
// caller's RESTRICT handling still applies.
func (s *Server) deleteRecord(ctx context.Context, collection, id string) error {
	if !s.emitsEvents(collection) {
		return s.db.Delete(ctx, collection, id)
	}
	return s.db.Tx(ctx, func(ctx context.Context, tx store.DB) error {
		if err := tx.Delete(ctx, collection, id); err != nil {
			return err
		}
		return s.captureEventRow(ctx, tx, collection, id, schema.EventDeleted, "", "")
	})
}

// isStatusOperation reports whether an operation changes the lifecycle _status,
// so the resulting to_status is worth recording on the event.
func isStatusOperation(operation string) bool {
	switch operation {
	case "publish", "unpublish", "archive", "restore":
		return true
	default:
		return false
	}
}

// eventForOperation maps a gateway write operation to a change-log event type. A
// soft-delete arrives as "delete" on a collection with SoftDelete set; a hard
// delete/purge arrives as "delete" without it — the two are distinguished so a
// consumer can tell a reversible trash from a permanent removal.
func eventForOperation(s *Server, collection, operation string) string {
	switch operation {
	case "create":
		return schema.EventCreated
	case "update":
		return schema.EventUpdated
	case "delete":
		if s.collections[collection].SoftDelete {
			return schema.EventSoftDeleted
		}
		return schema.EventDeleted
	case "publish":
		return schema.EventPublished
	case "unpublish":
		return schema.EventUnpublished
	case "archive":
		return schema.EventArchived
	case "restore":
		return schema.EventRestored
	default:
		return operation
	}
}

// handleChanges serves the change feed (ADR-0021): a keyset-paginated,
// id-ordered read of the _events log, so a consumer can poll for state changes in
// O(changes) rather than scanning records. Admin-only, since events reveal record
// ids and lifecycle transitions. `since` is the opaque cursor from a previous
// response (the last event's position); omit it to read from the beginning.
func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if !s.schema.AnyEvents() {
		// No collection emits events, so the _events table doesn't exist. Report an
		// empty, well-formed feed rather than a store error.
		writeList(w, store.Page{Data: []store.Record{}}, effectiveLimit(0))
		return
	}

	limit := 0 // 0 → store default; the store also caps the maximum
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "validation failed", Fields: map[string]string{"limit": "must be a non-negative integer"}})
			return
		}
		limit = n
	}
	q := store.Query{
		Collection: schema.EventsCollection,
		Sort:       "id", // time-ordered UUIDv7 → chronological, gap-free under SQLite
		Cursor:     r.URL.Query().Get("since"),
		Limit:      limit,
		SkipCount:  true, // a feed doesn't need a total; skip the COUNT
	}
	page, err := s.db.Find(r.Context(), q)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	for _, rec := range page.Data {
		projectEvent(rec)
	}
	writeListWith(w, r, page, effectiveLimit(q.Limit), nil)
}

// handleDeliveries lists webhook delivery rows (ADR-0021 phase 2), for
// monitoring and dead-letter inspection. Admin-only. Filter with ?status= (e.g.
// dead) and ?endpoint=; page with ?since=<cursor>&limit=.
func (s *Server) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if !s.schema.AnyEvents() {
		writeList(w, store.Page{Data: []store.Record{}}, effectiveLimit(0))
		return
	}
	var filters []store.Filter
	if st := r.URL.Query().Get("status"); st != "" {
		filters = append(filters, store.Filter{Field: schema.WebhookDeliveryStatus, Operator: store.Eq, Value: st})
	}
	if ep := r.URL.Query().Get("endpoint"); ep != "" {
		filters = append(filters, store.Filter{Field: schema.WebhookDeliveryEndpoint, Operator: store.Eq, Value: ep})
	}
	limit := 0
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			limit = n
		}
	}
	page, err := s.db.Find(r.Context(), store.Query{
		Collection: schema.WebhookDeliveriesCollection,
		Filters:    filters,
		Sort:       "id",
		Cursor:     r.URL.Query().Get("since"),
		Limit:      limit,
		SkipCount:  true,
	})
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	writeListWith(w, r, page, effectiveLimit(limit), nil)
}

// handleRetryDelivery re-arms a failed or dead delivery (ADR-0021 phase 2): its
// status returns to pending and it becomes due immediately, so the worker retries
// it on the next tick. Admin-only. A delivery that already succeeded is left
// alone (409) so recovery can't cause a duplicate.
func (s *Server) handleRetryDelivery(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	id := chi.URLParam(r, "id")
	d, err := s.db.FindOne(r.Context(), schema.WebhookDeliveriesCollection, id)
	if err != nil || d == nil {
		s.handleNotFound(w, r)
		return
	}
	if status, _ := d[schema.WebhookDeliveryStatus].(string); status == schema.WebhookDelivered {
		writeError(w, http.StatusConflict, apiError{Code: "CONFLICT", Message: "delivery already succeeded"})
		return
	}
	if _, err := s.db.Update(r.Context(), store.WriteInput{Collection: schema.WebhookDeliveriesCollection, Data: store.Record{
		"id":                         id,
		schema.WebhookDeliveryStatus: schema.WebhookPending,
		schema.WebhookDeliveryNextAt: nowUTC().Format(time.RFC3339),
	}}); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	writeData(w, http.StatusOK, map[string]any{"id": id, "status": schema.WebhookPending})
}

// projectEvent shapes a raw _events row into the change-feed contract: the audit
// columns are surfaced under stable public names (occurred_at, actor) and the
// internal audit/managed columns are dropped.
func projectEvent(rec store.Record) {
	if v, ok := rec["created_at"]; ok {
		rec["occurred_at"] = v
	}
	if v, ok := rec["created_by"]; ok && v != nil {
		rec["actor"] = v
	}
	for _, k := range []string{"created_at", "created_by", "updated_at", "updated_by"} {
		delete(rec, k)
	}
}
