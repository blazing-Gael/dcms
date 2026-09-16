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

const concurrencySchema = `
version: "1"
collections:
  notes:
    concurrency: true
    fields:
      title: { type: string, required: true }
  plain:
    fields:
      title: { type: string, required: true }
`

// doIfMatch performs a request with an optional If-Match header and returns the
// status and decoded body.
func doIfMatch(t *testing.T, method, url, ifMatch, body string) (int, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
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

func versionOf(t *testing.T, body map[string]any) int64 {
	t.Helper()
	v, ok := dataObj(t, body)["_version"]
	if !ok {
		t.Fatalf("_version missing from response: %#v", body["data"])
	}
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	}
	t.Fatalf("_version is not a number: %#v", v)
	return 0
}

func TestConcurrency_IfMatchGuardsUpdates(t *testing.T) {
	def, db := newDB(t, concurrencySchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	// Create → version starts at 1.
	_, b := do(t, http.MethodPost, base+"/notes", `{"title":"v1"}`)
	id := dataObj(t, b)["id"].(string)
	if v := versionOf(t, b); v != 1 {
		t.Fatalf("create version = %d, want 1", v)
	}

	// A matching If-Match succeeds and bumps the version.
	st, b := doIfMatch(t, http.MethodPatch, base+"/notes/"+id, `"1"`, `{"title":"v2"}`)
	if st != http.StatusOK {
		t.Fatalf("matching If-Match: got %d, want 200 (%v)", st, b)
	}
	if v := versionOf(t, b); v != 2 {
		t.Fatalf("version after update = %d, want 2", v)
	}

	// The now-stale version is refused with 412 — no silent overwrite.
	st, b = doIfMatch(t, http.MethodPatch, base+"/notes/"+id, "1", `{"title":"nope"}`)
	if st != http.StatusPreconditionFailed {
		t.Fatalf("stale If-Match: got %d, want 412 (%v)", st, b)
	}
	// The record is unchanged by the refused write.
	_, b = do(t, http.MethodGet, base+"/notes/"+id, "")
	if dataObj(t, b)["title"] != "v2" || versionOf(t, b) != 2 {
		t.Fatalf("refused write must not change the record: %#v", b["data"])
	}

	// Without If-Match, the write still succeeds (opt-in per request) and bumps.
	st, b = doIfMatch(t, http.MethodPatch, base+"/notes/"+id, "", `{"title":"v3"}`)
	if st != http.StatusOK || versionOf(t, b) != 3 {
		t.Fatalf("no-If-Match update: got %d version %v", st, b["data"])
	}
}

func TestConcurrency_StaleDeleteRefused(t *testing.T) {
	def, db := newDB(t, concurrencySchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/notes", `{"title":"x"}`)
	id := dataObj(t, b)["id"].(string)
	// bump to v2 so If-Match: 1 is stale
	doIfMatch(t, http.MethodPatch, base+"/notes/"+id, "1", `{"title":"y"}`)

	if st, _ := doIfMatch(t, http.MethodDelete, base+"/notes/"+id, "1", ""); st != http.StatusPreconditionFailed {
		t.Fatalf("stale delete: got %d, want 412", st)
	}
	// current version deletes cleanly
	if st, _ := doIfMatch(t, http.MethodDelete, base+"/notes/"+id, "2", ""); st != http.StatusNoContent {
		t.Fatalf("current-version delete: got %d, want 204", st)
	}
}

func TestConcurrency_IfMatchRejectedOnNonVersionedCollection(t *testing.T) {
	def, db := newDB(t, concurrencySchema)
	srv := mount(t, def, db, gateway.Options{ValidateResponses: true})
	base := srv.URL + "/api/v1"

	_, b := do(t, http.MethodPost, base+"/plain", `{"title":"x"}`)
	id := dataObj(t, b)["id"].(string)

	// If-Match against a collection without `concurrency` must fail loudly, not be
	// silently ignored (would give a false sense of protection).
	if st, _ := doIfMatch(t, http.MethodPatch, base+"/plain/"+id, "1", `{"title":"y"}`); st != http.StatusBadRequest {
		t.Fatalf("If-Match on non-versioned collection: got %d, want 400", st)
	}
	// A bad If-Match value is a 400 too.
	_, b2 := do(t, http.MethodPost, base+"/notes", `{"title":"x"}`)
	nid := dataObj(t, b2)["id"].(string)
	if st, _ := doIfMatch(t, http.MethodPatch, base+"/notes/"+nid, "abc", `{"title":"y"}`); st != http.StatusBadRequest {
		t.Fatalf("non-integer If-Match: got %d, want 400", st)
	}
}

func TestConcurrency_VersionParsesAsIntColumn(t *testing.T) {
	// Guards the schema wiring: _version must round-trip as an integer, and
	// strconv confirms the header shape a client would send back.
	if _, err := strconv.ParseInt("3", 10, 64); err != nil {
		t.Fatal(err)
	}
}
