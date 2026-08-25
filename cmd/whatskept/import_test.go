package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"whatskept/internal/backup"
	"whatskept/internal/workspace"
)

const (
	fakeUDID   = "00008101-000A1B2C3D4E5F9D"
	fakeNumber = "+15551234567"
)

// writeImportBackup creates a minimal encrypted-flagged backup with a
// Target Identifier, as import's checkpoint 1 and DetectNumber expect.
func writeImportBackup(t *testing.T, parent, udid string) string {
	t.Helper()
	dir := filepath.Join(parent, "backup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	info := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Target Identifier</key><string>` + udid + `</string></dict></plist>
`
	manifest := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>IsEncrypted</key><true/></dict></plist>
`
	if err := os.WriteFile(filepath.Join(dir, "Info.plist"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Manifest.plist"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeListBackup creates a full fixture under root for --list tests.
func writeListBackup(t *testing.T, root, name, deviceName, date string, encrypted bool) {
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

func stubNumber(t *testing.T, number string) {
	t.Helper()
	orig := backup.DetectNumber
	t.Cleanup(func() { backup.DetectNumber = orig })
	backup.DetectNumber = func(dir string) (string, error) { return number, nil }
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

func initedWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := workspace.Init(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestImportBindsUnboundWorkspace(t *testing.T) {
	ws := initedWorkspace(t)
	b := writeImportBackup(t, t.TempDir(), fakeUDID)
	chdir(t, ws)
	stubNumber(t, fakeNumber)

	before, _ := workspace.Load(ws)
	if err := runImport(b); err != nil {
		t.Fatal(err)
	}
	after, _ := workspace.Load(ws)
	if after.UDID != fakeUDID {
		t.Errorf("udid = %q, want %q", after.UDID, fakeUDID)
	}
	if after.WhatsAppNumber != fakeNumber {
		t.Errorf("whatsapp_number = %q, want %q", after.WhatsAppNumber, fakeNumber)
	}
	if !after.CreatedAt.Equal(before.CreatedAt) {
		t.Error("import changed created_at")
	}
}

func TestImportSameDeviceAgain(t *testing.T) {
	ws := initedWorkspace(t)
	b := writeImportBackup(t, t.TempDir(), fakeUDID)
	chdir(t, ws)
	stubNumber(t, fakeNumber)
	if err := runImport(b); err != nil {
		t.Fatal(err)
	}
	if err := runImport(b); err != nil {
		t.Fatalf("second import of same device: %v", err)
	}
}

func TestImportNumberUndetected(t *testing.T) {
	ws := initedWorkspace(t)
	b := writeImportBackup(t, t.TempDir(), fakeUDID)
	chdir(t, ws)
	stubNumber(t, "")
	if err := runImport(b); err != nil {
		t.Fatalf("undetected number must not fail the import: %v", err)
	}
	s, _ := workspace.Load(ws)
	if s.WhatsAppNumber != "" {
		t.Errorf("whatsapp_number = %q, want empty", s.WhatsAppNumber)
	}
	if s.UDID != fakeUDID {
		t.Errorf("udid = %q, want %q", s.UDID, fakeUDID)
	}
}

func TestImportRefusesDifferentNumber(t *testing.T) {
	ws := initedWorkspace(t)
	b := writeImportBackup(t, t.TempDir(), fakeUDID)
	chdir(t, ws)
	stubNumber(t, fakeNumber)
	if err := runImport(b); err != nil {
		t.Fatal(err)
	}

	stubNumber(t, "+19998887777")
	err := runImport(b)
	if err == nil {
		t.Fatal("expected refusal for different WhatsApp number")
	}
	if !strings.Contains(err.Error(), fakeNumber) || !strings.Contains(err.Error(), "+19998887777") {
		t.Errorf("error should name both numbers: %v", err)
	}
	s, _ := workspace.Load(ws)
	if s.WhatsAppNumber != fakeNumber {
		t.Errorf("refusal must not change binding: %q", s.WhatsAppNumber)
	}
}

func TestImportRefusesDifferentDevice(t *testing.T) {
	ws := initedWorkspace(t)
	chdir(t, ws)
	stubNumber(t, fakeNumber)
	if err := runImport(writeImportBackup(t, t.TempDir(), fakeUDID)); err != nil {
		t.Fatal(err)
	}
	other := writeImportBackup(t, t.TempDir(), "99998888-FFFFEEEEDDDDCCCC")
	err := runImport(other)
	if err == nil {
		t.Fatal("expected refusal for different device")
	}
	if !strings.Contains(err.Error(), fakeUDID) || !strings.Contains(err.Error(), "99998888-FFFFEEEEDDDDCCCC") {
		t.Errorf("error should name both devices: %v", err)
	}
	s, _ := workspace.Load(ws)
	if s.UDID != fakeUDID {
		t.Errorf("refusal must not change binding: udid = %q", s.UDID)
	}
}
