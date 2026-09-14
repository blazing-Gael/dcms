package gateway

import (
	"testing"
	"time"
)

// datetimeString must normalize both representations the store contract allows —
// a string (SQLite) and a time.Time (e.g. Postgres) — so token expiry and the
// last_used_at throttle work regardless of adapter. Without the time.Time case a
// non-null datetime would read as "" and be treated as absent.
func TestDatetimeString(t *testing.T) {
	if got := datetimeString("2026-01-02T03:04:05Z"); got != "2026-01-02T03:04:05Z" {
		t.Errorf("string passthrough = %q", got)
	}
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if got := datetimeString(ts); got != "2026-01-02T03:04:05Z" {
		t.Errorf("time.Time = %q, want RFC3339", got)
	}
	if got := datetimeString(nil); got != "" {
		t.Errorf("nil = %q, want empty", got)
	}
}
