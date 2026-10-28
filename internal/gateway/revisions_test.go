package gateway_test

import (
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

const revisionsSchema = `
version: "1"
collections:
  pages:
    revisions: true
    publishing: true
    soft_delete: true
    fields:
      title: { type: string, required: true }
      body:  { type: text }
`

// revList fetches the version-history list (preview-gated) and returns its rows.
func revList(t *testing.T, url, token string) []any {
	t.Helper()
	_, b := getJSON(t, url, token)
	d, _ := b["data"].([]any)
	return d
}

func TestRevisions_HistoryCapturedAcrossWrites(t *testing.T) {
	def, db := newDB(t, revisionsSchema)
	srv := mount(t, def, db, gateway.Options{PreviewToken: previewTok, ValidateResponses: true})
	api := srv.URL + "/api/v1/pages"

	// Create → version 1.
	_, b := do(t, http.MethodPost, api, `{"title":"Draft","body":"v1"}`)
	id := dataObj(t, b)["id"].(string)

	// Update → version 2.
	if st, _ := do(t, http.MethodPatch, api+"/"+id, `{"body":"v2"}`); st != http.StatusOK {
		t.Fatalf("update: %d", st)
	}
	// Publish → version 3 (transitions are revisioned too).
	if st, _ := do(t, http.MethodPost, api+"/"+id+"/publish", ""); st != http.StatusOK {
		t.Fatalf("publish: %d", st)
	}

	hist := revList(t, api+"/"+id+"/revisions", previewTok)
	if len(hist) != 3 {
		t.Fatalf("expected 3 revisions (create, update, publish), got %d: %#v", len(hist), hist)
	}
	// Newest first; check version + operation labels.
	newest, _ := hist[0].(map[string]any)
	if newest["operation"] != "publish" {
		t.Fatalf("newest revision should be the publish, got %#v", newest["operation"])
	}
	if v, _ := newest["version"].(float64); v != 3 {
		t.Fatalf("newest revision version should be 3, got %v", newest["version"])
	}

	// A specific version carries its decoded snapshot.
	_, gb := getJSON(t, api+"/"+id+"/revisions/1", previewTok)
	snap, _ := dataObj(t, gb)["data"].(map[string]any)
	if snap["body"] != "v1" {
		t.Fatalf("version 1 snapshot should hold body v1, got %#v", snap["body"])
	}
}

// TestRevisions_HiddenStateNotLeakedWithoutToken pins GHSA-8rpx-5wj5-c6jp: on a
// collection with revisions + publishing + soft_delete and a public read rule,
// the full snapshot of a record hidden in its lifecycle state (draft, trashed)
// must not be readable by a caller who lacks the preview token. History visibility
// must match get-one, not fall open just because no preview rule is configured.
func TestRevisions_HiddenStateNotLeakedWithoutToken(t *testing.T) {
	def, db := newDB(t, revisionsSchema)
	srv := mount(t, def, db, gateway.Options{PreviewToken: previewTok, ValidateResponses: true})
	api := srv.URL + "/api/v1/pages"

	// A never-published draft.
	_, b := do(t, http.MethodPost, api, `{"title":"Secret","body":"unpublished body"}`)
	id := dataObj(t, b)["id"].(string)

	// get-one hides the draft from an anonymous caller...
	if st, _ := getJSON(t, api+"/"+id, ""); st != http.StatusNotFound {
		t.Fatalf("anonymous get-one of a draft: got %d, want 404", st)
	}
	// ...and so must its history and snapshots (the leak).
	for _, path := range []string{"/revisions", "/revisions/1"} {
		if st, body := getJSON(t, api+"/"+id+path, ""); st != http.StatusNotFound {
			t.Fatalf("anonymous GET %s of a draft: got %d, want 404 (%v)", path, st, body)
		}
	}
	// The preview token still reaches the draft's history (operator bypass).
	if st, _ := getJSON(t, api+"/"+id+"/revisions/1", previewTok); st != http.StatusOK {
		t.Fatalf("preview-token GET of draft history: got %d, want 200", st)
	}

	// Publish, then soft-delete (trash) it. A trashed record is hidden again, so its
	// history must close back up for an anonymous caller.
	do(t, http.MethodPost, api+"/"+id+"/publish", "")
	if st, _ := getJSON(t, api+"/"+id+"/revisions", ""); st != http.StatusOK {
		t.Fatalf("anonymous history of a published record should be readable: got %d", st)
	}
	if st, _ := do(t, http.MethodDelete, api+"/"+id, ""); st != http.StatusNoContent && st != http.StatusOK {
		t.Fatalf("soft delete: %d", st)
	}
	for _, path := range []string{"/revisions", "/revisions/1"} {
		if st, _ := getJSON(t, api+"/"+id+path, ""); st != http.StatusNotFound {
			t.Fatalf("anonymous GET %s of a trashed record: got %d, want 404", path, st)
		}
	}
}

func TestRevisions_RestoreIsContentOnly(t *testing.T) {
	def, db := newDB(t, revisionsSchema)
	srv := mount(t, def, db, gateway.Options{PreviewToken: previewTok, ValidateResponses: true})
	api := srv.URL + "/api/v1/pages"

	// Create (v1) → publish (v2) → edit while live (v3).
	_, b := do(t, http.MethodPost, api, `{"title":"T","body":"original"}`)
	id := dataObj(t, b)["id"].(string)
	do(t, http.MethodPost, api+"/"+id+"/publish", "")
	do(t, http.MethodPatch, api+"/"+id, `{"body":"edited"}`)

	// Restore version 1's content. It must bring back the old body but leave the
	// record published (managed columns untouched), and record a new revision.
	st, rb := do(t, http.MethodPost, api+"/"+id+"/revisions/1/restore", "")
	if st != http.StatusOK {
		t.Fatalf("restore: %d (%v)", st, rb)
	}
	rec := dataObj(t, rb)
	if rec["body"] != "original" {
		t.Fatalf("restore should bring back the original body, got %#v", rec["body"])
	}
	if rec["_status"] != "published" {
		t.Fatalf("restore must not change publish state, got _status %#v", rec["_status"])
	}
	// The record is still publicly visible (proves lifecycle columns survived).
	if pst, _ := getJSON(t, api+"/"+id, ""); pst != http.StatusOK {
		t.Fatalf("restored-published record should remain public, got %d", pst)
	}

	// The restore itself is versioned (v4), labeled "restore".
	hist := revList(t, api+"/"+id+"/revisions", previewTok)
	if len(hist) != 4 {
		t.Fatalf("expected 4 revisions after restore, got %d", len(hist))
	}
	if newest, _ := hist[0].(map[string]any); newest["operation"] != "restore" {
		t.Fatalf("newest revision should be the restore, got %#v", newest["operation"])
	}
}

func TestRevisions_HistoryIsPreviewGated(t *testing.T) {
	def, db := newDB(t, revisionsSchema)
	srv := mount(t, def, db, gateway.Options{PreviewToken: previewTok})
	api := srv.URL + "/api/v1/pages"

	_, b := do(t, http.MethodPost, api, `{"title":"T"}`)
	id := dataObj(t, b)["id"].(string)

	// Without the preview token, history is not readable (404, not a leak of 403).
	if st, _ := getJSON(t, api+"/"+id+"/revisions", ""); st != http.StatusNotFound {
		t.Fatalf("history without preview token should be 404, got %d", st)
	}
	if st, _ := getJSON(t, api+"/"+id+"/revisions/1", ""); st != http.StatusNotFound {
		t.Fatalf("revision get without preview token should be 404, got %d", st)
	}
	// With the token, it's available.
	if st, _ := getJSON(t, api+"/"+id+"/revisions", previewTok); st != http.StatusOK {
		t.Fatalf("history with preview token should be 200, got %d", st)
	}
}

// A collection without the `revisions` directive has no history endpoints.
func TestRevisions_NotEnabledIs404(t *testing.T) {
	def, db := newDB(t, lifecycleSchema) // articles: publishing+soft_delete, no revisions
	srv := mount(t, def, db, gateway.Options{PreviewToken: previewTok})
	api := srv.URL + "/api/v1/articles"

	_, b := do(t, http.MethodPost, api, `{"title":"X"}`)
	id := dataObj(t, b)["id"].(string)

	if st, _ := getJSON(t, api+"/"+id+"/revisions", previewTok); st != http.StatusNotFound {
		t.Fatalf("revisions on a non-revisioned collection should be 404, got %d", st)
	}
}
