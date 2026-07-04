package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestLocal_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := New(Config{Driver: "local", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	payload := []byte("hello bytes")
	if err := s.Put(ctx, "abcd1234.txt", bytes.NewReader(payload), int64(len(payload)), "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := s.Get(ctx, "abcd1234.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip mismatch: got %q", got)
	}

	if s.URL("abcd1234.txt") != "" {
		t.Errorf("local store should be proxy-served (empty URL)")
	}

	if err := s.Delete(ctx, "abcd1234.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "abcd1234.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: want ErrNotFound, got %v", err)
	}
	// Deleting a missing key is not an error.
	if err := s.Delete(ctx, "abcd1234.txt"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestLocal_RejectsUnsafeKeys(t *testing.T) {
	ctx := context.Background()
	s, _ := New(Config{Driver: "local", Dir: t.TempDir()})
	for _, key := range []string{"../escape", "a/b", "..", ""} {
		if err := s.Put(ctx, key, bytes.NewReader([]byte("x")), 1, ""); err == nil {
			t.Errorf("Put(%q) should be rejected", key)
		}
	}
}

func TestNew_UnknownDriver(t *testing.T) {
	if _, err := New(Config{Driver: "gopher"}); err == nil {
		t.Fatal("unknown driver should error")
	}
}
