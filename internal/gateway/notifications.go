package gateway

import (
	"context"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

const (
	defaultNotificationPollInterval = 5 * time.Second
	notificationMaxAttempts         = 10
	notificationDeliverBatch        = 50
)

// enqueueNotification durably queues an outbound notification and returns as soon
// as it is persisted (ADR-0021 phase 3). Delivery happens off the request path in
// RunNotifications, so a slow or unavailable mailer never blocks or fails the
// request, and a crash cannot lose the message.
func (s *Server) enqueueNotification(ctx context.Context, msg Notification) error {
	_, err := s.db.Create(ctx, store.WriteInput{Collection: schema.NotificationsCollection, Data: store.Record{
		schema.NotificationTo:       msg.To,
		schema.NotificationKind:     msg.Kind,
		schema.NotificationLink:     msg.Link,
		schema.NotificationStatus:   schema.NotificationPending,
		schema.NotificationAttempts: 0,
		schema.NotificationNextAt:   nowUTC().Format(time.RFC3339),
	}})
	return err
}

// RunNotifications delivers queued notifications until ctx is cancelled, polling
// on a fixed interval. It is cheap when the queue is empty (one indexed query),
// so it is always safe to launch.
func (s *Server) RunNotifications(ctx context.Context) {
	ticker := time.NewTicker(defaultNotificationPollInterval)
	defer ticker.Stop()
	for {
		s.deliverNotifications(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// deliverNotifications attempts every due notification (pending or failed with a
// past next_attempt_at), oldest schedule first.
func (s *Server) deliverNotifications(ctx context.Context) {
	page, err := s.db.Find(ctx, store.Query{
		Collection: schema.NotificationsCollection,
		Filters: []store.Filter{
			{Field: schema.NotificationStatus, Operator: store.In, Value: []any{schema.NotificationPending, schema.NotificationFailed}},
			{Field: schema.NotificationNextAt, Operator: store.Lte, Value: nowUTC().Format(time.RFC3339)},
		},
		Sort:      schema.NotificationNextAt,
		Limit:     notificationDeliverBatch,
		SkipCount: true,
	})
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("notification scan failed", "err", err)
		}
		return
	}
	for _, n := range page.Data {
		if ctx.Err() != nil {
			return
		}
		s.attemptNotification(ctx, n)
	}
}

// attemptNotification delivers one queued notification via the configured
// transport. On success the row is deleted (so the raw action link does not
// linger); on failure it is retried with backoff, then dead-lettered.
func (s *Server) attemptNotification(ctx context.Context, n store.Record) {
	id, _ := n["id"].(string)
	err := s.notifier().Notify(ctx, Notification{
		To:   stringOf(n[schema.NotificationTo]),
		Kind: stringOf(n[schema.NotificationKind]),
		Link: stringOf(n[schema.NotificationLink]),
	})
	if err == nil {
		if delErr := s.db.Delete(ctx, schema.NotificationsCollection, id); delErr != nil {
			s.logger.Warn("notification delete failed", "id", id, "err", delErr)
		}
		return
	}

	attempts := intOf(n[schema.NotificationAttempts]) + 1
	reason := err.Error()
	if len(reason) > webhookErrLimit {
		reason = reason[:webhookErrLimit]
	}
	data := store.Record{
		"id":                         id,
		schema.NotificationAttempts:  attempts,
		schema.NotificationLastError: reason,
	}
	if attempts >= notificationMaxAttempts {
		data[schema.NotificationStatus] = schema.NotificationDead
	} else {
		data[schema.NotificationStatus] = schema.NotificationFailed
		data[schema.NotificationNextAt] = nowUTC().Add(deliveryBackoff(attempts)).Format(time.RFC3339)
	}
	if _, uerr := s.db.Update(ctx, store.WriteInput{Collection: schema.NotificationsCollection, Data: data}); uerr != nil {
		s.logger.Warn("notification update failed", "id", id, "err", uerr)
	}
}
