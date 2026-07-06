package gateway_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// jsonBody marshals a value to a JSON string for the do() helper.
func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(b)
}

const richtextSchema = `
version: "1"
collections:
  authors:
    fields:
      name: { type: string, required: true }
  posts:
    fields:
      title: { type: string, required: true }
      body:  { type: richtext, blocks: [image, reference], marks: [strong, em, link, reference] }
`

// refBlock is a richtext reference node pointing at a record.
func refBlock(collection, id string) map[string]any {
	return map[string]any{"_type": "reference", "collection": collection, "ref": id}
}

func TestRichText_InContentRefValidated(t *testing.T) {
	def, db := newDB(t, richtextSchema)
	srv := mount(t, def, db)
	authors := srv.URL + "/api/v1/authors"
	posts := srv.URL + "/api/v1/posts"

	// A real author to reference from within a post body.
	_, ab := do(t, http.MethodPost, authors, `{"name":"Ada"}`)
	authorID := dataObj(t, ab)["id"].(string)

	// Happy path: body references an existing author → 201.
	okDoc := postWithBody(t, posts, "Good", refBlock("authors", authorID))
	if okDoc != http.StatusCreated {
		t.Fatalf("post referencing a real author should be 201, got %d", okDoc)
	}

	// Dangling in-content reference → 422 naming the body field.
	st, b := postWithBodyResp(t, posts, "Bad", refBlock("authors", "does-not-exist"))
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("post referencing a missing author should be 422, got %d", st)
	}
	if !fieldErrored(b, "body") {
		t.Fatalf("expected a field error on 'body', got %v", b)
	}

	// Reference to a collection that doesn't exist → 422.
	st, b = postWithBodyResp(t, posts, "Bad2", refBlock("ghosts", "x"))
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("reference to an unknown collection should be 422, got %d", st)
	}
	if !fieldErrored(b, "body") {
		t.Fatalf("expected a field error on 'body' for unknown collection, got %v", b)
	}
}

// A richtext body must survive the write→read round-trip as real structure (an
// array of nodes), not an escaped JSON string — proving json-column encode on
// write and decode on read.
func TestRichText_RoundTrip(t *testing.T) {
	def, db := newDB(t, richtextSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	posts := srv.URL + "/api/v1/posts"

	doc := []any{
		map[string]any{
			"_type": "block", "style": "h2",
			"children": []any{map[string]any{"_type": "span", "text": "Hi", "marks": []any{"strong"}}},
		},
	}
	payload := jsonBody(t, map[string]any{"title": "RT", "body": doc})
	st, cb := do(t, http.MethodPost, posts, payload)
	if st != http.StatusCreated {
		t.Fatalf("create: %d (%v)", st, cb)
	}
	id := dataObj(t, cb)["id"].(string)

	_, gb := do(t, http.MethodGet, posts+"/"+id, "")
	body, ok := dataObj(t, gb)["body"].([]any)
	if !ok {
		t.Fatalf("body should read back as an array, got %T: %#v", dataObj(t, gb)["body"], dataObj(t, gb)["body"])
	}
	blk, _ := body[0].(map[string]any)
	if blk["style"] != "h2" {
		t.Fatalf("round-tripped block lost its structure: %#v", blk)
	}
}

// A malformed richtext document is a structural 422 before any reference check.
func TestRichText_StructuralValidation(t *testing.T) {
	def, db := newDB(t, richtextSchema)
	srv := mount(t, def, db)
	posts := srv.URL + "/api/v1/posts"

	// body must be an array of nodes, not a bare string.
	st, _ := do(t, http.MethodPost, posts, `{"title":"X","body":"not a document"}`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("a non-array body should be 422, got %d", st)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func postWithBody(t *testing.T, url, title string, nodes ...map[string]any) int {
	t.Helper()
	st, _ := postWithBodyResp(t, url, title, nodes...)
	return st
}

func postWithBodyResp(t *testing.T, url, title string, nodes ...map[string]any) (int, map[string]any) {
	t.Helper()
	arr := make([]any, len(nodes))
	for i, n := range nodes {
		arr[i] = n
	}
	payload := jsonBody(t, map[string]any{"title": title, "body": arr})
	return do(t, http.MethodPost, url, payload)
}

func fieldErrored(body map[string]any, field string) bool {
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil {
		return false
	}
	fields, _ := errObj["fields"].(map[string]any)
	_, ok := fields[field]
	return ok
}
