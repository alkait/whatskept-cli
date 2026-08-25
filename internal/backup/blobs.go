package backup

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// errBadMagic marks a decrypted payload whose leading bytes don't match
// the expected file signature — attachment extensions lie sometimes.
var errBadMagic = errors.New("bad magic")

// pdfMagic is the PDF file signature ("%PDF").
var pdfMagic = []byte("%PDF")

// magicOK reports whether head carries the expected signature. A nil
// magic accepts anything.
func magicOK(head, magic []byte) bool {
	return len(head) >= len(magic) && bytes.Equal(head[:len(magic)], magic)
}

// UnenrichedDir is the hidden workspace directory holding decrypted
// media awaiting enrichment. Enrichment deletes each file once its text
// is in the DB — an empty directory means fully enriched.
const UnenrichedDir = ".unenriched"

// ZWAMEDIAITEM.ZMEDIALOCALPATH is relative to Message/ inside the
// WhatsApp backup domain.
const mediaManifestPrefix = "Message/"

// BlobKindStats tallies one media kind's extraction. Missing means the
// DB references the blob but iOS didn't back its bytes up (common for
// media not viewed recently on the device) — expected, not an error.
type BlobKindStats struct {
	Downloaded int
	Missing    int
	Errors     int
}

// BlobStats summarizes one ExtractBlobs run.
type BlobStats struct {
	Images    BlobKindStats
	Voice     BlobKindStats
	Documents BlobKindStats
	// UnsupportedDocuments counts non-PDF attachments (docx, xlsx, …);
	// they stay in the backup, only their filenames are searchable.
	UnsupportedDocuments int
}

type blobCandidate struct {
	rowid     int64  // ZWAMESSAGE.Z_PK — the filename stem on disk
	localPath string // ZWAMEDIAITEM.ZMEDIALOCALPATH
}

func (c blobCandidate) manifestPath() string { return mediaManifestPrefix + c.localPath }

func (b *bundle) ExtractBlobs(root string, log func(string)) (BlobStats, error) {
	if log == nil {
		log = func(string) {}
	}
	var stats BlobStats

	db, err := sql.Open("sqlite", "file:"+filepath.Join(root, ChatStorageName)+"?mode=ro")
	if err != nil {
		return stats, fmt.Errorf("open %s: %w", ChatStorageName, err)
	}
	defer db.Close()

	// Rows already enriched (their text carried forward from the
	// previous live DB) are excluded — re-queueing them would re-pay
	// for enrichment the workspace already has.
	selectPending := func(kind, where string) ([]blobCandidate, error) {
		excl, err := enrichedExclusion(db, kind)
		if err != nil {
			return nil, err
		}
		return selectBlobCandidates(db, where+excl)
	}
	images, err := selectPending("images", "m.ZMEDIALOCALPATH LIKE '%.jpg'")
	if err != nil {
		return stats, err
	}
	voice, err := selectPending("voice", "m.ZMEDIALOCALPATH LIKE '%.opus'")
	if err != nil {
		return stats, err
	}
	docs, err := selectPending("documents", "wm.ZMESSAGETYPE = 8 AND m.ZMEDIALOCALPATH IS NOT NULL")
	if err != nil {
		return stats, err
	}

	// One manifest index for all three passes: path → record.
	idx := make(map[string]*Record, 4096)
	for i := range b.mb.Records {
		r := &b.mb.Records[i]
		if r.Domain == whatsAppDomain {
			idx[r.Path] = r
		}
	}

	for _, dir := range []string{"media", "voice", "documents"} {
		if err := os.MkdirAll(filepath.Join(root, UnenrichedDir, dir), 0o755); err != nil {
			return stats, err
		}
	}

	stats.Images = b.extractImages(images, idx, filepath.Join(root, UnenrichedDir, "media"), log)
	stats.Voice = b.extractStreamed("voice notes", voice, idx,
		filepath.Join(root, UnenrichedDir, "voice"), ".opus", nil, log)

	var pdfs []blobCandidate
	for _, c := range docs {
		if strings.ToLower(filepath.Ext(c.localPath)) != ".pdf" {
			stats.UnsupportedDocuments++
			continue
		}
		pdfs = append(pdfs, c)
	}
	stats.Documents = b.extractStreamed("documents", pdfs, idx,
		filepath.Join(root, UnenrichedDir, "documents"), ".pdf", pdfMagic, log)

	return stats, nil
}

// selectBlobCandidates returns the media rows matching where, ordered
// by message rowid. `m` is ZWAMEDIAITEM, `wm` its ZWAMESSAGE.
func selectBlobCandidates(db *sql.DB, where string) ([]blobCandidate, error) {
	rows, err := db.Query(`
		SELECT m.ZMESSAGE, m.ZMEDIALOCALPATH
		FROM   ZWAMEDIAITEM m
		JOIN   ZWAMESSAGE   wm ON wm.Z_PK = m.ZMESSAGE
		WHERE  ` + where + `
		ORDER BY m.ZMESSAGE ASC`)
	if err != nil {
		return nil, fmt.Errorf("select candidates: %w", err)
	}
	defer rows.Close()
	var out []blobCandidate
	for rows.Next() {
		var c blobCandidate
		if err := rows.Scan(&c.rowid, &c.localPath); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// extractImages decrypts each image into memory (they're small, and the
// bytes must be sniffed first: WhatsApp names everything .jpg but stores
// jpg/png/heic/gif), then writes <rowid>.<real-ext>.
func (b *bundle) extractImages(cands []blobCandidate, idx map[string]*Record, dir string, log func(string)) BlobKindStats {
	var s BlobKindStats
	for i, c := range cands {
		rec, ok := idx[c.manifestPath()]
		if !ok {
			s.Missing++
			continue
		}
		data, err := b.readRecord(*rec)
		switch {
		case errors.Is(err, io.EOF) || (err == nil && len(data) == 0):
			s.Missing++
			continue
		case err != nil:
			s.Errors++
			continue
		}
		ext, ok := detectImageFormat(data)
		if !ok {
			s.Errors++
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.%s", c.rowid, ext)), data, 0o644); err != nil {
			s.Errors++
			continue
		}
		s.Downloaded++
		progress(log, "images", i+1, len(cands))
	}
	return s
}

// extractStreamed decrypts each candidate straight to <rowid><ext>,
// verifying magic (when non-nil) against the leading decrypted bytes.
func (b *bundle) extractStreamed(kind string, cands []blobCandidate, idx map[string]*Record, dir, ext string, magic []byte, log func(string)) BlobKindStats {
	var s BlobKindStats
	for i, c := range cands {
		rec, ok := idx[c.manifestPath()]
		if !ok {
			s.Missing++
			continue
		}
		outPath := filepath.Join(dir, fmt.Sprintf("%d%s", c.rowid, ext))
		n, err := b.decryptTo(*rec, outPath, magic)
		switch {
		case errors.Is(err, io.EOF) || (err == nil && n == 0):
			_ = os.Remove(outPath)
			s.Missing++
		case err != nil:
			_ = os.Remove(outPath)
			s.Errors++
		default:
			s.Downloaded++
		}
		progress(log, kind, i+1, len(cands))
	}
	return s
}

// readRecord decrypts one record into memory.
func (b *bundle) readRecord(rec Record) (data []byte, err error) {
	err = silenceStdout(func() error {
		rd, err := b.mb.FileReader(rec)
		if rd != nil && errors.Is(err, io.EOF) {
			err = nil // payloads under one cipher block: valid, already-exhausted stream
		}
		if err != nil {
			return err
		}
		defer rd.Close()
		data, err = io.ReadAll(rd)
		return err
	})
	return data, err
}

// progress emits a log line every 1000 items and at the end.
func progress(log func(string), kind string, done, total int) {
	if done%1000 == 0 || done == total {
		log(fmt.Sprintf("%s: %d/%d", kind, done, total))
	}
}

// detectImageFormat sniffs data's magic bytes and returns a bare
// extension for the four formats seen in WhatsApp media. ok=false
// means corrupt or an unhandled format. HEIC is any ISO-BMFF "ftyp"
// box — brands vary (heic/heix/mif1/…).
func detectImageFormat(data []byte) (string, bool) {
	switch {
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return "jpg", true
	case len(data) >= 8 && string(data[:8]) == "\x89PNG\r\n\x1a\n":
		return "png", true
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		return "heic", true
	case len(data) >= 4 && string(data[:4]) == "GIF8":
		return "gif", true
	}
	return "", false
}
