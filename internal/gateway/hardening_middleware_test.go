package gateway_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

// A JSON body over the configured cap is rejected with 413 before it can be
// buffered; a body under the cap is unaffected.
func TestBodyCap_RejectsOversizeWith413(t *testing.T) {
	def, db := newDB(t, hardeningSchema)
	srv := mount(t, def, db, gateway.Options{MaxBodyBytes: 64})
	api := srv.URL + "/api/v1/widgets"

	// Under the cap: normal create succeeds.
	if st, body := do(t, http.MethodPost, api, `{"name":"a"}`); st != http.StatusCreated {
		t.Fatalf("small body should create, got %d %v", st, body)
	}

	// Over the cap: 413, not a 422 or a silent truncation.
	big := `{"name":"` + strings.Repeat("x", 200) + `"}`
	st, body := do(t, http.MethodPost, api, big)
	if st != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body should be 413, got %d %v", st, body)
	}
	if e, _ := body["error"].(map[string]any); e == nil || e["code"] != "PAYLOAD_TOO_LARGE" {
		t.Fatalf("expected PAYLOAD_TOO_LARGE error, got %v", body)
	}
}

// A generous timeout leaves normal requests untouched — the middleware is a
// bound, not a tax on the happy path.
func TestRequestTimeout_GenerousDeadlinePasses(t *testing.T) {
	def, db := newDB(t, hardeningSchema)
	srv := mount(t, def, db, gateway.Options{RequestTimeout: 30 * time.Second})
	api := srv.URL + "/api/v1/widgets"

	if st, body := do(t, http.MethodPost, api, `{"name":"a"}`); st != http.StatusCreated {
		t.Fatalf("normal request under a generous timeout should succeed, got %d %v", st, body)
	}
	if st, _ := do(t, http.MethodGet, api, ""); st != http.StatusOK {
		t.Fatalf("list under a generous timeout should be 200, got %d", st)
	}
}

// The API rate limiter admits the burst then answers 429 with a Retry-After
// header. A tiny burst makes it deterministic within a single test tick.
func TestRateLimit_APIBurstThen429(t *testing.T) {
	def, db := newDB(t, hardeningSchema)
	srv := mount(t, def, db, gateway.Options{
		RateLimit: &gateway.RateLimitOptions{APIPerMinute: 60, APIBurst: 2},
	})
	api := srv.URL + "/api/v1/widgets"

	// Burst of 2 passes (refill is 1/sec, negligible across these instant calls).
	for i := 0; i < 2; i++ {
		if code, _ := rawGet(t, api); code != http.StatusOK {
			t.Fatalf("request %d within burst should be 200, got %d", i+1, code)
		}
	}
	code, retry := rawGet(t, api)
	if code != http.StatusTooManyRequests {
		t.Fatalf("request past burst should be 429, got %d", code)
	}
	if retry == "" {
		t.Fatal("429 should carry a Retry-After header")
	}
}

// rawGet issues a GET and returns the status and Retry-After header, which the
// map-decoding `do` helper can't surface.
func rawGet(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Retry-After")
}
