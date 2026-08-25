package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func readSettings(t *testing.T, root string) Settings {
	t.Helper()
	s, err := Load(root)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	return s
}

func mustInit(t *testing.T, dir string) {
	t.Helper()
	if _, err := Init(dir); err != nil {
		t.Fatal(err)
	}
}

func TestInitFreshDirectory(t *testing.T) {
	dir := t.TempDir()
	already, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if already {
		t.Error("fresh dir reported as already a workspace")
	}
	if readSettings(t, dir).CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}
}

func TestInitCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")
	mustInit(t, dir)
	readSettings(t, dir)
}

func TestInitIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	before := readSettings(t, dir)
	already, err := Init(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !already {
		t.Error("second init not reported as already a workspace")
	}
	after := readSettings(t, dir)
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("re-init rewrote settings.json: %v -> %v", before.CreatedAt, after.CreatedAt)
	}
}

func TestInitNonEmptyNonWorkspaceDirectory(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(existing, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustInit(t, dir)
	readSettings(t, dir)
	data, err := os.ReadFile(existing)
	if err != nil || string(data) != "keep me" {
		t.Errorf("existing file touched: %q, %v", data, err)
	}
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
	if _, err := Init(dir); err == nil {
		t.Fatal("expected error for unwritable target, got nil")
	}
	if _, err := os.Stat(filepath.Join(dir, markerDir)); !os.IsNotExist(err) {
		t.Errorf("half-created marker left behind: %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mustInit(t, dir)
	s := readSettings(t, dir)
	s.UDID = "UDID-1"
	s.WhatsAppNumber = "+15551234567"
	if err := Save(dir, s); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, dir)
	if got.UDID != "UDID-1" || got.WhatsAppNumber != "+15551234567" {
		t.Errorf("round trip mismatch: %+v", got)
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
}

func TestFindFromSubdir(t *testing.T) {
	ws := t.TempDir()
	mustInit(t, ws)
	sub := filepath.Join(ws, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	chdir(t, sub)
	found, err := Find()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(ws)
	got, _ := filepath.EvalSymlinks(found)
	if got != want {
		t.Errorf("found %q, want %q", found, ws)
	}
}

func TestFindOutside(t *testing.T) {
	chdir(t, t.TempDir())
	if _, err := Find(); err == nil {
		t.Error("expected error outside a workspace")
	}
}
