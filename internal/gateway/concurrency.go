package gateway

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Optimistic concurrency (issue #26). A collection with `concurrency: true` carries
// an engine-managed `_version` counter (schema.ConcurrencyVersion). A client reads
// it from a record and passes it back as `If-Match` on a write; if the record has
// moved on since, the write is refused with 412 instead of silently overwriting the
// concurrent edit. The version is read and bumped inside the write transaction, so
// the check-and-increment is atomic under the store's single writer.

// errVersionConflict is returned by a write whose If-Match version no longer
// matches the record. writeStoreError maps it to 412 Precondition Failed.
var errVersionConflict = errors.New("version conflict")

// versioned reports whether a collection opts into optimistic concurrency.
func (s *Server) versioned(collection string) bool {
	return s.collections[collection].Concurrency
}

// parseIfMatch extracts an expected `_version` from the request's If-Match header.
// Returns present=false when the header is absent. The value may be a bare integer
// or an ETag-style quoted one (`"7"`); the wildcard `*` and a non-integer are
// client errors, so a malformed precondition never silently passes.
func parseIfMatch(r *http.Request) (expect int64, present bool, err error) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return 0, false, nil
	}
	if raw == "*" {
		return 0, false, errors.New("If-Match must be a record version, not *")
	}
	raw = strings.TrimPrefix(raw, "W/")
	raw = strings.Trim(raw, `"`)
	v, perr := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if perr != nil {
		return 0, false, errors.New("If-Match must be an integer record version")
	}
	return v, true, nil
}

// writePrecondition resolves the optimistic-concurrency precondition for a write.
// It returns the expected version (nil when no If-Match), having already written
// the error response and returned ok=false when the request is malformed or asks
// for concurrency on a collection that doesn't provide it — so an If-Match against
// a non-versioned collection fails loudly rather than being silently ignored.
func (s *Server) writePrecondition(w http.ResponseWriter, r *http.Request, collection string) (expect *int64, ok bool) {
	v, present, err := parseIfMatch(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, apiError{Code: "BAD_REQUEST", Message: err.Error()})
		return nil, false
	}
	if !present {
		return nil, true
	}
	if !s.versioned(collection) {
		writeError(w, http.StatusBadRequest, apiError{Code: "BAD_REQUEST",
			Message: "If-Match is not supported for this collection (it does not enable `concurrency`)"})
		return nil, false
	}
	return &v, true
}

// applyVersion enforces the If-Match precondition and stamps the next `_version`
// on a versioned update, mutating data in place. It must run inside the write's
// transaction (db is that tx) so the read-check-increment is atomic. A no-op for a
// collection without `concurrency`. Returns errVersionConflict when a stale write
// is refused, or the store's not-found when the record is gone.
func (s *Server) applyVersion(ctx context.Context, db store.DB, collection string, data store.Record, expect *int64) error {
	if !s.versioned(collection) {
		return nil
	}
	id, _ := data["id"].(string)
	prev, err := db.FindOne(ctx, collection, id)
	if err != nil {
		return err
	}
	cur := recordVersion(prev)
	if expect != nil && *expect != cur {
		return errVersionConflict
	}
	data[schema.ConcurrencyVersion] = cur + 1
	return nil
}

// checkVersion verifies the If-Match precondition against the current record
// without bumping it — for a delete, where there is no surviving row to increment.
// A no-op when the collection is not versioned or no If-Match was sent. Must run in
// the delete's transaction (db is that tx).
func (s *Server) checkVersion(ctx context.Context, db store.DB, collection, id string, expect *int64) error {
	if !s.versioned(collection) || expect == nil {
		return nil
	}
	prev, err := db.FindOne(ctx, collection, id)
	if err != nil {
		return err
	}
	if recordVersion(prev) != *expect {
		return errVersionConflict
	}
	return nil
}

// recordVersion reads the current `_version` from a fetched record. A json column
// may surface as int64, float64, or int depending on the adapter.
func recordVersion(rec store.Record) int64 {
	switch v := rec[schema.ConcurrencyVersion].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case int:
		return int64(v)
	default:
		return 0
	}
}
