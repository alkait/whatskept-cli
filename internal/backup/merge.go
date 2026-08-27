package backup

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// enrichmentTables maps each blob kind's enrichment-result table.
// These rows are produced by `whatskept enrich` (paid API work) and
// exist only in the live DB — a freshly decrypted backup never has
// them, so re-imports must carry them forward and blob extraction must
// not re-queue what they cover.
var enrichmentTables = map[string]string{
	"images":    "wa_image_text",
	"voice":     "wa_voice_text",
	"documents": "wa_document_text",
}

// mergeForward copies the enrichment tables from the current live DB
// into the staging DB, keeping only rows whose message still exists
// there (messages deleted on the device drop their enrichment too).
// Tables the live DB doesn't have are skipped — nothing to carry.
func mergeForward(livePath, tempPath string) error {
	db, err := sql.Open("sqlite", tempPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`ATTACH DATABASE ? AS old`, livePath); err != nil {
		return fmt.Errorf("attach live db: %w", err)
	}
	// Rows written by `whatskept live` carry locally assigned rowids;
	// after a re-import the same number belongs to a DIFFERENT message
	// (the phone re-delivers live-captured messages under its own
	// numbering). Carrying their enrichment forward would attach text
	// to the wrong rows, so those are dropped — the wa_live_pk ledger
	// says which they are. The ledger itself is not carried either: a
	// fresh import contains only the phone's own rows.
	liveExclusion := ""
	var hasLedger int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM old.sqlite_master WHERE type = 'table' AND name = 'wa_live_pk'`,
	).Scan(&hasLedger); err != nil {
		return fmt.Errorf("check wa_live_pk: %w", err)
	}
	if hasLedger > 0 {
		liveExclusion = ` AND rowid NOT IN (SELECT pk FROM old.wa_live_pk)`
	}

	for _, table := range enrichmentTables {
		var ddl string
		err := db.QueryRow(
			`SELECT sql FROM old.sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&ddl)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s schema: %w", table, err)
		}
		// The DDL names the table unqualified, so it creates in main
		// (the staging DB).
		if _, err := db.Exec(ddl); err != nil {
			return fmt.Errorf("create %s: %w", table, err)
		}
		if _, err := db.Exec(fmt.Sprintf(
			`INSERT INTO main.%[1]s SELECT * FROM old.%[1]s
			 WHERE rowid IN (SELECT Z_PK FROM main.ZWAMESSAGE)`+liveExclusion, table)); err != nil {
			return fmt.Errorf("copy %s rows: %w", table, err)
		}
	}
	return nil
}

// enrichedExclusion returns a WHERE-clause suffix excluding media rows
// already enriched (their text is in the DB; the blob must not be
// re-queued and re-paid). Empty when the table doesn't exist yet.
func enrichedExclusion(db *sql.DB, kind string) (string, error) {
	table := enrichmentTables[kind]
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&n); err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	return " AND m.ZMESSAGE NOT IN (SELECT rowid FROM " + table + ")", nil
}
