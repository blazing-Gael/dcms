package gateway_test

import (
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// A page has a fixed set of slots, each a short list of small records — the
// art-directed-marketing-page use case from issue #6.
const objectListSchema = `
version: "1"
collections:
  pages:
    fields:
      key: { type: string, required: true, unique: true }
      capabilities:
        type: object_list
        max: 3
        of:
          title: { type: string, required: true }
          body:  { type: text }
          image: { type: file }
`

func TestObjectList_RoundTrip(t *testing.T) {
	def, db := newDB(t, objectListSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1/pages"

	body := `{"key":"home","capabilities":[
		{"_key":"a","title":"Fast","body":"Loads instantly"},
		{"_key":"b","title":"Offline"}
	]}`
	st, resp := do(t, http.MethodPost, base, body)
	if st != http.StatusCreated {
		t.Fatalf("create: %d %v", st, resp)
	}
	rec := dataObj(t, resp)

	// The list comes back as a real JSON array of objects, not an escaped string.
	caps, ok := rec["capabilities"].([]any)
	if !ok {
		t.Fatalf("capabilities should be a JSON array, got %#v (%T)", rec["capabilities"], rec["capabilities"])
	}
	if len(caps) != 2 {
		t.Fatalf("got %d elements, want 2", len(caps))
	}
	first, _ := caps[0].(map[string]any)
	if first["title"] != "Fast" || first["_key"] != "a" || first["body"] != "Loads instantly" {
		t.Fatalf("first element round-tripped wrong: %#v", first)
	}

	// And it survives a re-read (decoded from the JSON column on GET).
	id, _ := rec["id"].(string)
	_, got := do(t, http.MethodGet, base+"/"+id, "")
	if caps2, ok := dataObj(t, got)["capabilities"].([]any); !ok || len(caps2) != 2 {
		t.Fatalf("re-read capabilities wrong: %#v", dataObj(t, got)["capabilities"])
	}
}

func TestObjectList_RejectsBadElement(t *testing.T) {
	def, db := newDB(t, objectListSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1/pages"

	// An element missing its required inner field → 422, not a silently-stored blob.
	st, body := do(t, http.MethodPost, base, `{"key":"home","capabilities":[{"body":"no title"}]}`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("bad element should be 422, got %d %v", st, body)
	}
}

func TestObjectList_InnerFileMustExist(t *testing.T) {
	def, db := newDB(t, objectListSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1/pages"

	// A dangling media id inside an element is caught by the batched reference
	// check, exactly as a top-level or richtext reference would be.
	st, body := do(t, http.MethodPost, base,
		`{"key":"home","capabilities":[{"title":"Card","image":"11111111-1111-7111-8111-111111111111"}]}`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("dangling inner file should be 422, got %d %v", st, body)
	}
}
