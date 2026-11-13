package backup

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"

	"whatskept/internal/views"
)

// emptySchema is the phone's ChatStorage schema with no rows — the same
// tables, indexes and Core Data entity counters an import would bring.
//
//go:embed embed/empty.sql
var emptySchema string

// CreateEmpty writes a ChatStorage database with no history to path,
// with the view layer and FTS index applied: the starting point for a
// workspace that goes straight to live without importing a backup.
// Built beside the target and renamed into place, so a crash never
// leaves a half-made database for live to append to.
func CreateEmpty(path string) error {
	tmp := path + ".new"
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(tmp + suffix)
	}
	db, err := sql.Open("sqlite", tmp)
	if err != nil {
		return err
	}
	_, err = db.Exec(emptySchema)
	db.Close()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("create empty %s: %w", ChatStorageName, err)
	}
	if _, err := views.Apply(tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("create empty %s: %w", ChatStorageName, err)
	}
	return promote(tmp, path)
}
