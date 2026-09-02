package gateway_test

import (
	"net/http"
	"testing"

	"github.com/blazing-Gael/dcms/internal/gateway"
)

func corsServer(t *testing.T, opts gateway.CORSOptions) string {
	t.Helper()
	def, db := newDB(t, hardeningSchema)
	srv := mount(t, def, db, gateway.Options{CORS: &opts})
	return srv.URL + "/api/v1/widgets"
}

// A preflight from an allowed origin is answered 204 with the echoed origin and
// the allow-methods/headers, without reaching a handler.
func TestCORS_PreflightAllowedOrigin(t *testing.T) {
	api := corsServer(t, gateway.CORSOptions{AllowedOrigins: []string{"https://blog.example"}})

	req, _ := http.NewRequest(http.MethodOptions, api, nil)
	req.Header.Set("Origin", "https://blog.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight should be 204, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://blog.example" {
		t.Fatalf("allow-origin = %q, want the echoed origin", got)
	}
	if resp.Header.Get("Access-Control-Allow-Methods") == "" {
		t.Fatal("preflight should advertise allowed methods")
	}
	if resp.Header.Get("Access-Control-Allow-Headers") == "" {
		t.Fatal("preflight should advertise allowed headers")
	}
}

// An actual request from an allowed origin carries the allow-origin and
// expose-headers so the browser hands the response to the page.
func TestCORS_ActualRequestAllowed(t *testing.T) {
	api := corsServer(t, gateway.CORSOptions{AllowedOrigins: []string{"https://blog.example"}})

	req, _ := http.NewRequest(http.MethodGet, api, nil)
	req.Header.Set("Origin", "https://blog.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET should be 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://blog.example" {
		t.Fatalf("allow-origin = %q, want the echoed origin", got)
	}
	if resp.Header.Get("Access-Control-Expose-Headers") == "" {
		t.Fatal("should expose response headers to the browser")
	}
}

// A disallowed origin gets no CORS headers — the browser then blocks the page
// from reading the response.
func TestCORS_DisallowedOriginGetsNoHeaders(t *testing.T) {
	api := corsServer(t, gateway.CORSOptions{AllowedOrigins: []string{"https://blog.example"}})

	req, _ := http.NewRequest(http.MethodGet, api, nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("disallowed origin must not get an allow-origin header, got %q", got)
	}
}

// A wildcard config without credentials answers with "*"; with credentials it
// must echo the specific origin instead (a wildcard is invalid there).
func TestCORS_WildcardVsCredentials(t *testing.T) {
	// Wildcard, no credentials → "*".
	api := corsServer(t, gateway.CORSOptions{AllowedOrigins: []string{"*"}})
	req, _ := http.NewRequest(http.MethodGet, api, nil)
	req.Header.Set("Origin", "https://anything.example")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("wildcard without credentials should be *, got %q", got)
	}

	// Wildcard + credentials → echo the origin, set allow-credentials.
	api2 := corsServer(t, gateway.CORSOptions{AllowedOrigins: []string{"*"}, AllowCredentials: true})
	req2, _ := http.NewRequest(http.MethodGet, api2, nil)
	req2.Header.Set("Origin", "https://anything.example")
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "https://anything.example" {
		t.Fatalf("wildcard+credentials must echo the origin, got %q", got)
	}
	if resp2.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatal("credentials mode should set allow-credentials: true")
	}
}
