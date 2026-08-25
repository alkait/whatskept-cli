package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const usage = `whatskept — WhatsApp history, kept.

Usage:
  whatskept init [dir]               initialize a workspace (default: current directory)
  whatskept import <ios-backup-path> import history from an iOS backup
  whatskept import --list            list iOS backups on this machine
  whatskept -h | --help              show this help
`

// settings is the portable configuration stored in .whatskept/settings.json.
type settings struct {
	CreatedAt time.Time `json:"created_at"`

	// UDID binds the workspace to one device. Stamped by the first
	// import; later imports from a different device are refused.
	UDID string `json:"udid,omitempty"`
}

func settingsPath(workspace string) string {
	return filepath.Join(workspace, ".whatskept", "settings.json")
}

func loadSettings(workspace string) (settings, error) {
	var s settings
	data, err := os.ReadFile(settingsPath(workspace))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("parse settings.json: %w", err)
	}
	return s, nil
}

// saveSettings writes atomically (temp + rename) so a crash mid-write
// can never leave a half-written settings.json.
func saveSettings(workspace string, s settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dst := settingsPath(workspace)
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

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "init":
		dir := "."
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		if err := initWorkspace(filepath.Clean(dir)); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "import":
		if len(os.Args) < 3 {
			fmt.Fprint(os.Stderr, "import requires the path to an iOS backup, or --list\n\n"+usage)
			os.Exit(2)
		}
		if os.Args[2] == "--list" {
			root := defaultBackupRoot()
			if len(os.Args) > 3 {
				root = os.Args[3]
			}
			if err := runList(root); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
		if err := runImport(filepath.Clean(os.Args[2])); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func initWorkspace(dir string) error {
	marker := filepath.Join(dir, ".whatskept")
	if _, err := os.Stat(marker); err == nil {
		fmt.Printf("%s is already a whatskept workspace\n", dir)
		return nil
	}
	if err := os.MkdirAll(marker, 0o755); err != nil {
		return err
	}
	if err := saveSettings(dir, settings{CreatedAt: time.Now().UTC()}); err != nil {
		return err
	}
	fmt.Printf("initialized whatskept workspace in %s\n", dir)
	return nil
}
