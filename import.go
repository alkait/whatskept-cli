package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"howett.net/plist"
)

// findWorkspace walks up from the current directory looking for a
// .whatskept/ marker and returns the workspace root.
func findWorkspace() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".whatskept")); err == nil && fi.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New(`not a whatskept workspace (run "whatskept init" first)`)
		}
		dir = parent
	}
}

// readBackupUDID validates that dir is an iOS backup (its Info.plist
// parses as a plist dict) and returns the device UDID: the plist's
// "Target Identifier" when present, otherwise the directory name —
// MobileSync names backup directories by UDID.
func readBackupUDID(dir string) (string, error) {
	f, err := os.Open(filepath.Join(dir, "Info.plist"))
	if err != nil {
		return "", fmt.Errorf("%s is not an iOS backup (no Info.plist)", dir)
	}
	defer f.Close()
	var info map[string]any
	if err := plist.NewDecoder(f).Decode(&info); err != nil {
		return "", fmt.Errorf("%s is not an iOS backup (unreadable Info.plist)", dir)
	}
	if s, ok := info["Target Identifier"].(string); ok && s != "" {
		return s, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.Base(abs), nil
}

func runImport(backupPath string) error {
	ws, err := findWorkspace()
	if err != nil {
		return err
	}
	udid, err := readBackupUDID(backupPath)
	if err != nil {
		return err
	}
	s, err := loadSettings(ws)
	if err != nil {
		return err
	}
	switch {
	case s.UDID == "":
		s.UDID = udid
		if err := saveSettings(ws, s); err != nil {
			return err
		}
		fmt.Printf("workspace bound to device %s\n", udid)
	case s.UDID == udid:
		fmt.Printf("backup matches workspace device %s\n", udid)
	default:
		return fmt.Errorf("backup is from device %s but this workspace is bound to device %s", udid, s.UDID)
	}
	fmt.Println("validation passed; importing messages is not implemented yet")
	return nil
}
