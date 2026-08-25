package mcpserve

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"whatskept/internal/views"
)

// writeFixtureDB builds a view-applied ChatStorage.sqlite with one DM
// chat: an incoming text, an outgoing reply, and a PDF document.
func writeFixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ChatStorage.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
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
			(1, '971501111111@s.whatsapp.net', NULL, 'Sarah', 3, 700000000, 0);
		INSERT INTO ZWAMESSAGE VALUES
			(1, 1, 700000000, 0, 0, 'lunch at the cafe tomorrow?', NULL, 's1', '971501111111@s.whatsapp.net', NULL),
			(2, 1, 700000100, 1, 0, 'sounds good', 1, 's2', NULL, NULL),
			(3, 1, 700000200, 0, 8, NULL, NULL, 's3', '971501111111@s.whatsapp.net', NULL);
		INSERT INTO ZWAMEDIAITEM VALUES
			(10, 3, 'Media/chat/scan.pdf', 'passport scan.pdf', 12345, NULL, NULL);`
	if _, err := db.Exec(fixture); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := views.Apply(path); err != nil {
		t.Fatal(err)
	}
	return path
}

// connect runs the server over an in-memory transport and returns a
// connected client session.
func connect(t *testing.T, dbPath string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	go newMCPServer(dbPath).Run(ctx, serverT)
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// call invokes a tool and decodes its structured JSON output into out.
func call(t *testing.T, sess *mcp.ClientSession, tool string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError || out == nil {
		return res
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: decode structured content: %v", tool, err)
	}
	return res
}

func errText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, " ")
}

func TestGetSchema(t *testing.T) {
	sess := connect(t, writeFixtureDB(t))
	var out schemaOut
	call(t, sess, "get_schema", nil, &out)
	for _, want := range []string{"CREATE VIEW v_messages", "CREATE VIEW v_chats", "messages_fts", "ZWAMESSAGE"} {
		if !strings.Contains(out.Schema, want) {
			t.Errorf("schema missing %q", want)
		}
	}
}

func TestQuery(t *testing.T) {
	sess := connect(t, writeFixtureDB(t))
	var out queryOut
	call(t, sess, "query",
		map[string]any{"sql": "SELECT sender_name, text FROM v_messages ORDER BY rowid"}, &out)
	if len(out.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(out.Rows))
	}
	if out.Rows[0][0] != "Sarah" || out.Rows[1][0] != "me" {
		t.Errorf("sender resolution: %v", out.Rows)
	}
	if out.Truncated {
		t.Error("unexpected truncation")
	}
}

func TestQueryLimitTruncates(t *testing.T) {
	sess := connect(t, writeFixtureDB(t))
	var out queryOut
	call(t, sess, "query",
		map[string]any{"sql": "SELECT rowid FROM v_messages", "limit": 1}, &out)
	if len(out.Rows) != 1 || !out.Truncated {
		t.Errorf("rows = %d truncated = %v, want 1/true", len(out.Rows), out.Truncated)
	}
}

func TestQueryRejectsWrites(t *testing.T) {
	sess := connect(t, writeFixtureDB(t))
	res := call(t, sess, "query",
		map[string]any{"sql": "DELETE FROM wa_contact"}, nil)
	if !res.IsError {
		t.Fatal("write must be rejected")
	}
}

func TestSearch(t *testing.T) {
	sess := connect(t, writeFixtureDB(t))
	var out searchOut
	call(t, sess, "search", map[string]any{"query": "café"}, &out)
	if len(out.Hits) != 1 {
		t.Fatalf("hits = %d, want 1", len(out.Hits))
	}
	h := out.Hits[0]
	if h.Rowid != 1 || h.Sender != "Sarah" || h.Chat != "Sarah" {
		t.Errorf("hit = %+v", h)
	}
	if !strings.Contains(h.Snippet, "[[cafe]]") {
		t.Errorf("snippet = %q", h.Snippet)
	}

	// Document filename surface, COALESCEd into text.
	out = searchOut{}
	call(t, sess, "search", map[string]any{"query": "passport"}, &out)
	if len(out.Hits) != 1 || out.Hits[0].Rowid != 3 || out.Hits[0].Text != "passport scan.pdf" {
		t.Errorf("document hit = %+v", out.Hits)
	}
}

func TestToolsWithoutDatabase(t *testing.T) {
	sess := connect(t, filepath.Join(t.TempDir(), "ChatStorage.sqlite"))
	res := call(t, sess, "query", map[string]any{"sql": "SELECT 1"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "whatskept import") {
		t.Errorf("want import hint, got %q", errText(res))
	}
}

// TestHTTPTransport drives a real streamable-HTTP client session
// against the handler, with token auth in all its forms.
func TestHTTPTransport(t *testing.T) {
	const token = "sekrit"
	ts := httptest.NewServer(newHandler(newMCPServer(writeFixtureDB(t)), token))
	t.Cleanup(ts.Close)

	// With a token set, /mcp itself is not served.
	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("bare /mcp with token set: status %d, want 404", resp.StatusCode)
	}

	// Full MCP session via the token-in-path variant.
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(
		context.Background(),
		&mcp.StreamableClientTransport{Endpoint: ts.URL + "/" + token + "/mcp"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	var out searchOut
	call(t, sess, "search", map[string]any{"query": "café"}, &out)
	if len(out.Hits) != 1 || out.Hits[0].Rowid != 1 {
		t.Errorf("hits over HTTP = %+v", out.Hits)
	}
}

// TestServeRequiresToken: no token, no server — one mode only.
func TestServeRequiresToken(t *testing.T) {
	err := Serve(context.Background(), writeFixtureDB(t), "127.0.0.1:0", "")
	if err == nil || !strings.Contains(err.Error(), TokenEnv) {
		t.Errorf("err = %v, want mention of %s", err, TokenEnv)
	}
}
