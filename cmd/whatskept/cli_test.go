package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"whatskept/internal/backup"
	"whatskept/internal/mcpserve"
)

// binPath is the whatskept binary built once for all CLI tests.
var binPath string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "whatskept-cli-test")
	if err != nil {
		panic(err)
	}
	binPath = filepath.Join(tmp, "whatskept")
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		panic("build failed: " + string(out))
	}
	// This Go toolchain (1.22.x) emits an invalid ad-hoc signature when
	// linking cgo binaries on current macOS; the kernel SIGKILLs them.
	// Re-signing fixes it. Harmless once Go is upgraded.
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("codesign", "-f", "-s", "-", binPath).CombinedOutput(); err != nil {
			panic("codesign failed: " + string(out))
		}
	}
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// run executes the binary in dir and returns exit code, stdout, stderr.
func run(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
	// Tests must not inherit a real backup password or MCP token from
	// the developer's environment; an empty value counts as unset.
	cmd.Env = append(os.Environ(), backup.PasswordEnv+"=", mcpserve.TokenEnv+"=")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return code, stdout.String(), stderr.String()
}

func TestCLIInit(t *testing.T) {
	dir := t.TempDir()
	code, stdout, _ := run(t, dir, "init", "ws1")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "initialized whatskept workspace in ws1") {
		t.Errorf("stdout = %q", stdout)
	}
	if _, err := os.Stat(filepath.Join(dir, "ws1", ".whatskept", "settings.json")); err != nil {
		t.Errorf("settings.json missing: %v", err)
	}
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(dir, "ws1", name)); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
}

func TestCLIInitIdempotent(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "ws1")
	code, stdout, _ := run(t, dir, "init", "ws1")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "already a whatskept workspace") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestCLIHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		code, stdout, _ := run(t, t.TempDir(), arg)
		if code != 0 {
			t.Errorf("%s: exit %d, want 0", arg, code)
		}
		if !strings.Contains(stdout, "Usage:") {
			t.Errorf("%s: stdout = %q", arg, stdout)
		}
	}
}

func TestCLINoArgs(t *testing.T) {
	code, _, stderr := run(t, t.TempDir())
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIUnknownCommand(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "bogus")
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown command: bogus") || !strings.Contains(stderr, "Usage:") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIInitFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, permission checks don't apply")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	code, _, stderr := run(t, dir, "init", "ws1")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "error:") {
		t.Errorf("stderr = %q", stderr)
	}
}

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
	b := writeImportBackup(t, dir, fakeUDID)
	code, _, stderr := run(t, dir, "import", b)
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "not a whatskept workspace") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIImportBindsThenNeedsPassword(t *testing.T) {
	ws := t.TempDir()
	run(t, ws, "init")
	b := writeImportBackup(t, t.TempDir(), fakeUDID)

	// Real decryption needs a password, so the run stops there (exit 1) —
	// but only after checkpoint 1 has bound the device.
	code, stdout, stderr := run(t, ws, "import", b)
	if code != 1 {
		t.Fatalf("exit %d, want 1 (missing password)", code)
	}
	if !strings.Contains(stdout, "workspace bound to device "+fakeUDID) {
		t.Errorf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, backup.PasswordEnv) {
		t.Errorf("stderr should point at %s: %q", backup.PasswordEnv, stderr)
	}

	other := writeImportBackup(t, t.TempDir(), "99998888-FFFFEEEEDDDDCCCC")
	code, _, stderr = run(t, ws, "import", other)
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "bound to device "+fakeUDID) {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIImportUnencryptedBackup(t *testing.T) {
	ws := t.TempDir()
	run(t, ws, "init")
	b := writeImportBackup(t, t.TempDir(), fakeUDID)
	manifest := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>IsEncrypted</key><false/></dict></plist>
`
	if err := os.WriteFile(filepath.Join(b, "Manifest.plist"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, ws, "import", b)
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "not encrypted") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestCLIMCPRequiresDatabase(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "mcp")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "--database") {
		t.Errorf("stderr = %q, want mention of --database", stderr)
	}
}

func TestCLIMCPRequiresToken(t *testing.T) {
	code, _, stderr := run(t, t.TempDir(), "mcp", "--database", "ChatStorage.sqlite")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, mcpserve.TokenEnv) {
		t.Errorf("stderr = %q, want mention of %s", stderr, mcpserve.TokenEnv)
	}
}

func TestCLIList(t *testing.T) {
	root := t.TempDir()
	writeListBackup(t, root, "ABC123", "Test iPhone", "2026-08-20T10:00:00Z", true)
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
