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

// waitForRow polls until the stanza ID appears in ZWAMESSAGE or the
// deadline passes, returning the row's PK.
func waitForRow(t *testing.T, db *sql.DB, stanzaID string) int64 {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var pk int64
		err := db.QueryRow(`SELECT Z_PK FROM ZWAMESSAGE WHERE ZSTANZAID = ?`, stanzaID).Scan(&pk)
		if err == nil {
			return pk
		}
		if err != sql.ErrNoRows {
			t.Fatalf("query workspace DB: %v", err)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("message %s never appeared in the workspace DB — is `whatskept live` running in the workspace?", stanzaID)
	return 0
}

// TestSendTextCaptured drives the full loop: the tester's second
// number sends a real text message, `whatskept live` (running against
// the workspace) captures it, and the row lands in the database with
// the right text, an FTS entry, and a wa_live_pk ledger mark.
func TestSendTextCaptured(t *testing.T) {
	cfg := loadConfig(t)
	text := fmt.Sprintf("whatskept e2e text %d", time.Now().UnixNano())

	out := tester(t, "send-text", cfg.Chat, text)
	m := sentID.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no stanza ID in tester output: %s", out)
	}
	stanza := m[1]
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
