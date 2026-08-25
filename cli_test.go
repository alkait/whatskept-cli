package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// run executes the binary in dir and returns exit code, stdout, stderr.
func run(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
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
