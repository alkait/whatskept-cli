package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeUDID = "00008101-000A1B2C3D4E5F9D"

// fakeBackup creates a minimal iOS backup directory: an Info.plist with
// a Target Identifier. Returns its path.
func fakeBackup(t *testing.T, parent, udid string) string {
	t.Helper()
	dir := filepath.Join(parent, "backup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	info := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Target Identifier</key>
	<string>` + udid + `</string>
</dict>
</plist>
`
	if err := os.WriteFile(filepath.Join(dir, "Info.plist"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// chdir switches cwd for the test and restores it on cleanup.
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

// initedWorkspace creates and initializes a workspace dir.
func initedWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := initWorkspace(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadBackupUDID(t *testing.T) {
	backup := fakeBackup(t, t.TempDir(), fakeUDID)
	udid, err := readBackupUDID(backup)
	if err != nil {
		t.Fatal(err)
	}
	if udid != fakeUDID {
		t.Errorf("udid = %q, want %q", udid, fakeUDID)
	}
}

func TestReadBackupUDIDFallsBackToDirName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "11112222-AAAABBBBCCCCDDDD")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Device Name</key><string>x</string></dict></plist>
`
	if err := os.WriteFile(filepath.Join(dir, "Info.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	udid, err := readBackupUDID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if udid != "11112222-AAAABBBBCCCCDDDD" {
		t.Errorf("udid = %q, want dir name", udid)
	}
}

func TestReadBackupUDIDNotABackup(t *testing.T) {
	empty := t.TempDir() // no Info.plist
	if _, err := readBackupUDID(empty); err == nil {
		t.Error("expected error for missing Info.plist")
	}

	garbage := t.TempDir()
	if err := os.WriteFile(filepath.Join(garbage, "Info.plist"), []byte("not a plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readBackupUDID(garbage); err == nil {
		t.Error("expected error for unreadable Info.plist")
	}
}

func TestFindWorkspaceFromSubdir(t *testing.T) {
	ws := initedWorkspace(t)
	sub := filepath.Join(ws, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	chdir(t, sub)
	found, err := findWorkspace()
	if err != nil {
		t.Fatal(err)
	}
	// Resolve symlinks: on macOS TempDir is under /var -> /private/var.
	wantResolved, _ := filepath.EvalSymlinks(ws)
	gotResolved, _ := filepath.EvalSymlinks(found)
	if gotResolved != wantResolved {
		t.Errorf("found %q, want %q", found, ws)
	}
}

func TestFindWorkspaceOutside(t *testing.T) {
	chdir(t, t.TempDir())
	if _, err := findWorkspace(); err == nil {
		t.Error("expected error outside a workspace")
	}
}

func TestImportBindsUnboundWorkspace(t *testing.T) {
	ws := initedWorkspace(t)
	backup := fakeBackup(t, t.TempDir(), fakeUDID)
	chdir(t, ws)

	before, err := loadSettings(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := runImport(backup); err != nil {
		t.Fatal(err)
	}
	after, err := loadSettings(ws)
	if err != nil {
		t.Fatal(err)
	}
	if after.UDID != fakeUDID {
		t.Errorf("udid = %q, want %q", after.UDID, fakeUDID)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Error("import changed created_at")
	}
}

func TestImportSameDeviceAgain(t *testing.T) {
	ws := initedWorkspace(t)
	backup := fakeBackup(t, t.TempDir(), fakeUDID)
	chdir(t, ws)
	if err := runImport(backup); err != nil {
		t.Fatal(err)
	}
	if err := runImport(backup); err != nil {
		t.Fatalf("second import of same device: %v", err)
	}
}

func TestImportRefusesDifferentDevice(t *testing.T) {
	ws := initedWorkspace(t)
	chdir(t, ws)
	if err := runImport(fakeBackup(t, t.TempDir(), fakeUDID)); err != nil {
		t.Fatal(err)
	}
	other := fakeBackup(t, t.TempDir(), "99998888-FFFFEEEEDDDDCCCC")
	err := runImport(other)
	if err == nil {
		t.Fatal("expected refusal for different device")
	}
	if !strings.Contains(err.Error(), fakeUDID) || !strings.Contains(err.Error(), "99998888-FFFFEEEEDDDDCCCC") {
		t.Errorf("error should name both devices: %v", err)
	}
	s, _ := loadSettings(ws)
	if s.UDID != fakeUDID {
		t.Errorf("refusal must not change binding: udid = %q", s.UDID)
	}
}

// --- binary tests ---

func TestCLIImportNoArg(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "import")
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr, "import requires the path") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIImportOutsideWorkspace(t *testing.T) {
	dir := t.TempDir()
	backup := fakeBackup(t, dir, fakeUDID)
	code, _, stderr := run(t, dir, "import", backup)
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "not a whatskept workspace") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIImportBindsAndRefuses(t *testing.T) {
	ws := t.TempDir()
	run(t, ws, "init")
	backup := fakeBackup(t, t.TempDir(), fakeUDID)

	code, stdout, _ := run(t, ws, "import", backup)
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "workspace bound to device "+fakeUDID) {
		t.Errorf("stdout = %q", stdout)
	}

	other := fakeBackup(t, t.TempDir(), "99998888-FFFFEEEEDDDDCCCC")
	code, _, stderr := run(t, ws, "import", other)
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "bound to device "+fakeUDID) {
		t.Errorf("stderr = %q", stderr)
	}
}
