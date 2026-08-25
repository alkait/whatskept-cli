// Package backup discovers, validates, and decrypts iOS backups, and
// extracts the WhatsApp account identity from them.
package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"howett.net/plist"
)

// Info is the metadata for one discovered backup.
type Info struct {
	Path        string
	DeviceName  string
	LastBackup  time.Time // zero if unknown
	IsEncrypted bool
}

// DefaultRoot is where macOS keeps iOS backups.
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "MobileSync", "Backup")
}

// ReadUDID validates that dir is an iOS backup (its Info.plist parses as
// a plist dict) and returns the device UDID: the plist's "Target
// Identifier" when present, otherwise the directory name — MobileSync
// names backup directories by UDID.
func ReadUDID(dir string) (string, error) {
	info, err := readPlist(filepath.Join(dir, "Info.plist"))
	if err != nil || info == nil {
		return "", fmt.Errorf("%s is not an iOS backup (no readable Info.plist)", dir)
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

// Discover returns every valid backup under root (a directory with
// parseable Info.plist and Manifest.plist), newest first. A missing
// root yields an empty list, not an error.
func Discover(root string) ([]Info, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if errors.Is(err, os.ErrPermission) {
		return nil, fmt.Errorf("cannot read %s (grant Full Disk Access to your terminal?)", root)
	}
	if err != nil {
		return nil, err
	}

	var out []Info
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		info, err := readPlist(filepath.Join(dir, "Info.plist"))
		if err != nil || info == nil {
			continue
		}
		manifest, err := readPlist(filepath.Join(dir, "Manifest.plist"))
		if err != nil || manifest == nil {
			continue
		}
		b := Info{Path: dir, DeviceName: "(unnamed)"}
		if s, ok := info["Device Name"].(string); ok && s != "" {
			b.DeviceName = s
		}
		if t, ok := info["Last Backup Date"].(time.Time); ok {
			b.LastBackup = t
		}
		if enc, ok := manifest["IsEncrypted"].(bool); ok {
			b.IsEncrypted = enc
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LastBackup.After(out[j].LastBackup)
	})
	return out, nil
}

// readPlist parses a plist file (binary or XML) into a map. Returns
// (nil, nil) if the file is missing or not a plist dict — the caller
// treats that as "not a backup".
func readPlist(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var v map[string]any
	if err := plist.NewDecoder(f).Decode(&v); err != nil {
		return nil, nil
	}
	return v, nil
}
