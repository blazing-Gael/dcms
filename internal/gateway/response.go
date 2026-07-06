package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/blazing-Gael/dcms/internal/store"
)

// apiError is the body of an error response.
type apiError struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// refManifest is the deduplicated set of entities resolved by ?expand= on a
// richtext field (ADR-0015), keyed "<collection>:<id>". It is returned at the
// response root as `included` so the document AST can stay pure (id-only) — which
// keeps documents cacheable, dedupes shared references across a list, and makes
// reference cycles harmless.
type refManifest map[string]store.Record

func manifestKey(collection, id string) string { return collection + ":" + id }

// add records an entity in the manifest under its collection:id key (no-op for a
// record without a string id).
func (m refManifest) add(collection string, rec store.Record) {
	if id, ok := rec["id"].(string); ok && id != "" {
		m[manifestKey(collection, id)] = rec
	}
}

// writeData writes a single-record success envelope: {"data": ..., "meta": {}}.
// The nil request means no conditional-GET handling (used by non-read callers).
func writeData(w http.ResponseWriter, status int, data any) {
	writeDataWith(w, nil, status, data, nil)
}

// writeDataWith writes a single-record envelope, adding the `included` reference
// manifest when non-empty (ADR-0015). When r is a GET it also emits an ETag and
// honors If-None-Match (304).
func writeDataWith(w http.ResponseWriter, r *http.Request, status int, data any, included refManifest) {
	env := map[string]any{"data": data, "meta": map[string]any{}}
	if len(included) > 0 {
		env["included"] = included
	}
	writeEnvelope(w, r, status, env)
}

// writeList writes a list success envelope with pagination metadata.
func writeList(w http.ResponseWriter, page store.Page, limit int) {
	writeListWith(w, nil, page, limit, nil)
}

// writeListWith writes a list envelope, adding the `included` reference manifest
// when non-empty (ADR-0015) and, for a GET, an ETag with If-None-Match handling.
func writeListWith(w http.ResponseWriter, r *http.Request, page store.Page, limit int, included refManifest) {
	data := page.Data
	if data == nil {
		data = []store.Record{} // encode an empty list as [], never null
	}
	meta := map[string]any{
		"limit":       limit,
		"next_cursor": page.NextCursor,
	}
	// A negative total means the count was skipped (?count=false) — omit it rather
	// than report a bogus number.
	if page.Total >= 0 {
		meta["total"] = page.Total
	}
	env := map[string]any{"data": data, "meta": meta}
	if len(included) > 0 {
		env["included"] = included
	}
	writeEnvelope(w, r, http.StatusOK, env)
}

// writeEnvelope marshals a success envelope. For a safe GET it derives a strong
// ETag from the response bytes and returns 304 when the client's If-None-Match
// matches — cheap conditional reads that let a cache/browser skip an unchanged
// body. Other methods (and the nil-request callers) just write.
func writeEnvelope(w http.ResponseWriter, r *http.Request, status int, env map[string]any) {
	if r == nil || r.Method != http.MethodGet {
		writeJSON(w, status, env)
		return
	}
	body, err := json.Marshal(env)
	if err != nil {
		writeJSON(w, status, env) // fall back; marshaling env should not fail
		return
	}
	etag := etagOf(body)
	w.Header().Set("ETag", etag)
	if ifNoneMatch(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// etagOf returns a strong ETag (a quoted hash of the body). Content-derived, so it
// changes exactly when the response would.
func etagOf(body []byte) string {
	sum := sha256.Sum256(body)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// ifNoneMatch reports whether a client's If-None-Match header matches etag —
// either "*" or a comma-separated list containing it (weak prefixes tolerated).
func ifNoneMatch(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, tok := range strings.Split(header, ",") {
		tok = strings.TrimSpace(tok)
		tok = strings.TrimPrefix(tok, "W/")
		if tok == etag {
			return true
		}
	}
	return false
}

// writeRecord shapes a single record to the schema's declared types (always)
// and, when strict response validation is enabled, verifies it conforms before
// sending. A validation failure means the server produced non-conforming data,
// so it is logged in full and surfaced as a 500 — never leaked to the client.
func (s *Server) writeRecord(w http.ResponseWriter, r *http.Request, status int, collection string, rec store.Record, expand []string) {
	cd := s.collections[collection]
	cd.CoerceResponse(rec)
	if s.opts.ValidateResponses {
		if errs := cd.ValidateResponse(rec); errs != nil {
			s.logger.Error("response contract violation",
				"collection", collection, "id", rec["id"], "fields", errs, "path", r.URL.Path)
			writeError(w, http.StatusInternalServerError, apiError{Code: "INTERNAL", Message: "internal server error"})
			return
		}
	}
	var included refManifest
	if len(expand) > 0 {
		included = refManifest{}
		if err := s.expandRecord(r.Context(), collection, rec, expand, included); err != nil {
			writeStoreError(w, s.logger, r, err)
			return
		}
	}
	writeDataWith(w, r, status, rec, included)
}

// writeRecords does the same for a list response: every record is shaped, and
// (when enabled) validated before any bytes are written.
func (s *Server) writeRecords(w http.ResponseWriter, r *http.Request, collection string, page store.Page, limit int, expand []string) {
	cd := s.collections[collection]
	for _, rec := range page.Data {
		cd.CoerceResponse(rec)
	}
	if s.opts.ValidateResponses {
		for _, rec := range page.Data {
			if errs := cd.ValidateResponse(rec); errs != nil {
				s.logger.Error("response contract violation",
					"collection", collection, "id", rec["id"], "fields", errs, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, apiError{Code: "INTERNAL", Message: "internal server error"})
				return
			}
		}
	}
	var included refManifest
	if len(expand) > 0 {
		included = refManifest{}
		if err := s.expandListRecords(r.Context(), collection, page.Data, expand, included); err != nil {
			writeStoreError(w, s.logger, r, err)
			return
		}
	}
	writeListWith(w, r, page, limit, included)
}

// writeError writes an error envelope: {"error": {...}}.
func writeError(w http.ResponseWriter, status int, e apiError) {
	writeJSON(w, status, map[string]any{"error": e})
}

// writeStoreError maps a store-layer error to the right HTTP status + error code.
// Internal errors are logged in full but never exposed to the client.
func writeStoreError(w http.ResponseWriter, logger *slog.Logger, r *http.Request, err error) {
	var ve *store.ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusUnprocessableEntity, apiError{
			Code: "VALIDATION_ERROR", Message: "validation failed", Fields: ve.Fields,
		})
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, apiError{Code: "NOT_FOUND", Message: "record not found"})
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, apiError{Code: "CONFLICT", Message: "resource already exists"})
	case errors.Is(err, store.ErrInvalidInput):
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: err.Error()})
	case errors.Is(err, store.ErrIntegrity):
		// A referential integrity violation on a write is an invariant breach:
		// the validation layer should have rejected the reference first
		// (ADR-0010). Log it as a real problem; never surface it as a friendly
		// 422, which would hide the bug. (Delete's RESTRICT 409 is handled in the
		// delete handler before reaching here.)
		logger.Error("referential integrity violation on write (validation layer gap?)",
			"err", err, "method", r.Method, "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, apiError{Code: "INTERNAL", Message: "internal server error"})
	default:
		// Never leak internal details. Log the full error, return a generic one.
		logger.Error("internal error", "err", err, "method", r.Method, "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, apiError{Code: "INTERNAL", Message: "internal server error"})
	}
}
