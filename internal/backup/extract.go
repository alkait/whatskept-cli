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

	n, err := b.decryptTo(*rec, tempPath)
	if err != nil {
		_ = os.Remove(tempPath)
		return 0, err
	}
	if n == 0 {
		_ = os.Remove(tempPath)
		return 0, errors.New("extraction produced an empty ChatStorage.sqlite")
	}
	return n, promote(tempPath, livePath)
}

// decryptTo streams one decrypted record to outPath.
func (b *bundle) decryptTo(rec dunhamsteve.Record, outPath string) (n int64, err error) {
	err = silenceStdout(func() error {
		rd, err := b.mb.FileReader(rec)
		if rd != nil && errors.Is(err, io.EOF) {
			err = nil // payloads under one cipher block: valid, already-exhausted stream
		}
		if err != nil {
			return err
		}
		defer rd.Close()
		w, err := os.Create(outPath)
		if err != nil {
			return err
		}
		var copyErr error
		n, copyErr = io.Copy(w, rd)
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
