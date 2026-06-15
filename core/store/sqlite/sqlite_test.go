package sqlite

import (
	"context"
	"path/filepath"
	"testing"
)

// newTestAdapter opens a fresh in-memory database for a single test.
func newTestAdapter(t *testing.T) *Adapter {
	t.Helper()
	a, err := New(Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a.(*Adapter)
}

func TestNew_InMemory_PingsAndCloses(t *testing.T) {
	a := newTestAdapter(t)

	if err := a.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestNew_FileDB_EnablesWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	a, err := New(Config{Path: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	// WAL mode only applies to file-backed databases. Confirm the pragma stuck.
	var mode string
	if err := a.(*Adapter).db.QueryRow("PRAGMA journal_mode;").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestNew_EmptyPath_Errors(t *testing.T) {
	if _, err := New(Config{Path: ""}); err == nil {
		t.Fatal("New with empty Path: want error, got nil")
	}
}
