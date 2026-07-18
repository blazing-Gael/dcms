package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Revisions (ADR-0013): append-only version history. A revision is captured in
// the same transaction as the write it records (see captureRevision), so history
// can never diverge from the record. History reads are privileged; restore is
// content-only and itself recorded as a new revision.

// revised reports whether a collection opts into version history.
func (s *Server) revised(collection string) bool {
	return s.collections[collection].Revisions
}

// captureRevision inserts a full-snapshot revision row for rec, using the given
// db (which must be the write's transaction so the two commit together). No-op
// for records without an id.
func (s *Server) captureRevision(ctx context.Context, db store.DB, collection string, rec store.Record, operation string) error {
	id, _ := rec["id"].(string)
	if id == "" {
		return nil
	}
	version, err := s.nextRevisionVersion(ctx, db, collection, id)
	if err != nil {
		return err
	}
	snapshot, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = db.Create(ctx, store.WriteInput{
		Collection: schema.RevisionsCollection,
		Data: store.Record{
			schema.RevisionCollection: collection,
			schema.RevisionRecordID:   id,
			schema.RevisionVersion:    version,
			schema.RevisionOperation:  operation,
			schema.RevisionData:       string(snapshot),
		},
	})
	return err
}

// nextRevisionVersion returns the next per-record version number (1 for the
// first). Backed by the (collection, record_id, version) index → one cheap query.
func (s *Server) nextRevisionVersion(ctx context.Context, db store.DB, collection, id string) (int64, error) {
	page, err := db.Find(ctx, store.Query{
		Collection: schema.RevisionsCollection,
		Filters: []store.Filter{
			{Field: schema.RevisionCollection, Operator: store.Eq, Value: collection},
			{Field: schema.RevisionRecordID, Operator: store.Eq, Value: id},
		},
		Sort:  "-" + schema.RevisionVersion,
		Limit: 1,
	})
	if err != nil {
		return 0, err
	}
	if len(page.Data) == 0 {
		return 1, nil
	}
	switch v := page.Data[0][schema.RevisionVersion].(type) {
	case int64:
		return v + 1, nil
	case float64:
		return int64(v) + 1, nil
	default:
		return 1, nil
	}
}

// updateAndRevise performs an Update and, on a revisioned collection, captures the
// resulting record as a revision in the same transaction.
func (s *Server) updateAndRevise(ctx context.Context, collection string, data store.Record, operation string) (store.Record, error) {
	if !s.revised(collection) {
		return s.db.Update(ctx, store.WriteInput{Collection: collection, Data: data})
	}
	var rec store.Record
	err := s.db.Tx(ctx, func(ctx context.Context, tx store.DB) error {
		var e error
		if rec, e = tx.Update(ctx, store.WriteInput{Collection: collection, Data: data}); e != nil {
			return e
		}
		return s.captureRevision(ctx, tx, collection, rec, operation)
	})
	return rec, err
}

// ── History endpoints ────────────────────────────────────────────────────────

// previewDenied reports whether a privileged (history) read should be refused:
// when a preview token is configured but this request didn't present it. With no
// token configured, history is open — consistent with the pre-auth posture of the
// rest of the API (auth will gate this by role later).
func (s *Server) previewDenied(r *http.Request) bool {
	return s.opts.PreviewToken != "" && !visibilityFromContext(r.Context()).preview
}

// handleRevisionList returns a record's version history, newest first, without the
// heavy snapshot blobs.
func (s *Server) handleRevisionList(w http.ResponseWriter, r *http.Request) {
	collection := chi.URLParam(r, "collection")
	if !s.routableCollection(collection) || !s.revised(collection) {
		s.handleNotFound(w, r)
		return
	}
	if s.previewDenied(r) || !s.authorizeCollectionRead(r, collection) {
		s.handleNotFound(w, r)
		return
	}
	page, err := s.db.Find(r.Context(), store.Query{
		Collection: schema.RevisionsCollection,
		Filters: []store.Filter{
			{Field: schema.RevisionCollection, Operator: store.Eq, Value: collection},
			{Field: schema.RevisionRecordID, Operator: store.Eq, Value: chi.URLParam(r, "id")},
		},
		Sort:  "-" + schema.RevisionVersion,
		Limit: 100,
	})
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	out := make([]map[string]any, 0, len(page.Data))
	for _, rev := range page.Data {
		out = append(out, revisionMeta(rev))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "meta": map[string]any{"total": page.Total}})
}

// handleRevisionGet returns one version, including its decoded snapshot.
func (s *Server) handleRevisionGet(w http.ResponseWriter, r *http.Request) {
	collection := chi.URLParam(r, "collection")
	if !s.routableCollection(collection) || !s.revised(collection) {
		s.handleNotFound(w, r)
		return
	}
	if s.previewDenied(r) || !s.authorizeCollectionRead(r, collection) {
		s.handleNotFound(w, r)
		return
	}
	rev, err := s.findRevision(r.Context(), collection, chi.URLParam(r, "id"), chi.URLParam(r, "version"))
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	meta := revisionMeta(rev)
	meta["data"] = decodeSnapshot(rev)
	writeData(w, http.StatusOK, meta)
}

// handleRevisionRestore rolls the record back to a version's content. Managed
// columns are not touched (stripManagedFields), so publish state is preserved; the
// restore is captured as a new revision.
func (s *Server) handleRevisionRestore(w http.ResponseWriter, r *http.Request) {
	collection := chi.URLParam(r, "collection")
	if !s.routableCollection(collection) || !s.revised(collection) {
		s.handleNotFound(w, r)
		return
	}
	id := chi.URLParam(r, "id")
	if !s.authorizeRecordWrite(w, r, collection, id, schema.ActionUpdate) {
		return
	}
	rev, err := s.findRevision(r.Context(), collection, id, chi.URLParam(r, "version"))
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	snapshot := decodeSnapshot(rev)
	if snapshot == nil {
		writeError(w, http.StatusUnprocessableEntity, apiError{Code: "VALIDATION_ERROR", Message: "revision has no snapshot to restore"})
		return
	}
	data := store.Record(snapshot)
	stripManagedFields(data) // content-only: leave _status/_published_at/_deleted_at as they are
	data["id"] = id
	rec, err := s.updateAndRevise(r.Context(), collection, data, "restore")
	if err != nil {
		writeStoreError(w, s.logger, r, err)
		return
	}
	s.writeRecord(w, r, http.StatusOK, collection, rec, nil)
}

// findRevision loads one revision row by (collection, record_id, version).
func (s *Server) findRevision(ctx context.Context, collection, id, versionStr string) (store.Record, error) {
	version, err := strconv.ParseInt(versionStr, 10, 64)
	if err != nil {
		return nil, badRequest("version", "must be an integer")
	}
	page, err := s.db.Find(ctx, store.Query{
		Collection: schema.RevisionsCollection,
		Filters: []store.Filter{
			{Field: schema.RevisionCollection, Operator: store.Eq, Value: collection},
			{Field: schema.RevisionRecordID, Operator: store.Eq, Value: id},
			{Field: schema.RevisionVersion, Operator: store.Eq, Value: version},
		},
		Limit: 1,
	})
	if err != nil {
		return nil, err
	}
	if len(page.Data) == 0 {
		return nil, store.ErrNotFound
	}
	return page.Data[0], nil
}

// revisionMeta projects a revision row to its history metadata (no snapshot blob).
func revisionMeta(rev store.Record) map[string]any {
	return map[string]any{
		"version":    rev[schema.RevisionVersion],
		"operation":  rev[schema.RevisionOperation],
		"created_at": rev["created_at"],
		"created_by": rev["created_by"],
	}
}

// decodeSnapshot parses the stored JSON snapshot back into an object. The json
// column may surface as a string or []byte depending on the adapter.
func decodeSnapshot(rev store.Record) map[string]any {
	var raw []byte
	switch v := rev[schema.RevisionData].(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	}
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
