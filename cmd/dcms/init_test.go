package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/blazing-Gael/dcms/internal/schema"
)

// TestInit_ScaffoldsRunnableProject asserts init writes the expected files and
// that the schema it emits actually parses — so a freshly-scaffolded project is
// runnable, not just present on disk.
func TestInit_ScaffoldsRunnableProject(t *testing.T) {
	dir := t.TempDir()
	cmd := newInitCmd()
	cmd.SetArgs([]string{dir})
	cmd.SetOut(os.NewFile(0, os.DevNull)) // silence next-steps output
	if err := cmd.Execute(); err != nil {
		t.Fatalf("init: %v", err)
	}

	for _, name := range []string{"dcms.schema.yaml", "dcms.config.yaml", ".env.example", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to be written: %v", name, err)
		}
	}

	// The scaffolded schema must compile.
	src, err := os.ReadFile(filepath.Join(dir, "dcms.schema.yaml"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if _, err := schema.Parse(src); err != nil {
		t.Fatalf("scaffolded schema does not parse: %v", err)
	}
}

// TestInit_RefusesOverwrite pins that init will not clobber an existing project
// unless --force is set.
func TestInit_RefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dcms.schema.yaml"), []byte("version: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newInitCmd()
	cmd.SetArgs([]string{dir})
	cmd.SetOut(os.NewFile(0, os.DevNull))
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected init to refuse overwriting an existing dcms.schema.yaml")
	}

	// With --force it proceeds.
	forced := newInitCmd()
	forced.SetArgs([]string{dir, "--force"})
	forced.SetOut(os.NewFile(0, os.DevNull))
	if err := forced.Execute(); err != nil {
		t.Fatalf("init --force: %v", err)
	}
}
