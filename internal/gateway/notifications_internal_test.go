package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
)

// stubTransport is a controllable Notifier standing in for SMTP.
type stubTransport struct {
	mu    sync.Mutex
	calls int
	fail  bool
	got   []Notification
}

func (s *stubTransport) Notify(_ context.Context, msg Notification) error {
	s.mu.Lock()
	s.calls++
	s.got = append(s.got, msg)
	f := s.fail
	s.mu.Unlock()
	if f {
		return errors.New("smtp unavailable")
	}
	return nil
}

func buildNotifyServer(t *testing.T, tr Notifier) (*Server, store.Adapter) {
	t.Helper()
	def, err := schema.Parse([]byte(webhookSchema)) // any schema; _notifications is always injected
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
	return New(def, db, nil, Options{Notifier: tr}), db
}

func allNotifications(t *testing.T, db store.Adapter) []store.Record {
	t.Helper()
	page, err := db.Find(context.Background(), store.Query{Collection: schema.NotificationsCollection, SkipCount: true})
	if err != nil {
		t.Fatalf("read notifications: %v", err)
	}
	return page.Data
}

func TestNotifications_DeliversThenDeletes(t *testing.T) {
	tr := &stubTransport{}
	srv, db := buildNotifyServer(t, tr)
	ctx := context.Background()

	link := "https://site/reset?token=abc123"
	if err := srv.enqueueNotification(ctx, Notification{To: "a@b.com", Kind: "password_reset", Link: link}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if n := len(allNotifications(t, db)); n != 1 {
		t.Fatalf("expected 1 queued notification, got %d", n)
	}

	srv.deliverNotifications(ctx)

	if tr.calls != 1 {
		t.Fatalf("transport should be called once, got %d", tr.calls)
	}
	if tr.got[0].Link != link || tr.got[0].To != "a@b.com" {
		t.Errorf("transport received wrong message: %+v", tr.got[0])
	}
	// A delivered notification is removed, so the raw link does not linger.
	if n := len(allNotifications(t, db)); n != 0 {
		t.Fatalf("delivered notification should be deleted, %d remain", n)
	}
}

func TestNotifications_TransientFailureReschedules(t *testing.T) {
	tr := &stubTransport{fail: true}
	srv, db := buildNotifyServer(t, tr)
	ctx := context.Background()

	_ = srv.enqueueNotification(ctx, Notification{To: "a@b.com", Kind: "password_reset", Link: "x"})
	srv.deliverNotifications(ctx)

	rows := allNotifications(t, db)
	if len(rows) != 1 {
		t.Fatalf("failed notification should remain queued, got %d", len(rows))
	}
	if st, _ := rows[0][schema.NotificationStatus].(string); st != schema.NotificationFailed {
		t.Errorf("status = %q, want failed", st)
	}
	if a := intOf(rows[0][schema.NotificationAttempts]); a != 1 {
		t.Errorf("attempts = %d, want 1", a)
	}
	next, _ := rows[0][schema.NotificationNextAt].(string)
	nt, err := time.Parse(time.RFC3339, next)
	if err != nil || !nt.After(time.Now().UTC()) {
		t.Errorf("expected a future next_attempt_at, got %q", next)
	}
}

func TestNotifications_DeadLettersAtMaxAttempts(t *testing.T) {
	tr := &stubTransport{fail: true}
	srv, db := buildNotifyServer(t, tr)
	ctx := context.Background()

	// Seed a row one attempt short of the cap, already due.
	_, err := db.Create(ctx, store.WriteInput{Collection: schema.NotificationsCollection, Data: store.Record{
		schema.NotificationTo:       "a@b.com",
		schema.NotificationKind:     "password_reset",
		schema.NotificationLink:     "x",
		schema.NotificationStatus:   schema.NotificationFailed,
		schema.NotificationAttempts: notificationMaxAttempts - 1,
		schema.NotificationNextAt:   nowUTC().Add(-time.Minute).Format(time.RFC3339),
	}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	srv.deliverNotifications(ctx)

	rows := allNotifications(t, db)
	if len(rows) != 1 {
		t.Fatalf("dead notification should remain for inspection, got %d", len(rows))
	}
	if st, _ := rows[0][schema.NotificationStatus].(string); st != schema.NotificationDead {
		t.Errorf("status = %q, want dead", st)
	}
}
