// Package workspace manages the .whatskept/ marker directory and the
// portable configuration in .whatskept/settings.json.
package workspace

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const markerDir = ".whatskept"

// agentGuide is the operator guide dropped into every workspace as
// CLAUDE.md and AGENTS.md. Rewritten on every init so an upgraded
// binary refreshes the guidance.
//
//go:embed embed/AGENTS.md
var agentGuide []byte

// Settings is the portable configuration stored in settings.json.
type Settings struct {
	CreatedAt time.Time `json:"created_at"`

	// UDID and WhatsAppNumber bind the workspace to one device and one
	// WhatsApp account. UDID is stamped at the start of the first import,
	// WhatsAppNumber once the backup is decrypted; later imports from a
	// different device or account are refused.
	UDID           string `json:"udid,omitempty"`
	WhatsAppNumber string `json:"whatsapp_number,omitempty"`
}

// SettingsPath returns the settings.json path for a workspace root.
func SettingsPath(root string) string {
	return filepath.Join(root, markerDir, "settings.json")
}

// Load reads the settings of a workspace.
func Load(root string) (Settings, error) {
	var s Settings
	data, err := os.ReadFile(SettingsPath(root))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("parse settings.json: %w", err)
	}
	return s, nil
}

// Save writes atomically (temp + rename) so a crash mid-write can never
// leave a half-written settings.json.
func Save(root string, s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dst := SettingsPath(root)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Init initializes dir as a workspace, creating it if needed. Returns
// already=true when dir is one — but the agent guides are (re)written
// either way, so re-running init after a binary upgrade refreshes them.
func Init(dir string) (already bool, err error) {
	marker := filepath.Join(dir, markerDir)
	if _, statErr := os.Stat(marker); statErr == nil {
		already = true
	} else {
		if err := os.MkdirAll(marker, 0o755); err != nil {
			return false, err
		}
		if err := Save(dir, Settings{CreatedAt: time.Now().UTC()}); err != nil {
			return false, err
		}
	}
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), agentGuide, 0o644); err != nil {
			return already, err
		}
	}
	return already, nil
}

// Find walks up from the current directory looking for a .whatskept/
// marker and returns the workspace root.
func Find() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, markerDir)); err == nil && fi.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New(`not a whatskept workspace (run "whatskept init" first)`)
		}
		dir = parent
	}
}
