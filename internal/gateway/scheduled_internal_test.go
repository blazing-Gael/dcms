package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
)

const scheduledSchema = `
version: "1"
collections:
  stories:
    publishing: true
    events: true
    fields:
      title: { type: string, required: true }
`

func buildScheduledServer(t *testing.T) (*Server, store.Adapter) {
	t.Helper()
	def, err := schema.Parse([]byte(scheduledSchema))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	db, err := sqlite.New(sqlite.Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, meta := range def.CollectionMetas() {
		plan, err := db.Diff(ctx, meta)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if err := db.Migrate(ctx, plan); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	return New(def, db, nil, Options{}), db
}

// seedStory creates a story with an explicit status and published_at (RFC3339 or
// empty), returning its id — a stand-in for a record in a given lifecycle state.
func seedStory(t *testing.T, db store.Adapter, status, publishedAt string) string {
	t.Helper()
	data := store.Record{"title": "S", schema.LifecycleStatus: status}
	if publishedAt != "" {
		data[schema.LifecyclePublishedAt] = publishedAt
	}
	rec, err := db.Create(context.Background(), store.WriteInput{Collection: "stories", Data: data})
	if err != nil {
		t.Fatalf("seed story: %v", err)
	}
	return rec["id"].(string)
}

func seedMarker(t *testing.T, db store.Adapter, id, dueAt string) {
	t.Helper()
	_, err := db.Create(context.Background(), store.WriteInput{Collection: schema.ScheduledPublishesCollection, Data: store.Record{
		schema.ScheduledCollection: "stories",
		schema.ScheduledRecordID:   id,
		schema.ScheduledDueAt:      dueAt,
	}})
	if err != nil {
		t.Fatalf("seed marker: %v", err)
	}
}

func rows(t *testing.T, db store.Adapter, collection string) []store.Record {
	t.Helper()
	page, err := db.Find(context.Background(), store.Query{Collection: collection, SkipCount: true})
	if err != nil {
		t.Fatalf("read %s: %v", collection, err)
	}
	return page.Data
}

func TestScheduled_WorkerEmitsWentLiveOnCrossing(t *testing.T) {
	srv, db := buildScheduledServer(t)
	ctx := context.Background()
	past := nowUTC().Add(-time.Minute).Format(time.RFC3339)

	// A record that is now live (published_at reached) with a due marker — exactly
	// the state the moment after a scheduled go-live crosses.
	id := seedStory(t, db, schema.StatusPublished, past)
	seedMarker(t, db, id, past)

	srv.emitDueGoLives(ctx)

	events := rows(t, db, schema.EventsCollection)
	if len(events) != 1 {
		t.Fatalf("want exactly 1 event, got %d: %#v", len(events), events)
	}
	e := events[0]
	if e[schema.EventType] != schema.EventWentLive {
		t.Errorf("event type = %v, want went_live", e[schema.EventType])
	}
	if e[schema.EventFromStatus] != "scheduled" || e[schema.EventToStatus] != schema.StatusPublished {
		t.Errorf("from/to = %v/%v, want scheduled/published", e[schema.EventFromStatus], e[schema.EventToStatus])
	}
	if e[schema.EventRecordID] != id {
		t.Errorf("event record_id = %v, want %v", e[schema.EventRecordID], id)
	}
	// The marker is consumed, so a re-run emits nothing (exactly-once).
	if n := len(rows(t, db, schema.ScheduledPublishesCollection)); n != 0 {
		t.Fatalf("marker should be deleted, %d remain", n)
	}
	srv.emitDueGoLives(ctx)
	if n := len(rows(t, db, schema.EventsCollection)); n != 1 {
		t.Fatalf("second run must not re-emit; events = %d", n)
	}
}

func TestScheduled_WorkerCancelsWhenNoLongerLive(t *testing.T) {
	srv, db := buildScheduledServer(t)
	ctx := context.Background()
	past := nowUTC().Add(-time.Minute).Format(time.RFC3339)

	// The go-live was cancelled (record returned to draft) before its time came.
	id := seedStory(t, db, schema.StatusDraft, "")
	seedMarker(t, db, id, past)

	srv.emitDueGoLives(ctx)

	if n := len(rows(t, db, schema.EventsCollection)); n != 0 {
		t.Fatalf("no event should fire for a cancelled go-live, got %d", n)
	}
	if n := len(rows(t, db, schema.ScheduledPublishesCollection)); n != 0 {
		t.Fatalf("stale marker should be cleared, %d remain", n)
	}
}

func TestScheduled_WorkerReArmsWhenStillFuture(t *testing.T) {
	srv, db := buildScheduledServer(t)
	ctx := context.Background()
	past := nowUTC().Add(-time.Minute).Format(time.RFC3339)
	future := nowUTC().Add(time.Hour).Format(time.RFC3339)

	// Marker is due, but the record was rescheduled further out — don't emit yet.
	id := seedStory(t, db, schema.StatusPublished, future)
	seedMarker(t, db, id, past)

	srv.emitDueGoLives(ctx)

	if n := len(rows(t, db, schema.EventsCollection)); n != 0 {
		t.Fatalf("no event should fire before the (rescheduled) time, got %d", n)
	}
	if n := len(rows(t, db, schema.ScheduledPublishesCollection)); n != 1 {
		t.Fatalf("marker should remain armed, got %d", n)
	}
}

func TestScheduled_ReconcileArmsRescheduleAndClears(t *testing.T) {
	srv, db := buildScheduledServer(t)
	ctx := context.Background()
	future := nowUTC().Add(time.Hour).Format(time.RFC3339)
	later := nowUTC().Add(2 * time.Hour).Format(time.RFC3339)

	id := seedStory(t, db, schema.StatusPublished, future)

	// Publish with a future time arms exactly one marker at that time.
	arm := store.Record{"id": id, schema.LifecycleStatus: schema.StatusPublished, schema.LifecyclePublishedAt: future}
	if err := srv.reconcileScheduled(ctx, db, "stories", arm, "publish"); err != nil {
		t.Fatalf("reconcile arm: %v", err)
	}
	markers := rows(t, db, schema.ScheduledPublishesCollection)
	if len(markers) != 1 || markers[0][schema.ScheduledDueAt] != future {
		t.Fatalf("expected one marker due %s, got %#v", future, markers)
	}

	// Rescheduling replaces (not duplicates) the marker.
	reArm := store.Record{"id": id, schema.LifecycleStatus: schema.StatusPublished, schema.LifecyclePublishedAt: later}
	if err := srv.reconcileScheduled(ctx, db, "stories", reArm, "publish"); err != nil {
		t.Fatalf("reconcile reschedule: %v", err)
	}
	markers = rows(t, db, schema.ScheduledPublishesCollection)
	if len(markers) != 1 || markers[0][schema.ScheduledDueAt] != later {
		t.Fatalf("reschedule should replace the marker with due %s, got %#v", later, markers)
	}

	// Unpublishing clears it.
	clear := store.Record{"id": id, schema.LifecycleStatus: schema.StatusDraft}
	if err := srv.reconcileScheduled(ctx, db, "stories", clear, "unpublish"); err != nil {
		t.Fatalf("reconcile clear: %v", err)
	}
	if n := len(rows(t, db, schema.ScheduledPublishesCollection)); n != 0 {
		t.Fatalf("unpublish should clear the marker, %d remain", n)
	}
}
