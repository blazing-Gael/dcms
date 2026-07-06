package gateway

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Lifecycle transition endpoints (ADR-0012). Each is a POST that flips managed
// columns server-side — clients never set _status/_published_at/_deleted_at
// through normal writes. A transition on a collection that lacks the matching
// directive is 404 (the route doesn't logically exist for it).

// handlePublish sets a record live, optionally at a future instant (scheduling):
// POST /{collection}/{id}/publish  {"at"?: "<RFC3339>"}.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	collection := chi.URLParam(r, "collection")
	if !s.routableCollection(collection) || !s.collections[collection].Publishing {
		s.handleNotFound(w, r)
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: err.Error()})
		return
	}
	at := nowUTC()
	if raw, ok := body["at"]; ok && raw != nil {
		str, ok := raw.(string)
		if !ok {
			writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "validation failed", Fields: map[string]string{"at": "must be an RFC3339 timestamp string"}})
			return
		}
		t, err := time.Parse(time.RFC3339, str)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "validation failed", Fields: map[string]string{"at": "must be an RFC3339 timestamp string"}})
			return
		}
		at = t.UTC()
	}
	s.applyTransition(w, r, collection, "publish", store.Record{
		"id":                        chi.URLParam(r, "id"),
		schema.LifecycleStatus:      schema.StatusPublished,
		schema.LifecyclePublishedAt: at,
	})
}

// handleUnpublish returns a record to draft: POST /{collection}/{id}/unpublish.
func (s *Server) handleUnpublish(w http.ResponseWriter, r *http.Request) {
	collection := chi.URLParam(r, "collection")
	if !s.routableCollection(collection) || !s.collections[collection].Publishing {
		s.handleNotFound(w, r)
		return
	}
	s.applyTransition(w, r, collection, "unpublish", store.Record{
		"id":                        chi.URLParam(r, "id"),
		schema.LifecycleStatus:      schema.StatusDraft,
		schema.LifecyclePublishedAt: nil,
	})
}

// handleArchive retires a record (kept, but hidden from public and draft views):
// POST /{collection}/{id}/archive.
func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	collection := chi.URLParam(r, "collection")
	if !s.routableCollection(collection) || !s.collections[collection].Publishing {
		s.handleNotFound(w, r)
		return
	}
	s.applyTransition(w, r, collection, "archive", store.Record{
		"id":                   chi.URLParam(r, "id"),
		schema.LifecycleStatus: schema.StatusArchived,
	})
}

// handleRestore undeletes a soft-deleted record: POST /{collection}/{id}/restore.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	collection := chi.URLParam(r, "collection")
	if !s.routableCollection(collection) || !s.collections[collection].SoftDelete {
		s.handleNotFound(w, r)
		return
	}
	s.applyTransition(w, r, collection, "restore", store.Record{
		"id":                      chi.URLParam(r, "id"),
		schema.LifecycleDeletedAt: nil,
	})
}

// applyTransition performs the managed Update and writes the resulting record,
// capturing a revision (labeled with operation) on revisioned collections. A
// missing id surfaces as 404 via the store's ErrNotFound.
func (s *Server) applyTransition(w http.ResponseWriter, r *http.Request, collection, operation string, data store.Record) {
	rec, err := s.updateAndRevise(r.Context(), collection, data, operation)
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	s.writeRecord(w, r, http.StatusOK, collection, rec, nil)
}
