package backup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	dunhamsteve "github.com/dunhamsteve/ios/backup"
	"github.com/dunhamsteve/ios/keybag"
	dsplist "github.com/dunhamsteve/plist"
	"howett.net/plist"
)

// PasswordEnv is where import reads the encrypted-backup password from.
const PasswordEnv = "WHATSKEPT_BACKUP_PASSWORD"

// whatsappPrefsPlists are the places WhatsApp stores the registered
// account's own number inside an iOS backup, checked in order.
var whatsappPrefsPlists = []struct{ domain, relPath string }{
	{"AppDomainGroup-group.net.whatsapp.WhatsApp.shared", "Library/Preferences/group.net.whatsapp.WhatsApp.shared.plist"},
	{"AppDomain-net.whatsapp.WhatsApp", "Library/Preferences/net.whatsapp.WhatsApp.plist"},
}

// silenceStdout redirects the process's stdout to /dev/null while fn
// runs. The upstream backup library prints debug lines straight to
// stdout — including a derived key that is a password-equivalent for
// the backup, which must never reach the terminal.
func silenceStdout(fn func() error) error {
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fn() // can't silence; run anyway
	}
	old := os.Stdout
	os.Stdout = devnull
	defer func() {
		os.Stdout = old
		devnull.Close()
	}()
	return fn()
}

// openBackup opens and unlocks an encrypted iOS backup (keybag unlock
// costs a couple of seconds — reuse the handle for all reads).
func openBackup(dir, password string) (*dunhamsteve.MobileBackup, error) {
	mb := &dunhamsteve.MobileBackup{Dir: dir}
	r, err := os.Open(filepath.Join(dir, "Manifest.plist"))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if err := dsplist.Unmarshal(r, &mb.Manifest); err != nil {
		return nil, fmt.Errorf("parse Manifest.plist: %w", err)
	}
	mb.Keybag = keybag.Read(mb.Manifest.BackupKeyBag)
	err = silenceStdout(func() error {
		if err := mb.SetPassword(password); err != nil {
			return fmt.Errorf("unlock backup (wrong password?): %w", err)
		}
		if err := mb.Load(); err != nil {
			return fmt.Errorf("load manifest: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return mb, nil
}

// readBackupFile decrypts one file record. The upstream library returns
// (reader, io.EOF) for payloads under one ~4 KB cipher block — valid
// content with an already-exhausted stream — so io.EOF is absorbed.
func readBackupFile(mb *dunhamsteve.MobileBackup, domain, relPath string) ([]byte, error) {
	for i := range mb.Records {
		rec := mb.Records[i]
		if rec.Domain != domain || rec.Path != relPath {
			continue
		}
		var data []byte
		err := silenceStdout(func() error {
			rd, err := mb.FileReader(rec)
			if rd != nil && errors.Is(err, io.EOF) {
				err = nil
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
	return nil, fmt.Errorf("%s/%s not in backup", domain, relPath)
}

// numberFromPrefs extracts the account's own number from a decrypted
// WhatsApp preferences plist: known keys first, then any top-level
// string shaped like a WhatsApp JID.
func numberFromPrefs(data []byte) string {
	var prefs map[string]any
	if _, err := plist.Unmarshal(data, &prefs); err != nil {
		return ""
	}
	// JID keys first: WhatsApp obfuscates OwnPhoneNumber (base64 blob)
	// but stores the account JID in the clear.
	for _, key := range []string{"OwnJabberID", "LastOwnJabberID", "CurrentUserJabberID", "OwnPhoneNumber"} {
		if s, ok := prefs[key].(string); ok {
			if n := normalizeNumber(s); n != "" {
				return n
			}
		}
	}
	for _, v := range prefs {
		if s, ok := v.(string); ok && strings.HasSuffix(s, "@s.whatsapp.net") {
			if n := normalizeNumber(s); n != "" {
				return n
			}
		}
	}
	return ""
}

// normalizeNumber turns "+971 50 000 0000", "971500000000@s.whatsapp.net"
// or "971500000000" into "+971500000000". Strict: only phone punctuation
// is allowed around the digits (a letter anywhere rejects the value —
// obfuscated/base64 blobs must never pass), and the digit count must be
// a plausible E.164 length (7–15).
func normalizeNumber(s string) string {
	if at := strings.IndexByte(s, '@'); at >= 0 {
		if !strings.HasSuffix(s, "@s.whatsapp.net") {
			return ""
		}
		s = s[:at]
	}
	var digits bytes.Buffer
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits.WriteRune(r)
		case r == '+' || r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
			// phone-number punctuation, skip
		default:
			return ""
		}
	}
	if digits.Len() < 7 || digits.Len() > 15 {
		return ""
	}
	return "+" + digits.String()
}
