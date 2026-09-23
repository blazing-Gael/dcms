package gateway

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
)

// Admin panel — 2B revision history (ADR-0035). "Undo" is the single biggest
// confidence-builder for a nervous non-technical client: they can change content
// freely knowing a previous version is one click away. This reuses the same
// preview-gated history reads and content-only restore as the API (ADR-0013/#24).

type adminRevisionsData struct {
	Collection, RecordID string
	Rows                 []adminRevisionRow
	Note                 string
}

type adminRevisionRow struct {
	Version             string
	Operation, When, By string
}

// adminRevisions lists a record's version history, newest first.
func (s *Server) adminRevisions(w http.ResponseWriter, r *http.Request) {
	cd, ok := s.adminCollection(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if !s.revised(cd.Name) {
		s.adminError(w, r, "This collection doesn't keep version history.")
		return
	}
	// History follows the same preview + read gates as the API (#24).
	if s.revisionHistoryDenied(r, cd.Name, id) || !s.authorizeRecordRead(r.Context(), cd.Name, id) {
		s.adminForbidden(w, r)
		return
	}
	page, err := s.db.Find(r.Context(), store.Query{
		Collection: schema.RevisionsCollection,
		Filters: []store.Filter{
			{Field: schema.RevisionCollection, Operator: store.Eq, Value: cd.Name},
			{Field: schema.RevisionRecordID, Operator: store.Eq, Value: id},
		},
		Sort:  "-" + schema.RevisionVersion,
		Limit: adminListLimit,
	})
	if err != nil {
		s.adminError(w, r, "Could not load history.")
		return
	}
	data := adminRevisionsData{Collection: cd.Name, RecordID: id}
	for _, rev := range page.Data {
		data.Rows = append(data.Rows, adminRevisionRow{
			Version:   adminRawString(rev[schema.RevisionVersion]),
			Operation: adminRawString(rev[schema.RevisionOperation]),
			When:      adminDisplay(rev["created_at"]),
			By:        adminDisplay(rev["created_by"]),
		})
	}
	s.renderAdmin(w, r, "revisions", &adminPage{
		Title: "History", Flash: r.URL.Query().Get("flash"), Data: data,
	})
}

// adminRevisionRestore rolls a record back to a version's content. Managed columns
// (status, publish/delete markers, _version) are left untouched, and the restore is
// itself captured as a new revision — identical to the API path.
func (s *Server) adminRevisionRestore(w http.ResponseWriter, r *http.Request) {
	cd, ok := s.adminCollection(w, r)
	if !ok {
		return
	}
	if !s.adminCSRFValid(r) {
		s.adminForbidden(w, r)
		return
	}
	id := chi.URLParam(r, "id")
	if !s.revised(cd.Name) {
		s.adminError(w, r, "This collection doesn't keep version history.")
		return
	}
	if s.writeHidden(r.Context(), cd.Name, id) {
		s.adminError(w, r, "Record not found.")
		return
	}
	if !s.adminCanRecord(r, cd.Name, id, schema.ActionUpdate) {
		s.adminForbidden(w, r)
		return
	}
	rev, err := s.findRevision(r.Context(), cd.Name, id, chi.URLParam(r, "version"))
	if err != nil {
		s.adminError(w, r, "That version was not found.")
		return
	}
	snapshot := decodeSnapshot(rev)
	if snapshot == nil {
		s.adminError(w, r, "That version has no content to restore.")
		return
	}
	data := store.Record(snapshot)
	stripManagedFields(data) // content-only restore
	data["id"] = id
	if _, err := s.updateAndRevise(r.Context(), cd.Name, data, "restore", nil); err != nil {
		s.adminError(w, r, "Could not restore that version.")
		return
	}
	http.Redirect(w, r, adminBasePath+"/c/"+cd.Name+"/"+id+"?flash=Restored+that+version", http.StatusSeeOther)
}
