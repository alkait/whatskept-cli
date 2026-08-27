package live

import (
	"strings"
	"testing"

	"whatskept/internal/workspace"
)

func initWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := workspace.Init(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBindNumberFirstLinkBinds(t *testing.T) {
	dir := initWorkspace(t)
	s, err := workspace.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindNumber(dir, &s, "971500000000"); err != nil {
		t.Fatalf("bindNumber: %v", err)
	}
	if s.WhatsAppNumber != "+971500000000" {
		t.Errorf("in-memory number = %q", s.WhatsAppNumber)
	}
	saved, err := workspace.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if saved.WhatsAppNumber != "+971500000000" {
		t.Errorf("saved number = %q", saved.WhatsAppNumber)
	}
}

func TestBindNumberMatchPasses(t *testing.T) {
	dir := initWorkspace(t)
	s := workspace.Settings{WhatsAppNumber: "+971500000000"}
	if err := bindNumber(dir, &s, "971500000000"); err != nil {
		t.Fatalf("bindNumber on matching account: %v", err)
	}
}

func TestBindNumberMismatchRefused(t *testing.T) {
	dir := initWorkspace(t)
	s := workspace.Settings{WhatsAppNumber: "+971500000000"}
	err := bindNumber(dir, &s, "971511111111")
	if err == nil {
		t.Fatal("mismatched account was accepted")
	}
	for _, want := range []string{"+971511111111", "+971500000000", "bound"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	// The refusal must not have rebound the workspace.
	saved, loadErr := workspace.Load(dir)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if saved.WhatsAppNumber != "" {
		t.Errorf("settings were modified on refusal: %q", saved.WhatsAppNumber)
	}
}
