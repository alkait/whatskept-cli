package backup

import (
	"os"
	"path/filepath"
	"testing"
)

const fakeUDID = "00008101-000A1B2C3D4E5F9D"

// writeInfoPlist writes an Info.plist with a Target Identifier.
func writeInfoPlist(t *testing.T, dir, udid string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	info := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Target Identifier</key><string>` + udid + `</string></dict></plist>
`
	if err := os.WriteFile(filepath.Join(dir, "Info.plist"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeFullBackup writes Info.plist + Manifest.plist (a complete fixture
// as Discover expects).
func writeFullBackup(t *testing.T, root, name, deviceName, date string, encrypted bool) {
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
<plist version="1.0"><dict><key>IsEncrypted</key>` + enc + `</dict></plist>
`
	if err := os.WriteFile(filepath.Join(dir, "Info.plist"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Manifest.plist"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadUDID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backup")
	writeInfoPlist(t, dir, fakeUDID)
	udid, err := ReadUDID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if udid != fakeUDID {
		t.Errorf("udid = %q, want %q", udid, fakeUDID)
	}
}

func TestReadUDIDFallsBackToDirName(t *testing.T) {
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
	udid, err := ReadUDID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if udid != "11112222-AAAABBBBCCCCDDDD" {
		t.Errorf("udid = %q, want dir name", udid)
	}
}

func TestReadUDIDNotABackup(t *testing.T) {
	if _, err := ReadUDID(t.TempDir()); err == nil {
		t.Error("expected error for missing Info.plist")
	}
	garbage := t.TempDir()
	if err := os.WriteFile(filepath.Join(garbage, "Info.plist"), []byte("not a plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadUDID(garbage); err == nil {
		t.Error("expected error for unreadable Info.plist")
	}
}

func TestDiscoverNewestFirst(t *testing.T) {
	root := t.TempDir()
	writeFullBackup(t, root, "OLD", "Old iPhone", "2024-01-01T10:00:00Z", true)
	writeFullBackup(t, root, "NEW", "New iPhone", "2026-08-20T10:00:00Z", true)
	backups, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 2 {
		t.Fatalf("got %d, want 2", len(backups))
	}
	if backups[0].DeviceName != "New iPhone" || backups[1].DeviceName != "Old iPhone" {
		t.Errorf("not newest first: %q, %q", backups[0].DeviceName, backups[1].DeviceName)
	}
	if !backups[0].IsEncrypted {
		t.Error("IsEncrypted not read")
	}
}

func TestDiscoverSkipsInvalidEntries(t *testing.T) {
	root := t.TempDir()
	writeFullBackup(t, root, "GOOD", "iPhone", "2026-01-01T00:00:00Z", true)
	if err := os.MkdirAll(filepath.Join(root, "no-manifest"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	backups, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 || backups[0].DeviceName != "iPhone" {
		t.Errorf("got %v, want just GOOD", backups)
	}
}

func TestDiscoverMissingRoot(t *testing.T) {
	backups, err := Discover(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("missing root should not error: %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("got %d, want 0", len(backups))
	}
}
