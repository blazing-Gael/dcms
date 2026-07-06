package gateway_test

import (
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// authors here opt into publishing so we can prove expansion respects the
// request's lifecycle view: a draft (hidden) reference target must not appear in
// the manifest.
const richtextExpandSchema = `
version: "1"
collections:
  authors:
    publishing: true
    fields:
      name: { type: string, required: true }
  posts:
    fields:
      title: { type: string, required: true }
      body:  { type: richtext, blocks: [reference], marks: [strong, reference] }
`

// included returns the response-root reference manifest (ADR-0015).
func included(body map[string]any) map[string]any {
	m, _ := body["included"].(map[string]any)
	return m
}

// bodyIsPure asserts the richtext AST still holds only ids (no inlined objects) —
// the reference node keeps its ref and gains nothing.
func bodyIsPure(t *testing.T, post map[string]any) {
	t.Helper()
	arr, _ := post["body"].([]any)
	for _, raw := range arr {
		node, _ := raw.(map[string]any)
		if node["_type"] == "reference" {
			if _, leaked := node["_resolved"]; leaked {
				t.Fatal("richtext AST must stay pure — no _resolved on the node")
			}
		}
	}
}

func TestRichText_ExpandResolvesIntoManifest(t *testing.T) {
	def, db := newDB(t, richtextExpandSchema)
	srv := mount(t, def, db, gateway.Options{PreviewToken: previewTok, ValidateResponses: true})
	authors := srv.URL + "/api/v1/authors"
	posts := srv.URL + "/api/v1/posts"

	// A draft author (publishing defaults to draft → hidden from public reads).
	_, ab := do(t, http.MethodPost, authors, `{"name":"Ada"}`)
	authorID := dataObj(t, ab)["id"].(string)

	// A post whose body references that author. The reference exists, so the write
	// is accepted even though the author is a draft.
	body := jsonBody(t, map[string]any{"title": "P", "body": []any{refBlock("authors", authorID)}})
	st, cb := do(t, http.MethodPost, posts, body)
	if st != http.StatusCreated {
		t.Fatalf("create: %d (%v)", st, cb)
	}
	postID := dataObj(t, cb)["id"].(string)

	// Without expand, there is no manifest at all.
	_, gb := do(t, http.MethodGet, posts+"/"+postID, "")
	if included(gb) != nil {
		t.Fatal("no included manifest should be present without ?expand=body")
	}

	// With expand=body but no preview: the author is a draft → hidden → absent from
	// the manifest. The AST stays pure either way.
	_, gb = do(t, http.MethodGet, posts+"/"+postID+"?expand=body", "")
	bodyIsPure(t, dataObj(t, gb))
	if len(included(gb)) != 0 {
		t.Fatalf("a hidden (draft) reference target must not be in the manifest, got %v", included(gb))
	}

	// Publish the author, then expand → it appears in the manifest under its key.
	do(t, http.MethodPost, authors+"/"+authorID+"/publish", "")
	_, gb = do(t, http.MethodGet, posts+"/"+postID+"?expand=body", "")
	bodyIsPure(t, dataObj(t, gb))
	entity, ok := included(gb)["authors:"+authorID].(map[string]any)
	if !ok {
		t.Fatalf("published reference target should be in the manifest, got %v", included(gb))
	}
	if entity["name"] != "Ada" || entity["id"] != authorID {
		t.Fatalf("manifest entity should be the author record, got %#v", entity)
	}
}

func TestRichText_ExpandDedupedAndBatchedOnList(t *testing.T) {
	def, db := newDB(t, richtextExpandSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	authors := srv.URL + "/api/v1/authors"
	posts := srv.URL + "/api/v1/posts"

	_, ab := do(t, http.MethodPost, authors, `{"name":"Grace"}`)
	authorID := dataObj(t, ab)["id"].(string)
	do(t, http.MethodPost, authors+"/"+authorID+"/publish", "")

	// Three posts, each referencing the same (published) author in its body.
	for _, title := range []string{"one", "two", "three"} {
		body := jsonBody(t, map[string]any{"title": title, "body": []any{refBlock("authors", authorID)}})
		if st, b := do(t, http.MethodPost, posts, body); st != http.StatusCreated {
			t.Fatalf("create %s: %d (%v)", title, st, b)
		}
	}

	_, lb := do(t, http.MethodGet, posts+"?expand=body", "")
	list, _ := lb["data"].([]any)
	if len(list) != 3 {
		t.Fatalf("expected 3 posts, got %d", len(list))
	}
	// The shared author appears exactly once in the manifest (deduped), even though
	// all three posts reference it.
	inc := included(lb)
	if len(inc) != 1 {
		t.Fatalf("manifest should hold the one shared author once, got %d entries: %v", len(inc), inc)
	}
	if e, _ := inc["authors:"+authorID].(map[string]any); e["name"] != "Grace" {
		t.Fatalf("manifest should contain Grace, got %v", inc)
	}
}
