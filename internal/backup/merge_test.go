package backup

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// writeDB creates a SQLite file at path and runs stmts against it.
func writeDB(t *testing.T, path, stmts string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(stmts); err != nil {
		t.Fatal(err)
	}
}

const voiceTextDDL = `CREATE TABLE wa_voice_text (
	rowid INTEGER PRIMARY KEY, transcript TEXT, language TEXT,
	duration_sec REAL, model TEXT, generated_at TEXT);`

func TestMergeForwardCarriesEnrichment(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite")
	temp := filepath.Join(dir, "temp.sqlite")
	// Live DB: two enriched voice notes, one enriched image.
	writeDB(t, live, voiceTextDDL+`
		CREATE TABLE wa_image_text (rowid INTEGER PRIMARY KEY, ocr_text TEXT, description TEXT,
			language TEXT, source TEXT, model TEXT, generated_at TEXT);
		INSERT INTO wa_voice_text VALUES (1, 'keep me', '', NULL, 'm', 't');
		INSERT INTO wa_voice_text VALUES (2, 'message deleted on device', '', NULL, 'm', 't');
		INSERT INTO wa_image_text VALUES (3, 'receipt text', 'a receipt', '', 'cloud', 'm', 't');`)
	// Staging DB (fresh extract): messages 1 and 3 still exist, 2 is gone.
	writeDB(t, temp, `
		CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY);
		INSERT INTO ZWAMESSAGE VALUES (1), (3);`)

	if err := mergeForward(live, temp); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", temp)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var transcript string
	if err := db.QueryRow(`SELECT transcript FROM wa_voice_text WHERE rowid = 1`).Scan(&transcript); err != nil || transcript != "keep me" {
		t.Errorf("voice row 1: %q, %v", transcript, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wa_voice_text`).Scan(&n); err != nil || n != 1 {
		t.Errorf("voice rows = %d, want 1 (deleted message's row dropped)", n)
	}
	var ocr string
	if err := db.QueryRow(`SELECT ocr_text FROM wa_image_text WHERE rowid = 3`).Scan(&ocr); err != nil || ocr != "receipt text" {
		t.Errorf("image row 3: %q, %v", ocr, err)
	}
}

func TestMergeForwardNoEnrichmentTables(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite")
	temp := filepath.Join(dir, "temp.sqlite")
	writeDB(t, live, `CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY);`)
	writeDB(t, temp, `CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY);`)
	if err := mergeForward(live, temp); err != nil {
		t.Fatalf("never-enriched live DB must merge as a no-op: %v", err)
	}
}

func TestMergeForwardCorruptLiveDBFails(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite")
	temp := filepath.Join(dir, "temp.sqlite")
	if err := os.WriteFile(live, []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDB(t, temp, `CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY);`)
	if err := mergeForward(live, temp); err == nil {
		t.Error("corrupt live DB must fail the merge, not silently drop enrichment")
	}
}

// Blob candidate selection must skip rows whose enrichment text is
// already in the DB — re-queueing them would re-pay for it.
func TestSelectBlobCandidatesSkipsEnriched(t *testing.T) {
	dir := t.TempDir()
	db := writeFixtureChatDB(t, dir) // image rowid 1, voice rowid 2, pdf rowid 3, docx rowid 4
	if _, err := db.Exec(voiceTextDDL + `
		CREATE TABLE wa_image_text (rowid INTEGER PRIMARY KEY, ocr_text TEXT);
		INSERT INTO wa_image_text VALUES (1, 'done');
		INSERT INTO wa_voice_text VALUES (2, 'done', '', NULL, 'm', 't');`); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		kind, where string
		want        int
	}{
		{"images", "m.ZMEDIALOCALPATH LIKE '%.jpg'", 0},
		{"voice", "m.ZMEDIALOCALPATH LIKE '%.opus'", 0},
		// wa_document_text doesn't exist → documents unaffected.
		{"documents", "wm.ZMESSAGETYPE = 8 AND m.ZMEDIALOCALPATH IS NOT NULL", 2},
	} {
		excl, err := enrichedExclusion(db, c.kind)
		if err != nil {
			t.Fatal(err)
		}
		got, err := selectBlobCandidates(db, c.where+excl)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != c.want {
			t.Errorf("%s: %d candidates, want %d", c.kind, len(got), c.want)
		}
	}
}
