package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func readSettings(t *testing.T, dir string) settings {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".whatskept", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var s settings
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	return s
}

func TestInitFreshDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := initWorkspace(dir); err != nil {
		t.Fatalf("initWorkspace: %v", err)
	}
	s := readSettings(t, dir)
	if s.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}
}

func TestInitCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")
	if err := initWorkspace(dir); err != nil {
		t.Fatalf("initWorkspace: %v", err)
	}
	readSettings(t, dir)
}

func TestInitIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := initWorkspace(dir); err != nil {
		t.Fatalf("first init: %v", err)
	}
	before := readSettings(t, dir)
	if err := initWorkspace(dir); err != nil {
		t.Fatalf("second init: %v", err)
	}
	after := readSettings(t, dir)
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("re-init rewrote settings.json: created_at %v -> %v", before.CreatedAt, after.CreatedAt)
	}
}

func TestInitNonEmptyNonWorkspaceDirectory(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(existing, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := initWorkspace(dir); err != nil {
		t.Fatalf("initWorkspace: %v", err)
	}
	readSettings(t, dir)
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "keep me" {
		t.Errorf("existing file touched: %q, %v", data, err)
	}
}

func TestInitRelativePaths(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}

	if err := initWorkspace("."); err != nil {
		t.Fatalf(`init ".": %v`, err)
	}
	readSettings(t, sub)

	if err := initWorkspace(".."); err != nil {
		t.Fatalf(`init "..": %v`, err)
	}
	readSettings(t, dir)
}

func TestInitUnwritableTarget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, permission checks don't apply")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(parent, 0o755) })

	dir := filepath.Join(parent, "ws")
	if err := initWorkspace(dir); err == nil {
		t.Fatal("expected error for unwritable target, got nil")
	}
	if _, err := os.Stat(filepath.Join(dir, ".whatskept")); !os.IsNotExist(err) {
		t.Errorf("half-created .whatskept left behind: %v", err)
	}
}
