package gateway

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// WebhookEndpoint is one configured delivery target (ADR-0021 phase 2).
type WebhookEndpoint struct {
	Name        string   // stable identifier, used as the delivery key
	URL         string   // where to POST
	Secret      string   // HMAC-SHA256 key; resolved from env by the caller
	Events      []string // deliver only these event types; empty = all
	Collections []string // deliver only for these collections; empty = all
	MaxAttempts int      // give up (dead-letter) after this many; 0 uses the default
}

// WebhookOptions configures the delivery worker.
type WebhookOptions struct {
	Endpoints    []WebhookEndpoint
	PollInterval time.Duration // how often to enqueue+deliver; 0 uses the default
}

const (
	defaultWebhookPollInterval = 2 * time.Second
	defaultWebhookMaxAttempts  = 12
	webhookEnqueueBatch        = 200 // events scanned per endpoint per tick
	webhookDeliverBatch        = 50  // deliveries attempted per endpoint per tick
	webhookHTTPTimeout         = 10 * time.Second
	webhookMaxBackoff          = time.Hour
	webhookErrLimit            = 500 // stored last_error is truncated to this
)

// RunWebhooks is the delivery worker: on each tick it fans new events out to
// pending deliveries (enqueue) and attempts due deliveries with retry/backoff
// (deliver). It runs until ctx is cancelled and does nothing if no endpoints are
// configured, so it is safe to always launch. Delivery is at-least-once and
// unordered; consumers dedupe on the stable X-DCMS-Delivery id.
func (s *Server) RunWebhooks(ctx context.Context) {
	if s.opts.Webhooks == nil || len(s.opts.Webhooks.Endpoints) == 0 || !s.schema.AnyEvents() {
		return
	}
	interval := s.opts.Webhooks.PollInterval
	if interval <= 0 {
		interval = defaultWebhookPollInterval
	}
	client := &http.Client{Timeout: webhookHTTPTimeout}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		for i := range s.opts.Webhooks.Endpoints {
			ep := &s.opts.Webhooks.Endpoints[i]
			s.enqueueEndpoint(ctx, ep)
			s.deliverEndpoint(ctx, client, ep)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// enqueueEndpoint fans events newer than the endpoint's cursor into pending
// delivery rows, then advances the cursor. A fresh endpoint starts at the current
// tip so adding a webhook does not replay history.
func (s *Server) enqueueEndpoint(ctx context.Context, ep *WebhookEndpoint) {
	cursor, initialized, err := s.webhookCursor(ctx, ep.Name)
	if err != nil {
		s.logWebhook("cursor read failed", ep, err)
		return
	}
	if !initialized {
		tip, err := s.maxEventID(ctx)
		if err != nil {
			s.logWebhook("tip read failed", ep, err)
			return
		}
		if err := s.setWebhookCursor(ctx, ep.Name, tip); err != nil {
			s.logWebhook("cursor init failed", ep, err)
		}
		return
	}

	var filters []store.Filter
	if cursor != "" {
		filters = append(filters, store.Filter{Field: "id", Operator: store.Gt, Value: cursor})
	}
	page, err := s.db.Find(ctx, store.Query{
		Collection: schema.EventsCollection,
		Filters:    filters,
		Sort:       "id",
		Limit:      webhookEnqueueBatch,
		SkipCount:  true,
	})
	if err != nil {
		s.logWebhook("event scan failed", ep, err)
		return
	}
	last := cursor
	for _, ev := range page.Data {
		id, _ := ev["id"].(string)
		if id == "" {
			continue
		}
		if endpointWants(ep, ev) {
			if _, err := s.db.Create(ctx, store.WriteInput{Collection: schema.WebhookDeliveriesCollection, Data: store.Record{
				schema.WebhookDeliveryEvent:    id,
				schema.WebhookDeliveryEndpoint: ep.Name,
				schema.WebhookDeliveryStatus:   schema.WebhookPending,
				schema.WebhookDeliveryAttempts: 0,
				schema.WebhookDeliveryNextAt:   nowUTC().Format(time.RFC3339),
			}}); err != nil {
				// Stop advancing the cursor on a write error so this event is
				// retried next tick rather than skipped.
				s.logWebhook("enqueue failed", ep, err)
				break
			}
		}
		last = id
	}
	if last != cursor {
		if err := s.setWebhookCursor(ctx, ep.Name, last); err != nil {
			s.logWebhook("cursor advance failed", ep, err)
		}
	}
}

// deliverEndpoint attempts the endpoint's due deliveries (pending or failed with a
// past next_attempt_at), oldest schedule first.
func (s *Server) deliverEndpoint(ctx context.Context, client *http.Client, ep *WebhookEndpoint) {
	page, err := s.db.Find(ctx, store.Query{
		Collection: schema.WebhookDeliveriesCollection,
		Filters: []store.Filter{
			{Field: schema.WebhookDeliveryEndpoint, Operator: store.Eq, Value: ep.Name},
			{Field: schema.WebhookDeliveryStatus, Operator: store.In, Value: []any{schema.WebhookPending, schema.WebhookFailed}},
			{Field: schema.WebhookDeliveryNextAt, Operator: store.Lte, Value: nowUTC().Format(time.RFC3339)},
		},
		Sort:      schema.WebhookDeliveryNextAt,
		Limit:     webhookDeliverBatch,
		SkipCount: true,
	})
	if err != nil {
		s.logWebhook("delivery scan failed", ep, err)
		return
	}
	for _, d := range page.Data {
		if ctx.Err() != nil {
			return
		}
		s.attemptDelivery(ctx, client, ep, d)
	}
}

// attemptDelivery POSTs one delivery's event and records the outcome: delivered on
// a 2xx, otherwise a retry with exponential backoff up to the endpoint's
// max attempts, after which it is dead-lettered.
func (s *Server) attemptDelivery(ctx context.Context, client *http.Client, ep *WebhookEndpoint, d store.Record) {
	deliveryID, _ := d["id"].(string)
	eventID, _ := d[schema.WebhookDeliveryEvent].(string)

	event, err := s.db.FindOne(ctx, schema.EventsCollection, eventID)
	if err != nil || event == nil {
		// The event vanished (log swept, etc.) — nothing to deliver. Mark done so
		// it stops being scanned.
		s.finishDelivery(ctx, deliveryID, schema.WebhookDelivered, intOf(d[schema.WebhookDeliveryAttempts]), "event no longer available")
		return
	}
	projectEvent(event)
	body, err := json.Marshal(event)
	if err != nil {
		s.retryOrDie(ctx, ep, d, "marshal event: "+err.Error())
		return
	}

	ts := nowUTC().Unix()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		s.retryOrDie(ctx, ep, d, "build request: "+err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DCMS-Event", stringOf(event["event"]))
	req.Header.Set("X-DCMS-Delivery", eventID) // stable across retries → dedup key
	req.Header.Set("X-DCMS-Timestamp", fmt.Sprintf("%d", ts))
	req.Header.Set("X-DCMS-Signature", signWebhook(ep.Secret, ts, body))

	resp, err := client.Do(req)
	if err != nil {
		s.retryOrDie(ctx, ep, d, "post: "+err.Error())
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.finishDelivery(ctx, deliveryID, schema.WebhookDelivered, intOf(d[schema.WebhookDeliveryAttempts])+1, "")
		return
	}
	s.retryOrDie(ctx, ep, d, fmt.Sprintf("endpoint returned %d", resp.StatusCode))
}

// retryOrDie records a failed attempt: schedule the next retry with backoff, or
// dead-letter once the endpoint's max attempts are exhausted.
func (s *Server) retryOrDie(ctx context.Context, ep *WebhookEndpoint, d store.Record, reason string) {
	deliveryID, _ := d["id"].(string)
	attempts := intOf(d[schema.WebhookDeliveryAttempts]) + 1
	if reason != "" && len(reason) > webhookErrLimit {
		reason = reason[:webhookErrLimit]
	}
	if attempts >= endpointMaxAttempts(ep) {
		s.finishDelivery(ctx, deliveryID, schema.WebhookDead, attempts, reason)
		return
	}
	next := nowUTC().Add(deliveryBackoff(attempts)).Format(time.RFC3339)
	if _, err := s.db.Update(ctx, store.WriteInput{Collection: schema.WebhookDeliveriesCollection, Data: store.Record{
		"id":                            deliveryID,
		schema.WebhookDeliveryStatus:    schema.WebhookFailed,
		schema.WebhookDeliveryAttempts:  attempts,
		schema.WebhookDeliveryLastError: reason,
		schema.WebhookDeliveryNextAt:    next,
	}}); err != nil {
		s.logWebhook("delivery update failed", ep, err)
	}
}

// finishDelivery terminally marks a delivery (delivered or dead).
func (s *Server) finishDelivery(ctx context.Context, deliveryID, status string, attempts int, lastErr string) {
	data := store.Record{
		"id":                           deliveryID,
		schema.WebhookDeliveryStatus:   status,
		schema.WebhookDeliveryAttempts: attempts,
	}
	if lastErr != "" {
		data[schema.WebhookDeliveryLastError] = lastErr
	}
	if status == schema.WebhookDelivered {
		data[schema.WebhookDeliveryDelivered] = nowUTC().Format(time.RFC3339)
	}
	if _, err := s.db.Update(ctx, store.WriteInput{Collection: schema.WebhookDeliveriesCollection, Data: data}); err != nil {
		s.logger.Warn("webhook finish failed", "delivery", deliveryID, "err", err)
	}
}

// deliveryBackoff is exponential (2^(attempts-1) × 10s) capped at webhookMaxBackoff.
func deliveryBackoff(attempts int) time.Duration {
	d := 10 * time.Second
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= webhookMaxBackoff {
			return webhookMaxBackoff
		}
	}
	return d
}

func endpointMaxAttempts(ep *WebhookEndpoint) int {
	if ep.MaxAttempts > 0 {
		return ep.MaxAttempts
	}
	return defaultWebhookMaxAttempts
}

// endpointWants reports whether an event matches an endpoint's type/collection
// filters (empty filter = match all).
func endpointWants(ep *WebhookEndpoint, ev store.Record) bool {
	if len(ep.Events) > 0 && !slices.Contains(ep.Events, stringOf(ev[schema.EventType])) {
		return false
	}
	if len(ep.Collections) > 0 && !slices.Contains(ep.Collections, stringOf(ev[schema.EventCollection])) {
		return false
	}
	return true
}

// signWebhook computes the X-DCMS-Signature value: HMAC-SHA256 over
// "<timestamp>.<rawBody>" so a receiver verifies the exact bytes, and the signed
// timestamp defeats replay.
func signWebhook(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// webhookCursor returns an endpoint's enqueue cursor and whether a state row
// exists yet (false ⇒ never seen, caller initialises it to the current tip).
func (s *Server) webhookCursor(ctx context.Context, endpoint string) (cursor string, initialized bool, err error) {
	page, err := s.db.Find(ctx, store.Query{
		Collection: schema.WebhookStateCollection,
		Filters:    []store.Filter{{Field: schema.WebhookStateEndpoint, Operator: store.Eq, Value: endpoint}},
		Limit:      1,
		SkipCount:  true,
	})
	if err != nil {
		return "", false, err
	}
	if len(page.Data) == 0 {
		return "", false, nil
	}
	c, _ := page.Data[0][schema.WebhookStateCursor].(string)
	return c, true, nil
}

// setWebhookCursor upserts an endpoint's enqueue cursor.
func (s *Server) setWebhookCursor(ctx context.Context, endpoint, cursor string) error {
	page, err := s.db.Find(ctx, store.Query{
		Collection: schema.WebhookStateCollection,
		Filters:    []store.Filter{{Field: schema.WebhookStateEndpoint, Operator: store.Eq, Value: endpoint}},
		Limit:      1,
		SkipCount:  true,
	})
	if err != nil {
		return err
	}
	if len(page.Data) == 0 {
		_, err = s.db.Create(ctx, store.WriteInput{Collection: schema.WebhookStateCollection, Data: store.Record{
			schema.WebhookStateEndpoint: endpoint,
			schema.WebhookStateCursor:   cursor,
		}})
		return err
	}
	id, _ := page.Data[0]["id"].(string)
	_, err = s.db.Update(ctx, store.WriteInput{Collection: schema.WebhookStateCollection, Data: store.Record{
		"id":                      id,
		schema.WebhookStateCursor: cursor,
	}})
	return err
}

// maxEventID returns the id of the newest event, or "" if the log is empty.
func (s *Server) maxEventID(ctx context.Context) (string, error) {
	page, err := s.db.Find(ctx, store.Query{
		Collection: schema.EventsCollection,
		Sort:       "-id",
		Limit:      1,
		SkipCount:  true,
	})
	if err != nil || len(page.Data) == 0 {
		return "", err
	}
	id, _ := page.Data[0]["id"].(string)
	return id, nil
}

func (s *Server) logWebhook(msg string, ep *WebhookEndpoint, err error) {
	if err != nil {
		s.logger.Warn("webhook: "+msg, "endpoint", ep.Name, "err", err)
	}
}

// intOf coerces a store numeric (int64/float64/int) to int.
func intOf(v any) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

// stringOf coerces a value to string, or "" .
func stringOf(v any) string {
	s, _ := v.(string)
	return s
}
