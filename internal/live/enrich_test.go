package live

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"whatskept/internal/backup"
	"whatskept/internal/enrich"
)

// fakeOpenRouter serves /key and answers every chat completion with an
// image-style result, counting the paid calls.
func fakeOpenRouter(t *testing.T, status int, body string) (*enrich.Client, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/key", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &enrich.Client{
		APIKey:     "test-key",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		MaxRetries: 1,
		Sleep:      func(ctx context.Context, d time.Duration) bool { return true },
	}, &calls
}

const imageOK = `{"choices":[{"message":{"content":"TEXT: total 45\n\nDESCRIPTION: a receipt"}}],"usage":{"cost":0.001}}`

// enrichFixture builds a workspace where pk 1 is a live-captured image
// (in the ledger, file queued) and pk 99 is a backlog image (queued,
// not in the ledger).
func enrichFixture(t *testing.T) (*Writer, *enricher, string) {
	t.Helper()
	w, root := newTestWriter(t)
	w.download = func(context.Context, downloadable) ([]byte, error) { return []byte("jpgbytes"), nil }
	if _, err := w.Apply(context.Background(), decide(imageEvent("LIVE1", "sunset"), nil)); err != nil {
		t.Fatal(err)
	}

	db, err := openRW(w.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := enrich.EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO ZWAMESSAGE (Z_PK, ZSTANZAID, ZMESSAGETYPE) VALUES (99, 'BACKLOG', 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := os.WriteFile(filepath.Join(root, backup.UnenrichedDir, "media", "99.jpg"), []byte("backlogjpg"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &enricher{root: root, dbPath: w.dbPath, nudge: make(chan struct{}, 1)}
	return w, e, root
}

func TestSweepEnrichesOnlyLiveCaptures(t *testing.T) {
	w, e, root := enrichFixture(t)
	client, calls := fakeOpenRouter(t, 200, imageOK)
	e.c = client

	e.sweep(context.Background())

	// The live capture (pk 1) is enriched: row present, file gone, FTS
	// folds caption + ocr + description, cost accounted.
	var ocr, desc string
	if err := queryDB(t, w, `SELECT ocr_text, description FROM wa_image_text WHERE rowid = 1`).Scan(&ocr, &desc); err != nil {
		t.Fatalf("wa_image_text row 1: %v", err)
	}
	if ocr != "total 45" || desc != "a receipt" {
		t.Errorf("ocr=%q desc=%q", ocr, desc)
	}
	if _, err := os.Stat(filepath.Join(root, backup.UnenrichedDir, "media", "1.jpg")); !os.IsNotExist(err) {
		t.Errorf("live file still queued: %v", err)
	}
	var fts string
	if err := queryDB(t, w, `SELECT text FROM messages_fts WHERE rowid = 1`).Scan(&fts); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"total 45", "a receipt", "sunset"} {
		if !strings.Contains(fts, want) {
			t.Errorf("fts %q missing %q", fts, want)
		}
	}

	// The backlog (pk 99) is untouched: no row, file still there, and
	// exactly ONE paid call was made.
	var n int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM wa_image_text WHERE rowid = 99`).Scan(&n); err != nil || n != 0 {
		t.Errorf("backlog was enriched: n=%d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(root, backup.UnenrichedDir, "media", "99.jpg")); err != nil {
		t.Errorf("backlog file touched: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("paid calls = %d, want 1", calls.Load())
	}
	if e.enriched != 1 || e.costUSD == 0 {
		t.Errorf("stats: enriched=%d cost=%f", e.enriched, e.costUSD)
	}

	// The attempt log recorded the live enrichment.
	logText, err := os.ReadFile(filepath.Join(root, backup.UnenrichedDir, enrich.LogName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logText), "images 1 enriched (live)") {
		t.Errorf("enrich.log = %q", logText)
	}
}

func TestSweepQuarantinesPermanentFailure(t *testing.T) {
	w, e, root := enrichFixture(t)
	client, _ := fakeOpenRouter(t, 400, `{"error":{"code":400,"message":"bad image"}}`)
	e.c = client

	e.sweep(context.Background())

	if _, err := os.Stat(filepath.Join(root, backup.UnenrichedDir, "media", "failed", "1.jpg")); err != nil {
		t.Errorf("file not quarantined: %v", err)
	}
	var n int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM wa_image_text`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows = %d, %v", n, err)
	}
	if e.failed != 1 || e.stopped != "" {
		t.Errorf("failed=%d stopped=%q", e.failed, e.stopped)
	}
}

func TestSweepStopsOnHardError(t *testing.T) {
	_, e, root := enrichFixture(t)
	client, _ := fakeOpenRouter(t, 402, `{"error":{"code":402,"message":"insufficient credits"}}`)
	e.c = client

	e.sweep(context.Background())

	if e.stopped == "" {
		t.Error("hard error did not stop the enricher")
	}
	// The file stays queued for a later `whatskept enrich`.
	if _, err := os.Stat(filepath.Join(root, backup.UnenrichedDir, "media", "1.jpg")); err != nil {
		t.Errorf("file missing after hard stop: %v", err)
	}
}

func TestSweepTransientKeepsQueued(t *testing.T) {
	w, e, root := enrichFixture(t)
	client, _ := fakeOpenRouter(t, 503, `{"error":{"code":503,"message":"overloaded"}}`)
	e.c = client

	e.sweep(context.Background())

	if _, err := os.Stat(filepath.Join(root, backup.UnenrichedDir, "media", "1.jpg")); err != nil {
		t.Errorf("file missing after transient failure: %v", err)
	}
	var n int
	if err := queryDB(t, w, `SELECT COUNT(*) FROM wa_image_text`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows = %d, %v", n, err)
	}
	if e.stopped != "" || e.failed != 0 {
		t.Errorf("stopped=%q failed=%d, want running with nothing failed", e.stopped, e.failed)
	}
}

// TestPendingRemovesLeftovers: a file whose enrichment already exists
// (crash between commit and delete) is removed without an API call.
func TestPendingRemovesLeftovers(t *testing.T) {
	w, e, root := enrichFixture(t)
	client, calls := fakeOpenRouter(t, 200, imageOK)
	e.c = client
	db, err := openRW(w.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO wa_image_text (rowid, ocr_text, description, language, source, model, generated_at)
		VALUES (1, 'x', 'y', '', 'cloud', 'm', 't')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	e.sweep(context.Background())

	if _, err := os.Stat(filepath.Join(root, backup.UnenrichedDir, "media", "1.jpg")); !os.IsNotExist(err) {
		t.Errorf("leftover file not removed: %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("paid calls = %d, want 0", calls.Load())
	}
}
