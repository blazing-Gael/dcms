package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

const (
	idempotencyHeader     = "Idempotency-Key"
	idempotencyReplay     = "Idempotent-Replay"
	maxIdempotencyKeyLen  = 255
	defaultIdempotencyTTL = 24 * time.Hour
)

// errIdemReserveConflict signals that the reserve INSERT lost the UNIQUE(key)
// race to a concurrent request — the caller re-resolves via a fresh lookup.
var errIdemReserveConflict = errors.New("idempotency reserve conflict")

func idempotencyKeyHeader(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get(idempotencyHeader))
}

func (s *Server) idempotencyEnabled() bool { return s.opts.Idempotency != nil }

func (s *Server) idempotencyTTL() time.Duration {
	if o := s.opts.Idempotency; o != nil && o.TTL > 0 {
		return o.TTL
	}
	return defaultIdempotencyTTL
}

// hashIdemKey scopes the client key per principal so one caller's key can never
// read back another's created record.
func hashIdemKey(principalID, rawKey string) string {
	sum := sha256.Sum256([]byte(principalID + "\x00" + rawKey))
	return hex.EncodeToString(sum[:])
}

// fingerprintOf identifies the request payload, so the same key sent with a
// different body is detectable as misuse.
func fingerprintOf(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// createWithIdempotency handles a POST create carrying an Idempotency-Key. It
// replays a recorded response for a repeated key, rejects a key reused with a
// different body, and otherwise runs reserve → create → finalize atomically so a
// concurrent duplicate can never execute twice (ADR-0018). Called from
// handleCreate after authorization; the caller guarantees rawKey != "".
func (s *Server) createWithIdempotency(w http.ResponseWriter, r *http.Request, collection, rawKey string) {
	if len(rawKey) > maxIdempotencyKeyLen {
		writeError(w, http.StatusBadRequest, apiError{Code: "VALIDATION_ERROR", Message: "Idempotency-Key is too long"})
		return
	}

	// Read the raw body once (respecting the body cap) to fingerprint it, then
	// decode it exactly as the plain path would.
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeDecodeError(w, errBodyTooLarge)
			return
		}
		writeError(w, http.StatusBadRequest, apiError{Code: "VALIDATION_ERROR", Message: "could not read request body"})
		return
	}
	data, derr := decodeBytes(raw)
	if derr != nil {
		writeDecodeError(w, derr)
		return
	}
	stripManagedFields(data)
	s.stripUnwritableFields(r.Context(), collection, "", data, true)

	key := hashIdemKey(principalFromContext(r.Context()).ID, rawKey)
	fp := fingerprintOf(r.Method, r.URL.Path, raw)

	// Fast path: a live recorded row short-circuits before any write.
	if rec, ok := s.idempotencyLookup(r.Context(), key); ok {
		s.respondFromIdem(w, rec, fp)
		return
	}

	var cap *responseCapture
	txErr := s.db.Tx(r.Context(), func(ctx context.Context, tx store.DB) error {
		reserved, e := tx.Create(ctx, store.WriteInput{Collection: schema.IdempotencyCollection, Data: store.Record{
			schema.IdempKey:         key,
			schema.IdempFingerprint: fp,
			schema.IdempStatus:      schema.IdempStatusInProgress,
			schema.IdempExpiresAt:   nowUTC().Add(s.idempotencyTTL()),
		}})
		if e != nil {
			if errors.Is(e, store.ErrConflict) {
				return errIdemReserveConflict
			}
			return e
		}
		rec, e := s.createRecord(ctx, tx, collection, data, 0)
		if e != nil {
			return e // rolls back the reserve too → the key stays free to retry
		}
		if e := s.captureWrite(ctx, tx, collection, rec, "create"); e != nil {
			return e
		}
		// Render the real response into a buffer so it can be both stored and sent.
		cap = newResponseCapture()
		s.writeRecord(cap, r, http.StatusCreated, collection, rec, nil)
		_, e = tx.Update(ctx, store.WriteInput{Collection: schema.IdempotencyCollection, Data: store.Record{
			"id":                     reserved["id"],
			schema.IdempStatus:       schema.IdempStatusDone,
			schema.IdempResponseCode: cap.status,
			schema.IdempResponseBody: cap.body.String(),
		}})
		return e
	})

	switch {
	case txErr == nil:
		cap.flushTo(w)
	case errors.Is(txErr, errIdemReserveConflict):
		// A racer won the reserve; re-resolve against the now-committed row.
		if rec, ok := s.idempotencyLookup(r.Context(), key); ok {
			s.respondFromIdem(w, rec, fp)
			return
		}
		writeError(w, http.StatusConflict, apiError{Code: "CONFLICT", Message: "a request with this Idempotency-Key is in progress"})
	default:
		writeStoreError(w, s.logger, r, txErr)
	}
}

// idempotencySweepInterval is how often expired idempotency rows are purged.
const idempotencySweepInterval = time.Hour

// RunMaintenance runs the gateway's periodic background upkeep until ctx is
// cancelled: sweeping expired idempotency keys and expired auth tokens (reset
// links) so rows that are never consumed don't accumulate. Engine.Serve launches
// this; tests that build a gateway directly do not, so no goroutine leaks.
func (s *Server) RunMaintenance(ctx context.Context) {
	ticker := time.NewTicker(idempotencySweepInterval)
	defer ticker.Stop()
	for {
		if s.idempotencyEnabled() {
			s.runSweep(ctx, "idempotency", s.sweepExpiredIdempotency)
		}
		s.runSweep(ctx, "auth-tokens", s.sweepExpiredAuthTokens)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runSweep executes one background delete-sweep, logging its outcome.
func (s *Server) runSweep(ctx context.Context, name string, sweep func(context.Context) (int64, error)) {
	if n, err := sweep(ctx); err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("sweep failed", "sweep", name, "err", err)
		}
	} else if n > 0 {
		s.logger.Debug("sweep", "sweep", name, "deleted", n)
	}
}

// sweepExpiredIdempotency deletes recorded rows past their TTL in one statement.
// The lexicographic order of same-format RFC3339 UTC timestamps matches time
// order, so a string comparison is a correct expiry test.
func (s *Server) sweepExpiredIdempotency(ctx context.Context) (int64, error) {
	return s.db.RawExec(ctx,
		`DELETE FROM `+schema.IdempotencyCollection+` WHERE `+schema.IdempExpiresAt+` < $1`,
		nowUTC().Format(time.RFC3339))
}

// idempotencyLookup returns a live (non-expired) recorded row for key. An expired
// row is reclaimed (deleted) and reported as absent, freeing the unique key.
func (s *Server) idempotencyLookup(ctx context.Context, key string) (store.Record, bool) {
	page, err := s.db.Find(ctx, store.Query{
		Collection: schema.IdempotencyCollection,
		Filters:    []store.Filter{{Field: schema.IdempKey, Operator: store.Eq, Value: key}},
		Limit:      1,
		SkipCount:  true,
	})
	if err != nil || len(page.Data) == 0 {
		return nil, false
	}
	rec := page.Data[0]
	if s.idemExpired(rec) {
		if id, ok := rec["id"].(string); ok {
			_ = s.db.Delete(ctx, schema.IdempotencyCollection, id)
		}
		return nil, false
	}
	return rec, true
}

// respondFromIdem answers from a recorded row: replay on a fingerprint match,
// 422 on a different body under the same key, 409 while another request holds it.
func (s *Server) respondFromIdem(w http.ResponseWriter, rec store.Record, fp string) {
	if status, _ := rec[schema.IdempStatus].(string); status == schema.IdempStatusInProgress {
		writeError(w, http.StatusConflict, apiError{Code: "CONFLICT", Message: "a request with this Idempotency-Key is in progress"})
		return
	}
	if stored, _ := rec[schema.IdempFingerprint].(string); stored != fp {
		writeError(w, http.StatusUnprocessableEntity, apiError{
			Code: "IDEMPOTENCY_KEY_REUSED", Message: "Idempotency-Key was reused with a different request body",
		})
		return
	}
	body, _ := rec[schema.IdempResponseBody].(string)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set(idempotencyReplay, "true")
	w.WriteHeader(idemInt(rec[schema.IdempResponseCode], http.StatusCreated))
	_, _ = w.Write([]byte(body))
}

// idemExpired reports whether a recorded row's TTL has passed.
func (s *Server) idemExpired(rec store.Record) bool {
	return pastRFC3339(rec[schema.IdempExpiresAt])
}

// decodeBytes decodes an already-read JSON object body, mirroring decodeBody: an
// empty body is an empty object.
func decodeBytes(raw []byte) (store.Record, error) {
	data := store.Record{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return data, nil
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, errors.New("request body must be a JSON object")
	}
	return data, nil
}

// idemInt coerces a stored integer (which an adapter may surface as int64, int,
// or float64) back to an int, falling back to def.
func idemInt(v any, def int) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	}
	return def
}

// ── response capture ─────────────────────────────────────────────────────────

// responseCapture is an http.ResponseWriter that buffers status, headers, and
// body so a create's real response can be both persisted for replay and sent to
// the client, without re-deriving the envelope.
type responseCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func newResponseCapture() *responseCapture {
	return &responseCapture{header: make(http.Header), status: http.StatusOK}
}

func (c *responseCapture) Header() http.Header { return c.header }

func (c *responseCapture) WriteHeader(code int) {
	if !c.wrote {
		c.status = code
		c.wrote = true
	}
}

func (c *responseCapture) Write(b []byte) (int, error) {
	c.wrote = true
	return c.body.Write(b)
}

// flushTo replays the captured response onto a real writer.
func (c *responseCapture) flushTo(w http.ResponseWriter) {
	for k, vs := range c.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(c.status)
	_, _ = w.Write(c.body.Bytes())
}
