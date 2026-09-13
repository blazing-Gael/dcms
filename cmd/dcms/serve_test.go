package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serve does not migrate; it treats an unmigrated database as an operator error
// and refuses to start, rather than failing later at query time against a schema
// the tables don't match.
func TestServe_RefusesPendingMigrations(t *testing.T) {
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
	dbPath := filepath.Join(dir, "dcms.db") // opened fresh → no tables → pending

	cmd := newServeCmd()
	cmd.SetArgs([]string{"--schema", schemaPath, "--db", dbPath})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "pending migration") {
		t.Fatalf("serve against an unmigrated database should refuse with a pending-migration error, got %v", err)
	}
}

// Both run commands are registered and distinct.
func TestServeAndDevRegistered(t *testing.T) {
	root := newRootCmd()
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, want := range []string{"dev", "serve"} {
		if !have[want] {
			t.Errorf("root command %q not registered", want)
		}
	}
}
