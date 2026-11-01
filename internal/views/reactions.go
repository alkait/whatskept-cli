package views

import (
	"database/sql"
	_ "embed"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

//go:embed embed/reactions.sql
var reactionsSQL string

// EnsureReactions creates the reaction table and view. When the table is
// new it is also filled from the receipt blobs the phone's database
// already carries, exactly once: from then on the table is the record,
// and reading the blobs again would bring back reactions withdrawn
// since. The whole step is one transaction, so an interrupted run leaves
// no table behind and simply happens again. Returns the rows backfilled.
func EnsureReactions(db *sql.DB) (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	existed, err := hasTable(tx, "wa_reaction")
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(reactionsSQL); err != nil {
		return 0, fmt.Errorf("apply reactions schema: %w", err)
	}
	// Resolving a LID through wa_jid_alias otherwise scans every chat,
	// once per reaction: over three minutes on a Pi for a whole-table
	// question, against a 15-second MCP query timeout, and 2.4s with it.
	if ok, err := hasTable(tx, "ZWACHATSESSION"); err != nil {
		return 0, err
	} else if ok {
		if _, err := tx.Exec(
			`CREATE INDEX IF NOT EXISTS wa_chatsession_identifier_idx ON ZWACHATSESSION(ZCONTACTIDENTIFIER)`); err != nil {
			return 0, fmt.Errorf("index chat identifiers: %w", err)
		}
	}
	n := 0
	if !existed {
		if n, err = backfillReactions(tx); err != nil {
			return 0, fmt.Errorf("backfill reactions: %w", err)
		}
	}
	return n, tx.Commit()
}

func hasTable(tx *sql.Tx, name string) (bool, error) {
	var n int
	err := tx.QueryRow(
		`SELECT COUNT(*) FROM main.sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&n)
	return n > 0, err
}

// backfillReactions decodes every ZWAMESSAGEINFO receipt blob into
// wa_reaction, skipping databases too minimal to have the table.
func backfillReactions(tx *sql.Tx) (int, error) {
	if ok, err := hasTable(tx, "ZWAMESSAGEINFO"); err != nil || !ok {
		return 0, err
	}

	type row struct {
		target string
		receiptReaction
	}
	rows, err := tx.Query(`
		SELECT m.ZSTANZAID, i.ZRECEIPTINFO
		FROM   ZWAMESSAGEINFO i
		JOIN   ZWAMESSAGE m ON m.Z_PK = i.ZMESSAGE
		WHERE  i.ZRECEIPTINFO IS NOT NULL AND m.ZSTANZAID IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	var found []row
	for rows.Next() {
		var target string
		var blob []byte
		if err := rows.Scan(&target, &blob); err != nil {
			rows.Close()
			return 0, err
		}
		for _, r := range parseReactions(blob) {
			found = append(found, row{target, r})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	// One person can appear twice in a blob; the newest entry stands.
	stmt, err := tx.Prepare(`
		INSERT INTO wa_reaction (target_stanza, sender_jid, is_from_me, emoji, ts)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (target_stanza, sender_jid) DO UPDATE
		SET emoji = excluded.emoji, ts = excluded.ts
		WHERE excluded.ts > wa_reaction.ts`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, r := range found {
		fromMe := 0
		if r.Sender == "" {
			fromMe = 1
		}
		if _, err := stmt.Exec(r.target, r.Sender, fromMe, r.Emoji, r.TS); err != nil {
			return 0, err
		}
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM wa_reaction`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// receiptReaction is one reaction read from a receipt blob. Sender is
// empty when the reaction is the user's own.
type receiptReaction struct {
	Sender string
	Emoji  string
	TS     string // UTC, "2006-01-02 15:04:05"
}

// parseReactions reads the reactions out of a ZRECEIPTINFO blob. The
// format is undocumented; this is what a survey of a real 123k-blob
// database found. Field 7 of the blob holds, once per reaction:
//
//	1: someone's reaction  {1 id, 2 sender jid, 3 emoji, 4 unix ms, 6 mine}
//	2: the user's own      {1 id, 2 emoji, 3 unix ms}
//
// The first shape turns up without a sender (and with 6 set) for the
// user's own reactions too. An entry with no emoji is a withdrawn
// reaction. Old messages hold a binary plist here instead, which never
// carries reactions, so anything unparseable simply has none.
func parseReactions(blob []byte) []receiptReaction {
	var out []receiptReaction
	ok := eachField(blob, func(num protowire.Number, _ uint64, val []byte) {
		if num != 7 {
			return
		}
		eachField(val, func(shape protowire.Number, _ uint64, entry []byte) {
			var r receiptReaction
			var ms uint64
			switch shape {
			case 1:
				eachField(entry, func(n protowire.Number, v uint64, b []byte) {
					switch n {
					case 2:
						r.Sender = string(b)
					case 3:
						r.Emoji = string(b)
					case 4:
						ms = v
					}
				})
			case 2:
				eachField(entry, func(n protowire.Number, v uint64, b []byte) {
					switch n {
					case 2:
						r.Emoji = string(b)
					case 3:
						ms = v
					}
				})
			default:
				return
			}
			if r.Emoji == "" || ms == 0 {
				return
			}
			r.TS = time.UnixMilli(int64(ms)).UTC().Format("2006-01-02 15:04:05")
			out = append(out, r)
		})
	})
	if !ok {
		return nil
	}
	return out
}

// eachField walks one protobuf message, calling fn with each varint's
// value or each length-delimited field's bytes. It reports whether the
// whole of b parsed.
func eachField(b []byte, fn func(num protowire.Number, v uint64, val []byte)) bool {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return false
		}
		b = b[n:]
		switch typ {
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return false
			}
			fn(num, v, nil)
			b = b[n:]
		case protowire.BytesType:
			val, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return false
			}
			fn(num, 0, val)
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return false
			}
			b = b[n:]
		}
	}
	return true
}
