package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// withTimeout must install a future deadline on the request context so a
// ctx-respecting store can abandon an over-budget query. (The end-to-end 504 is
// exercised via writeStoreError below: the SQLite adapter reserves a single
// connection, whose fast queries can outrun an already-expired deadline, so a
// nanosecond integration timeout would be non-deterministic — the deadline being
// present is the property the store relies on.)
func TestWithTimeout_InstallsDeadline(t *testing.T) {
	s := &Server{opts: Options{RequestTimeout: 5 * time.Second}}
	var future bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dl, ok := r.Context().Deadline(); ok {
			future = time.Until(dl) > 0
		}
	})
	s.withTimeout(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if !future {
		t.Fatal("withTimeout should install a future deadline on the request context")
	}
}

// A negative RequestTimeout disables the deadline entirely (an escape hatch for
// deployments that terminate slow requests at a proxy instead).
func TestWithTimeout_NegativeDisables(t *testing.T) {
	s := &Server{opts: Options{RequestTimeout: -1}}
	var hadDeadline bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadDeadline = r.Context().Deadline()
	})
	s.withTimeout(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if hadDeadline {
		t.Fatal("a negative RequestTimeout should disable the per-request deadline")
	}
}

// An abandoned request (context.DeadlineExceeded from the store) maps to 504
// Gateway Timeout with a TIMEOUT code — never a 500 that would read as a bug.
func TestWriteStoreError_DeadlineIs504(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	writeStoreError(rec, slog.Default(), req, context.DeadlineExceeded)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("context.DeadlineExceeded should map to 504, got %d", rec.Code)
	}
}

// maxBodyBytes and requestTimeout fall back to the engine defaults when unset.
func TestHardeningDefaults(t *testing.T) {
	s := &Server{}
	if got := s.maxBodyBytes(); got != defaultMaxBodyBytes {
		t.Fatalf("default body cap: got %d, want %d", got, defaultMaxBodyBytes)
	}
	if got := s.requestTimeout(); got != defaultRequestTimeout {
		t.Fatalf("default timeout: got %v, want %v", got, defaultRequestTimeout)
	}
}
