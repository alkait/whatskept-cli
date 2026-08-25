package views

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// writeFixtureDB builds a ChatStorage.sqlite with the Z* subset the view
// layer reads: one DM chat with an incoming text, an outgoing reply, a
// PDF document, and a link message.
func writeFixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const fixture = `
		CREATE TABLE ZWAMESSAGE (
			Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZMESSAGEDATE REAL,
			ZISFROMME INTEGER, ZMESSAGETYPE INTEGER, ZTEXT TEXT,
			ZPARENTMESSAGE INTEGER, ZSTANZAID TEXT, ZFROMJID TEXT, ZGROUPMEMBER INTEGER
		);
		CREATE TABLE ZWACHATSESSION (
			Z_PK INTEGER PRIMARY KEY, ZCONTACTJID TEXT, ZCONTACTIDENTIFIER TEXT,
			ZPARTNERNAME TEXT, ZMESSAGECOUNTER INTEGER, ZLASTMESSAGEDATE REAL, ZARCHIVED INTEGER
		);
		CREATE TABLE ZWAGROUPMEMBER (
			Z_PK INTEGER PRIMARY KEY, ZMEMBERJID TEXT, ZCONTACTNAME TEXT, ZFIRSTNAME TEXT
		);
		CREATE TABLE ZWAPROFILEPUSHNAME (ZJID TEXT, ZPUSHNAME TEXT);
		CREATE TABLE ZWAMEDIAITEM (
			Z_PK INTEGER PRIMARY KEY, ZMESSAGE INTEGER, ZMEDIALOCALPATH TEXT,
			ZAUTHORNAME TEXT, ZFILESIZE INTEGER, ZMEDIAURL TEXT, ZTITLE TEXT
		);

		INSERT INTO ZWACHATSESSION VALUES
			(1, '971501111111@s.whatsapp.net', '12345@lid', 'Sarah', 4, 700000000, 0);
		INSERT INTO ZWAMESSAGE VALUES
			(1, 1, 700000000, 0, 0, 'lunch at the cafe tomorrow?', NULL, 's1', '971501111111@s.whatsapp.net', NULL),
			(2, 1, 700000100, 1, 0, 'sounds good', 1, 's2', NULL, NULL),
			(3, 1, 700000200, 0, 8, NULL, NULL, 's3', '971501111111@s.whatsapp.net', NULL),
			(4, 1, 700000300, 0, 7, NULL, NULL, 's4', '971501111111@s.whatsapp.net', NULL);
		INSERT INTO ZWAMEDIAITEM VALUES
			(10, 3, 'Media/chat/scan.pdf', 'passport scan.pdf', 12345, NULL, NULL),
			(11, 4, NULL, NULL, NULL, 'https://example.com', 'Example headline');`
	if _, err := db.Exec(fixture); err != nil {
		t.Fatal(err)
	}
	return path
}

func query1(t *testing.T, path, q string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var s sql.NullString
	if err := db.QueryRow(q).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s.String
}

func TestApply(t *testing.T) {
	path := writeFixtureDB(t)
	n, err := Apply(path)
	if err != nil {
		t.Fatal(err)
	}
	// Indexed: two texts + one document filename + one link headline.
	if n != 4 {
		t.Errorf("fts rows = %d, want 4", n)
	}
}

func TestApplyIdempotent(t *testing.T) {
	path := writeFixtureDB(t)
	if _, err := Apply(path); err != nil {
		t.Fatal(err)
	}
	n, err := Apply(path)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if n != 4 {
		t.Errorf("fts rows after re-apply = %d, want 4", n)
	}
}

func TestVMessages(t *testing.T) {
	path := writeFixtureDB(t)
	if _, err := Apply(path); err != nil {
		t.Fatal(err)
	}
	if got := query1(t, path, `SELECT sender_name FROM v_messages WHERE rowid = 1`); got != "Sarah" {
		t.Errorf("incoming sender_name = %q, want Sarah", got)
	}
	if got := query1(t, path, `SELECT sender_name FROM v_messages WHERE rowid = 2`); got != "me" {
		t.Errorf("outgoing sender_name = %q, want me", got)
	}
	// Cocoa epoch 700000000 = 2023-03-08 20:26:40 UTC.
	if got := query1(t, path, `SELECT ts FROM v_messages WHERE rowid = 1`); got != "2023-03-08 20:26:40" {
		t.Errorf("ts = %q", got)
	}
	if got := query1(t, path, `SELECT message_type_name FROM v_messages WHERE rowid = 3`); got != "document" {
		t.Errorf("type = %q, want document", got)
	}
	if got := query1(t, path, `SELECT link_title FROM v_messages WHERE rowid = 4`); got != "Example headline" {
		t.Errorf("link_title = %q", got)
	}
}

func TestVChats(t *testing.T) {
	path := writeFixtureDB(t)
	if _, err := Apply(path); err != nil {
		t.Fatal(err)
	}
	if got := query1(t, path, `SELECT title FROM v_chats WHERE chat_id = 1`); got != "Sarah" {
		t.Errorf("title = %q, want Sarah", got)
	}
	if got := query1(t, path, `SELECT kind FROM v_chats WHERE chat_id = 1`); got != "dm" {
		t.Errorf("kind = %q, want dm", got)
	}
}

func TestWaDocument(t *testing.T) {
	path := writeFixtureDB(t)
	if _, err := Apply(path); err != nil {
		t.Fatal(err)
	}
	if got := query1(t, path, `SELECT filename FROM wa_document WHERE rowid = 3`); got != "passport scan.pdf" {
		t.Errorf("filename = %q", got)
	}
	if got := query1(t, path, `SELECT ext FROM wa_document WHERE rowid = 3`); got != "pdf" {
		t.Errorf("ext = %q, want pdf", got)
	}
}

func TestFTSSearch(t *testing.T) {
	path := writeFixtureDB(t)
	if _, err := Apply(path); err != nil {
		t.Fatal(err)
	}
	// Typed text; diacritic folding: café must match cafe.
	if got := query1(t, path, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'café'`); got != "1" {
		t.Errorf("match café = %s, want 1", got)
	}
	// Document filename is a searchable surface.
	if got := query1(t, path, `SELECT rowid FROM messages_fts WHERE messages_fts MATCH 'passport'`); got != "3" {
		t.Errorf("match passport rowid = %s, want 3", got)
	}
	// Link-preview headline is a searchable surface.
	if got := query1(t, path, `SELECT rowid FROM messages_fts WHERE messages_fts MATCH 'headline'`); got != "4" {
		t.Errorf("match headline rowid = %s, want 4", got)
	}
}

// TestFTSIncludesEnrichmentTables verifies the probe-and-extend design:
// once enrichment tables exist, their text becomes searchable on the
// next rebuild without any migration.
func TestFTSIncludesEnrichmentTables(t *testing.T) {
	path := writeFixtureDB(t)
	if _, err := Apply(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE wa_voice_text (rowid INTEGER PRIMARY KEY, transcript TEXT);
		INSERT INTO wa_voice_text VALUES (2, 'remember the tickets');`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Apply(path); err != nil {
		t.Fatal(err)
	}
	if got := query1(t, path, `SELECT rowid FROM messages_fts WHERE messages_fts MATCH 'tickets'`); got != "2" {
		t.Errorf("match tickets rowid = %s, want 2", got)
	}
}
