package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	dunhamsteve "github.com/dunhamsteve/ios/backup"
)

// ChatStorageName is the filename of WhatsApp's database, both inside
// the backup and once extracted at the workspace root.
const ChatStorageName = "ChatStorage.sqlite"

const whatsAppDomain = "AppDomainGroup-group.net.whatsapp.WhatsApp.shared"

// Record is a backup-manifest entry for one file, aliased so callers
// don't import the underlying library.
type Record = dunhamsteve.Record

// Bundle is an opened-and-unlocked encrypted iOS backup. The keybag
// unlock costs seconds on a large backup, so one Bundle is reused for
// every read during an import.
type Bundle interface {
	// DetectNumber reads the WhatsApp account's own number from the
	// backup's preference plists. Returns ("", nil) when the backup
	// is readable but no number was found.
	DetectNumber() (string, error)
	// ExtractChatStorage decrypts WhatsApp's ChatStorage.sqlite to
	// the workspace root, via a staging file promoted atomically over
	// any previous copy. Returns the number of bytes written.
	ExtractChatStorage(root string) (int64, error)
	// ExtractBlobs decrypts images, voice notes and PDFs referenced by
	// the extracted ChatStorage.sqlite into <root>/.unenriched/. log
	// (nil for silent) receives progress lines.
	ExtractBlobs(root string, log func(string)) (BlobStats, error)
}

// Open validates and unlocks the encrypted backup at dir, reading the
// password from PasswordEnv. A package var so tests can stub the
// decryption step.
var Open = func(dir string) (Bundle, error) {
	manifest, err := readPlist(filepath.Join(dir, "Manifest.plist"))
	if err != nil {
		return nil, err
	}
	if manifest == nil {
		return nil, fmt.Errorf("%s is not an iOS backup (no readable Manifest.plist)", dir)
	}
	if enc, _ := manifest["IsEncrypted"].(bool); !enc {
		return nil, errors.New("backup is not encrypted; WhatsApp data is only present in encrypted backups")
	}
	password := os.Getenv(PasswordEnv)
	if password == "" {
		return nil, fmt.Errorf("backup is encrypted; set %s to your backup password", PasswordEnv)
	}
	mb, err := openBackup(dir, password)
	if err != nil {
		return nil, err
	}
	return &bundle{mb: mb}, nil
}

type bundle struct{ mb *dunhamsteve.MobileBackup }

func (b *bundle) DetectNumber() (string, error) {
	for _, loc := range whatsappPrefsPlists {
		data, err := readBackupFile(b.mb, loc.domain, loc.relPath)
		if err != nil {
			continue
		}
		if n := numberFromPrefs(data); n != "" {
			return n, nil
		}
	}
	return "", nil
}

func (b *bundle) ExtractChatStorage(root string) (int64, error) {
	var rec *dunhamsteve.Record
	for i := range b.mb.Records {
		r := &b.mb.Records[i]
		if r.Domain == whatsAppDomain && r.Path == ChatStorageName {
			rec = r
			break
		}
	}
	if rec == nil {
		return 0, errors.New("ChatStorage.sqlite not found in backup (was WhatsApp installed when it was made?)")
	}

	livePath := filepath.Join(root, ChatStorageName)
	tempPath := livePath + ".new"
	_ = os.Remove(tempPath) // stale staging from a crashed run

	n, err := b.decryptTo(*rec, tempPath, nil)
	if err != nil {
		_ = os.Remove(tempPath)
		return 0, err
	}
	if n == 0 {
		_ = os.Remove(tempPath)
		return 0, errors.New("extraction produced an empty ChatStorage.sqlite")
	}
	// Re-import: enrichment rows in the current live DB are paid-for
	// work the backup can never contain — carry them into the staging
	// DB before it replaces the live one. A merge failure aborts the
	// import (removing the staging file) rather than silently wiping
	// the rows.
	if _, statErr := os.Stat(livePath); statErr == nil {
		if err := mergeForward(livePath, tempPath); err != nil {
			_ = os.Remove(tempPath)
			return 0, fmt.Errorf("carry enrichment forward: %w", err)
		}
	}
	return n, promote(tempPath, livePath)
}

// decryptTo streams one decrypted record to outPath. A non-nil magic
// requires the decrypted bytes to start with that signature; a payload
// failing the check returns errBadMagic and writes nothing.
func (b *bundle) decryptTo(rec dunhamsteve.Record, outPath string, magic []byte) (n int64, err error) {
	err = silenceStdout(func() error {
		rd, err := b.mb.FileReader(rec)
		if rd != nil && errors.Is(err, io.EOF) {
			err = nil // payloads under one cipher block: valid, already-exhausted stream
		}
		if err != nil {
			return err
		}
		defer rd.Close()

		head := make([]byte, len(magic))
		m, err := io.ReadFull(rd, head)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
		if m == 0 && len(magic) > 0 {
			return io.EOF // empty payload: blob not persisted, not a format problem
		}
		if !magicOK(head[:m], magic) {
			return errBadMagic
		}

		w, err := os.Create(outPath)
		if err != nil {
			return err
		}
		var copyErr error
		if _, copyErr = w.Write(head[:m]); copyErr == nil {
			var copied int64
			copied, copyErr = io.Copy(w, rd)
			n = int64(m) + copied
		}
		if closeErr := w.Close(); copyErr == nil {
			copyErr = closeErr
		}
		return copyErr
	})
	return n, err
}

// promote renames the staging file over the live DB. Stale SQLite WAL
// companions of the previous live DB are removed first — a leftover
// -wal replayed into the fresh file would corrupt it.
func promote(tempPath, livePath string) error {
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(livePath + suffix)
	}
	return os.Rename(tempPath, livePath)
}
