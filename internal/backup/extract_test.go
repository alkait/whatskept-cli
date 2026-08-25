package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromoteRemovesStaleWALCompanions(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, ChatStorageName)
	temp := live + ".new"
	for path, content := range map[string]string{
		live:          "old",
		live + "-wal": "stale",
		live + "-shm": "stale",
		temp:          "fresh",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := promote(temp, live); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(live)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fresh" {
		t.Errorf("live = %q, want %q", got, "fresh")
	}
	for _, suffix := range []string{"-wal", "-shm", ".new"} {
		if _, err := os.Stat(live + suffix); !os.IsNotExist(err) {
			t.Errorf("%s%s should be gone", ChatStorageName, suffix)
		}
	}
}

func TestPromoteFirstImport(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, ChatStorageName)
	temp := live + ".new"
	if err := os.WriteFile(temp, []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := promote(temp, live); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live DB missing: %v", err)
	}
}

func writeManifest(t *testing.T, dir string, encrypted bool) {
	t.Helper()
	enc := "<false/>"
	if encrypted {
		enc = "<true/>"
	}
	manifest := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>IsEncrypted</key>` + enc + `</dict></plist>
`
	if err := os.WriteFile(filepath.Join(dir, "Manifest.plist"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOpenNotABackup(t *testing.T) {
	if _, err := Open(t.TempDir()); err == nil {
		t.Error("expected error for missing Manifest.plist")
	}
}

func TestOpenRejectsUnencrypted(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, false)
	_, err := Open(dir)
	if err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Errorf("err = %v, want 'not encrypted'", err)
	}
}

func TestOpenNeedsPassword(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, true)
	t.Setenv(PasswordEnv, "")
	_, err := Open(dir)
	if err == nil || !strings.Contains(err.Error(), PasswordEnv) {
		t.Errorf("err = %v, want mention of %s", err, PasswordEnv)
	}
}
