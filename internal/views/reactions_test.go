package views

import (
	"database/sql"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func bytesField(b []byte, num protowire.Number, v []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(b, num, protowire.BytesType), v)
}

func varintField(b []byte, num protowire.Number, v uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(b, num, protowire.VarintType), v)
}

// Both shapes the phone writes, wrapped as field 7 of a receipt blob.
func theirs(sender, emoji string, ms uint64) []byte {
	e := bytesField(nil, 1, []byte("REACTIONID"))
	if sender != "" {
		e = bytesField(e, 2, []byte(sender))
	} else {
		e = varintField(e, 6, 1)
	}
	if emoji != "" {
		e = bytesField(e, 3, []byte(emoji))
	}
	e = varintField(e, 4, ms)
	return bytesField(nil, 7, bytesField(nil, 1, e))
}

func mine(emoji string, ms uint64) []byte {
	e := bytesField(nil, 1, []byte("REACTIONID"))
	e = bytesField(e, 2, []byte(emoji))
	e = varintField(e, 3, ms)
	e = varintField(e, 4, 1)
	return bytesField(nil, 7, bytesField(nil, 2, e))
}

const ms2024 = 1730223776500 // 2024-10-29 17:42:56 UTC

func TestParseReactions(t *testing.T) {
	receipts := bytesField(varintField(nil, 4, 2), 2, []byte{0x08, 0x03})
	for name, tc := range map[string]struct {
		blob []byte
		want []receiptReaction
	}{
		"someone": {append(receipts, theirs("971501111111@s.whatsapp.net", "❤️", ms2024)...),
			[]receiptReaction{{"971501111111@s.whatsapp.net", "❤️", "2024-10-29 17:42:56"}}},
		"lid sender": {theirs("12345@lid", "😂", ms2024),
			[]receiptReaction{{"12345@lid", "😂", "2024-10-29 17:42:56"}}},
		"mine":                {mine("👍", ms2024), []receiptReaction{{"", "👍", "2024-10-29 17:42:56"}}},
		"mine, first shape":   {theirs("", "😄", ms2024), []receiptReaction{{"", "😄", "2024-10-29 17:42:56"}}},
		"withdrawn":           {theirs("12345@lid", "", ms2024), nil},
		"receipts only":       {receipts, nil},
		"legacy binary plist": {[]byte("bplist00\xd4\x01\x02\x03\x04\x05\x06\x2f\x30"), nil},
		"truncated":           {theirs("12345@lid", "😂", ms2024)[:12], nil},
		"empty":               {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := parseReactions(tc.blob)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got %+v, want %+v", got[i], tc.want[i])
				}
			}
		})
	}
}

func addReceipts(t *testing.T, path string, rows map[int64][]byte) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE ZWAMESSAGEINFO (Z_PK INTEGER PRIMARY KEY, ZMESSAGE INTEGER, ZRECEIPTINFO BLOB)`); err != nil {
		t.Fatal(err)
	}
	for pk, blob := range rows {
		if _, err := db.Exec(`INSERT INTO ZWAMESSAGEINFO (ZMESSAGE, ZRECEIPTINFO) VALUES (?, ?)`, pk, blob); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReactionsBackfilledIntoView(t *testing.T) {
	path := writeFixtureDB(t)
	blob := append(theirs("12345@lid", "❤️", ms2024), mine("😂", ms2024)...)
	addReceipts(t, path, map[int64][]byte{
		1: blob,
		2: theirs("971501111111@s.whatsapp.net", "👍", ms2024),
		3: []byte("bplist00"),
	})
	if _, err := Apply(path); err != nil {
		t.Fatal(err)
	}
	if got := query1(t, path, `SELECT COUNT(*) FROM wa_reaction`); got != "3" {
		t.Errorf("reactions = %s, want 3", got)
	}
	// A LID sender resolves through the alias bridge to the DM's name.
	if got := query1(t, path, `SELECT sender_name || '|' || emoji || '|' || ts || '|' || chat_title || '|' || rowid || '|' || message_text
		FROM v_reactions WHERE stanza_id = 's1' AND is_from_me = 0`); got != "Sarah|❤️|2024-10-29 17:42:56|Sarah|1|lunch at the cafe tomorrow?" {
		t.Errorf("their reaction = %s", got)
	}
	if got := query1(t, path, `SELECT sender_name || '|' || emoji FROM v_reactions
		WHERE stanza_id = 's1' AND is_from_me = 1`); got != "me|😂" {
		t.Errorf("my reaction = %s", got)
	}
}

// Apply runs again after every enrichment batch. The blobs must be read
// only once, or a withdrawn reaction would come back.
func TestApplyDoesNotResurrectReactions(t *testing.T) {
	path := writeFixtureDB(t)
	addReceipts(t, path, map[int64][]byte{1: theirs("12345@lid", "❤️", ms2024)})
	if _, err := Apply(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM wa_reaction`); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(path); err != nil {
		t.Fatal(err)
	}
	if got := query1(t, path, `SELECT COUNT(*) FROM wa_reaction`); got != "0" {
		t.Errorf("reactions after re-apply = %s, want 0", got)
	}
}

// A person listed twice in a blob keeps the newer reaction, whichever
// comes first.
func TestBackfillKeepsNewestReaction(t *testing.T) {
	for name, blob := range map[string][]byte{
		"newest last":  append(mine("😂", ms2024), mine("👍", ms2024+5000)...),
		"newest first": append(mine("👍", ms2024+5000), mine("😂", ms2024)...),
	} {
		t.Run(name, func(t *testing.T) {
			path := writeFixtureDB(t)
			addReceipts(t, path, map[int64][]byte{1: blob})
			if _, err := Apply(path); err != nil {
				t.Fatal(err)
			}
			if got := query1(t, path, `SELECT group_concat(emoji) FROM wa_reaction`); got != "👍" {
				t.Errorf("reactions = %s", got)
			}
		})
	}
}

// A reaction can outlive or precede what it points at.
func TestReactionWithoutStoredMessage(t *testing.T) {
	path := writeFixtureDB(t)
	if _, err := Apply(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO wa_reaction VALUES ('gone', '555@s.whatsapp.net', 0, '🔥', '2026-01-01 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if got := query1(t, path, `SELECT sender_name || '|' || (rowid IS NULL) FROM v_reactions`); got != "555@s.whatsapp.net|1" {
		t.Errorf("orphan reaction = %s", got)
	}
}
