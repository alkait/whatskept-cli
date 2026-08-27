package live

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatskept/internal/backup"
)

// chatStorageDDL is the minimal slice of the iOS schema the writer
// touches, mirroring the real column sets.
const chatStorageDDL = `
CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER,
	ZCHATSESSION INTEGER, ZGROUPMEMBER INTEGER, ZISFROMME INTEGER,
	ZMESSAGETYPE INTEGER, ZMESSAGESTATUS INTEGER, ZMESSAGEDATE REAL, ZSENTDATE REAL,
	ZFROMJID TEXT, ZTOJID TEXT, ZSTANZAID TEXT, ZTEXT TEXT, ZPARENTMESSAGE INTEGER);
CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER,
	ZSESSIONTYPE INTEGER, ZCONTACTJID TEXT, ZCONTACTIDENTIFIER TEXT,
	ZPARTNERNAME TEXT, ZMESSAGECOUNTER INTEGER,
	ZLASTMESSAGE INTEGER, ZLASTMESSAGEDATE REAL, ZLASTMESSAGETEXT TEXT);
CREATE TABLE ZWAGROUPMEMBER (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER,
	ZCHATSESSION INTEGER, ZISACTIVE INTEGER, ZMEMBERJID TEXT);
CREATE TABLE ZWAMEDIAITEM (Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER,
	ZMESSAGE INTEGER, ZMEDIALOCALPATH TEXT, ZFILESIZE INTEGER,
	ZMOVIEDURATION INTEGER, ZAUTHORNAME TEXT, ZTITLE TEXT);
CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER, Z_NAME TEXT, Z_SUPER INTEGER, Z_MAX INTEGER);
INSERT INTO Z_PRIMARYKEY VALUES (9, 'WAMessage', 0, 0), (4, 'WAChatSession', 0, 0),
	(6, 'WAGroupMember', 0, 0), (8, 'WAMediaItem', 0, 0);
CREATE VIRTUAL TABLE messages_fts USING fts5(text);
`

// newTestWriter creates a workspace-shaped temp dir with a minimal
// ChatStorage and returns a ready Writer over it.
func newTestWriter(t *testing.T) (*Writer, string) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, backup.ChatStorageName)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(chatStorageDDL); err != nil {
		t.Fatal(err)
	}
	db.Close()
	w := &Writer{dbPath: dbPath, root: root}
	if err := w.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	return w, root
}

func queryDB(t *testing.T, w *Writer, query string, args ...any) *sql.Row {
	t.Helper()
	db, err := openRW(w.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db.QueryRow(query, args...)
}

func dmEvent(stanza, text string) *events.Message {
	v := &events.Message{Message: &waE2E.Message{Conversation: proto.String(text)}}
	v.Info.ID = types.MessageID(stanza)
	v.Info.Chat = types.NewJID("971500000001", types.DefaultUserServer)
	v.Info.Sender = types.NewJID("971500000001", types.DefaultUserServer)
	v.Info.PushName = "Sam"
	v.Info.Timestamp = time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	return v
}

func TestDecideClassification(t *testing.T) {
	// Ordinary text → insert.
	p := decide(dmEvent("S1", "hello"), nil)
	if p.Action != actionInsert || p.Text != "hello" || p.StanzaID != "S1" {
		t.Errorf("text: %+v", p)
	}

	// Status/broadcast noise → skip.
	noise := dmEvent("S2", "story")
	noise.Info.Chat = types.NewJID("status", "broadcast")
	if p := decide(noise, nil); p.Action != actionSkip || !strings.HasPrefix(p.SkipReason, "noise:") {
		t.Errorf("noise: %+v", p)
	}

	// Revoke → target from the protocol key, not the revoke's own ID.
	rev := &events.Message{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
		Key:  &waCommon.MessageKey{ID: proto.String("TARGET")},
	}}}
	rev.Info.ID = "R1"
	rev.Info.Chat = types.NewJID("971500000001", types.DefaultUserServer)
	if p := decide(rev, nil); p.Action != actionRevoke || p.TargetID != "TARGET" {
		t.Errorf("revoke: %+v", p)
	}

	// Contentless (sender-key distribution half of the two-event
	// duplicate) → skip, so the content-carrying twin can insert.
	empty := &events.Message{Message: &waE2E.Message{}}
	empty.Info.ID = "S3"
	empty.Info.Chat = types.NewJID("971500000001", types.DefaultUserServer)
	if p := decide(empty, nil); p.Action != actionSkip {
		t.Errorf("contentless: %+v", p)
	}
}

func TestApplyInsertTextDM(t *testing.T) {
	w, _ := newTestWriter(t)
	res, err := w.Apply(context.Background(), decide(dmEvent("S1", "hello there"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != actionInsert || !res.NewChat {
		t.Fatalf("res = %+v", res)
	}

	var text, from, stanza string
	var chatPK int64
	var date float64
	if err := queryDB(t, w, `SELECT ZTEXT, ZFROMJID, ZSTANZAID, ZCHATSESSION, ZMESSAGEDATE
		FROM ZWAMESSAGE WHERE Z_PK = ?`, res.PK).Scan(&text, &from, &stanza, &chatPK, &date); err != nil {
		t.Fatal(err)
	}
	if text != "hello there" || from != "971500000001@s.whatsapp.net" || stanza != "S1" || chatPK != res.ChatPK {
		t.Errorf("row: text=%q from=%q stanza=%q chat=%d", text, from, stanza, chatPK)
	}
	wantDate := float64(time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC).Unix() - cocoaEpoch)
	if date != wantDate {
		t.Errorf("ZMESSAGEDATE = %f, want %f", date, wantDate)
	}

	// Chat created with the partner's push name and counter bumped.
	var partner string
	var counter int
	if err := queryDB(t, w, `SELECT ZPARTNERNAME, ZMESSAGECOUNTER FROM ZWACHATSESSION WHERE Z_PK = ?`,
		res.ChatPK).Scan(&partner, &counter); err != nil {
		t.Fatal(err)
	}
	if partner != "Sam" || counter != 1 {
		t.Errorf("chat: partner=%q counter=%d", partner, counter)
	}

	// FTS row shares the PK; live ledger records it.
	var n int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND text = 'hello there'`,
		res.PK).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts rows = %d, %v", n, err)
	}
	if err := queryDB(t, w, `SELECT COUNT(*) FROM wa_live_pk WHERE pk = ?`, res.PK).Scan(&n); err != nil || n != 1 {
		t.Errorf("wa_live_pk rows = %d, %v", n, err)
	}
}

func TestApplyDedupAndChatReuse(t *testing.T) {
	w, _ := newTestWriter(t)
	ctx := context.Background()
	first, err := w.Apply(ctx, decide(dmEvent("S1", "one"), nil))
	if err != nil {
		t.Fatal(err)
	}

	// Same stanza again → skip, no second row.
	dup, err := w.Apply(ctx, decide(dmEvent("S1", "one"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if dup.Action != actionSkip || dup.Reason != "already present" {
		t.Errorf("dup = %+v", dup)
	}

	// New message, same chat → reused session, not a fork.
	second, err := w.Apply(ctx, decide(dmEvent("S2", "two"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if second.NewChat || second.ChatPK != first.ChatPK {
		t.Errorf("second = %+v (first chat %d)", second, first.ChatPK)
	}
	var counter int
	if err := queryDB(t, w, `SELECT ZMESSAGECOUNTER FROM ZWACHATSESSION WHERE Z_PK = ?`,
		first.ChatPK).Scan(&counter); err != nil || counter != 2 {
		t.Errorf("counter = %d, %v", counter, err)
	}
}

func TestApplyEdit(t *testing.T) {
	w, _ := newTestWriter(t)
	ctx := context.Background()
	ins, err := w.Apply(ctx, decide(dmEvent("S1", "typo"), nil))
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.Apply(ctx, plan{Action: actionEdit, TargetID: "S1", Text: "fixed"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != actionEdit || res.PK != ins.PK {
		t.Fatalf("res = %+v", res)
	}
	var text string
	if err := queryDB(t, w, `SELECT ZTEXT FROM ZWAMESSAGE WHERE Z_PK = ?`, ins.PK).Scan(&text); err != nil || text != "fixed" {
		t.Errorf("text = %q, %v", text, err)
	}
	var n int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND text = 'fixed'`,
		ins.PK).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts = %d, %v", n, err)
	}
}

func imageEvent(stanza, caption string) *events.Message {
	v := &events.Message{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption:       proto.String(caption),
		DirectPath:    proto.String("/v/t62.7118-24/x"),
		MediaKey:      []byte{1},
		FileSHA256:    []byte{2},
		FileEncSHA256: []byte{3},
		FileLength:    proto.Uint64(3),
	}}}
	v.Info.ID = types.MessageID(stanza)
	v.Info.Chat = types.NewJID("971500000001", types.DefaultUserServer)
	v.Info.Sender = types.NewJID("971500000001", types.DefaultUserServer)
	v.Info.Timestamp = time.Date(2026, 8, 27, 11, 0, 0, 0, time.UTC)
	return v
}

func TestApplyMediaInsertAndRevoke(t *testing.T) {
	w, root := newTestWriter(t)
	w.download = func(context.Context, downloadable) ([]byte, error) {
		return []byte("jpgbytes"), nil
	}
	ctx := context.Background()

	res, err := w.Apply(ctx, decide(imageEvent("M1", "sunset"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != actionInsert {
		t.Fatalf("res = %+v", res)
	}

	// File in the enrichment queue, named by PK.
	wantRel := filepath.Join(backup.UnenrichedDir, "media", "1.jpg")
	if res.MediaPath != wantRel {
		t.Errorf("MediaPath = %q, want %q", res.MediaPath, wantRel)
	}
	data, err := os.ReadFile(filepath.Join(root, wantRel))
	if err != nil || string(data) != "jpgbytes" {
		t.Errorf("queued file: %q, %v", data, err)
	}

	// Media item row: caption in ZTITLE, size from the real bytes, and
	// a .jpg local path (the extension queue selection filters on).
	var title, localPath string
	var size int64
	if err := queryDB(t, w, `SELECT ZTITLE, ZMEDIALOCALPATH, ZFILESIZE FROM ZWAMEDIAITEM WHERE ZMESSAGE = ?`,
		res.PK).Scan(&title, &localPath, &size); err != nil {
		t.Fatal(err)
	}
	if title != "sunset" || size != int64(len("jpgbytes")) || !strings.HasSuffix(localPath, ".jpg") {
		t.Errorf("media item: title=%q size=%d path=%q", title, size, localPath)
	}
	// ZTEXT stays NULL for media; the caption is the FTS surface.
	var text sql.NullString
	if err := queryDB(t, w, `SELECT ZTEXT FROM ZWAMESSAGE WHERE Z_PK = ?`, res.PK).Scan(&text); err != nil || text.Valid {
		t.Errorf("ZTEXT = %+v, want NULL", text)
	}
	var n int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND text = 'sunset'`,
		res.PK).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts = %d, %v", n, err)
	}

	// Revoke: tombstone, FTS gone, queued file gone.
	rev, err := w.Apply(ctx, plan{Action: actionRevoke, TargetID: "M1"})
	if err != nil {
		t.Fatal(err)
	}
	if rev.Action != actionRevoke || rev.PK != res.PK {
		t.Fatalf("rev = %+v", rev)
	}
	var msgType int
	if err := queryDB(t, w, `SELECT ZMESSAGETYPE FROM ZWAMESSAGE WHERE Z_PK = ?`, res.PK).Scan(&msgType); err != nil || msgType != 14 {
		t.Errorf("type = %d, %v", msgType, err)
	}
	if _, err := os.Stat(filepath.Join(root, wantRel)); !os.IsNotExist(err) {
		t.Errorf("revoked file still present: %v", err)
	}
	if err := queryDB(t, w, `SELECT COUNT(*) FROM messages_fts WHERE rowid = ?`, res.PK).Scan(&n); err != nil || n != 0 {
		t.Errorf("fts after revoke = %d, %v", n, err)
	}
}

func TestApplyMediaDownloadFailureSkips(t *testing.T) {
	w, root := newTestWriter(t)
	w.download = func(context.Context, downloadable) ([]byte, error) {
		return nil, errors.New("cdn said no")
	}
	res, err := w.Apply(context.Background(), decide(imageEvent("M1", ""), nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != actionSkip || !strings.Contains(res.Reason, "download failed") {
		t.Fatalf("res = %+v", res)
	}
	// No row, no file: the next backup brings both together.
	var n int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM ZWAMESSAGE`).Scan(&n); err != nil || n != 0 {
		t.Errorf("messages = %d, %v", n, err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, backup.UnenrichedDir, "media"))
	if len(entries) != 0 {
		t.Errorf("unexpected queued files: %v", entries)
	}
}

func TestMediaPathFor(t *testing.T) {
	cases := []struct {
		pk      int64
		msgType int
		local   string
		want    string
	}{
		{7, 1, "Media/x/a/b/uuid.jpg", filepath.Join(backup.UnenrichedDir, "media", "7.jpg")},
		{8, 3, "Media/x/a/b/uuid.opus", filepath.Join(backup.UnenrichedDir, "voice", "8.opus")},
		{9, 8, "Media/x/a/b/uuid.docx", filepath.Join(backup.UnenrichedDir, "documents", "9.docx")},
		{10, 0, "", ""},
		{11, 14, "", ""},
	}
	for _, c := range cases {
		if got := mediaPathFor(c.pk, c.msgType, c.local); got != c.want {
			t.Errorf("mediaPathFor(%d, %d, %q) = %q, want %q", c.pk, c.msgType, c.local, got, c.want)
		}
	}
}
