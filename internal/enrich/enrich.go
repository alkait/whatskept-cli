package enrich

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	// defaultConcurrency is the in-flight OpenRouter request cap. The
	// original repo defaulted to a conservative 8; we run much hotter
	// because the error taxonomy absorbs pushback safely (429s retry
	// with Retry-After-aware backoff, and sustained failure trips the
	// circuit breaker). --concurrency overrides in either direction.
	defaultConcurrency = 48

	// breakerThreshold aborts the run after this many CONSECUTIVE items
	// exhaust their transient retries — that pattern is an outage, not
	// bad files, and churning the rest of the queue would waste hours.
	breakerThreshold = 5

	progressEvery = 25
)

// FailedDir is where permanently-failing files are quarantined,
// relative to the .unenriched/ queue directory. One permanent failure
// is enough — no retry will ever help those, so no retry counter is
// needed anywhere.
const FailedDir = "failed"

// LogName is the append-only attempt log at .unenriched/enrich.log:
// one timestamped line per event across all runs. Pure observability —
// the runner never reads it (the queue directory stays the only
// control state); it exists so an operator agent can reconstruct what
// happened in previous runs (e.g. spot a file that fails transiently
// every single run) and suggest the next action.
const LogName = "enrich.log"

// attemptLog appends to the run history. A log that can't be opened
// degrades to a no-op — observability must never fail a run.
type attemptLog struct{ f *os.File }

func openAttemptLog(unenriched string) *attemptLog {
	_ = os.MkdirAll(unenriched, 0o755)
	f, err := os.OpenFile(filepath.Join(unenriched, LogName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return &attemptLog{}
	}
	return &attemptLog{f: f}
}

func (l *attemptLog) line(format string, args ...any) {
	if l.f == nil {
		return
	}
	fmt.Fprintf(l.f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func (l *attemptLog) close() {
	if l.f != nil {
		l.f.Close()
	}
}

// KindStats tallies one media kind for a run.
type KindStats struct {
	Enriched  int // text committed, file deleted
	Failed    int // moved to .unenriched/failed/
	Remaining int // transient failures — still queued for the next run
	Orphaned  int // rowid no longer in the DB — file deleted
}

// Stats summarizes one Run.
type Stats struct {
	Images    KindStats
	Voice     KindStats
	Documents KindStats
	FTSRows   int
}

// Drained reports whether the queue is fully processed (nothing left
// to retry and nothing quarantined this run).
func (s Stats) Drained() bool {
	for _, k := range []KindStats{s.Images, s.Voice, s.Documents} {
		if k.Remaining > 0 || k.Failed > 0 {
			return false
		}
	}
	return true
}

// Options configures a Run.
type Options struct {
	Root        string // workspace root: <Root>/ChatStorage.sqlite, <Root>/.unenriched/
	Client      *Client
	Concurrency int          // in-flight requests; 0 = defaultConcurrency
	Log         func(string) // progress lines; nil = silent
	// RebuildFTS is called once at the end when anything was enriched
	// (the caller wires views.Apply; injected to keep packages
	// decoupled). Returns the FTS row count.
	RebuildFTS func(dbPath string) (int, error)
}

// item is one queued file.
type item struct {
	kind  string // "images", "voice", "documents"
	rowid int64
	path  string
}

// outcome is a worker's verdict on one item, applied by the collector.
type outcome struct {
	it    item
	write func(db *sql.DB) error // nil when err != nil
	err   error
}

// Run drains the .unenriched/ queue. Committed work always survives:
// a hard abort or circuit-break mid-run loses nothing already written.
func Run(ctx context.Context, opts Options) (Stats, error) {
	var stats Stats
	log := opts.Log
	if log == nil {
		log = func(string) {}
	}
	dbPath := filepath.Join(opts.Root, "ChatStorage.sqlite")
	if _, err := os.Stat(dbPath); err != nil {
		return stats, errors.New("the workspace has no database yet — run `whatskept import` first")
	}

	log("Checking OpenRouter key…")
	if err := opts.Client.Preflight(ctx); err != nil {
		return stats, err
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return stats, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := ensureSchema(db); err != nil {
		return stats, err
	}

	unenriched := filepath.Join(opts.Root, ".unenriched")
	queue, err := scanQueue(unenriched)
	if err != nil {
		return stats, err
	}
	if len(queue) == 0 {
		log("Queue is empty — nothing to enrich.")
		return stats, nil
	}
	al := openAttemptLog(unenriched)
	defer al.close()
	al.line("run start queue=%d", len(queue))

	// Drop orphans (message deleted on device / older import) and
	// already-enriched leftovers (a crash between commit and delete)
	// without spending an API call on either.
	alive, err := loadRowidSet(db, "SELECT Z_PK FROM ZWAMESSAGE")
	if err != nil {
		return stats, err
	}
	done := map[string]map[int64]bool{}
	for kind, table := range map[string]string{
		"images": "wa_image_text", "voice": "wa_voice_text", "documents": "wa_document_text",
	} {
		if done[kind], err = loadRowidSet(db, "SELECT rowid FROM "+table); err != nil {
			return stats, err
		}
	}
	var pending []item
	for _, it := range queue {
		ks := stats.kind(it.kind)
		switch {
		case !alive[it.rowid]:
			_ = os.Remove(it.path)
			ks.Orphaned++
			al.line("%s %d orphaned (message no longer in DB; file removed)", it.kind, it.rowid)
		case done[it.kind][it.rowid]:
			_ = os.Remove(it.path)
			ks.Enriched++
			al.line("%s %d enriched (already in DB; leftover file removed)", it.kind, it.rowid)
		default:
			pending = append(pending, it)
		}
	}
	log(fmt.Sprintf("Queue: %d files to enrich.", len(pending)))

	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan item)
	results := make(chan outcome, concurrency)
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				results <- process(runCtx, opts.Client, it)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, it := range pending {
			select {
			case <-runCtx.Done():
				return
			case jobs <- it:
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()

	// Collector: the only DB writer, so commits are serialized. Also
	// owns the circuit breaker and first-hard-error-wins abort.
	var hardErr error
	consecutiveTransient := 0
	processed := 0
	for res := range results {
		ks := stats.kind(res.it.kind)
		switch {
		case res.err == nil:
			if err := res.write(db); err != nil {
				// Commit failed: keep the file — it stays queued.
				log(fmt.Sprintf("%s %d: db write failed (kept in queue): %v", res.it.kind, res.it.rowid, err))
				al.line("%s %d transient db write failed: %v (still queued)", res.it.kind, res.it.rowid, err)
				ks.Remaining++
				continue
			}
			_ = os.Remove(res.it.path)
			ks.Enriched++
			al.line("%s %d enriched", res.it.kind, res.it.rowid)
			consecutiveTransient = 0
			processed++
			if processed%progressEvery == 0 {
				log(fmt.Sprintf("enriched %d/%d…", processed, len(pending)))
			}
		case ClassOf(res.err) == ClassHard:
			if hardErr == nil {
				hardErr = res.err
			}
			cancel()
		case ClassOf(res.err) == ClassPermanent:
			if err := quarantine(res.it.path); err != nil {
				log(fmt.Sprintf("%s %d: quarantine failed (kept in queue): %v", res.it.kind, res.it.rowid, err))
				al.line("%s %d transient quarantine failed: %v (still queued)", res.it.kind, res.it.rowid, err)
				ks.Remaining++
			} else {
				log(fmt.Sprintf("%s %d: permanent failure, moved to failed/: %v", res.it.kind, res.it.rowid, res.err))
				al.line("%s %d failed %v -> failed/", res.it.kind, res.it.rowid, res.err)
				ks.Failed++
			}
			consecutiveTransient = 0
		default: // transient, retries exhausted — stays queued
			if runCtx.Err() == nil {
				log(fmt.Sprintf("%s %d: transient failure (still queued): %v", res.it.kind, res.it.rowid, res.err))
				al.line("%s %d transient %v (still queued)", res.it.kind, res.it.rowid, res.err)
			}
			ks.Remaining++
			consecutiveTransient++
			if consecutiveTransient >= breakerThreshold && hardErr == nil {
				hardErr = fmt.Errorf(
					"aborting: %d consecutive transient failures — OpenRouter or the model looks down; re-run later (last: %v)",
					consecutiveTransient, res.err)
				cancel()
			}
		}
	}
	if hardErr != nil {
		al.line("run aborted: %v", hardErr)
		return stats, hardErr
	}
	if ctx.Err() != nil {
		al.line("run interrupted")
		return stats, ctx.Err()
	}

	if opts.RebuildFTS != nil && (stats.Images.Enriched+stats.Voice.Enriched+stats.Documents.Enriched) > 0 {
		log("Rebuilding full-text index…")
		db.Close() // RebuildFTS opens its own connection
		n, err := opts.RebuildFTS(dbPath)
		if err != nil {
			return stats, fmt.Errorf("rebuild FTS: %w", err)
		}
		stats.FTSRows = n
		al.line("fts rebuilt rows=%d", n)
	}
	al.line("run done enriched=%d failed=%d remaining=%d orphaned=%d",
		stats.Images.Enriched+stats.Voice.Enriched+stats.Documents.Enriched,
		stats.Images.Failed+stats.Voice.Failed+stats.Documents.Failed,
		stats.Images.Remaining+stats.Voice.Remaining+stats.Documents.Remaining,
		stats.Images.Orphaned+stats.Voice.Orphaned+stats.Documents.Orphaned)
	return stats, nil
}

// kind returns the mutable per-kind stats bucket.
func (s *Stats) kind(k string) *KindStats {
	switch k {
	case "images":
		return &s.Images
	case "voice":
		return &s.Voice
	default:
		return &s.Documents
	}
}

// process runs one item's engine call and returns the DB write to apply.
func process(ctx context.Context, c *Client, it item) outcome {
	if ctx.Err() != nil {
		return outcome{it: it, err: ctx.Err()}
	}
	data, err := os.ReadFile(it.path)
	if err != nil {
		return outcome{it: it, err: &Error{Class: ClassPermanent, Msg: "read file: " + err.Error()}}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	switch it.kind {
	case "images":
		res, err := describeImage(ctx, c, strings.TrimPrefix(filepath.Ext(it.path), "."), data)
		if err != nil {
			return outcome{it: it, err: err}
		}
		return outcome{it: it, write: func(db *sql.DB) error {
			_, err := db.Exec(`INSERT OR REPLACE INTO wa_image_text
				(rowid, ocr_text, description, language, source, model, generated_at)
				VALUES (?, ?, ?, '', 'cloud', ?, ?)`,
				it.rowid, res.OCRText, res.Description, ImageModel, now)
			return err
		}}
	case "voice":
		transcript, err := transcribeVoice(ctx, c, data)
		if err != nil {
			return outcome{it: it, err: err}
		}
		return outcome{it: it, write: func(db *sql.DB) error {
			_, err := db.Exec(`INSERT OR REPLACE INTO wa_voice_text
				(rowid, transcript, language, duration_sec, model, generated_at)
				VALUES (?, ?, '', NULL, ?, ?)`,
				it.rowid, transcript, VoiceModel, now)
			return err
		}}
	default: // documents
		res, err := extractDocument(ctx, c, filepath.Base(it.path), data)
		if err != nil {
			return outcome{it: it, err: err}
		}
		return outcome{it: it, write: func(db *sql.DB) error {
			_, err := db.Exec(`INSERT OR REPLACE INTO wa_document_text
				(rowid, text, page_count, method, generated_at)
				VALUES (?, ?, ?, ?, ?)`,
				it.rowid, res.Text, res.PageCount, res.Method, now)
			return err
		}}
	}
}

// scanQueue lists the pending files: numeric-stem files directly in the
// three kind directories. failed/ and anything unrecognised is left
// alone — we never touch files we didn't put there.
func scanQueue(unenriched string) ([]item, error) {
	var out []item
	for _, kind := range []string{"media", "voice", "documents"} {
		dir := filepath.Join(unenriched, kind)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		k := kind
		if kind == "media" {
			k = "images"
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			stem := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
			rowid, err := strconv.ParseInt(stem, 10, 64)
			if err != nil {
				continue
			}
			out = append(out, item{kind: k, rowid: rowid, path: filepath.Join(dir, e.Name())})
		}
	}
	return out, nil
}

// quarantine moves a permanently-failed file to <dir>/failed/<name>.
func quarantine(path string) error {
	dir := filepath.Join(filepath.Dir(path), FailedDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.Rename(path, filepath.Join(dir, filepath.Base(path)))
}

func loadRowidSet(db *sql.DB, query string) (map[int64]bool, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ensureSchema creates the enrichment tables. The FTS rebuild (in the
// views package) probes for exactly these names and folds their text
// into messages_fts.
func ensureSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS wa_image_text (
			rowid        INTEGER PRIMARY KEY,
			ocr_text     TEXT,
			description  TEXT,
			language     TEXT,
			source       TEXT,
			model        TEXT,
			generated_at TEXT
		);
		CREATE TABLE IF NOT EXISTS wa_voice_text (
			rowid        INTEGER PRIMARY KEY,
			transcript   TEXT,
			language     TEXT,
			duration_sec REAL,
			model        TEXT,
			generated_at TEXT
		);
		CREATE TABLE IF NOT EXISTS wa_document_text (
			rowid        INTEGER PRIMARY KEY,
			text         TEXT,
			page_count   INTEGER,
			method       TEXT,
			generated_at TEXT
		);`)
	return err
}
