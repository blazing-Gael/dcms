package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// postKey POSTs a JSON body with an optional Idempotency-Key and returns the
// status, the Idempotent-Replay header, and the decoded envelope.
func postKey(t *testing.T, url, body, key string) (int, string, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, resp.Header.Get("Idempotent-Replay"), out
}

func idemServer(t *testing.T) string {
	t.Helper()
	def, db := newDB(t, hardeningSchema)
	srv := mount(t, def, db, gateway.Options{Idempotency: &gateway.IdempotencyOptions{}})
	return srv.URL + "/api/v1/widgets"
}

func widgetCount(t *testing.T, api string) int {
	t.Helper()
	_, body := do(t, http.MethodGet, api, "")
	data, _ := body["data"].([]any)
	return len(data)
}

// A retried POST with the same key returns the original response verbatim,
// flagged as a replay, and creates exactly one record.
func TestIdempotency_ReplaySameKey(t *testing.T) {
	api := idemServer(t)

	st1, replay1, b1 := postKey(t, api, `{"name":"a"}`, "key-1")
	if st1 != http.StatusCreated {
		t.Fatalf("first create should be 201, got %d %v", st1, b1)
	}
	if replay1 != "" {
		t.Fatalf("first create should not be a replay, got header %q", replay1)
	}
	id1, _ := dataObj(t, b1)["id"].(string)

	st2, replay2, b2 := postKey(t, api, `{"name":"a"}`, "key-1")
	if st2 != http.StatusCreated {
		t.Fatalf("replay should return the original 201, got %d %v", st2, b2)
	}
	if replay2 != "true" {
		t.Fatalf("replay should carry Idempotent-Replay: true, got %q", replay2)
	}
	if id2, _ := dataObj(t, b2)["id"].(string); id2 != id1 {
		t.Fatalf("replay should return the original record id %q, got %q", id1, id2)
	}
	if n := widgetCount(t, api); n != 1 {
		t.Fatalf("a retried create must not duplicate: want 1 record, got %d", n)
	}
}

// The same key with a different body is misuse: a 422, and no second record.
func TestIdempotency_SameKeyDifferentBodyIs422(t *testing.T) {
	api := idemServer(t)

	if st, _, _ := postKey(t, api, `{"name":"a"}`, "key-2"); st != http.StatusCreated {
		t.Fatalf("first create should be 201, got %d", st)
	}
	st, _, body := postKey(t, api, `{"name":"DIFFERENT"}`, "key-2")
	if st != http.StatusUnprocessableEntity {
		t.Fatalf("key reuse with a different body should be 422, got %d %v", st, body)
	}
	if e, _ := body["error"].(map[string]any); e == nil || e["code"] != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("expected IDEMPOTENCY_KEY_REUSED, got %v", body)
	}
	if n := widgetCount(t, api); n != 1 {
		t.Fatalf("rejected reuse must not create a record: want 1, got %d", n)
	}
}

// Without a key, behavior is unchanged: two identical POSTs create two records.
func TestIdempotency_NoKeyStillDuplicates(t *testing.T) {
	api := idemServer(t)
	postKey(t, api, `{"name":"a"}`, "")
	postKey(t, api, `{"name":"a"}`, "")
	if n := widgetCount(t, api); n != 2 {
		t.Fatalf("un-keyed identical creates should both persist: want 2, got %d", n)
	}
}

// Distinct keys are independent — each creates its own record.
func TestIdempotency_DifferentKeysAreIndependent(t *testing.T) {
	api := idemServer(t)
	if st, _, _ := postKey(t, api, `{"name":"a"}`, "k-a"); st != http.StatusCreated {
		t.Fatalf("k-a create should be 201, got %d", st)
	}
	if st, _, _ := postKey(t, api, `{"name":"a"}`, "k-b"); st != http.StatusCreated {
		t.Fatalf("k-b create should be 201, got %d", st)
	}
	if n := widgetCount(t, api); n != 2 {
		t.Fatalf("distinct keys should each create: want 2, got %d", n)
	}
}

// Once a recorded key has expired, the same key re-executes rather than
// replaying a stale response. Uses a short real TTL and waits past it — well
// clear of the platform's wall-clock granularity, so the row is unambiguously
// expired by the retry (a sub-tick TTL would race the coarse Windows clock).
func TestIdempotency_ExpiredKeyReExecutes(t *testing.T) {
	def, db := newDB(t, hardeningSchema)
	srv := mount(t, def, db, gateway.Options{Idempotency: &gateway.IdempotencyOptions{TTL: 20 * time.Millisecond}})
	api := srv.URL + "/api/v1/widgets"

	if st, _, _ := postKey(t, api, `{"name":"a"}`, "exp"); st != http.StatusCreated {
		t.Fatalf("first create should be 201, got %d", st)
	}
	time.Sleep(80 * time.Millisecond) // > TTL + wall-clock granularity

	st, replay, _ := postKey(t, api, `{"name":"a"}`, "exp")
	if st != http.StatusCreated {
		t.Fatalf("post-expiry retry should re-execute (201), got %d", st)
	}
	if replay == "true" {
		t.Fatal("post-expiry retry should not be a replay")
	}
	if n := widgetCount(t, api); n != 2 {
		t.Fatalf("post-expiry retry should create a second record: want 2, got %d", n)
	}
}

// When idempotency is not enabled, the header is ignored.
func TestIdempotency_DisabledIgnoresHeader(t *testing.T) {
	def, db := newDB(t, hardeningSchema)
	srv := mount(t, def, db) // no Idempotency option
	api := srv.URL + "/api/v1/widgets"
	postKey(t, api, `{"name":"a"}`, "key-x")
	postKey(t, api, `{"name":"a"}`, "key-x")
	if n := widgetCount(t, api); n != 2 {
		t.Fatalf("with idempotency disabled the header is ignored: want 2, got %d", n)
	}
}
