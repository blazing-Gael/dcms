package gateway_test

import (
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

const relSchema = `
version: "1"
collections:
  users:
    fields:
      name: { type: string, required: true }
  posts:
    fields:
      title:  { type: string, required: true }
      author: { type: relation, target: users, required: true }
`

// A belongs-to relation is a string FK column: you create the target, then
// reference it by id. This proves the whole path (schema → FK column → CRUD).
func TestRelation_BelongsToRoundTrip(t *testing.T) {
	def, db := newDB(t, relSchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})

	// Create a user to point at.
	st, body := do(t, http.MethodPost, srv.URL+"/api/v1/users", `{"name":"Sam"}`)
	if st != http.StatusCreated {
		t.Fatalf("create user: %d %v", st, body)
	}
	userID := dataObj(t, body)["id"].(string)

	// Create a post referencing that user by id.
	st, body = do(t, http.MethodPost, srv.URL+"/api/v1/posts", `{"title":"Hello","author":"`+userID+`"}`)
	if st != http.StatusCreated {
		t.Fatalf("create post: %d %v", st, body)
	}
	post := dataObj(t, body)
	if post["author"] != userID {
		t.Fatalf("author FK not stored/returned: got %#v, want %q", post["author"], userID)
	}

	// A post missing the required relation is rejected by request validation.
	st, _ = do(t, http.MethodPost, srv.URL+"/api/v1/posts", `{"title":"Orphan"}`)
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("missing required relation: got %d, want 422", st)
	}

	// Filter by the FK id works like any other column.
	st, body = do(t, http.MethodGet, srv.URL+"/api/v1/posts?filter[author]="+userID, "")
	if st != http.StatusOK {
		t.Fatalf("filter by fk: %d %v", st, body)
	}
	if arr, _ := body["data"].([]any); len(arr) != 1 {
		t.Fatalf("filter by author should return 1 post, got %#v", body["data"])
	}
}
