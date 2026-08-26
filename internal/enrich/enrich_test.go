package enrich

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"whatskept/internal/views"
)

// writeWorkspace builds a workspace root: a ChatStorage.sqlite with the
// full Z* fixture schema (so views.Apply works) holding messages with
// the given rowids, and empty .unenriched/ kind directories.
func writeWorkspace(t *testing.T, rowids ...int64) string {
	t.Helper()
	root := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(root, "ChatStorage.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	schema := `
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
		);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	for _, id := range rowids {
		if _, err := db.Exec(
			`INSERT INTO ZWAMESSAGE VALUES (?, 1, 700000000, 0, 1, NULL, NULL, 's', 'x@s.whatsapp.net', NULL)`, id); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	for _, d := range []string{"media", "voice", "documents"} {
		if err := os.MkdirAll(filepath.Join(root, ".unenriched", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func writeQueued(t *testing.T, root, kindDir, name string) string {
	t.Helper()
	path := filepath.Join(root, ".unenriched", kindDir, name)
	if err := os.WriteFile(path, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// perModel answers /chat/completions by the requested model: images get
// a TEXT/DESCRIPTION reply, voice a transcript, documents a file
// annotation. calls counts every completion request.
func perModel(calls *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Model {
		case ImageModel:
			chatOK(w, "TEXT: total 45 AED\n\nDESCRIPTION: a cafe receipt")
		case VoiceModel:
			chatOK(w, "remember the tickets tomorrow")
		default:
			fmt.Fprint(w, `{"choices":[{"message":{"content":"ok","annotations":[`+
				`{"type":"file","file":{"content":[{"type":"text","text":"This lease agreement covers the tenancy period in detail."}]}}]}}],`+
				`"usage":{"cost":0.001}}`)
		}
	}
}

func query1(t *testing.T, dbPath, q string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
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

func run(t *testing.T, root string, c *Client, concurrency int, fts bool) (Stats, error) {
	t.Helper()
	opts := Options{Root: root, Client: c, Concurrency: concurrency}
	if fts {
		opts.RebuildFTS = views.Apply
	}
	return Run(context.Background(), opts)
}

func TestRunHappyPathWithFTS(t *testing.T) {
	root := writeWorkspace(t, 1, 2, 3)
	img := writeQueued(t, root, "media", "1.jpg")
	vo := writeQueued(t, root, "voice", "2.opus")
	doc := writeQueued(t, root, "documents", "3.pdf")
	var calls atomic.Int64
	c, _ := newTestClient(t, 200, perModel(&calls))

	stats, err := run(t, root, c, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Images.Enriched != 1 || stats.Voice.Enriched != 1 || stats.Documents.Enriched != 1 {
		t.Errorf("stats = %+v", stats)
	}
	if !stats.Drained() {
		t.Error("queue should be drained")
	}
	for _, p := range []string{img, vo, doc} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be deleted", p)
		}
	}
	dbPath := filepath.Join(root, "ChatStorage.sqlite")
	if got := query1(t, dbPath, `SELECT ocr_text FROM wa_image_text WHERE rowid = 1`); got != "total 45 AED" {
		t.Errorf("ocr_text = %q", got)
	}
	if got := query1(t, dbPath, `SELECT transcript FROM wa_voice_text WHERE rowid = 2`); got != "remember the tickets tomorrow" {
		t.Errorf("transcript = %q", got)
	}
	if got := query1(t, dbPath, `SELECT method FROM wa_document_text WHERE rowid = 3`); got != "cloud-text" {
		t.Errorf("method = %q", got)
	}
	// The rebuilt FTS must find the new text surfaces.
	for _, term := range []string{"tickets", "tenancy", "receipt"} {
		if got := query1(t, dbPath, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH '`+term+`'`); got != "1" {
			t.Errorf("MATCH %s = %s, want 1", term, got)
		}
	}
	// Cost accounting: three calls at $0.001 each, split per kind.
	if d := stats.TotalCostUSD() - 0.003; d > 1e-9 || d < -1e-9 {
		t.Errorf("total cost = %f, want 0.003", stats.TotalCostUSD())
	}
	if stats.Images.CostUSD != 0.001 || stats.Voice.CostUSD != 0.001 || stats.Documents.CostUSD != 0.001 {
		t.Errorf("per-kind cost: %+v", stats)
	}
	logText := readLog(t, root)
	for _, want := range []string{
		"credit balance at start usd=7.5000",
		"credit balance at end usd=7.5000",
		"cost_usd=0.0030",
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("log missing %q:\n%s", want, logText)
		}
	}
}

func TestRunHardAbortKeepsCommittedWork(t *testing.T) {
	root := writeWorkspace(t, 1, 2)
	writeQueued(t, root, "media", "1.jpg")
	writeQueued(t, root, "media", "2.jpg")
	var calls atomic.Int64
	c, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			chatOK(w, "TEXT: first\n\nDESCRIPTION: ok")
			return
		}
		w.WriteHeader(402)
	})

	stats, err := run(t, root, c, 1, false)
	if err == nil || ClassOf(err) != ClassHard {
		t.Fatalf("err = %v, want hard abort", err)
	}
	if stats.Images.Enriched != 1 {
		t.Errorf("committed work lost: %+v", stats.Images)
	}
	dbPath := filepath.Join(root, "ChatStorage.sqlite")
	if got := query1(t, dbPath, `SELECT COUNT(*) FROM wa_image_text`); got != "1" {
		t.Errorf("rows = %s, want 1", got)
	}
	// The failed item's file must remain queued.
	left, _ := os.ReadDir(filepath.Join(root, ".unenriched", "media"))
	if len(left) != 1 {
		t.Errorf("queue holds %d files, want 1", len(left))
	}
}

func TestRunModerationQuarantines(t *testing.T) {
	root := writeWorkspace(t, 1, 2)
	writeQueued(t, root, "media", "1.jpg")
	writeQueued(t, root, "media", "2.jpg")
	var calls atomic.Int64
	c, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"error":{"code":403,"message":"flagged","metadata":{"reasons":["x"]}}}`)
			return
		}
		chatOK(w, "TEXT: ok\n\nDESCRIPTION: fine")
	})

	stats, err := run(t, root, c, 1, false)
	if err != nil {
		t.Fatalf("moderation must not abort the run: %v", err)
	}
	if stats.Images.Failed != 1 || stats.Images.Enriched != 1 {
		t.Errorf("stats = %+v", stats.Images)
	}
	failed, _ := os.ReadDir(filepath.Join(root, ".unenriched", "media", FailedDir))
	if len(failed) != 1 || failed[0].Name() != "1.jpg" {
		t.Errorf("failed/ = %v, want [1.jpg]", failed)
	}
}

func TestRunTransientStaysQueued(t *testing.T) {
	root := writeWorkspace(t, 1)
	path := writeQueued(t, root, "voice", "1.opus")
	c, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	})
	c.MaxRetries = 2

	stats, err := run(t, root, c, 1, false)
	if err != nil {
		t.Fatalf("a single transient item must not fail the run: %v", err)
	}
	if stats.Voice.Remaining != 1 {
		t.Errorf("stats = %+v", stats.Voice)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("file must stay queued")
	}
	if stats.Drained() {
		t.Error("Drained must be false")
	}
}

func TestRunCircuitBreaker(t *testing.T) {
	ids := []int64{1, 2, 3, 4, 5, 6, 7, 8}
	root := writeWorkspace(t, ids...)
	for _, id := range ids {
		writeQueued(t, root, "voice", fmt.Sprintf("%d.opus", id))
	}
	var calls atomic.Int64
	c, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
	})
	c.MaxRetries = 1

	_, err := run(t, root, c, 1, false)
	if err == nil || !strings.Contains(err.Error(), "consecutive transient failures") {
		t.Fatalf("err = %v, want circuit-breaker abort", err)
	}
	// The breaker must stop the run well short of the full queue.
	if calls.Load() >= int64(len(ids)) {
		t.Errorf("calls = %d — breaker did not stop the churn", calls.Load())
	}
}

func TestRunEmptyTranscriptIsSuccess(t *testing.T) {
	root := writeWorkspace(t, 1)
	path := writeQueued(t, root, "voice", "1.opus")
	c, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		chatOK(w, "") // silent clip: valid, empty
	})

	stats, err := run(t, root, c, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Voice.Enriched != 1 {
		t.Errorf("stats = %+v", stats.Voice)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file must be deleted — an empty transcript must not re-queue forever")
	}
}

func TestRunSkipsOrphansAndAlreadyEnriched(t *testing.T) {
	root := writeWorkspace(t, 1) // rowid 99 will NOT exist
	orphan := writeQueued(t, root, "media", "99.jpg")
	leftover := writeQueued(t, root, "media", "1.jpg")
	dbPath := filepath.Join(root, "ChatStorage.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureSchema(db); err != nil {
		t.Fatal(err)
	}
	// rowid 1 already enriched: a crash between commit and delete left
	// the file behind.
	if _, err := db.Exec(`INSERT INTO wa_image_text (rowid, ocr_text) VALUES (1, 'done')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	var calls atomic.Int64
	c, _ := newTestClient(t, 200, perModel(&calls))
	stats, err := run(t, root, c, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Errorf("calls = %d — neither file should cost an API call", calls.Load())
	}
	if stats.Images.Orphaned != 1 || stats.Images.Enriched != 1 {
		t.Errorf("stats = %+v", stats.Images)
	}
	for _, p := range []string{orphan, leftover} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be deleted", p)
		}
	}
}

func TestRunCommitFailureKeepsFile(t *testing.T) {
	root := writeWorkspace(t, 1)
	path := writeQueued(t, root, "voice", "1.opus")
	// Pre-create wa_voice_text with a CHECK that rejects the fake
	// transcript, so the INSERT (not the API call) fails.
	dbPath := filepath.Join(root, "ChatStorage.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE wa_voice_text (
		rowid INTEGER PRIMARY KEY, transcript TEXT CHECK (transcript <> 'boom'),
		language TEXT, duration_sec REAL, model TEXT, generated_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	c, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		chatOK(w, "boom")
	})
	stats, err := run(t, root, c, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Voice.Remaining != 1 {
		t.Errorf("stats = %+v", stats.Voice)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("file must be kept when the DB write fails")
	}
}

// TestRunResumes: a failing run leaves the queue; a healthy re-run
// completes exactly the remainder.
func TestRunResumes(t *testing.T) {
	root := writeWorkspace(t, 1)
	writeQueued(t, root, "voice", "1.opus")
	cDown, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	})
	cDown.MaxRetries = 1
	if stats, err := run(t, root, cDown, 1, false); err != nil || stats.Voice.Remaining != 1 {
		t.Fatalf("first run: stats=%+v err=%v", stats, err)
	}

	var calls atomic.Int64
	cUp, _ := newTestClient(t, 200, perModel(&calls))
	stats, err := run(t, root, cUp, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Voice.Enriched != 1 || calls.Load() != 1 {
		t.Errorf("second run: stats=%+v calls=%d", stats.Voice, calls.Load())
	}
	dbPath := filepath.Join(root, "ChatStorage.sqlite")
	if got := query1(t, dbPath, `SELECT COUNT(*) FROM wa_voice_text`); got != "1" {
		t.Errorf("rows = %s, want exactly 1", got)
	}
}

func TestRunConcurrentExactlyOnce(t *testing.T) {
	var ids []int64
	for i := int64(1); i <= 20; i++ {
		ids = append(ids, i)
	}
	root := writeWorkspace(t, ids...)
	for _, id := range ids {
		writeQueued(t, root, "media", fmt.Sprintf("%d.jpg", id))
	}
	var calls atomic.Int64
	c, _ := newTestClient(t, 200, perModel(&calls))

	stats, err := run(t, root, c, 8, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Images.Enriched != 20 || calls.Load() != 20 {
		t.Errorf("enriched=%d calls=%d, want 20/20", stats.Images.Enriched, calls.Load())
	}
	dbPath := filepath.Join(root, "ChatStorage.sqlite")
	if got := query1(t, dbPath, `SELECT COUNT(*) FROM wa_image_text`); got != "20" {
		t.Errorf("rows = %s, want 20", got)
	}
}

func TestRunPreflightFailureTouchesNothing(t *testing.T) {
	root := writeWorkspace(t, 1)
	path := writeQueued(t, root, "media", "1.jpg")
	var calls atomic.Int64
	c, _ := newTestClient(t, 401, perModel(&calls))

	_, err := run(t, root, c, 1, false)
	if err == nil || ClassOf(err) != ClassHard {
		t.Fatalf("err = %v, want hard", err)
	}
	if calls.Load() != 0 {
		t.Errorf("completion calls = %d, want 0", calls.Load())
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("queue must be untouched")
	}
}

func readLog(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".unenriched", LogName))
	if err != nil {
		t.Fatalf("enrich.log: %v", err)
	}
	return string(data)
}

// The attempt log is the cross-run history an operator agent reads:
// every outcome, timestamped, appended across runs.
func TestRunWritesAttemptLog(t *testing.T) {
	root := writeWorkspace(t, 1, 2, 3)
	writeQueued(t, root, "media", "1.jpg")   // will be flagged → failed/
	writeQueued(t, root, "voice", "2.opus")  // will succeed
	writeQueued(t, root, "media", "99.jpg")  // orphan
	var calls atomic.Int64
	c, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		calls.Add(1)
		if req.Model == ImageModel {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"error":{"code":403,"message":"flagged","metadata":{"reasons":["x"]}}}`)
			return
		}
		chatOK(w, "hello there")
	})

	if _, err := run(t, root, c, 1, false); err != nil {
		t.Fatal(err)
	}
	logText := readLog(t, root)
	for _, want := range []string{
		"run start queue=3",
		"images 99 orphaned",
		"voice 2 enriched",
		"images 1 failed",
		"-> failed/",
		"run done enriched=1 failed=1 remaining=0 orphaned=1",
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("log missing %q:\n%s", want, logText)
		}
	}
	// Every line starts with an RFC3339 timestamp.
	for _, line := range strings.Split(strings.TrimSpace(logText), "\n") {
		if len(line) < 20 || line[4] != '-' || line[10] != 'T' {
			t.Errorf("line not timestamped: %q", line)
		}
	}

	// A second run appends — history survives across runs.
	writeQueued(t, root, "voice", "3.opus")
	cUp, _ := newTestClient(t, 200, perModel(new(atomic.Int64)))
	if _, err := run(t, root, cUp, 1, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(readLog(t, root), "run start"); got != 2 {
		t.Errorf("run start lines = %d, want 2 (append-only)", got)
	}
}

func TestRunLogsTransientAndAbort(t *testing.T) {
	root := writeWorkspace(t, 1)
	writeQueued(t, root, "voice", "1.opus")
	c, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	})
	c.MaxRetries = 1
	if _, err := run(t, root, c, 1, false); err != nil {
		t.Fatal(err)
	}
	logText := readLog(t, root)
	if !strings.Contains(logText, "voice 1 transient") || !strings.Contains(logText, "still queued") {
		t.Errorf("log missing transient line:\n%s", logText)
	}

	cHard, _ := newTestClient(t, 200, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
	})
	if _, err := run(t, root, cHard, 1, false); err == nil {
		t.Fatal("expected hard abort")
	}
	logText = readLog(t, root)
	if !strings.Contains(logText, "run aborted") {
		t.Error("log missing 'run aborted' line")
	}
	// The end-of-run balance must land even on an abort — the deferred
	// fetch uses a fresh context, so a cancelled run still reports it.
	if got := strings.Count(logText, "credit balance at end usd="); got != 2 {
		t.Errorf("balance-at-end lines = %d, want 2 (one per run, aborted included)", got)
	}
}

func TestRunNoDatabase(t *testing.T) {
	root := t.TempDir()
	_, err := Run(context.Background(), Options{Root: root, Client: &Client{APIKey: "k"}})
	if err == nil || !strings.Contains(err.Error(), "whatskept import") {
		t.Errorf("err = %v, want import hint", err)
	}
}
