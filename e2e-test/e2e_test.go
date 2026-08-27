//go:build e2e

// Real end-to-end tests against the real WhatsApp account. Never part
// of a plain `go test ./...` — the build tag hides this file until you
// ask, passing the test workspace explicitly:
//
//	go test -tags e2e ./e2e-test -workspace /path/to/test-workspace
//
// Prerequisites:
//   - the tester's second number paired: `go run ./cmd/whatsapp-tester pair`
//     (session lives at e2e-test/session.db, gitignored)
//   - `whatskept live` RUNNING in the test workspace — the suite sends
//     real messages and waits for live to capture them.
//
// Hard rule: e2e traffic is strictly between the tester number and the
// workspace's own account, in chats the suite creates. The destination
// is DERIVED from the workspace's binding — there is no way to
// configure a third party.
package e2etest

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var workspaceFlag = flag.String("workspace", "", "whatskept workspace `whatskept live` is running in")

type config struct {
	// Workspace is the whatskept workspace under test; assertions read
	// its ChatStorage.sqlite (read-only).
	Workspace string
	// Chat is the JID the tester sends into — always the workspace
	// account's own number, derived from its binding.
	Chat string
}

func loadConfig(t *testing.T) config {
	t.Helper()
	if *workspaceFlag == "" {
		t.Fatal("pass the test workspace: go test -tags e2e ./e2e-test -workspace <dir>")
	}
	ws := *workspaceFlag
	if !filepath.IsAbs(ws) {
		// The test binary runs with cwd = e2e-test/, but a relative
		// path on the command line was typed from the repo root.
		ws = filepath.Join("..", ws)
	}
	settings, err := os.ReadFile(filepath.Join(ws, ".whatskept", "settings.json"))
	if err != nil {
		t.Fatalf("read workspace settings: %v", err)
	}
	var s struct {
		WhatsAppNumber string `json:"whatsapp_number"`
	}
	if err := json.Unmarshal(settings, &s); err != nil {
		t.Fatalf("parse workspace settings: %v", err)
	}
	if s.WhatsAppNumber == "" {
		t.Fatal("workspace is not bound to a WhatsApp number — import (or pair live) first")
	}
	return config{
		Workspace: ws,
		Chat:      strings.TrimPrefix(s.WhatsAppNumber, "+") + "@s.whatsapp.net",
	}
}

// tester runs the whatsapp-tester CLI from the repo root and returns
// its combined output.
func tester(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"run", "./cmd/whatsapp-tester"}, args...)...)
	cmd.Dir = ".."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("whatsapp-tester %v: %v\n%s", args, err, out)
	}
	return string(out)
}

var sentID = regexp.MustCompile(`sent id=(\S+)`)

// openWorkspaceDB opens the test workspace's database read-only.
func openWorkspaceDB(t *testing.T, cfg config) *sql.DB {
	t.Helper()
	path := filepath.Join(cfg.Workspace, "ChatStorage.sqlite")
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// waitFor polls cond every 2s until it returns true or the deadline
// passes. Everything asserted here depends on the running live process
// having captured an event, so patience is the assertion.
func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s never happened — is `whatskept live` running in the workspace?", desc)
}

// waitForRow polls until the stanza ID appears in ZWAMESSAGE, returning
// the row's PK.
func waitForRow(t *testing.T, db *sql.DB, stanzaID string) int64 {
	t.Helper()
	var pk int64
	waitFor(t, "message "+stanzaID+" appearing in the workspace DB", func() bool {
		err := db.QueryRow(`SELECT Z_PK FROM ZWAMESSAGE WHERE ZSTANZAID = ?`, stanzaID).Scan(&pk)
		if err != nil && err != sql.ErrNoRows {
			t.Fatalf("query workspace DB: %v", err)
		}
		return err == nil
	})
	return pk
}

// sendText sends a uniquely stamped text and returns its stanza ID and
// the text.
func sendText(t *testing.T, cfg config, label string) (stanza, text string) {
	t.Helper()
	text = fmt.Sprintf("whatskept e2e %s %d", label, time.Now().UnixNano())
	out := tester(t, "send-text", cfg.Chat, text)
	m := sentID.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no stanza ID in tester output: %s", out)
	}
	return m[1], text
}

// TestSendTextCaptured drives the full loop: the tester's second
// number sends a real text message, `whatskept live` (running against
// the workspace) captures it, and the row lands in the database with
// the right text, an FTS entry, and a wa_live_pk ledger mark.
func TestSendTextCaptured(t *testing.T) {
	cfg := loadConfig(t)
	stanza, text := sendText(t, cfg, "text")
	t.Logf("sent %s", stanza)

	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	var gotText string
	if err := db.QueryRow(`SELECT ZTEXT FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&gotText); err != nil {
		t.Fatal(err)
	}
	if gotText != text {
		t.Errorf("ZTEXT = %q, want %q", gotText, text)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND text = ?`, pk, text).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts rows = %d, %v", n, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM wa_live_pk WHERE pk = ?`, pk).Scan(&n); err != nil || n != 1 {
		t.Errorf("wa_live_pk rows = %d, %v", n, err)
	}
}

// TestEditCaptured: send, edit, and expect the row's text (and FTS) to
// become the new text.
func TestEditCaptured(t *testing.T) {
	cfg := loadConfig(t)
	stanza, _ := sendText(t, cfg, "edit-me")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	newText := fmt.Sprintf("whatskept e2e edited %d", time.Now().UnixNano())
	tester(t, "edit", cfg.Chat, stanza, newText)

	waitFor(t, "edit applied", func() bool {
		var text sql.NullString
		if err := db.QueryRow(`SELECT ZTEXT FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&text); err != nil {
			t.Fatal(err)
		}
		return text.Valid && text.String == newText
	})
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND text = ?`, pk, newText).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts rows for edited text = %d, %v", n, err)
	}
}

// TestRevokeCaptured: send, revoke, and expect the tombstone — type 14,
// text NULL, FTS row gone.
func TestRevokeCaptured(t *testing.T) {
	cfg := loadConfig(t)
	stanza, _ := sendText(t, cfg, "revoke-me")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	tester(t, "revoke", cfg.Chat, stanza)

	waitFor(t, "revoke applied", func() bool {
		var msgType int
		if err := db.QueryRow(`SELECT ZMESSAGETYPE FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&msgType); err != nil {
			t.Fatal(err)
		}
		return msgType == 14
	})
	var text sql.NullString
	if err := db.QueryRow(`SELECT ZTEXT FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&text); err != nil || text.Valid {
		t.Errorf("ZTEXT after revoke = %+v, want NULL (%v)", text, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ?`, pk).Scan(&n); err != nil || n != 0 {
		t.Errorf("fts rows after revoke = %d, %v", n, err)
	}
}

// sendMedia sends a fixture via the given tester subcommand and
// returns the stanza ID.
func sendMedia(t *testing.T, cfg config, subcmd, fixture string, extra ...string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	out := tester(t, append([]string{subcmd, cfg.Chat, abs}, extra...)...)
	m := sentID.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no stanza ID in %s output: %s", subcmd, out)
	}
	return m[1]
}

// assertQueuedFile checks the captured blob sits in the enrichment
// queue with exactly the fixture's bytes.
func assertQueuedFile(t *testing.T, cfg config, rel, fixture string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(cfg.Workspace, rel))
	if err != nil {
		t.Fatalf("queued file: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("queued file differs from fixture: %d vs %d bytes", len(got), len(want))
	}
}

// TestImageCaptured: image with caption → ZWAMEDIAITEM row, caption in
// ZTITLE and FTS, file queued under .unenriched/media/<pk>.jpg.
func TestImageCaptured(t *testing.T) {
	cfg := loadConfig(t)
	caption := fmt.Sprintf("whatskept e2e image %d", time.Now().UnixNano())
	stanza := sendMedia(t, cfg, "send-image", "image.jpg", caption)
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	var title, localPath string
	if err := db.QueryRow(`SELECT ZTITLE, ZMEDIALOCALPATH FROM ZWAMEDIAITEM WHERE ZMESSAGE = ?`, pk).Scan(&title, &localPath); err != nil {
		t.Fatal(err)
	}
	if title != caption || !strings.HasSuffix(localPath, ".jpg") {
		t.Errorf("media item: title=%q path=%q", title, localPath)
	}
	assertQueuedFile(t, cfg, filepath.Join(".unenriched", "media", fmt.Sprintf("%d.jpg", pk)), "image.jpg")
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages_fts WHERE rowid = ? AND text = ?`, pk, caption).Scan(&n); err != nil || n != 1 {
		t.Errorf("fts rows for caption = %d, %v", n, err)
	}
}

// TestVoiceCaptured: PTT voice note → duration recorded, file queued
// under .unenriched/voice/<pk>.opus.
func TestVoiceCaptured(t *testing.T) {
	cfg := loadConfig(t)
	stanza := sendMedia(t, cfg, "send-voice", "voice.opus")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	var duration int
	var localPath string
	if err := db.QueryRow(`SELECT ZMOVIEDURATION, ZMEDIALOCALPATH FROM ZWAMEDIAITEM WHERE ZMESSAGE = ?`, pk).Scan(&duration, &localPath); err != nil {
		t.Fatal(err)
	}
	if duration != 2 || !strings.HasSuffix(localPath, ".opus") {
		t.Errorf("media item: duration=%d path=%q", duration, localPath)
	}
	assertQueuedFile(t, cfg, filepath.Join(".unenriched", "voice", fmt.Sprintf("%d.opus", pk)), "voice.opus")
}

// TestPDFCaptured: document → original filename in ZAUTHORNAME (and
// FTS), file queued under .unenriched/documents/<pk>.pdf.
func TestPDFCaptured(t *testing.T) {
	cfg := loadConfig(t)
	stanza := sendMedia(t, cfg, "send-pdf", "doc.pdf")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)

	var author, localPath string
	if err := db.QueryRow(`SELECT ZAUTHORNAME, ZMEDIALOCALPATH FROM ZWAMEDIAITEM WHERE ZMESSAGE = ?`, pk).Scan(&author, &localPath); err != nil {
		t.Fatal(err)
	}
	if author != "doc.pdf" || !strings.HasSuffix(localPath, ".pdf") {
		t.Errorf("media item: author=%q path=%q", author, localPath)
	}
	assertQueuedFile(t, cfg, filepath.Join(".unenriched", "documents", fmt.Sprintf("%d.pdf", pk)), "doc.pdf")
}

// TestImageRevokeCleans: revoking a captured image tombstones the row
// AND removes the queued file.
func TestImageRevokeCleans(t *testing.T) {
	cfg := loadConfig(t)
	stanza := sendMedia(t, cfg, "send-image", "image.jpg")
	db := openWorkspaceDB(t, cfg)
	pk := waitForRow(t, db, stanza)
	queued := filepath.Join(cfg.Workspace, ".unenriched", "media", fmt.Sprintf("%d.jpg", pk))
	if _, err := os.Stat(queued); err != nil {
		t.Fatalf("queued file missing before revoke: %v", err)
	}

	tester(t, "revoke", cfg.Chat, stanza)

	waitFor(t, "media revoke applied", func() bool {
		var msgType int
		if err := db.QueryRow(`SELECT ZMESSAGETYPE FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&msgType); err != nil {
			t.Fatal(err)
		}
		return msgType == 14
	})
	if _, err := os.Stat(queued); !os.IsNotExist(err) {
		t.Errorf("queued file still present after revoke: %v", err)
	}
}

// TestReplyCaptured: send A, reply to it, and expect the reply's
// ZPARENTMESSAGE to resolve to A's PK.
func TestReplyCaptured(t *testing.T) {
	cfg := loadConfig(t)
	stanzaA, _ := sendText(t, cfg, "quote-me")
	db := openWorkspaceDB(t, cfg)
	pkA := waitForRow(t, db, stanzaA)

	replyText := fmt.Sprintf("whatskept e2e reply %d", time.Now().UnixNano())
	out := tester(t, "reply", cfg.Chat, stanzaA, replyText)
	m := sentID.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no stanza ID in reply output: %s", out)
	}
	pkB := waitForRow(t, db, m[1])

	var text string
	var parent sql.NullInt64
	if err := db.QueryRow(`SELECT ZTEXT, ZPARENTMESSAGE FROM ZWAMESSAGE WHERE Z_PK = ?`, pkB).Scan(&text, &parent); err != nil {
		t.Fatal(err)
	}
	if text != replyText {
		t.Errorf("reply ZTEXT = %q, want %q", text, replyText)
	}
	if !parent.Valid || parent.Int64 != pkA {
		t.Errorf("ZPARENTMESSAGE = %+v, want %d", parent, pkA)
	}
}
