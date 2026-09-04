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
    publishing: true
    soft_delete: true
    events: true
    access:
      read:   public
      create: [admin, author]
      update: [admin, author]
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
	// The publish event records the destination status.
	if to, _ := evs[2]["to_status"].(string); to != schema.StatusPublished {
		t.Errorf("publish to_status = %q, want %q", to, schema.StatusPublished)
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
