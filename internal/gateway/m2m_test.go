package gateway_test

import (
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

const m2mSchema = `
version: "1"
collections:
  tags:
    fields:
      name: { type: string, required: true }
  posts:
    fields:
      title: { type: string, required: true }
      tags:  { type: relation, target: tags, many: true }
`

func TestM2M_LinkWriteAndExpandBothWays(t *testing.T) {
	def, db := newDB(t, m2mSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	// Create two tags.
	_, b := do(t, http.MethodPost, base+"/tags", `{"name":"go"}`)
	goID := dataObj(t, b)["id"].(string)
	_, b = do(t, http.MethodPost, base+"/tags", `{"name":"db"}`)
	dbID := dataObj(t, b)["id"].(string)

	// Create a post linked to both tags.
	st, b := do(t, http.MethodPost, base+"/posts", `{"title":"Hello","tags":["`+goID+`","`+dbID+`"]}`)
	if st != http.StatusCreated {
		t.Fatalf("create post with tags: %d %v", st, b)
	}
	post := dataObj(t, b)
	postID := post["id"].(string)
	// The m2m field is not a base column, so it isn't echoed on the plain create.
	if _, present := post["tags"]; present {
		t.Fatalf("m2m field should not appear un-expanded on create: %#v", post)
	}

	// Expand tags on the post → the two tag objects.
	_, b = do(t, http.MethodGet, base+"/posts/"+postID+"?expand=tags", "")
	tags, ok := dataObj(t, b)["tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Fatalf("expanded tags should be 2 objects, got %#v", dataObj(t, b)["tags"])
	}

	// Update the post's tag set to just one tag (replace semantics).
	st, b = do(t, http.MethodPatch, base+"/posts/"+postID, `{"tags":["`+goID+`"]}`)
	if st != http.StatusOK {
		t.Fatalf("patch tags: %d %v", st, b)
	}
	_, b = do(t, http.MethodGet, base+"/posts/"+postID+"?expand=tags", "")
	tags = dataObj(t, b)["tags"].([]any)
	if len(tags) != 1 {
		t.Fatalf("after replace, expected 1 tag, got %d", len(tags))
	}
	if tags[0].(map[string]any)["id"] != goID {
		t.Fatalf("remaining tag should be %q, got %#v", goID, tags[0])
	}

	// Clearing links: set tags to empty.
	st, _ = do(t, http.MethodPatch, base+"/posts/"+postID, `{"tags":[]}`)
	if st != http.StatusOK {
		t.Fatalf("clear tags: %d", st)
	}
	_, b = do(t, http.MethodGet, base+"/posts/"+postID+"?expand=tags", "")
	if len(dataObj(t, b)["tags"].([]any)) != 0 {
		t.Fatalf("tags should be empty after clearing, got %#v", dataObj(t, b)["tags"])
	}

	// A join table is engine-managed and must not be routable.
	if st, _ := do(t, http.MethodGet, base+"/posts_tags", ""); st != http.StatusNotFound {
		t.Fatalf("join table must not be a routable collection, got %d", st)
	}
}

func TestM2M_BadLinkValueRejected(t *testing.T) {
	def, db := newDB(t, m2mSchema)
	srv := mount(t, def, db)
	base := srv.URL + "/api/v1"

	// tags must be a list of ids, not a scalar.
	if st, _ := do(t, http.MethodPost, base+"/posts", `{"title":"x","tags":"go"}`); st != http.StatusUnprocessableEntity {
		t.Fatalf("scalar for m2m: got %d, want 422", st)
	}
	// list must contain strings.
	if st, _ := do(t, http.MethodPost, base+"/posts", `{"title":"x","tags":[1,2]}`); st != http.StatusUnprocessableEntity {
		t.Fatalf("non-string ids: got %d, want 422", st)
	}
}
