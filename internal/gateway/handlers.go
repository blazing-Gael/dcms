package gateway

import (
	"context"
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

	// Authorize the read (ADR-0016). A role/public/authenticated rule gates the
	// whole endpoint; an `owner` rule narrows the query to the caller's own rows.
	ownerFilters, ok := s.listReadFilters(w, r, collection)
	if !ok {
		return
	}

	q, err := s.parseListQuery(r.URL.Query(), collection)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	q.Filters = append(q.Filters, ownerFilters...)
	// Hide non-live / trashed records unless the request's view widens (ADR-0012).
	q.Filters = append(q.Filters, s.lifecycleFilters(collection, visibilityFromContext(r.Context()))...)

	page, err := s.db.Find(r.Context(), q)
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
	stripManagedFields(data) // _status/_published_at/_deleted_at change only via transitions
	// Drop fields the caller may not write (ADR-0016 M2). isCreate: `owner` write
	// rules resolve as authenticated (you become the owner of what you create).
	s.stripUnwritableFields(r.Context(), collection, "", data, true)

	// Inline related objects → create the whole tree transactionally.
	if s.hasInlineRelations(collection, data) {
		var rec store.Record
		err := s.db.Tx(r.Context(), func(ctx context.Context, tx store.DB) error {
			var e error
			if rec, e = s.createRecord(ctx, tx, collection, data, 0); e != nil {
				return e
			}
			if s.revised(collection) {
				return s.captureRevision(ctx, tx, collection, rec, "create")
			}
			return nil
		})
		if err != nil {
			writeStoreError(w, s.logger, r, err)
			return
		}
		s.writeRecord(w, r, http.StatusCreated, collection, rec, nil)
		return
	}

	if errs := s.collections[collection].ValidateCreate(data); errs != nil {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "validation failed", Fields: errs})
		return
	}
	// Decimal strings → int64 minor units before the store write (ADR-0017).
	s.collections[collection].EncodeDecimals(data)
	if err := s.checkReferences(r.Context(), s.db, collection, data); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}

	rec, err := s.writeWithLinks(r.Context(), collection, "create", data, func(ctx context.Context, db store.DB, base store.Record) (store.Record, error) {
		return db.Create(ctx, store.WriteInput{Collection: collection, Data: base})
	})
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

	rec, err := s.db.FindOne(r.Context(), collection, chi.URLParam(r, "id"))
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	// A record hidden by the request's lifecycle view (ADR-0012) or not readable
	// under the access rule (ADR-0016) is 404 — never 403, so neither its existence
	// nor an owner boundary leaks.
	if !s.recordVisible(collection, rec, visibilityFromContext(r.Context())) ||
		!s.recordReadable(r.Context(), collection, rec) {
		writeError(w, http.StatusNotFound, apiError{Code: "NOT_FOUND", Message: "record not found"})
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
	if !s.authorizeRecordWrite(w, r, collection, chi.URLParam(r, "id"), schema.ActionUpdate) {
		return
	}

	data, err := decodeBody(r)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	stripManagedFields(data) // _status/_published_at/_deleted_at change only via transitions
	// The id comes from the URL, not the body — it is the source of truth.
	data["id"] = chi.URLParam(r, "id")
	// Drop fields the caller may not write (ADR-0016 M2). On update an `owner`
	// write rule is checked against the stored record's created_by, loaded lazily.
	s.stripUnwritableFields(r.Context(), collection, chi.URLParam(r, "id"), data, false)

	// Inline related objects → resolve + update transactionally.
	if s.hasInlineRelations(collection, data) {
		var rec store.Record
		err := s.db.Tx(r.Context(), func(ctx context.Context, tx store.DB) error {
			var e error
			if rec, e = s.updateRecord(ctx, tx, collection, data, 0); e != nil {
				return e
			}
			if s.revised(collection) {
				return s.captureRevision(ctx, tx, collection, rec, "update")
			}
			return nil
		})
		if err != nil {
			writeStoreError(w, s.logger, r, err)
			return
		}
		s.writeRecord(w, r, http.StatusOK, collection, rec, nil)
		return
	}

	if errs := s.collections[collection].ValidateUpdate(data); errs != nil {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "validation failed", Fields: errs})
		return
	}
	// Decimal strings → int64 minor units before the store write (ADR-0017).
	s.collections[collection].EncodeDecimals(data)
	if err := s.checkReferences(r.Context(), s.db, collection, data); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}

	rec, err := s.writeWithLinks(r.Context(), collection, "update", data, func(ctx context.Context, db store.DB, base store.Record) (store.Record, error) {
		return db.Update(ctx, store.WriteInput{Collection: collection, Data: base})
	})
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
	if !s.authorizeRecordWrite(w, r, collection, id, schema.ActionDelete) {
		return
	}

	// Soft-delete collections trash the row (reversible via /restore) unless the
	// caller asks to purge. Purge falls through to the hard delete below and still
	// honors on_delete: restrict (ADR-0012).
	if s.collections[collection].SoftDelete && !strings.EqualFold(r.URL.Query().Get("purge"), "true") {
		_, err := s.updateAndRevise(r.Context(), collection,
			store.Record{"id": id, schema.LifecycleDeletedAt: nowUTC()}, "delete")
		if err != nil {
			writeStoreError(w, s.logger, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := s.db.Delete(r.Context(), collection, id); err != nil {
		// A foreign-key violation on delete is the RESTRICT default (ADR-0010):
		// the record is still referenced. That's a legitimate client-facing
		// conflict, not the invariant breach it would be on a write.
		if errors.Is(err, store.ErrIntegrity) {
			writeError(w, http.StatusConflict, apiError{
				Code: "CONFLICT", Message: "record is still referenced by other records",
			})
			return
		}
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
