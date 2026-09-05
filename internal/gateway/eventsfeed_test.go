package gateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
)

const eventsSchema = `
version: "1"
auth:
  roles:
    admin:  { label: Administrator }
    author: { label: Author }
  session:
    ttl: 1h
collections:
  posts:
    fields:
      title: { type: string, required: true }
      slug:  { type: string, unique: true }
    publishing: true
    soft_delete: true
    events: true
    access:
      read:   public
      create: [admin, author]
      update: [admin, author]
      delete: [admin]
  notes:
    fields:
      body: { type: string, required: true }
    events: true
    access:
      read:   public
      create: [admin]
      delete: [admin]
  tags:
    fields:
      name: { type: string, required: true }
    access:
      read:   public
      create: [admin]
`

func newEventsServer(t *testing.T) (*httptest.Server, store.Adapter) {
	t.Helper()
	def, err := schema.Parse([]byte(eventsSchema))
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
	srv := httptest.NewServer(gateway.New(def, db, nil, gateway.Options{
		Authenticator: gateway.NewSessionAuthenticator(db),
		Idempotency:   &gateway.IdempotencyOptions{}, // enable the Idempotency-Key path
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, db
}

// changes reads the change feed as the given admin token and returns the event
// list plus the next cursor.
func changes(t *testing.T, base, token, since string) ([]map[string]any, string) {
	t.Helper()
	url := base + "/api/v1/_changes"
	if since != "" {
		url += "?since=" + since
	}
	st, body := doAs(t, http.MethodGet, url, token, "")
	if st != http.StatusOK {
		t.Fatalf("_changes: status %d (%v)", st, body)
	}
	raw, _ := body["data"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	meta, _ := body["meta"].(map[string]any)
	next, _ := meta["next_cursor"].(string)
	return out, next
}

func TestChangeFeed_CapturesLifecycle(t *testing.T) {
	srv, db := newEventsServer(t)
	seedUser(t, db, "admin@x.com", "password123", "admin")
	tok := login(t, srv.URL, "admin@x.com", "password123")

	// A brand-new backend has an empty feed.
	if evs, _ := changes(t, srv.URL, tok, ""); len(evs) != 0 {
		t.Fatalf("expected empty feed, got %d events", len(evs))
	}

	// Create → updated → publish → soft-delete on posts; create a tag (no events).
	_, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/posts", tok, `{"title":"Hello"}`)
	id := recordID(t, body)
	if st, _ := doAs(t, http.MethodPatch, srv.URL+"/api/v1/posts/"+id, tok, `{"title":"Hello v2"}`); st != http.StatusOK {
		t.Fatalf("update: %d", st)
	}
	if st, _ := doAs(t, http.MethodPost, srv.URL+"/api/v1/posts/"+id+"/publish", tok, `{}`); st != http.StatusOK {
		t.Fatalf("publish: %d", st)
	}
	if st, _ := doAs(t, http.MethodPost, srv.URL+"/api/v1/tags", tok, `{"name":"news"}`); st != http.StatusCreated {
		t.Fatalf("create tag: %d", st)
	}
	if st, _ := doAs(t, http.MethodDelete, srv.URL+"/api/v1/posts/"+id, tok, ""); st != http.StatusNoContent {
		t.Fatalf("delete: %d", st)
	}

	evs, _ := changes(t, srv.URL, tok, "")
	// Exactly the four posts events, in order — the tag create emitted nothing.
	wantSeq := []string{
		schema.EventCreated, schema.EventUpdated, schema.EventPublished, schema.EventSoftDeleted,
	}
	if len(evs) != len(wantSeq) {
		t.Fatalf("expected %d events, got %d: %v", len(wantSeq), len(evs), evs)
	}
	for i, want := range wantSeq {
		if got, _ := evs[i]["event"].(string); got != want {
			t.Errorf("event[%d] = %q, want %q", i, got, want)
		}
		if col, _ := evs[i]["collection"].(string); col != "posts" {
			t.Errorf("event[%d].collection = %q, want posts", i, col)
		}
		if rid, _ := evs[i]["record_id"].(string); rid != id {
			t.Errorf("event[%d].record_id = %q, want %q", i, rid, id)
		}
		// Actor is the admin who made the change (from the verified principal).
		if evs[i]["actor"] == nil {
			t.Errorf("event[%d] missing actor", i)
		}
		if evs[i]["occurred_at"] == nil {
			t.Errorf("event[%d] missing occurred_at", i)
		}
	}
	// The publish event records both ends of the transition: from draft (the
	// default status a new post carries) to published.
	if to, _ := evs[2]["to_status"].(string); to != schema.StatusPublished {
		t.Errorf("publish to_status = %q, want %q", to, schema.StatusPublished)
	}
	if from, _ := evs[2]["from_status"].(string); from != schema.StatusDraft {
		t.Errorf("publish from_status = %q, want %q", from, schema.StatusDraft)
	}
	// A plain (non-transition) update carries no status fields — from_status is
	// read only for lifecycle transitions, never for an ordinary edit.
	if _, ok := evs[1]["from_status"]; ok {
		t.Errorf("plain update event should have no from_status: %v", evs[1])
	}
	if _, ok := evs[1]["to_status"]; ok {
		t.Errorf("plain update event should have no to_status: %v", evs[1])
	}
}

func TestChangeFeed_CursorAdvances(t *testing.T) {
	srv, db := newEventsServer(t)
	seedUser(t, db, "admin@x.com", "password123", "admin")
	tok := login(t, srv.URL, "admin@x.com", "password123")

	for range 3 {
		doAs(t, http.MethodPost, srv.URL+"/api/v1/posts", tok, `{"title":"p"}`)
	}
	// First page of 2, then resume from the cursor: no repeats, the 3rd event.
	st, body := doAs(t, http.MethodGet, srv.URL+"/api/v1/_changes?limit=2", tok, "")
	if st != http.StatusOK {
		t.Fatalf("page 1: %d", st)
	}
	first, _ := body["data"].([]any)
	meta, _ := body["meta"].(map[string]any)
	cur, _ := meta["next_cursor"].(string)
	if len(first) != 2 || cur == "" {
		t.Fatalf("expected 2 events + a cursor, got %d events cursor=%q", len(first), cur)
	}
	rest, _ := changes(t, srv.URL, tok, cur)
	if len(rest) != 1 {
		t.Fatalf("expected 1 remaining event after cursor, got %d", len(rest))
	}
}

// TestChangeFeed_FailedWriteEmitsNoEvent is the core outbox guarantee: an event
// is captured in the write's transaction, so a write that rolls back leaves no
// event behind. A second create with a duplicate unique slug must fail and emit
// nothing.
func TestChangeFeed_FailedWriteEmitsNoEvent(t *testing.T) {
	srv, db := newEventsServer(t)
	seedUser(t, db, "admin@x.com", "password123", "admin")
	tok := login(t, srv.URL, "admin@x.com", "password123")

	if st, _ := doAs(t, http.MethodPost, srv.URL+"/api/v1/posts", tok, `{"title":"A","slug":"dup"}`); st != http.StatusCreated {
		t.Fatalf("first create: %d", st)
	}
	// Same slug → unique violation → the whole transaction rolls back.
	st, _ := doAs(t, http.MethodPost, srv.URL+"/api/v1/posts", tok, `{"title":"B","slug":"dup"}`)
	if st == http.StatusCreated {
		t.Fatalf("duplicate slug create unexpectedly succeeded (%d)", st)
	}
	evs, _ := changes(t, srv.URL, tok, "")
	if len(evs) != 1 {
		t.Fatalf("expected exactly 1 event (the successful create), got %d: %v", len(evs), evs)
	}
	if got, _ := evs[0]["event"].(string); got != schema.EventCreated {
		t.Errorf("event = %q, want %q", got, schema.EventCreated)
	}
}

// TestChangeFeed_HardDelete covers a delete on a collection WITHOUT soft_delete,
// which removes the row and emits `deleted` (distinct from `soft_deleted`).
func TestChangeFeed_HardDelete(t *testing.T) {
	srv, db := newEventsServer(t)
	seedUser(t, db, "admin@x.com", "password123", "admin")
	tok := login(t, srv.URL, "admin@x.com", "password123")

	_, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/notes", tok, `{"body":"n"}`)
	id := recordID(t, body)
	if st, _ := doAs(t, http.MethodDelete, srv.URL+"/api/v1/notes/"+id, tok, ""); st != http.StatusNoContent {
		t.Fatalf("delete: %d", st)
	}
	evs, _ := changes(t, srv.URL, tok, "")
	if len(evs) != 2 {
		t.Fatalf("expected created + deleted, got %d: %v", len(evs), evs)
	}
	if got, _ := evs[1]["event"].(string); got != schema.EventDeleted {
		t.Errorf("second event = %q, want %q", got, schema.EventDeleted)
	}
}

// TestChangeFeed_AllTransitions exercises the transitions the lifecycle test
// didn't: unpublish, archive, and restore, each with its destination status.
func TestChangeFeed_AllTransitions(t *testing.T) {
	srv, db := newEventsServer(t)
	seedUser(t, db, "admin@x.com", "password123", "admin")
	tok := login(t, srv.URL, "admin@x.com", "password123")

	_, body := doAs(t, http.MethodPost, srv.URL+"/api/v1/posts", tok, `{"title":"T"}`)
	id := recordID(t, body)
	for _, step := range []string{"publish", "unpublish", "archive", "restore"} {
		if st, _ := doAs(t, http.MethodPost, srv.URL+"/api/v1/posts/"+id+"/"+step, tok, `{}`); st != http.StatusOK {
			t.Fatalf("%s: %d", step, st)
		}
	}
	evs, _ := changes(t, srv.URL, tok, "")
	want := []string{
		schema.EventCreated, schema.EventPublished, schema.EventUnpublished,
		schema.EventArchived, schema.EventRestored,
	}
	if len(evs) != len(want) {
		t.Fatalf("expected %d events, got %d: %v", len(want), len(evs), evs)
	}
	for i, w := range want {
		if got, _ := evs[i]["event"].(string); got != w {
			t.Errorf("event[%d] = %q, want %q", i, got, w)
		}
	}
	// Each status transition records both ends. (Restore only clears the
	// soft-delete marker, so it leaves _status where archive left it.)
	transitions := map[int][2]string{
		1: {schema.StatusDraft, schema.StatusPublished}, // publish
		2: {schema.StatusPublished, schema.StatusDraft}, // unpublish
		3: {schema.StatusDraft, schema.StatusArchived},  // archive
	}
	for i, ft := range transitions {
		if from, _ := evs[i]["from_status"].(string); from != ft[0] {
			t.Errorf("event[%d] from_status = %q, want %q", i, from, ft[0])
		}
		if to, _ := evs[i]["to_status"].(string); to != ft[1] {
			t.Errorf("event[%d] to_status = %q, want %q", i, to, ft[1])
		}
	}
}

// TestChangeFeed_IdempotentCreateEmitsOnce pins that a create retried with the
// same Idempotency-Key (ADR-0018) records exactly one `created` event — the
// replay must not double-emit.
func TestChangeFeed_IdempotentCreateEmitsOnce(t *testing.T) {
	srv, db := newEventsServer(t)
	seedUser(t, db, "admin@x.com", "password123", "admin")
	tok := login(t, srv.URL, "admin@x.com", "password123")

	post := func() int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/posts", newReader(`{"title":"once","slug":"once"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "key-123")
		st, _ := doReq(t, req)
		return st
	}
	post()
	post() // replay of the same key

	evs, _ := changes(t, srv.URL, tok, "")
	if len(evs) != 1 {
		t.Fatalf("idempotent create should emit exactly 1 event, got %d: %v", len(evs), evs)
	}
}

// TestWebhookOps_DeadLetterViewAndRetry covers the admin operational endpoints:
// listing deliveries (dead-letter inspection) and re-arming one for retry.
func TestWebhookOps_DeadLetterViewAndRetry(t *testing.T) {
	srv, db := newEventsServer(t)
	seedUser(t, db, "admin@x.com", "password123", "admin")
	seedUser(t, db, "author@x.com", "password123", "author")
	admin := login(t, srv.URL, "admin@x.com", "password123")
	author := login(t, srv.URL, "author@x.com", "password123")

	// A dead delivery, inserted directly (as the worker would leave it).
	rec, err := db.Create(context.Background(), store.WriteInput{
		Collection: schema.WebhookDeliveriesCollection,
		Data: store.Record{
			schema.WebhookDeliveryEvent:    "evt-1",
			schema.WebhookDeliveryEndpoint: "site",
			schema.WebhookDeliveryStatus:   schema.WebhookDead,
			schema.WebhookDeliveryAttempts: 12,
		},
	})
	if err != nil {
		t.Fatalf("seed delivery: %v", err)
	}
	id, _ := rec["id"].(string)

	// Admin sees the dead delivery; anon is refused.
	st, body := doAs(t, http.MethodGet, srv.URL+"/api/v1/_events/deliveries?status=dead", admin, "")
	if st != http.StatusOK {
		t.Fatalf("list dead: %d", st)
	}
	if data, _ := body["data"].([]any); len(data) != 1 {
		t.Fatalf("expected 1 dead delivery, got %d", len(data))
	}
	if st, _ := do(t, http.MethodGet, srv.URL+"/api/v1/_events/deliveries", ""); st != http.StatusUnauthorized {
		t.Fatalf("anon list: got %d, want 401", st)
	}
	if st, _ := doAs(t, http.MethodPost, srv.URL+"/api/v1/_events/deliveries/"+id+"/retry", author, ""); st != http.StatusForbidden {
		t.Fatalf("non-admin retry: got %d, want 403", st)
	}

	// Admin re-arms it → pending, and it now shows under status=pending.
	if st, _ := doAs(t, http.MethodPost, srv.URL+"/api/v1/_events/deliveries/"+id+"/retry", admin, ""); st != http.StatusOK {
		t.Fatalf("retry: %d", st)
	}
	_, body = doAs(t, http.MethodGet, srv.URL+"/api/v1/_events/deliveries?status=pending", admin, "")
	if data, _ := body["data"].([]any); len(data) != 1 {
		t.Fatalf("expected the delivery to be pending after retry")
	}
}

func TestChangeFeed_AdminOnly(t *testing.T) {
	srv, db := newEventsServer(t)
	seedUser(t, db, "author@x.com", "password123", "author")
	authorTok := login(t, srv.URL, "author@x.com", "password123")

	// Anonymous → 401.
	if st, _ := do(t, http.MethodGet, srv.URL+"/api/v1/_changes", ""); st != http.StatusUnauthorized {
		t.Fatalf("anon _changes: got %d, want 401", st)
	}
	// Authenticated non-admin → 403.
	if st, _ := doAs(t, http.MethodGet, srv.URL+"/api/v1/_changes", authorTok, ""); st != http.StatusForbidden {
		t.Fatalf("author _changes: got %d, want 403", st)
	}
}
