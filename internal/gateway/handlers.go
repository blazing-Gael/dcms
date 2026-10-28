package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// ── Introspection / probes ──────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// introspectionGate enforces Options.Introspection on the /__schema, /__openapi,
// and /__docs routes and marks them noindex. "public" (default) exposes them;
// "admin" requires an admin principal; "off" returns 404 so the routes are not
// even addressable. The noindex header is always sent, so a search engine never
// indexes the data model even when it is publicly readable.
func (s *Server) introspectionGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Robots-Tag", "noindex, nofollow")
		switch s.opts.Introspection {
		case "off":
			s.handleNotFound(w, r)
			return
		case "admin":
			if _, ok := s.requireAdmin(w, r); !ok {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleSchema(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("ETag", `"`+s.schema.ContractHash()+`"`)
	writeJSON(w, http.StatusOK, s.schema)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("ETag", `"`+s.schema.ContractHash()+`"`)
	writeJSON(w, http.StatusOK, s.schema.OpenAPI())
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, apiError{Code: "NOT_FOUND", Message: "route not found"})
}

// recordNotFound is the 404 for a specific record — used where a record is absent
// or hidden from the caller (never 403, so nothing about it leaks).
func (s *Server) recordNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, apiError{Code: "NOT_FOUND", Message: "record not found"})
}

func (s *Server) handleMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusMethodNotAllowed, apiError{Code: "NOT_FOUND", Message: "method not allowed"})
}

// ── Collection CRUD ──────────────────────────────────────────────────────────

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	collection := chi.URLParam(r, "collection")
	if !s.routableCollection(collection) {
		s.handleNotFound(w, r)
		return
	}

	// Authorize the read (ADR-0016) before parsing, so a denied caller gets 401/403
	// rather than a query error. listCore applies the scope.
	if _, ok := s.listReadFilters(w, r, collection); !ok {
		return
	}

	q, err := s.parseListQuery(r.Context(), r.URL.Query(), collection)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	page, err := s.listCore(r.Context(), q)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	s.writeRecords(w, r, collection, page, effectiveLimit(q.Limit), parseExpand(r.URL.Query()))
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	collection := chi.URLParam(r, "collection")
	if !s.routableCollection(collection) {
		s.handleNotFound(w, r)
		return
	}
	if !s.authorizeCreate(w, r, collection) {
		return
	}

	// A create carrying an Idempotency-Key takes the reserve/replay path so a
	// retried POST can't create a duplicate (ADR-0018).
	if rawKey := idempotencyKeyHeader(r); rawKey != "" && s.idempotencyEnabled() {
		s.createWithIdempotency(w, r, collection, rawKey)
		return
	}

	data, err := decodeBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	rec, err := s.createCore(r.Context(), collection, data)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	s.writeRecord(w, r, http.StatusCreated, collection, rec, nil)
}

func (s *Server) handleGetOne(w http.ResponseWriter, r *http.Request) {
	collection := chi.URLParam(r, "collection")
	if !s.routableCollection(collection) {
		s.handleNotFound(w, r)
		return
	}

	// Hidden or unreadable is a 404 like absent (getCore).
	rec, err := s.getCore(r.Context(), collection, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	s.writeRecord(w, r, http.StatusOK, collection, rec, parseExpand(r.URL.Query()))
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	collection := chi.URLParam(r, "collection")
	if !s.routableCollection(collection) {
		s.handleNotFound(w, r)
		return
	}
	id := chi.URLParam(r, "id")
	// Hidden ⇒ 404 before the rule's 403, so write agrees with get-one (ADR-0023).
	if err := s.authzRecordWrite(r.Context(), collection, id, schema.ActionUpdate); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}

	data, err := decodeBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	// Optimistic concurrency (issue #26): resolve any If-Match precondition.
	expect, ok := s.writePrecondition(w, r, collection)
	if !ok {
		return
	}
	rec, err := s.updateCore(r.Context(), collection, id, data, expect)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	s.writeRecord(w, r, http.StatusOK, collection, rec, nil)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	collection := chi.URLParam(r, "collection")
	if !s.routableCollection(collection) {
		s.handleNotFound(w, r)
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.authzRecordWrite(r.Context(), collection, id, schema.ActionDelete); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	// Optimistic concurrency (issue #26): a stale delete is refused like a stale write.
	expect, ok := s.writePrecondition(w, r, collection)
	if !ok {
		return
	}
	// Soft-delete collections trash the row unless the caller asks to purge; a hard
	// delete of a still-referenced record is a 409 (deleteCore → ErrReferenced).
	purge := strings.EqualFold(r.URL.Query().Get("purge"), "true")
	if err := s.deleteCore(r.Context(), collection, id, expect, purge); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── helpers ──────────────────────────────────────────────────────────────────

// errBodyTooLarge is returned by decodeBody when the request body exceeds the
// configured cap (enforced by the limitBody middleware's MaxBytesReader). It maps
// to a 413, distinct from the 422 a malformed body gets.
var errBodyTooLarge = errors.New("request body too large")

// decodeBody decodes a JSON object request body into a store.Record. An empty
// body is treated as an empty object (valid for create-with-defaults). A body
// over the size cap returns errBodyTooLarge.
func decodeBody(r *http.Request) (store.Record, error) {
	data := store.Record{}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&data); err != nil {
		if errors.Is(err, io.EOF) {
			return data, nil // empty body → empty object
		}
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, errBodyTooLarge
		}
		return nil, errors.New("request body must be a JSON object")
	}
	return data, nil
}

// writeDecodeError maps a decodeBody error to the right status: an over-cap body
// is a 413, anything else a 422.
func writeDecodeError(w http.ResponseWriter, err error) {
	if errors.Is(err, errBodyTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, apiError{Code: "PAYLOAD_TOO_LARGE", Message: "request body too large"})
		return
	}
	writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: err.Error()})
}

// effectiveLimit mirrors the store's default so the list meta reports the limit
// actually applied.
func effectiveLimit(requested int) int {
	if requested <= 0 {
		return 20
	}
	if requested > 100 {
		return 100
	}
	return requested
}
