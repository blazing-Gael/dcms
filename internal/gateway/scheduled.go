package gateway

import (
	"context"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Scheduled go-live events (issue #28). A scheduled publish becomes visible when
// the clock passes its future `_published_at`, with no write — so the write-driven
// change feed and webhooks (ADR-0021) would never report it. This closes that hole
// with a durable outbox: a publish transition that sets a future `_published_at`
// enqueues a marker (reconcileScheduled, in the write's own transaction), and this
// worker emits a `went_live` event when the marker comes due, deleting it in the
// same transaction so the emit is exactly-once. It is the only time-triggered
// behaviour in core, and deliberately not a general scheduler (ADR-0026).

const (
	defaultScheduledPollInterval = 15 * time.Second
	scheduledEmitBatch           = 100 // the store caps a page at 100 anyway
)

// reconcilesScheduling reports whether an operation can change whether a record
// has a pending future go-live, so the marker outbox must be reconciled after it.
// Ordinary PATCH ("update") can't touch the managed lifecycle columns, so it is
// excluded.
func reconcilesScheduling(operation string) bool {
	switch operation {
	case "publish", "unpublish", "archive", "restore", "delete":
		return true
	default:
		return false
	}
}

// isFutureGoLive reports whether a record is published with a not-yet-reached
// `_published_at` and is not trashed — i.e. a go-live is still pending for it.
func isFutureGoLive(rec store.Record) bool {
	if status, _ := rec[schema.LifecycleStatus].(string); status != schema.StatusPublished {
		return false
	}
	if isSet(rec[schema.LifecycleDeletedAt]) {
		return false
	}
	pa, ok := rec[schema.LifecyclePublishedAt].(string)
	if !ok || pa == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, pa)
	if err != nil {
		return false
	}
	return t.After(time.Now().UTC())
}

// reconcileScheduled arms or clears a record's go-live marker after a lifecycle
// write, in that write's transaction so the marker and the status change commit
// together. Only collections that both publish and emit events carry markers.
// Every reconciled write first clears any existing marker, then re-arms it iff the
// record is now a future go-live — so publish→reschedule replaces the marker, and
// unpublish/archive/trash clear it, with no per-operation special-casing.
func (s *Server) reconcileScheduled(ctx context.Context, db store.DB, collection string, rec store.Record, operation string) error {
	cd := s.collections[collection]
	if !cd.Publishing || !cd.Events || !reconcilesScheduling(operation) {
		return nil
	}
	id, _ := rec["id"].(string)
	if id == "" {
		return nil
	}
	if err := s.clearScheduledMarker(ctx, db, collection, id); err != nil {
		return err
	}
	if !isFutureGoLive(rec) {
		return nil
	}
	pubAt, _ := rec[schema.LifecyclePublishedAt].(string)
	_, err := db.Create(ctx, store.WriteInput{Collection: schema.ScheduledPublishesCollection, Data: store.Record{
		schema.ScheduledCollection: collection,
		schema.ScheduledRecordID:   id,
		schema.ScheduledDueAt:      pubAt,
	}})
	return err
}

// clearScheduledMarker removes any pending go-live marker for a record. Safe to
// call for a record that has none. Runs on whatever DB it is given (a tx).
func (s *Server) clearScheduledMarker(ctx context.Context, db store.DB, collection, id string) error {
	_, err := db.RawExec(ctx,
		`DELETE FROM "`+schema.ScheduledPublishesCollection+`" WHERE `+schema.ScheduledCollection+` = $1 AND `+schema.ScheduledRecordID+` = $2`,
		collection, id)
	return err
}

// RunScheduledPublishes emits go-live events for scheduled publishes whose time has
// come, until ctx is cancelled. Cheap when nothing is due (one indexed query), so
// it is always safe to launch. A no-op when no collection both publishes and emits
// events (the outbox table doesn't exist).
func (s *Server) RunScheduledPublishes(ctx context.Context) {
	if !s.schema.AnyScheduledEvents() {
		return
	}
	ticker := time.NewTicker(defaultScheduledPollInterval)
	defer ticker.Stop()
	for {
		s.emitDueGoLives(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// emitDueGoLives fires every marker whose due_at has passed, oldest first.
func (s *Server) emitDueGoLives(ctx context.Context) {
	page, err := s.db.Find(ctx, store.Query{
		Collection: schema.ScheduledPublishesCollection,
		Filters:    []store.Filter{{Field: schema.ScheduledDueAt, Operator: store.Lte, Value: nowUTC().Format(time.RFC3339)}},
		Sort:       schema.ScheduledDueAt,
		Limit:      scheduledEmitBatch,
		SkipCount:  true,
	})
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("scheduled go-live scan failed", "err", err)
		}
		return
	}
	for _, marker := range page.Data {
		if ctx.Err() != nil {
			return
		}
		if err := s.fireGoLive(ctx, marker); err != nil && ctx.Err() == nil {
			s.logger.Warn("scheduled go-live failed", "err", err,
				"collection", marker[schema.ScheduledCollection], "record_id", marker[schema.ScheduledRecordID])
		}
	}
}

// fireGoLive resolves one due marker. It re-reads the record so a publish that was
// unpublished, archived, trashed, deleted, or rescheduled since is handled
// correctly: the event is emitted only if the record is genuinely live now, and
// the marker is consumed (or re-armed for a later reschedule) in the same
// transaction — so a `went_live` is emitted at most once per go-live.
func (s *Server) fireGoLive(ctx context.Context, marker store.Record) error {
	collection, _ := marker[schema.ScheduledCollection].(string)
	id, _ := marker[schema.ScheduledRecordID].(string)
	if collection == "" || id == "" {
		return s.deleteMarkerByID(ctx, marker) // malformed: drop it
	}

	rec, err := s.db.FindOne(ctx, collection, id)
	if err != nil {
		return s.deleteMarkerByID(ctx, marker) // record gone: drop the marker
	}

	// Rescheduled to a still-future time: the reconcile on that write already
	// updated the marker, but guard anyway — re-arm and wait.
	if isFutureGoLive(rec) {
		return nil
	}

	live := false
	if status, _ := rec[schema.LifecycleStatus].(string); status == schema.StatusPublished {
		if !isSet(rec[schema.LifecycleDeletedAt]) && publishedAtReached(rec) {
			live = true
		}
	}

	return s.db.Tx(ctx, func(ctx context.Context, tx store.DB) error {
		if live {
			// "scheduled" is the view-only pseudo-status for a published-but-future
			// record; it is the record's effective prior state at the crossing.
			if err := s.captureEventRow(ctx, tx, collection, id, schema.EventWentLive, "scheduled", schema.StatusPublished); err != nil {
				return err
			}
		}
		return s.clearScheduledMarker(ctx, tx, collection, id)
	})
}

// deleteMarkerByID removes a marker by its own id (for the rare malformed row).
func (s *Server) deleteMarkerByID(ctx context.Context, marker store.Record) error {
	id, _ := marker["id"].(string)
	if id == "" {
		return nil
	}
	return s.db.Delete(ctx, schema.ScheduledPublishesCollection, id)
}
