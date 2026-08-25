package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeListedBackup creates a complete backup fixture (Info.plist +
// Manifest.plist) under root, as discoverBackups expects.
func fakeListedBackup(t *testing.T, root, name, deviceName, date string, encrypted bool) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	info := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>Device Name</key><string>` + deviceName + `</string>
	<key>Last Backup Date</key><date>` + date + `</date>
</dict></plist>
`
	enc := "<false/>"
	if encrypted {
		enc = "<true/>"
	}
	manifest := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>IsEncrypted</key>` + enc + `
</dict></plist>
`
	if err := os.WriteFile(filepath.Join(dir, "Info.plist"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Manifest.plist"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverBackupsNewestFirst(t *testing.T) {
	root := t.TempDir()
	fakeListedBackup(t, root, "OLD", "Old iPhone", "2024-01-01T10:00:00Z", true)
	fakeListedBackup(t, root, "NEW", "New iPhone", "2026-08-20T10:00:00Z", true)

	backups, err := discoverBackups(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 2 {
		t.Fatalf("got %d backups, want 2", len(backups))
	}
	if backups[0].DeviceName != "New iPhone" || backups[1].DeviceName != "Old iPhone" {
		t.Errorf("not newest first: %q, %q", backups[0].DeviceName, backups[1].DeviceName)
	}
	if !backups[0].IsEncrypted {
		t.Error("IsEncrypted not read")
	}
}

func TestDiscoverBackupsSkipsInvalidEntries(t *testing.T) {
	root := t.TempDir()
	fakeListedBackup(t, root, "GOOD", "iPhone", "2026-01-01T00:00:00Z", true)
	// A directory without Manifest.plist (e.g. import's minimal fixture shape).
	if err := os.MkdirAll(filepath.Join(root, "no-manifest"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A plain file.
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	backups, err := discoverBackups(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 || backups[0].DeviceName != "iPhone" {
		t.Errorf("got %v, want just GOOD", backups)
	}
}

func TestDiscoverBackupsMissingRoot(t *testing.T) {
	backups, err := discoverBackups(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing root should not error: %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("got %d backups, want 0", len(backups))
	}
}

// --- binary tests ---

func TestCLIList(t *testing.T) {
	root := t.TempDir()
	fakeListedBackup(t, root, "ABC123", "Test iPhone", "2026-08-20T10:00:00Z", true)

	code, stdout, _ := run(t, t.TempDir(), "import", "--list", root)
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	for _, want := range []string{"Test iPhone", "encrypted", "2026-08-20", "ABC123"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q: %q", want, stdout)
		}
	}
}

func TestCLIListEmpty(t *testing.T) {
	root := filepath.Join(t.TempDir(), "empty-root")
	code, stdout, _ := run(t, t.TempDir(), "import", "--list", root)
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "no backups found") {
		t.Errorf("stdout = %q", stdout)
	}
}
