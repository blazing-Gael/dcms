package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// doIfMatchAs is doIfMatch with a bearer token (for writes that need a principal).
func doIfMatchAs(t *testing.T, method, url, token, ifMatch, body string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode %s %s: %v", method, url, err)
	}
	return resp.StatusCode, out
}

// Advanced optimistic-concurrency coverage (issue #26): the version counter across
// mixed write kinds, the canonical lost-update race, tamper-resistance, and the
// interaction with transitions, revisions, and events on one collection.

const fullStackSchema = `
version: "1"
auth:
  roles:
    admin: { label: Administrator }
  session:
    ttl: 1h
collections:
  articles:
    publishing: true
    soft_delete: true
    revisions: true
    events: true
    concurrency: true
    fields:
      title: { type: string, required: true }
`

// bump asserts a write returns 200/201 and the expected new version.
func expectVersion(t *testing.T, st int, body map[string]any, wantStatus int, wantVersion int64) {
	t.Helper()
	if st != wantStatus {
		t.Fatalf("status = %d, want %d (%v)", st, wantStatus, body)
	}
	if v := versionOf(t, body); v != wantVersion {
		t.Fatalf("version = %d, want %d", v, wantVersion)
	}
}

func TestConcurrency_VersionMonotonicAcrossMixedWrites(t *testing.T) {
	srv, db := newServerWith(t, fullStackSchema)
	seedUser(t, db, "ed@x.com", "pw-editor-123", "admin")
	tok := login(t, srv.URL, "ed@x.com", "pw-editor-123")
	base := srv.URL + "/api/v1"

	iff := func(method, path, ifm, body string) (int, map[string]any) {
		// authenticated variant of doIfMatch (transitions/writes need a principal here)
		return doIfMatchAs(t, method, base+path, tok, ifm, body)
	}

	_, b := doAs(t, http.MethodPost, base+"/articles", tok, `{"title":"v1"}`)
	id := dataObj(t, b)["id"].(string)
	if versionOf(t, b) != 1 {
		t.Fatalf("create version != 1")
	}

	// Each distinct write kind bumps the counter by exactly one, and a stale
	// If-Match at each step is refused.
	st, body := iff(http.MethodPatch, "/articles/"+id, "1", `{"title":"v2"}`)
	expectVersion(t, st, body, http.StatusOK, 2)

	if st, _ := iff(http.MethodPost, "/articles/"+id+"/publish", "1", `{}`); st != http.StatusPreconditionFailed {
		t.Fatalf("stale publish: got %d, want 412", st)
	}
	st, body = iff(http.MethodPost, "/articles/"+id+"/publish", "2", `{}`)
	expectVersion(t, st, body, http.StatusOK, 3)

	st, body = iff(http.MethodPost, "/articles/"+id+"/unpublish", "3", `{}`)
	expectVersion(t, st, body, http.StatusOK, 4)

	st, body = iff(http.MethodPost, "/articles/"+id+"/archive", "4", `{}`)
	expectVersion(t, st, body, http.StatusOK, 5)

	// A revision restore is a write too — it bumps the version (and is guarded by a
	// stale If-Match). The write response carries the fresh record even though it is
	// archived, so we read the new version straight from it.
	if st, _ := iff(http.MethodPost, "/articles/"+id+"/revisions/1/restore", "4", `{}`); st != http.StatusPreconditionFailed {
		t.Fatalf("stale restore: got %d, want 412", st)
	}
	st, rb := iff(http.MethodPost, "/articles/"+id+"/revisions/1/restore", "5", `{}`)
	expectVersion(t, st, rb, http.StatusOK, 6)
}

func TestConcurrency_LostUpdateRace(t *testing.T) {
	def, db := newDB(t, concurrencySchema) // notes: concurrency
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/notes", `{"title":"orig"}`)
	id := dataObj(t, b)["id"].(string)
	// Two clients both read version 1.
	clientA, clientB := int64(1), int64(1)

	// Client A writes first and wins.
	st, ba := doIfMatch(t, http.MethodPatch, base+"/notes/"+id, strconv.FormatInt(clientA, 10), `{"title":"A"}`)
	expectVersion(t, st, ba, http.StatusOK, 2)

	// Client B's write, based on the now-stale version 1, is refused — no silent
	// overwrite of A's change.
	st, _ = doIfMatch(t, http.MethodPatch, base+"/notes/"+id, strconv.FormatInt(clientB, 10), `{"title":"B"}`)
	if st != http.StatusPreconditionFailed {
		t.Fatalf("client B stale write: got %d, want 412", st)
	}
	// A's change stands.
	_, cur := do(t, http.MethodGet, base+"/notes/"+id, "")
	if dataObj(t, cur)["title"] != "A" {
		t.Fatalf("lost update: title = %v, want A", dataObj(t, cur)["title"])
	}
	// Client B re-reads and retries against the fresh version → succeeds.
	clientB = versionOf(t, cur)
	st, bb := doIfMatch(t, http.MethodPatch, base+"/notes/"+id, strconv.FormatInt(clientB, 10), `{"title":"B2"}`)
	expectVersion(t, st, bb, http.StatusOK, 3)
}

func TestConcurrency_ClientCannotForgeVersion(t *testing.T) {
	def, db := newDB(t, concurrencySchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/notes", `{"title":"x","_version":999}`)
	id := dataObj(t, b)["id"].(string)
	if versionOf(t, b) != 1 {
		t.Fatalf("client-supplied _version on create must be ignored; got %d", versionOf(t, b))
	}
	// A client-set _version in the body is stripped; the engine bumps to 2, not 1000.
	st, ub := doIfMatch(t, http.MethodPatch, base+"/notes/"+id, "1", `{"title":"y","_version":1000}`)
	expectVersion(t, st, ub, http.StatusOK, 2)
}

// Concurrency shares the write transaction with revisions and events: a versioned
// write must still capture its snapshot and emit its change event, and the version
// bump must not disturb either. This exercises the full stack on one collection.
func TestConcurrency_CoexistsWithRevisionsAndEvents(t *testing.T) {
	srv, db := newServerWith(t, fullStackSchema)
	seedUser(t, db, "admin@x.com", "pw-admin-1234", "admin")
	tok := login(t, srv.URL, "admin@x.com", "pw-admin-1234")
	base := srv.URL + "/api/v1"

	_, b := doAs(t, http.MethodPost, base+"/articles", tok, `{"title":"v1"}`)
	id := dataObj(t, b)["id"].(string)
	if st, body := doIfMatchAs(t, http.MethodPatch, base+"/articles/"+id, tok, "1", `{"title":"v2"}`); st != http.StatusOK || versionOf(t, body) != 2 {
		t.Fatalf("update: %d v=%d", st, versionOf(t, body))
	}
	if st, body := doIfMatchAs(t, http.MethodPost, base+"/articles/"+id+"/publish", tok, "2", `{}`); st != http.StatusOK || versionOf(t, body) != 3 {
		t.Fatalf("publish: %d v=%d", st, versionOf(t, body))
	}

	// Revisions: the create/update/publish were each snapshotted.
	_, hb := doAs(t, http.MethodGet, base+"/articles/"+id+"/revisions", tok, "")
	if revs, _ := hb["data"].([]any); len(revs) != 3 {
		t.Fatalf("want 3 revisions (create/update/publish), got %d", len(hb["data"].([]any)))
	}

	// Events: the same three writes each emitted a change-feed row, in order.
	evs, _ := changes(t, srv.URL, tok, "")
	var types []string
	for _, e := range evs {
		if e["collection"] == "articles" {
			types = append(types, e["event"].(string))
		}
	}
	if len(types) != 3 || types[0] != "created" || types[1] != "updated" || types[2] != "published" {
		t.Fatalf("event sequence = %v, want [created updated published]", types)
	}
}
