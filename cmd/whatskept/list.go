package main

import (
	"fmt"

	"whatskept/internal/backup"
)

func runList(root string) error {
	backups, err := backup.Discover(root)
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
