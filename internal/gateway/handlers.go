package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

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

	q, err := s.parseListQuery(r.URL.Query(), collection)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}

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

	data, err := decodeBody(r)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: err.Error()})
		return
	}

	// Inline related objects → create the whole tree transactionally.
	if s.hasInlineRelations(collection, data) {
		var rec store.Record
		err := s.db.Tx(r.Context(), func(ctx context.Context, tx store.DB) error {
			var e error
			rec, e = s.createRecord(ctx, tx, collection, data, 0)
			return e
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
	if err := s.checkReferences(r.Context(), s.db, collection, data); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}

	rec, err := s.writeWithLinks(r.Context(), collection, data, func(ctx context.Context, db store.DB, base store.Record) (store.Record, error) {
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
	s.writeRecord(w, r, http.StatusOK, collection, rec, parseExpand(r.URL.Query()))
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	collection := chi.URLParam(r, "collection")
	if !s.routableCollection(collection) {
		s.handleNotFound(w, r)
		return
	}

	data, err := decodeBody(r)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: err.Error()})
		return
	}
	// The id comes from the URL, not the body — it is the source of truth.
	data["id"] = chi.URLParam(r, "id")

	// Inline related objects → resolve + update transactionally.
	if s.hasInlineRelations(collection, data) {
		var rec store.Record
		err := s.db.Tx(r.Context(), func(ctx context.Context, tx store.DB) error {
			var e error
			rec, e = s.updateRecord(ctx, tx, collection, data, 0)
			return e
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
	if err := s.checkReferences(r.Context(), s.db, collection, data); err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}

	rec, err := s.writeWithLinks(r.Context(), collection, data, func(ctx context.Context, db store.DB, base store.Record) (store.Record, error) {
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

	if err := s.db.Delete(r.Context(), collection, chi.URLParam(r, "id")); err != nil {
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

// decodeBody decodes a JSON object request body into a store.Record. An empty
// body is treated as an empty object (valid for create-with-defaults).
func decodeBody(r *http.Request) (store.Record, error) {
	data := store.Record{}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&data); err != nil {
		if errors.Is(err, io.EOF) {
			return data, nil // empty body → empty object
		}
		return nil, errors.New("request body must be a JSON object")
	}
	return data, nil
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
