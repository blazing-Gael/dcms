package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Schema != "./dcms.schema.yaml" {
		t.Errorf("schema default = %q", c.Schema)
	}
	if c.Database.Driver != "sqlite" {
		t.Errorf("driver default = %q", c.Database.Driver)
	}
	if c.Server.Port != 3000 {
		t.Errorf("port default = %d", c.Server.Port)
	}
}

func TestLoad_MissingDefaultPathIsNotAnError(t *testing.T) {
	cfg, found, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Error("found = true for a missing file")
	}
	// Absent file → defaults untouched.
	if cfg.Server.Port != 3000 {
		t.Errorf("port = %d, want default 3000", cfg.Server.Port)
	}
}

func TestLoad_PartialFileOnlyOverridesPresentKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dcms.config.yaml")
	// Only sets the port; everything else must keep its default.
	if err := os.WriteFile(path, []byte("server:\n  port: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, found, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !found {
		t.Fatal("found = false for an existing file")
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("port = %d, want 8080 from file", cfg.Server.Port)
	}
	if cfg.Schema != "./dcms.schema.yaml" {
		t.Errorf("schema = %q, want untouched default", cfg.Schema)
	}
	if cfg.Database.Driver != "sqlite" {
		t.Errorf("driver = %q, want untouched default", cfg.Database.Driver)
	}
}

func TestLoad_BadYAMLReportsFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dcms.config.yaml")
	if err := os.WriteFile(path, []byte("server: : :\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, found, err := Load(path)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !found {
		t.Error("found = false even though the file exists")
	}
}

func TestApplyEnv_OverridesAndValidates(t *testing.T) {
	t.Setenv("DCMS_SCHEMA", "/etc/dcms/schema.yaml")
	t.Setenv("DCMS_DB", "/var/lib/dcms/store.db")
	t.Setenv("DCMS_DB_DRIVER", "postgres")
	t.Setenv("DCMS_PORT", "9090")

	cfg := Default()
	if err := cfg.ApplyEnv(); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.Schema != "/etc/dcms/schema.yaml" {
		t.Errorf("schema = %q", cfg.Schema)
	}
	if cfg.Database.Path != "/var/lib/dcms/store.db" {
		t.Errorf("db path = %q", cfg.Database.Path)
	}
	if cfg.Database.Driver != "postgres" {
		t.Errorf("driver = %q", cfg.Database.Driver)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("port = %d", cfg.Server.Port)
	}
}

func TestApplyEnv_UnsetVarsLeaveValuesAlone(t *testing.T) {
	// No DCMS_* vars set here → nothing should change.
	cfg := Default()
	cfg.Server.Port = 1234
	if err := cfg.ApplyEnv(); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.Server.Port != 1234 {
		t.Errorf("port = %d, want 1234 preserved", cfg.Server.Port)
	}
}

func TestApplyEnv_BadPortIsAnError(t *testing.T) {
	t.Setenv("DCMS_PORT", "not-a-number")
	cfg := Default()
	if err := cfg.ApplyEnv(); err == nil {
		t.Fatal("expected error for non-numeric DCMS_PORT")
	}
}
