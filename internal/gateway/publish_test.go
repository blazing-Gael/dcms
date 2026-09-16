package gateway_test

import (
	"net/http"
	"testing"
)

// A `publish` access rule (issue #23) gates publish/unpublish/archive separately
// from update, so an owner who may edit a draft cannot necessarily take it live.
const publishSchema = `
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
    fields:
      title: { type: string, required: true }
    access:
      read:    public
      create:  authenticated
      update:  { any: [admin, editor, owner] }
      publish: [admin, editor]
  notes:
    publishing: true
    fields:
      body: { type: string, required: true }
    access:
      create: authenticated
      update: { any: [admin, owner] }
`

func TestPublishRule_GatesTransitionsSeparatelyFromUpdate(t *testing.T) {
	srv, db := newServerWith(t, publishSchema)
	seedUser(t, db, "alice@x.com", "pw-alice-123")        // writer/owner, no role
	seedUser(t, db, "ed@x.com", "pw-ed-123456", "editor") // editor
	alice := login(t, srv.URL, "alice@x.com", "pw-alice-123")
	ed := login(t, srv.URL, "ed@x.com", "pw-ed-123456")
	base := srv.URL + "/api/v1"

	_, body := doAs(t, http.MethodPost, base+"/stories", alice, `{"title":"Draft"}`)
	id := recordID(t, body)

	// The owner may edit (update rule includes owner)...
	if st, _ := doAs(t, http.MethodPatch, base+"/stories/"+id, alice, `{"title":"Draft v2"}`); st != http.StatusOK {
		t.Fatalf("owner update: got %d, want 200", st)
	}
	// ...but may NOT publish (publish rule is admin|editor only).
	if st, _ := doAs(t, http.MethodPost, base+"/stories/"+id+"/publish", alice, `{}`); st != http.StatusForbidden {
		t.Fatalf("owner publish: got %d, want 403", st)
	}
	// An editor can publish.
	if st, _ := doAs(t, http.MethodPost, base+"/stories/"+id+"/publish", ed, `{}`); st != http.StatusOK {
		t.Fatalf("editor publish: got %d, want 200", st)
	}
	// Archive is also gated by publish — the owner can't.
	if st, _ := doAs(t, http.MethodPost, base+"/stories/"+id+"/archive", alice, `{}`); st != http.StatusForbidden {
		t.Fatalf("owner archive: got %d, want 403", st)
	}
}

// With no `publish` rule declared, transitions fall back to the update rule —
// today's behaviour, so existing schemas are unchanged.
func TestPublishRule_FallsBackToUpdateWhenUnset(t *testing.T) {
	srv, db := newServerWith(t, publishSchema)
	seedUser(t, db, "carol@x.com", "pw-carol-123")
	carol := login(t, srv.URL, "carol@x.com", "pw-carol-123")
	base := srv.URL + "/api/v1"

	// notes has no publish rule; update = admin|owner. Carol owns her note.
	_, body := doAs(t, http.MethodPost, base+"/notes", carol, `{"body":"hi"}`)
	id := recordID(t, body)
	if st, _ := doAs(t, http.MethodPost, base+"/notes/"+id+"/publish", carol, `{}`); st != http.StatusOK {
		t.Fatalf("owner publish with no publish rule (falls back to update): got %d, want 200", st)
	}
}
