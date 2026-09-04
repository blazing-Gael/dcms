package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
)

const webhookSchema = `
version: "1"
collections:
  posts:
    fields:
      title: { type: string, required: true }
    events: true
`

// receiver records webhook POSTs and can be toggled to fail.
type receiver struct {
	mu   sync.Mutex
	got  []received
	fail bool
}

type received struct {
	headers http.Header
	body    []byte
}

func (rc *receiver) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.got = append(rc.got, received{r.Header.Clone(), b})
	f := rc.fail
	rc.mu.Unlock()
	if f {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.got)
}

func (rc *receiver) last() received {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.got[len(rc.got)-1]
}

func buildWebhookServer(t *testing.T) (*Server, store.Adapter) {
	t.Helper()
	def, err := schema.Parse([]byte(webhookSchema))
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
	return New(def, db, nil), db
}

// addEvent inserts an _events row directly, standing in for a captured write.
func addEvent(t *testing.T, db store.Adapter, event, toStatus string) string {
	t.Helper()
	data := store.Record{
		schema.EventCollection: "posts",
		schema.EventRecordID:   "rec-" + event,
		schema.EventType:       event,
	}
	if toStatus != "" {
		data[schema.EventToStatus] = toStatus
	}
	rec, err := db.Create(context.Background(), store.WriteInput{Collection: schema.EventsCollection, Data: data})
	if err != nil {
		t.Fatalf("add event: %v", err)
	}
	return rec["id"].(string)
}

func deliveries(t *testing.T, db store.Adapter, endpoint, status string) []store.Record {
	t.Helper()
	page, err := db.Find(context.Background(), store.Query{
		Collection: schema.WebhookDeliveriesCollection,
		Filters: []store.Filter{
			{Field: schema.WebhookDeliveryEndpoint, Operator: store.Eq, Value: endpoint},
			{Field: schema.WebhookDeliveryStatus, Operator: store.Eq, Value: status},
		},
		SkipCount: true,
	})
	if err != nil {
		t.Fatalf("query deliveries: %v", err)
	}
	return page.Data
}

func TestWebhook_DeliversSignedEvents(t *testing.T) {
	rc := &receiver{}
	ts := httptest.NewServer(http.HandlerFunc(rc.handler))
	defer ts.Close()

	srv, db := buildWebhookServer(t)
	ep := &WebhookEndpoint{Name: "t", URL: ts.URL, Secret: "sh-secret"}
	ctx := context.Background()
	client := &http.Client{}

	srv.enqueueEndpoint(ctx, ep) // empty log → cursor initialised to ""
	id1 := addEvent(t, db, schema.EventCreated, "")
	id2 := addEvent(t, db, schema.EventPublished, "published")
	srv.enqueueEndpoint(ctx, ep) // enqueue both
	srv.deliverEndpoint(ctx, client, ep)

	if rc.count() != 2 {
		t.Fatalf("expected 2 deliveries, got %d", rc.count())
	}
	// Both delivery rows are terminal-delivered.
	if got := len(deliveries(t, db, "t", schema.WebhookDelivered)); got != 2 {
		t.Fatalf("expected 2 delivered rows, got %d", got)
	}
	// The last POST is signed correctly and carries the stable delivery id.
	last := rc.last()
	if !verifySig("sh-secret", last.headers, last.body) {
		t.Error("signature did not verify")
	}
	if d := last.headers.Get("X-DCMS-Delivery"); d != id1 && d != id2 {
		t.Errorf("X-DCMS-Delivery %q is not one of the event ids", d)
	}
	if last.headers.Get("X-DCMS-Event") == "" {
		t.Error("missing X-DCMS-Event header")
	}
	// A wrong secret must NOT verify (guards the check itself).
	if verifySig("wrong", last.headers, last.body) {
		t.Error("signature verified under the wrong secret")
	}
}

func TestWebhook_RetriesThenDeadLetters(t *testing.T) {
	rc := &receiver{fail: true}
	ts := httptest.NewServer(http.HandlerFunc(rc.handler))
	defer ts.Close()

	srv, db := buildWebhookServer(t)
	ep := &WebhookEndpoint{Name: "t", URL: ts.URL, Secret: "s", MaxAttempts: 1}
	ctx := context.Background()
	client := &http.Client{}

	srv.enqueueEndpoint(ctx, ep)
	addEvent(t, db, schema.EventCreated, "")
	srv.enqueueEndpoint(ctx, ep)
	srv.deliverEndpoint(ctx, client, ep)

	if rc.count() != 1 {
		t.Fatalf("expected 1 attempt, got %d", rc.count())
	}
	// One attempt with MaxAttempts=1 → dead-lettered, not retried.
	if got := len(deliveries(t, db, "t", schema.WebhookDead)); got != 1 {
		t.Fatalf("expected 1 dead delivery, got %d", got)
	}
	if got := len(deliveries(t, db, "t", schema.WebhookPending)); got != 0 {
		t.Errorf("expected no pending deliveries, got %d", got)
	}
}

func TestWebhook_RetryScheduledOnTransientFailure(t *testing.T) {
	rc := &receiver{fail: true}
	ts := httptest.NewServer(http.HandlerFunc(rc.handler))
	defer ts.Close()

	srv, db := buildWebhookServer(t)
	ep := &WebhookEndpoint{Name: "t", URL: ts.URL, Secret: "s", MaxAttempts: 5}
	ctx := context.Background()

	srv.enqueueEndpoint(ctx, ep)
	addEvent(t, db, schema.EventCreated, "")
	srv.enqueueEndpoint(ctx, ep)
	srv.deliverEndpoint(ctx, &http.Client{}, ep)

	// Below max attempts → scheduled for retry (failed) with a future next_attempt_at.
	failed := deliveries(t, db, "t", schema.WebhookFailed)
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed (retry-scheduled) delivery, got %d", len(failed))
	}
	next, _ := failed[0][schema.WebhookDeliveryNextAt].(string)
	nt, err := time.Parse(time.RFC3339, next)
	if err != nil || !nt.After(time.Now().UTC()) {
		t.Errorf("expected a future next_attempt_at, got %q", next)
	}
	if a := intOf(failed[0][schema.WebhookDeliveryAttempts]); a != 1 {
		t.Errorf("attempts = %d, want 1", a)
	}
}

func TestWebhook_FiltersByEventType(t *testing.T) {
	rc := &receiver{}
	ts := httptest.NewServer(http.HandlerFunc(rc.handler))
	defer ts.Close()

	srv, db := buildWebhookServer(t)
	ep := &WebhookEndpoint{Name: "t", URL: ts.URL, Secret: "s", Events: []string{schema.EventPublished}}
	ctx := context.Background()

	srv.enqueueEndpoint(ctx, ep)
	addEvent(t, db, schema.EventCreated, "")            // filtered out
	addEvent(t, db, schema.EventPublished, "published") // wanted
	srv.enqueueEndpoint(ctx, ep)

	// Only the published event should have produced a delivery row.
	if got := len(deliveries(t, db, "t", schema.WebhookPending)); got != 1 {
		t.Fatalf("expected 1 pending delivery (published only), got %d", got)
	}
}

func TestWebhook_NewEndpointStartsFromNow(t *testing.T) {
	srv, db := buildWebhookServer(t)
	ep := &WebhookEndpoint{Name: "t", URL: "http://unused", Secret: "s"}
	ctx := context.Background()

	// Events already exist before the endpoint is first seen.
	addEvent(t, db, schema.EventCreated, "")
	addEvent(t, db, schema.EventCreated, "")
	srv.enqueueEndpoint(ctx, ep) // first sight → cursor jumps to the tip
	srv.enqueueEndpoint(ctx, ep) // no new events since

	// History is not replayed: no delivery rows at all.
	all := deliveries(t, db, "t", schema.WebhookPending)
	if len(all) != 0 {
		t.Fatalf("a new endpoint should not replay history; got %d pending", len(all))
	}
}

// verifySig recomputes the HMAC the way a real receiver would.
func verifySig(secret string, h http.Header, body []byte) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s.", h.Get("X-DCMS-Timestamp"))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(h.Get("X-DCMS-Signature")))
}
