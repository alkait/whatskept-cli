package enrich

// The exported single-file surface for `whatskept live`'s in-process
// trickle enrichment: the same engines, error taxonomy, quarantine and
// attempt log the batch command uses, callable one file at a time. The
// batch pipeline in Run stays the only bulk consumer.

import (
	"context"
	"database/sql"
)

// QueueItem is one pending file in the .unenriched/ queue.
type QueueItem struct {
	Kind  string // "images", "voice", "documents"
	Rowid int64
	Path  string
}

// ScanQueue lists the queue's pending files (failed/ and unrecognised
// names are never touched).
func ScanQueue(unenriched string) ([]QueueItem, error) {
	items, err := scanQueue(unenriched)
	if err != nil {
		return nil, err
	}
	out := make([]QueueItem, len(items))
	for i, it := range items {
		out[i] = QueueItem{Kind: it.kind, Rowid: it.rowid, Path: it.path}
	}
	return out, nil
}

// Outcome is ProcessFile's verdict on one file. Exactly one of Write
// and Err is set; CostUSD is the spend incurred either way.
type Outcome struct {
	Write   func(db *sql.DB) error
	CostUSD float64
	Err     error
}

// ProcessFile runs one file through its engine (paid API call) and
// returns the DB write to apply. It does not touch the database or
// delete the file — that is the caller's collector logic.
func ProcessFile(ctx context.Context, c *Client, it QueueItem) Outcome {
	o := process(ctx, c, item{kind: it.Kind, rowid: it.Rowid, path: it.Path})
	return Outcome{Write: o.write, CostUSD: o.costUSD, Err: o.err}
}

// Quarantine moves a permanently-failed file to its kind's failed/
// directory.
func Quarantine(path string) error { return quarantine(path) }

// EnsureSchema creates the wa_*_text result tables if missing.
func EnsureSchema(db *sql.DB) error { return ensureSchema(db) }

// Log is the shared append-only attempt history at
// .unenriched/enrich.log.
type Log struct{ inner *attemptLog }

// OpenLog opens the attempt log for appending. Never fails: on error
// the returned Log silently drops lines (observability must not stop
// enrichment).
func OpenLog(unenriched string) *Log { return &Log{openAttemptLog(unenriched)} }

func (l *Log) Line(format string, args ...any) { l.inner.line(format, args...) }
func (l *Log) Close()                          { l.inner.close() }
