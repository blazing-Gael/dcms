package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blazing-Gael/dcms/internal/engine"
	"github.com/blazing-Gael/dcms/internal/gateway"
)

func TestParseExpiry(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 0, false},
		{"90d", 90 * 24 * time.Hour, false},
		{"720h", 720 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"banana", 0, true},
		{"-5d", 0, true},
		{"0d", 0, true}, // a non-empty zero lifetime is a mistake, not "never"
		{"0s", 0, true},
	}
	for _, c := range cases {
		got, err := parseExpiry(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseExpiry(%q): expected an error", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("parseExpiry(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
}

// The token CLI mints a row and revoke removes it — the create/revoke path the
// gateway resolves against.
func TestTokenCreateListRevoke(t *testing.T) {
	dir := t.TempDir()
	schemaPath := filepath.Join(dir, "dcms.schema.yaml")
	if err := os.WriteFile(schemaPath, []byte(`version: "1"
collections:
  posts:
    fields:
      title: { type: string, required: true }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "dcms.db")
	args := []string{"--schema", schemaPath, "--db", dbPath}

	create := newTokenCreateCmd()
	create.SetArgs(append([]string{"--name", "ci", "--role", "reader"}, args...))
	create.SetOut(os.NewFile(0, os.DevNull))
	if err := create.Execute(); err != nil {
		t.Fatalf("token create: %v", err)
	}

	// The row exists and carries the name.
	db, err := engine.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	toks, err := gateway.ListAPITokens(ctx, db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(toks) != 1 || toks[0]["name"] != "ci" {
		t.Fatalf("expected one token named ci, got %v", toks)
	}
	// The hash is never surfaced by the listing.
	if _, leaked := toks[0]["token_hash"]; leaked {
		t.Fatalf("token_hash leaked in listing: %v", toks[0])
	}

	// Revoke removes it.
	id, _ := toks[0]["id"].(string)
	if err := gateway.RevokeAPIToken(ctx, db, id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	toks, _ = gateway.ListAPITokens(ctx, db)
	if len(toks) != 0 {
		t.Fatalf("expected no tokens after revoke, got %d", len(toks))
	}
}
