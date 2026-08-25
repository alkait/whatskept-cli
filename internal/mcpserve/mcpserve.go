// Package mcpserve exposes the workspace database over the Model
// Context Protocol (streamable HTTP) — the only query surface of
// whatskept. Read-only by construction: the database is opened with
// mode=ro and PRAGMA query_only per tool call, and never held open, so
// a re-import can replace the file mid-serve without a restart.
//
// The server is deliberately schema-agnostic: get_schema hands the
// agent the real CREATE statements, query runs arbitrary read-only
// SQL, and search is a convenience wrapper over messages_fts.
package mcpserve

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

//go:embed instructions.md
var instructions string

const (
	queryTimeout  = 15 * time.Second
	defaultRowCap = 200
	maxRowCap     = 500
	cellByteCap   = 4_000
	resultByteCap = 256 * 1024
	defaultHits   = 25
	maxHits       = 100
)

type server struct {
	// dbPath is re-checked on every open so the file may be replaced
	// by a re-import while the server runs.
	dbPath string
}

// Serve runs the MCP server over streamable HTTP on addr until ctx is
// cancelled. dbPath is the workspace's ChatStorage.sqlite; it may be
// absent (tools report "run whatskept import first"). The token is
// required — the endpoint lives ONLY at the token-in-path URL
// /<token>/mcp, and the unguessable path is the credential. One mode,
// dev and production alike.
func Serve(ctx context.Context, dbPath, addr, token string) error {
	if token == "" {
		return errors.New("set " + TokenEnv + " — the MCP endpoint is served at /<token>/mcp and the token is required")
	}
	// Startup probe: report state but serve regardless — the workspace
	// may simply not be imported yet.
	dbState := dbPath
	s := &server{dbPath: dbPath}
	if db, err := s.openDB(); err != nil {
		dbState = fmt.Sprintf("NOT available (%v) — serving anyway", err)
	} else {
		db.Close()
	}

	httpServer := &http.Server{
		Addr:    addr,
		Handler: newHandler(newMCPServer(dbPath), token),
		// No WriteTimeout: streamable HTTP holds SSE streams open.
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()
	fmt.Printf("MCP endpoint:  http://%s/%s/mcp (the token in the path is the credential)\n", addr, token)
	fmt.Printf("Database:      %s\n", dbState)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// TokenEnv is where Serve's callers read the auth token from.
const TokenEnv = "WHATSKEPT_MCP_TOKEN"

// newHandler routes the MCP endpoint at /<token>/mcp — the unguessable
// path is the sole credential.
func newHandler(mcpServer *mcp.Server, token string) http.Handler {
	// The SDK's DNS-rebinding protection 403s requests that arrive on a
	// localhost address with a non-localhost Host header — which is
	// exactly how every tunnel (cloudflared, ssh -R) delivers traffic.
	// The token path is the gate, so lift it.
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true},
	)
	mux := http.NewServeMux()
	mux.Handle("/"+token+"/mcp", handler)
	return mux
}

// newMCPServer builds the server with its three tools registered —
// split from Serve so tests can run it over an in-memory transport.
func newMCPServer(dbPath string) *mcp.Server {
	s := &server{dbPath: dbPath}

	mcpServer := mcp.NewServer(
		&mcp.Implementation{Name: "whatskept", Title: "WhatsKept", Version: "dev"},
		&mcp.ServerOptions{Instructions: instructions},
	)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "get_schema",
		Description: "Return the database schema: every CREATE TABLE / CREATE VIEW / " +
			"CREATE VIRTUAL TABLE statement. Call this once before writing SQL — the " +
			"views (v_messages, v_chats, messages_fts, ...) are the intended query surface.",
	}, s.getSchema)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "query",
		Description: "Run a read-only SQL statement against the WhatsApp workspace database " +
			"and return rows as JSON. Writes are blocked at the connection level. Always " +
			"aggregate or LIMIT; results are capped at " + fmt.Sprint(maxRowCap) + " rows.",
	}, s.query)
	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "search",
		Description: "Full-text search across ALL surfaces — typed messages, image OCR and " +
			"descriptions, voice-note transcripts, document filenames and PDF body text. " +
			"FTS5 syntax: \"exact phrase\", AND, OR, NOT, NEAR/3, prefix*. Use this first " +
			"for any 'did anyone mention X' question; fall back to query for joins and aggregates.",
	}, s.search)

	return mcpServer
}

// openDB opens the database read-only for a single tool call. The
// caller must Close it.
func (s *server) openDB() (*sql.DB, error) {
	if info, err := os.Stat(s.dbPath); err != nil {
		return nil, errors.New("the workspace has no database yet — run `whatskept import <ios-backup-path>` first")
	} else if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not a database", s.dbPath)
	}
	dsn := "file:" + s.dbPath + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return db, nil
}

// existingTables reports which of the named tables/views exist, so
// search can adapt to a workspace whose enrichment hasn't run yet.
// Checked per call: a re-import or enrichment run may add tables and
// the server should pick that up without a restart.
func existingTables(ctx context.Context, db *sql.DB, names []string) (map[string]bool, error) {
	placeholders := strings.TrimRight(strings.Repeat("?,", len(names)), ",")
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type IN ('table','view') AND name IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// --- get_schema ---------------------------------------------------------

type schemaOut struct {
	Schema string `json:"schema" jsonschema:"the full DDL: CREATE TABLE / VIEW / VIRTUAL TABLE statements, semicolon-separated"`
}

func (s *server) getSchema(ctx context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, schemaOut, error) {
	db, err := s.openDB()
	if err != nil {
		return nil, schemaOut{}, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT sql FROM sqlite_master WHERE sql IS NOT NULL
		 ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'view' THEN 1 ELSE 2 END, name`)
	if err != nil {
		return nil, schemaOut{}, err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var ddl string
		if err := rows.Scan(&ddl); err != nil {
			return nil, schemaOut{}, err
		}
		b.WriteString(ddl)
		b.WriteString(";\n\n")
	}
	return nil, schemaOut{Schema: b.String()}, rows.Err()
}

// --- query --------------------------------------------------------------

type queryIn struct {
	SQL   string `json:"sql" jsonschema:"a single read-only SQL statement (SELECT / WITH); always aggregate or LIMIT"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum rows to return, default 200, max 500"`
}

type queryOut struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	Truncated bool     `json:"truncated,omitempty" jsonschema:"true when the row or byte cap cut the result short — narrow the query"`
}

func (s *server) query(ctx context.Context, _ *mcp.CallToolRequest, in queryIn) (*mcp.CallToolResult, queryOut, error) {
	db, err := s.openDB()
	if err != nil {
		return nil, queryOut{}, err
	}
	defer db.Close()

	limit := in.Limit
	if limit <= 0 {
		limit = defaultRowCap
	}
	if limit > maxRowCap {
		limit = maxRowCap
	}

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	rows, err := db.QueryContext(ctx, in.SQL)
	if err != nil {
		return nil, queryOut{}, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, queryOut{}, err
	}

	out := queryOut{Columns: cols, Rows: [][]any{}}
	scan := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range scan {
		ptrs[i] = &scan[i]
	}
	sizeBudget := resultByteCap
	for rows.Next() {
		if len(out.Rows) >= limit {
			out.Truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, queryOut{}, err
		}
		row := make([]any, len(cols))
		for i, v := range scan {
			cell, n := normalizeCell(v)
			row[i] = cell
			sizeBudget -= n
		}
		out.Rows = append(out.Rows, row)
		if sizeBudget <= 0 {
			out.Truncated = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, queryOut{}, err
	}
	return nil, out, nil
}

// normalizeCell makes a driver value JSON-friendly and returns an
// approximate encoded size for the result byte cap. Long text cells are
// clipped so one verbatim OCR blob can't eat the whole budget.
func normalizeCell(v any) (any, int) {
	switch t := v.(type) {
	case nil:
		return nil, 4
	case []byte:
		return clipString(string(t))
	case string:
		return clipString(t)
	case time.Time:
		s := t.UTC().Format(time.RFC3339)
		return s, len(s)
	default:
		return t, 20
	}
}

func clipString(s string) (string, int) {
	if len(s) > cellByteCap {
		return s[:cellByteCap] + "…[clipped]", cellByteCap
	}
	return s, len(s)
}

// --- search -------------------------------------------------------------

type searchIn struct {
	Query string `json:"query" jsonschema:"FTS5 match expression, e.g. 'kitchen AND budget', '\"exact phrase\"', 'pizz*'"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum hits, default 25, max 100"`
}

type searchHit struct {
	Rowid   int64  `json:"rowid"`
	Ts      string `json:"ts"`
	Chat    string `json:"chat"`
	Sender  string `json:"sender"`
	Type    string `json:"type" jsonschema:"message type: text, image, audio, document, link, ..."`
	Text    string `json:"text" jsonschema:"the matched surface: typed text, image description/OCR, voice transcript, document text, or filename"`
	Snippet string `json:"snippet" jsonschema:"FTS snippet with [[ ]] around matched terms"`
}

type searchOut struct {
	Hits []searchHit `json:"hits"`
}

func (s *server) search(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, searchOut, error) {
	db, err := s.openDB()
	if err != nil {
		return nil, searchOut{}, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	has, err := existingTables(ctx, db, []string{
		"messages_fts", "v_messages", "wa_image_text", "wa_voice_text", "wa_document", "wa_document_text",
	})
	if err != nil {
		return nil, searchOut{}, err
	}
	if !has["v_messages"] || !has["messages_fts"] {
		return nil, searchOut{}, errors.New("this database has no v_messages/messages_fts — re-run `whatskept import`")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultHits
	}
	if limit > maxHits {
		limit = maxHits
	}

	// The COALESCE chain surfaces whichever text layer produced the FTS
	// hit; the optional enrichment joins are included only when the
	// tables exist (SQLite errors on a missing table, it does not null it).
	sel := []string{"NULLIF(m.text, '')"}
	joins := []string{}
	if has["wa_image_text"] {
		sel = append(sel, "NULLIF(t.description, '')", "NULLIF(t.ocr_text, '')")
		joins = append(joins, "LEFT JOIN wa_image_text t ON t.rowid = m.rowid")
	}
	if has["wa_voice_text"] {
		sel = append(sel, "NULLIF(v.transcript, '')")
		joins = append(joins, "LEFT JOIN wa_voice_text v ON v.rowid = m.rowid")
	}
	if has["wa_document_text"] {
		sel = append(sel, "NULLIF(dt.text, '')")
		joins = append(joins, "LEFT JOIN wa_document_text dt ON dt.rowid = m.rowid")
	}
	if has["wa_document"] {
		sel = append(sel, "d.filename")
		joins = append(joins, "LEFT JOIN wa_document d ON d.rowid = m.rowid")
	}
	sel = append(sel, "m.link_title", "''")

	q := fmt.Sprintf(`
		SELECT m.rowid, m.ts, m.chat_title, m.sender_name, m.message_type_name,
		       COALESCE(%s) AS hit_text,
		       snippet(messages_fts, 0, '[[', ']]', '…', 12) AS hit_snippet
		FROM   messages_fts f
		JOIN   v_messages m ON m.rowid = f.rowid
		%s
		WHERE  messages_fts MATCH ?
		ORDER  BY m.ts DESC
		LIMIT  ?`, strings.Join(sel, ",\n"), strings.Join(joins, "\n"))

	rows, err := db.QueryContext(ctx, q, in.Query, limit)
	if err != nil {
		return nil, searchOut{}, err
	}
	defer rows.Close()

	out := searchOut{Hits: []searchHit{}}
	for rows.Next() {
		var h searchHit
		var ts, chat, sender, typ, text, snip sql.NullString
		if err := rows.Scan(&h.Rowid, &ts, &chat, &sender, &typ, &text, &snip); err != nil {
			return nil, searchOut{}, err
		}
		h.Ts, h.Chat, h.Sender, h.Type = ts.String, chat.String, sender.String, typ.String
		h.Text, _ = clipString(text.String)
		h.Snippet = snip.String
		out.Hits = append(out.Hits, h)
	}
	return nil, out, rows.Err()
}
