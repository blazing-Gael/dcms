package gateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
	"github.com/blazing-Gael/dcms/internal/schema"
	"github.com/blazing-Gael/dcms/internal/store"
	"github.com/blazing-Gael/dcms/internal/store/sqlite"
)

// An editorial-review workflow: an owner (writer) may move review writing⇄submitted,
// only an editor/admin may approve or request changes (issue #27).
const workflowSchema = `
version: "1"
auth:
  roles:
    admin:  { label: Admin }
    editor: { label: Editor }
    writer: { label: Writer }
collections:
  stories:
    access:
      read:   authenticated
      create: authenticated
      update: authenticated
    fields:
      title: { type: string, required: true }
      review:
        type: enum
        values: [writing, submitted, changes_requested, approved]
        default: writing
        access:
          write:
            rules:
              - who: owner
                from: [writing, changes_requested]
                to:   [writing, submitted]
              - who: [admin, editor]
`

func newWorkflowServer(t *testing.T) (string, store.Adapter) {
	t.Helper()
	def, err := schema.Parse([]byte(workflowSchema))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	db, err := sqlite.New(sqlite.Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, meta := range def.CollectionMetas() {
		plan, err := db.Diff(ctx, meta)
		if err != nil {
			t.Fatalf("Diff: %v", err)
		}
		if err := db.Migrate(ctx, plan); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	srv := httptest.NewServer(gateway.New(def, db, nil, gateway.Options{
		Authenticator: gateway.NewSessionAuthenticator(db),
	}).Handler())
	t.Cleanup(srv.Close)
	return srv.URL, db
}

func TestTransitions_EditorialWorkflow(t *testing.T) {
	base, db := newWorkflowServer(t)
	api := base + "/api/v1/stories"
	seedUser(t, db, "writer@x.com", "correcthorse", "writer")
	seedUser(t, db, "editor@x.com", "correcthorse", "editor")
	wtok := login(t, base, "writer@x.com", "correcthorse")
	etok := login(t, base, "editor@x.com", "correcthorse")

	// Writer creates a story; review defaults to writing.
	st, body := doAs(t, http.MethodPost, api, wtok, `{"title":"Draft"}`)
	if st != http.StatusCreated {
		t.Fatalf("create: %d %v", st, body)
	}
	id := dataObj(t, body)["id"].(string)
	if dataObj(t, body)["review"] != "writing" {
		t.Fatalf("review should default to writing, got %v", dataObj(t, body)["review"])
	}

	// Owner may submit (writing → submitted).
	if st, b := doAs(t, http.MethodPatch, api+"/"+id, wtok, `{"review":"submitted"}`); st != http.StatusOK {
		t.Fatalf("owner submit should be 200, got %d %v", st, b)
	}

	// Owner may NOT approve (submitted → approved): they can write review, but not
	// this transition → a loud 403 naming the field, not a silent drop.
	st, b := doAs(t, http.MethodPatch, api+"/"+id, wtok, `{"review":"approved"}`)
	if st != http.StatusForbidden {
		t.Fatalf("owner approve should be 403, got %d %v", st, b)
	}
	// And it did not change: still submitted.
	_, got := doAs(t, http.MethodGet, api+"/"+id, wtok, "")
	if dataObj(t, got)["review"] != "submitted" {
		t.Fatalf("review should still be submitted after the rejected approve, got %v", dataObj(t, got)["review"])
	}

	// An editor may make any transition — approve it.
	if st, b := doAs(t, http.MethodPatch, api+"/"+id, etok, `{"review":"approved"}`); st != http.StatusOK {
		t.Fatalf("editor approve should be 200, got %d %v", st, b)
	}
}

func TestTransitions_NoOpAndSilentDrop(t *testing.T) {
	base, db := newWorkflowServer(t)
	api := base + "/api/v1/stories"
	seedUser(t, db, "a@x.com", "correcthorse", "writer")
	seedUser(t, db, "b@x.com", "correcthorse", "writer")
	atok := login(t, base, "a@x.com", "correcthorse")
	btok := login(t, base, "b@x.com", "correcthorse")

	_, body := doAs(t, http.MethodPost, api, atok, `{"title":"A"}`)
	id := dataObj(t, body)["id"].(string)

	// Re-sending the same value (a round-tripped record) is a no-op, never a 403.
	if st, _ := doAs(t, http.MethodPatch, api+"/"+id, atok, `{"title":"A2","review":"writing"}`); st != http.StatusOK {
		t.Fatalf("no-op review write should be 200, got %d", st)
	}

	// A non-owner writer (b) has no rule that applies to them for review, so the
	// field is silently dropped — the update succeeds, review is untouched.
	if st, _ := doAs(t, http.MethodPatch, api+"/"+id, btok, `{"review":"submitted"}`); st != http.StatusOK {
		t.Fatalf("non-owner review write should drop silently (200), got %d", st)
	}
	_, got := doAs(t, http.MethodGet, api+"/"+id, atok, "")
	if dataObj(t, got)["review"] != "writing" {
		t.Fatalf("non-owner write should not change review, got %v", dataObj(t, got)["review"])
	}
}
