package gateway_test

import (
	"net/http"
	"testing"
)

// Revision history now follows the `preview` rule (issue #24), record-level, with
// no shared token in play — a writer sees their own draft's history, an editor
// diffs a submission, a stranger gets 404.
const revisionPreviewSchema = `
version: "1"
auth:
  roles:
    editor: { label: Editor }
  session:
    ttl: 1h
collections:
  stories:
    publishing: true
    revisions: true
    fields:
      title: { type: string, required: true }
    access:
      read:    public
      create:  authenticated
      update:  { any: [editor, owner] }
      preview: { any: [editor, owner] }
`

func TestRevisionHistory_FollowsPreviewRule(t *testing.T) {
	srv, db := newServerWith(t, revisionPreviewSchema)
	seedUser(t, db, "alice@x.com", "pw-alice-123")
	seedUser(t, db, "bob@x.com", "pw-bob-12345")
	seedUser(t, db, "ed@x.com", "pw-ed-123456", "editor")
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-123")
	bob := login(t, srv.URL, "bob@x.com", "pw-bob-12345")
	ed := login(t, srv.URL, "ed@x.com", "pw-ed-123456")
	base := srv.URL + "/api/v1"

	// Alice creates a draft (revision v1) and edits it (v2).
	_, body := doAs(t, http.MethodPost, base+"/stories", alice, `{"title":"Secret v1"}`)
	id := recordID(t, body)
	if st, _ := doAs(t, http.MethodPatch, base+"/stories/"+id, alice, `{"title":"Secret v2"}`); st != http.StatusOK {
		t.Fatalf("update: %d", st)
	}

	// Owner and editor see the draft's history; a stranger and anon get 404.
	for _, tc := range []struct {
		name, tok string
		want      int
	}{
		{"owner", alice, http.StatusOK},
		{"editor", ed, http.StatusOK},
		{"stranger", bob, http.StatusNotFound},
		{"anon", "", http.StatusNotFound},
	} {
		if st, _ := doAs(t, http.MethodGet, base+"/stories/"+id+"/revisions", tc.tok, ""); st != tc.want {
			t.Errorf("%s history list: got %d, want %d", tc.name, st, tc.want)
		}
		if st, _ := doAs(t, http.MethodGet, base+"/stories/"+id+"/revisions/1", tc.tok, ""); st != tc.want {
			t.Errorf("%s history get v1: got %d, want %d", tc.name, st, tc.want)
		}
	}

	// The owner's snapshot round-trips.
	_, gb := doAs(t, http.MethodGet, base+"/stories/"+id+"/revisions/1", alice, "")
	snap, _ := dataObj(t, gb)["data"].(map[string]any)
	if snap["title"] != "Secret v1" {
		t.Fatalf("owner v1 snapshot = %#v, want title Secret v1", snap)
	}

	// A stranger can't restore the hidden record's version either (agrees w/ #20).
	if st, _ := doAs(t, http.MethodPost, base+"/stories/"+id+"/revisions/1/restore", bob, `{}`); st != http.StatusNotFound {
		t.Fatalf("stranger restore hidden record: got %d, want 404", st)
	}
}
