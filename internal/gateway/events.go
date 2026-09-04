package gateway

import (
	"context"
	"net/http"
	"strconv"

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
	if s.revised(collection) {
		if err := s.captureRevision(ctx, db, collection, rec, operation); err != nil {
			return err
		}
	}
	if s.emitsEvents(collection) {
		if err := s.captureEvent(ctx, db, collection, rec, operation); err != nil {
			return err
		}
	}
	return nil
}

// captureEvent appends one row to the _events log for a write, in the given
// transaction. The event type is derived from the operation; for a lifecycle
// transition the destination status is recorded (from_status is captured in a
// later phase — the event type already names the destination). Audit columns
// supply the timestamp and actor, so no separate fields are written.
func (s *Server) captureEvent(ctx context.Context, db store.DB, collection string, rec store.Record, operation string) error {
	id, _ := rec["id"].(string)
	if id == "" {
		return nil
	}
	to := ""
	if isStatusOperation(operation) {
		to, _ = rec[schema.LifecycleStatus].(string)
	}
	return s.captureEventRow(ctx, db, collection, id, eventForOperation(s, collection, operation), to)
}

// captureEventRow appends one _events row with an explicit event type. It is the
// low-level insert both the operation-derived path and the hard-delete path use.
func (s *Server) captureEventRow(ctx context.Context, db store.DB, collection, recordID, event, toStatus string) error {
	data := store.Record{
		schema.EventCollection: collection,
		schema.EventRecordID:   recordID,
		schema.EventType:       event,
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
		return s.captureEventRow(ctx, tx, collection, id, schema.EventDeleted, "")
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
