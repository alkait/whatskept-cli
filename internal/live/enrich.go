package live

// In-process trickle enrichment. When OPENROUTER_API_KEY is set, live
// enriches media AS IT ARRIVES — through the same engines, taxonomy,
// quarantine and enrich.log as `whatskept enrich` — but strictly
// session-scoped: only files whose PK is in the wa_live_pk ledger are
// ever touched. The import backlog was never consented to here; it
// belongs to the batch command alone, no matter how long it sits.
//
// The trigger is a coalescing doorbell: capture rings a size-1 channel
// after publishing a file; the loop wakes, drains everything currently
// pending (queue ∩ ledger), and sleeps again. Rings during a sweep
// collapse into one pending wake. A periodic backstop retries
// transient failures and anything a crash left behind.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"whatskept/internal/backup"
	"whatskept/internal/enrich"
)

const enrichBackstop = 5 * time.Minute

type enricher struct {
	c      *enrich.Client
	root   string
	dbPath string
	nudge  chan struct{}

	mu       sync.Mutex
	enriched int
	failed   int
	costUSD  float64
	stopped  string // non-empty once a hard error ended the loop
}

// startEnricher wires trickle enrichment if the key is present.
// Returns (nil, nil) when enrichment is off — capture is unaffected.
// A bad key is a startup error: fail loud, not quietly unenriched.
func startEnricher(ctx context.Context, root, dbPath string) (*enricher, error) {
	key := strings.TrimSpace(os.Getenv(enrich.APIKeyEnv))
	if key == "" {
		logf("enrichment: OFF — %s is not set; media queues in %s for `whatskept enrich`",
			enrich.APIKeyEnv, backup.UnenrichedDir)
		return nil, nil
	}
	c := &enrich.Client{APIKey: key}
	if err := c.Preflight(ctx); err != nil {
		return nil, fmt.Errorf("enrichment key check: %w", err)
	}

	db, err := openRW(dbPath)
	if err != nil {
		return nil, err
	}
	if err := enrich.EnsureSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	db.Close()

	e := &enricher{c: c, root: root, dbPath: dbPath, nudge: make(chan struct{}, 1)}
	mine, backlog, err := e.pending(nil)
	if err != nil {
		return nil, err
	}
	logf("enrichment: on — this session's captures only; %d straggler(s) adopted, %d backlog file(s) untouched (run `whatskept enrich` for those)",
		len(mine), backlog)
	if len(mine) > 0 {
		e.Nudge()
	}
	go e.loop(ctx)
	return e, nil
}

// Nudge wakes the loop; safe on a nil enricher and never blocks the
// capture path.
func (e *enricher) Nudge() {
	if e == nil {
		return
	}
	select {
	case e.nudge <- struct{}{}:
	default:
	}
}

func (e *enricher) loop(ctx context.Context) {
	t := time.NewTicker(enrichBackstop)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.nudge:
		case <-t.C:
		}
		e.sweep(ctx)
		e.mu.Lock()
		dead := e.stopped
		e.mu.Unlock()
		if dead != "" {
			logf("enrichment STOPPED: %s — capture continues; queued files wait for `whatskept enrich`", dead)
			return
		}
	}
}

// pending returns live's own queued files (queue ∩ wa_live_pk),
// removing leftovers (already enriched) and orphans (message gone)
// without spending an API call, and counts the untouchable backlog.
// The db handle is opened if nil.
func (e *enricher) pending(db *sql.DB) ([]enrich.QueueItem, int, error) {
	if db == nil {
		var err error
		if db, err = openRW(e.dbPath); err != nil {
			return nil, 0, err
		}
		defer db.Close()
	}
	items, err := enrich.ScanQueue(filepath.Join(e.root, backup.UnenrichedDir))
	if err != nil {
		return nil, 0, err
	}
	var mine []enrich.QueueItem
	backlog := 0
	for _, it := range items {
		var inLedger int
		if err := db.QueryRow(`SELECT COUNT(*) FROM wa_live_pk WHERE pk = ?`, it.Rowid).Scan(&inLedger); err != nil {
			return nil, 0, err
		}
		if inLedger == 0 {
			backlog++
			continue
		}
		var done, alive int
		table := map[string]string{"images": "wa_image_text", "voice": "wa_voice_text", "documents": "wa_document_text"}[it.Kind]
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE rowid = ?`, it.Rowid).Scan(&done); err != nil {
			return nil, 0, err
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM ZWAMESSAGE WHERE Z_PK = ?`, it.Rowid).Scan(&alive); err != nil {
			return nil, 0, err
		}
		switch {
		case done > 0, alive == 0:
			_ = os.Remove(it.Path) // leftover or orphan; nothing to pay for
		default:
			mine = append(mine, it)
		}
	}
	return mine, backlog, nil
}

// sweep drains everything currently pending, serially — this is the
// trickle path; volume lives in `whatskept enrich`.
func (e *enricher) sweep(ctx context.Context) {
	db, err := openRW(e.dbPath)
	if err != nil {
		logf("enrichment sweep: %v", err)
		return
	}
	defer db.Close()
	mine, _, err := e.pending(db)
	if err != nil {
		logf("enrichment sweep: %v", err)
		return
	}
	if len(mine) == 0 {
		return
	}
	al := enrich.OpenLog(filepath.Join(e.root, backup.UnenrichedDir))
	defer al.Close()

	for _, it := range mine {
		if ctx.Err() != nil {
			return
		}
		out := enrich.ProcessFile(ctx, e.c, it)
		e.mu.Lock()
		e.costUSD += out.CostUSD
		e.mu.Unlock()
		switch {
		case out.Err == nil:
			if err := out.Write(db); err != nil {
				logf("enrich %s %d: db write failed (kept in queue): %v", it.Kind, it.Rowid, err)
				al.Line("%s %d transient db write failed: %v (still queued)", it.Kind, it.Rowid, err)
				continue
			}
			_ = os.Remove(it.Path)
			if err := refreshFTSRow(db, it.Rowid); err != nil {
				logf("enrich %s %d: fts refresh failed: %v", it.Kind, it.Rowid, err)
			}
			al.Line("%s %d enriched (live) cost_usd=%.4f", it.Kind, it.Rowid, out.CostUSD)
			logf("enriched %s %d cost_usd=%.4f", it.Kind, it.Rowid, out.CostUSD)
			e.mu.Lock()
			e.enriched++
			e.mu.Unlock()
		case enrich.ClassOf(out.Err) == enrich.ClassHard:
			// Invalid key, out of credits: no call can succeed. Stop
			// enriching, keep capturing.
			al.Line("live enrichment stopped: %v", out.Err)
			e.mu.Lock()
			e.stopped = out.Err.Error()
			e.mu.Unlock()
			return
		case enrich.ClassOf(out.Err) == enrich.ClassPermanent:
			if err := enrich.Quarantine(it.Path); err != nil {
				logf("enrich %s %d: quarantine failed (kept in queue): %v", it.Kind, it.Rowid, err)
				al.Line("%s %d transient quarantine failed: %v (still queued)", it.Kind, it.Rowid, err)
				continue
			}
			logf("enrich %s %d: permanent failure, moved to failed/: %v", it.Kind, it.Rowid, out.Err)
			al.Line("%s %d failed %v -> failed/", it.Kind, it.Rowid, out.Err)
			e.mu.Lock()
			e.failed++
			e.mu.Unlock()
		default: // transient — stays queued for the next ring or backstop
			logf("enrich %s %d: transient failure (still queued): %v", it.Kind, it.Rowid, out.Err)
			al.Line("%s %d transient %v (still queued)", it.Kind, it.Rowid, out.Err)
		}
	}
}

// status renders the heartbeat fragment; empty when enrichment is off.
func (e *enricher) status() string {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s := fmt.Sprintf(" | enriched=%d enrich_failed=%d cost_usd=%.4f", e.enriched, e.failed, e.costUSD)
	if e.stopped != "" {
		s += " ENRICHMENT-STOPPED"
	}
	return s
}

// queueSize counts the files waiting in the queue directories (all of
// them, backlog included) — the heartbeat's visibility into how much
// unenriched material is sitting on disk.
func queueSize(root string) int {
	n := 0
	for _, kind := range []string{"media", "voice", "documents"} {
		entries, err := os.ReadDir(filepath.Join(root, backup.UnenrichedDir, kind))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				n++
			}
		}
	}
	return n
}

// refreshFTSRow rewrites one message's FTS entry as the concatenation
// of every text surface that now exists for it — the same surfaces the
// full rebuild in the views package folds in.
func refreshFTSRow(db *sql.DB, pk int64) error {
	var parts []string
	add := func(s sql.NullString) {
		if s.Valid && s.String != "" {
			parts = append(parts, s.String)
		}
	}
	var text sql.NullString
	if err := db.QueryRow(`SELECT ZTEXT FROM ZWAMESSAGE WHERE Z_PK = ?`, pk).Scan(&text); err != nil {
		return err
	}
	add(text)
	for _, q := range []struct {
		table string
		query string
	}{
		{"wa_image_text", `SELECT COALESCE(ocr_text,'') || ' ' || COALESCE(description,'') FROM wa_image_text WHERE rowid = ?`},
		{"wa_voice_text", `SELECT COALESCE(transcript,'') FROM wa_voice_text WHERE rowid = ?`},
		{"wa_document", `SELECT COALESCE(filename,'') FROM wa_document WHERE rowid = ?`},
		{"wa_document_text", `SELECT COALESCE(text,'') FROM wa_document_text WHERE rowid = ?`},
	} {
		var exists int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, q.table).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			continue
		}
		var s sql.NullString
		if err := db.QueryRow(q.query, pk).Scan(&s); err != nil && err != sql.ErrNoRows {
			return err
		}
		add(s)
	}
	var title sql.NullString
	if err := db.QueryRow(`SELECT ZTITLE FROM ZWAMEDIAITEM WHERE ZMESSAGE = ?`, pk).Scan(&title); err != nil && err != sql.ErrNoRows {
		return err
	}
	add(title)

	joined := strings.TrimSpace(strings.Join(parts, " "))
	if _, err := db.Exec(`DELETE FROM messages_fts WHERE rowid = ?`, pk); err != nil {
		return err
	}
	if joined == "" {
		return nil
	}
	_, err := db.Exec(`INSERT INTO messages_fts(rowid, text) VALUES (?, ?)`, pk, joined)
	return err
}
