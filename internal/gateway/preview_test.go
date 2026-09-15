package gateway_test

import (
	"net/http"
	"testing"
)

// Identity-based preview (ADR-0023, issue #20): who may see and act on hidden
// lifecycle states is decided by the `preview` access rule, per record, not only
// by the shared token.
const previewSchema = `
version: "1"
auth:
  roles:
    admin:  { label: Administrator }
    editor: { label: Editor }
  session:
    ttl: 1h
collections:
  stories:
    publishing: true
    soft_delete: true
    fields:
      title: { type: string, required: true }
    access:
      read:    public
      create:  authenticated
      update:  { any: [admin, owner] }
      delete:  { any: [admin, owner] }
      preview: { any: [admin, editor, owner] }
`

// setup: alice (writer/owner), bob (another writer), ed (editor). alice creates a
// draft story and returns its id + the three tokens.
func previewSetup(t *testing.T) (base string, alice, bob, ed, storyID string) {
	t.Helper()
	srv, db := newServerWith(t, previewSchema)
	seedUser(t, db, "alice@x.com", "pw-alice-123")
	seedUser(t, db, "bob@x.com", "pw-bob-12345")
	seedUser(t, db, "ed@x.com", "pw-ed-123456", "editor")
	base = srv.URL + "/api/v1"
	alice = login(t, srv.URL, "alice@x.com", "pw-alice-123")
	bob = login(t, srv.URL, "bob@x.com", "pw-bob-12345")
	ed = login(t, srv.URL, "ed@x.com", "pw-ed-123456")

	_, body := doAs(t, http.MethodPost, base+"/stories", alice, `{"title":"Draft One"}`)
	storyID = recordID(t, body)
	return
}

func TestPreview_OwnerSeesAndEditsOwnDraft(t *testing.T) {
	base, alice, bob, _, id := previewSetup(t)

	// Owner sees their own draft; a non-owner writer does not.
	if st, _ := doAs(t, http.MethodGet, base+"/stories/"+id, alice, ""); st != http.StatusOK {
		t.Fatalf("owner get own draft: got %d, want 200", st)
	}
	if st, _ := doAs(t, http.MethodGet, base+"/stories/"+id, bob, ""); st != http.StatusNotFound {
		t.Fatalf("non-owner get draft: got %d, want 404", st)
	}
	// Anonymous also 404 (draft, not published).
	if st, _ := do(t, http.MethodGet, base+"/stories/"+id, ""); st != http.StatusNotFound {
		t.Fatalf("anon get draft: got %d, want 404", st)
	}

	// Owner can edit their draft; the write lands.
	if st, _ := doAs(t, http.MethodPatch, base+"/stories/"+id, alice, `{"title":"Draft One v2"}`); st != http.StatusOK {
		t.Fatalf("owner patch draft: got %d, want 200", st)
	}
	// A non-owner's write to the hidden draft is 404 (agrees with get-one), not 403.
	if st, _ := doAs(t, http.MethodPatch, base+"/stories/"+id, bob, `{"title":"hijack"}`); st != http.StatusNotFound {
		t.Fatalf("non-owner patch draft: got %d, want 404", st)
	}
}

func TestPreview_ListDefaultsPublishedAndHonorsStatus(t *testing.T) {
	base, alice, bob, ed, _ := previewSetup(t)

	// Default list is published-only → the draft shows for nobody.
	for name, tok := range map[string]string{"owner": alice, "other": bob, "editor": ed, "anon": ""} {
		var st int
		var body map[string]any
		if tok == "" {
			st, body = do(t, http.MethodGet, base+"/stories", "")
		} else {
			st, body = doAs(t, http.MethodGet, base+"/stories", tok, "")
		}
		if st != http.StatusOK {
			t.Fatalf("%s list: %d", name, st)
		}
		if rows, _ := body["data"].([]any); len(rows) != 0 {
			t.Errorf("%s default list should be published-only (0 rows), got %d", name, len(rows))
		}
	}

	// ?status=draft: the owner sees their own draft; the editor (allow) sees it too;
	// a non-eligible writer sees nothing; anonymous sees nothing.
	if _, b := doAs(t, http.MethodGet, base+"/stories?status=draft", alice, ""); len(b["data"].([]any)) != 1 {
		t.Errorf("owner ?status=draft should see own draft, got %v", b["data"])
	}
	if _, b := doAs(t, http.MethodGet, base+"/stories?status=draft", ed, ""); len(b["data"].([]any)) != 1 {
		t.Errorf("editor ?status=draft should see the draft, got %v", b["data"])
	}
	if _, b := doAs(t, http.MethodGet, base+"/stories?status=draft", bob, ""); len(b["data"].([]any)) != 0 {
		t.Errorf("non-eligible ?status=draft should see nothing, got %v", b["data"])
	}
	if _, b := do(t, http.MethodGet, base+"/stories?status=draft", ""); len(b["data"].([]any)) != 0 {
		t.Errorf("anon ?status=draft should see nothing, got %v", b["data"])
	}
}

func TestPreview_OwnerScopeNarrowsList(t *testing.T) {
	base, alice, bob, ed, _ := previewSetup(t)
	// bob also has a draft.
	doAs(t, http.MethodPost, base+"/stories", bob, `{"title":"Bob Draft"}`)

	// The editor (preview allow) sees BOTH drafts via ?status=draft.
	if _, b := doAs(t, http.MethodGet, base+"/stories?status=draft", ed, ""); len(b["data"].([]any)) != 2 {
		t.Errorf("editor should see both drafts, got %v", b["data"])
	}
	// The owner (preview ownerScope) sees only their OWN draft.
	if _, b := doAs(t, http.MethodGet, base+"/stories?status=draft", alice, ""); len(b["data"].([]any)) != 1 {
		t.Errorf("owner should see only their own draft, got %v", b["data"])
	}
}

func TestPreview_OwnerRestoresOwnTrash(t *testing.T) {
	base, alice, bob, _, id := previewSetup(t)

	// Owner soft-deletes their draft.
	if st, _ := doAs(t, http.MethodDelete, base+"/stories/"+id, alice, ""); st != http.StatusNoContent {
		t.Fatalf("owner delete: got %d, want 204", st)
	}
	// It's gone from the default and draft views.
	if _, b := doAs(t, http.MethodGet, base+"/stories?status=draft", alice, ""); len(b["data"].([]any)) != 0 {
		t.Errorf("trashed draft should not show in ?status=draft, got %v", b["data"])
	}
	// The owner can still see it via include_deleted, and restore it.
	if _, b := doAs(t, http.MethodGet, base+"/stories?include_deleted=only", alice, ""); len(b["data"].([]any)) != 1 {
		t.Errorf("owner should see own trash, got %v", b["data"])
	}
	// A non-owner cannot restore it (404, hidden).
	if st, _ := doAs(t, http.MethodPost, base+"/stories/"+id+"/restore", bob, `{}`); st != http.StatusNotFound {
		t.Fatalf("non-owner restore: got %d, want 404", st)
	}
	// The owner restores their own trashed record.
	if st, _ := doAs(t, http.MethodPost, base+"/stories/"+id+"/restore", alice, `{}`); st != http.StatusOK {
		t.Fatalf("owner restore: got %d, want 200", st)
	}
	if st, _ := doAs(t, http.MethodGet, base+"/stories/"+id, alice, ""); st != http.StatusOK {
		t.Fatalf("restored record should be visible to owner: got %d", st)
	}
}

func TestPreview_PublishedIsPublicAgain(t *testing.T) {
	base, alice, bob, _, id := previewSetup(t)
	// Owner publishes (update rule = admin|owner).
	if st, _ := doAs(t, http.MethodPost, base+"/stories/"+id+"/publish", alice, `{}`); st != http.StatusOK {
		t.Fatalf("owner publish: got %d, want 200", st)
	}
	// Now anyone reads it (read: public), no preview needed.
	if st, _ := do(t, http.MethodGet, base+"/stories/"+id, ""); st != http.StatusOK {
		t.Fatalf("anon get published: got %d, want 200", st)
	}
	if st, _ := doAs(t, http.MethodGet, base+"/stories/"+id, bob, ""); st != http.StatusOK {
		t.Fatalf("other get published: got %d, want 200", st)
	}
}
