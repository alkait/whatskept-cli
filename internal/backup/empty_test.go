package backup

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), ChatStorageName)
	if err := CreateEmpty(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".new"); !os.IsNotExist(err) {
		t.Errorf("staging file left behind: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The phone's tables, empty; Core Data's counters, at zero; and the
	// query surface MCP reads.
	for _, q := range []string{
		`SELECT COUNT(*) FROM ZWAMESSAGE`,
		`SELECT COUNT(*) FROM ZWACHATSESSION`,
		`SELECT COUNT(*) FROM ZWAPROFILEPUSHNAME`,
		`SELECT COUNT(*) FROM v_messages`,
		`SELECT COUNT(*) FROM v_chats`,
		`SELECT COUNT(*) FROM messages_fts`,
		`SELECT COUNT(*) FROM Z_PRIMARYKEY WHERE Z_MAX <> 0`,
	} {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s = %d, %v", q, n, err)
		}
	}
	var name string
	if err := db.QueryRow(`SELECT Z_NAME FROM Z_PRIMARYKEY WHERE Z_ENT = 9`).Scan(&name); err != nil || name != "WAMessage" {
		t.Errorf("entity 9 = %q, %v", name, err)
	}
}

// A workspace that went straight to live can import a backup later: the
// backup replaces the live-only history, which it contains anyway.
func TestMergeForwardOverEmptyDatabase(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite")
	temp := filepath.Join(dir, "temp.sqlite")
	if err := CreateEmpty(live); err != nil {
		t.Fatal(err)
	}
	writeDB(t, live, voiceTextDDL+`
		CREATE TABLE wa_live_pk (pk INTEGER PRIMARY KEY);
		INSERT INTO ZWAMESSAGE (Z_PK, ZSTANZAID) VALUES (1, 'live-1');
		INSERT INTO wa_live_pk VALUES (1);
		INSERT INTO wa_voice_text VALUES (1, 'live capture', '', NULL, 'm', 't');`)
	// The fresh extract: the phone's own schema, its own row 1.
	writeDB(t, temp, emptySchema+`
		INSERT INTO ZWAMESSAGE (Z_PK, ZSTANZAID) VALUES (1, 'phone-1');`)

	if err := mergeForward(live, temp); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", temp)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wa_voice_text`).Scan(&n); err != nil || n != 0 {
		t.Errorf("voice rows = %d, %v; a live rowid's text must not land on the phone's row 1", n, err)
	}
}
