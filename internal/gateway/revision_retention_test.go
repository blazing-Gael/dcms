package gateway_test

import (
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

const skipRevSchema = `
version: "1"
collections:
  docs:
    revisions: true
    fields:
      body: { type: string, required: true }
`

// revisionCount returns how many versions a record's history holds.
func revisionCount(t *testing.T, base, id string) int {
	t.Helper()
	_, body := do(t, http.MethodGet, base+"/docs/"+id+"/revisions", "")
	rows, _ := body["data"].([]any)
	return len(rows)
}

func TestRevisions_SkipUnchanged(t *testing.T) {
	def, db := newDB(t, skipRevSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/docs", `{"body":"v1"}`)
	id := dataObj(t, b)["id"].(string)
	if n := revisionCount(t, base, id); n != 1 {
		t.Fatalf("after create: %d revisions, want 1", n)
	}

	// A PATCH that changes nothing (the autosave case) captures no new revision.
	if st, _ := do(t, http.MethodPatch, base+"/docs/"+id, `{"body":"v1"}`); st != http.StatusOK {
		t.Fatalf("no-op patch: %d", st)
	}
	if n := revisionCount(t, base, id); n != 1 {
		t.Fatalf("after identical patch: %d revisions, want still 1", n)
	}

	// A real change captures; a repeat of it does not.
	do(t, http.MethodPatch, base+"/docs/"+id, `{"body":"v2"}`)
	do(t, http.MethodPatch, base+"/docs/"+id, `{"body":"v2"}`)
	if n := revisionCount(t, base, id); n != 2 {
		t.Fatalf("after change + repeat: %d revisions, want 2", n)
	}
}

const retentionRevSchema = `
version: "1"
collections:
  docs:
    revisions: { max: 3 }
    fields:
      body: { type: string, required: true }
`

func TestRevisions_Retention(t *testing.T) {
	def, db := newDB(t, retentionRevSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/docs", `{"body":"v1"}`)
	id := dataObj(t, b)["id"].(string)
	for _, v := range []string{"v2", "v3", "v4", "v5"} { // 5 distinct versions total
		if st, _ := do(t, http.MethodPatch, base+"/docs/"+id, `{"body":"`+v+`"}`); st != http.StatusOK {
			t.Fatalf("patch %s: %d", v, st)
		}
	}

	// History is capped at max=3 — the newest three (versions 3,4,5) survive.
	if n := revisionCount(t, base, id); n != 3 {
		t.Fatalf("retained %d revisions, want 3 (max)", n)
	}
	// The pruned oldest versions are gone.
	if st, _ := do(t, http.MethodGet, base+"/docs/"+id+"/revisions/1", ""); st != http.StatusNotFound {
		t.Fatalf("pruned version 1: got %d, want 404", st)
	}
	// The newest is still fetchable and holds the latest content.
	_, gv := do(t, http.MethodGet, base+"/docs/"+id+"/revisions/5", "")
	snap, _ := dataObj(t, gv)["data"].(map[string]any)
	if snap["body"] != "v5" {
		t.Fatalf("version 5 body = %#v, want v5", snap)
	}
}
