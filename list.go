package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"howett.net/plist"
)

// defaultBackupRoot is where macOS keeps iOS backups.
func defaultBackupRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "MobileSync", "Backup")
}

type backupInfo struct {
	Path        string
	DeviceName  string
	LastBackup  time.Time // zero if unknown
	IsEncrypted bool
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

// discoverBackups returns every valid backup under root (a directory
// with parseable Info.plist and Manifest.plist), newest first. A
// missing root yields an empty list, not an error.
func discoverBackups(root string) ([]backupInfo, error) {
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

	var out []backupInfo
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
		b := backupInfo{Path: dir, DeviceName: "(unnamed)"}
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

func runList(root string) error {
	backups, err := discoverBackups(root)
	if err != nil {
		return err
	}
	if len(backups) == 0 {
		fmt.Printf("no backups found in %s\n", root)
		return nil
	}
	for _, b := range backups {
		date := "unknown"
		if !b.LastBackup.IsZero() {
			date = b.LastBackup.Format("2006-01-02 15:04")
		}
		enc := "unencrypted"
		if b.IsEncrypted {
			enc = "encrypted"
		}
		fmt.Printf("%s  %s  %s\n  %s\n", date, enc, b.DeviceName, b.Path)
	}
	return nil
}
