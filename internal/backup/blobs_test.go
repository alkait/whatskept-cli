package backup

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestDetectImageFormat(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		ext  string
		ok   bool
	}{
		{"jpg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "jpg", true},
		{"png", []byte("\x89PNG\r\n\x1a\nrest"), "png", true},
		{"heic", []byte("\x00\x00\x00\x18ftypheic"), "heic", true},
		{"gif", []byte("GIF89a"), "gif", true},
		{"garbage", []byte("not an image"), "", false},
		{"empty", nil, "", false},
	}
	for _, c := range cases {
		ext, ok := detectImageFormat(c.data)
		if ext != c.ext || ok != c.ok {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", c.name, ext, ok, c.ext, c.ok)
		}
	}
}

// writeFixtureChatDB creates a minimal ChatStorage.sqlite with one
// image, one voice note, one PDF, one docx, and one pathless document.
func writeFixtureChatDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, ChatStorageName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	const schema = `
		CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZMESSAGETYPE INTEGER);
		CREATE TABLE ZWAMEDIAITEM (Z_PK INTEGER PRIMARY KEY, ZMESSAGE INTEGER, ZMEDIALOCALPATH TEXT);
		INSERT INTO ZWAMESSAGE VALUES (1, 1), (2, 3), (3, 8), (4, 8), (5, 8);
		INSERT INTO ZWAMEDIAITEM VALUES
			(10, 1, 'Media/chat/img.jpg'),
			(11, 2, 'Media/chat/note.opus'),
			(12, 3, 'Media/chat/lease.pdf'),
			(13, 4, 'Media/chat/sheet.docx'),
			(14, 5, NULL);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSelectBlobCandidates(t *testing.T) {
	db := writeFixtureChatDB(t, t.TempDir())
	cases := []struct {
		name  string
		where string
		want  []blobCandidate
	}{
		{"images", "m.ZMEDIALOCALPATH LIKE '%.jpg'",
			[]blobCandidate{{1, "Media/chat/img.jpg"}}},
		{"voice", "m.ZMEDIALOCALPATH LIKE '%.opus'",
			[]blobCandidate{{2, "Media/chat/note.opus"}}},
		{"documents", "wm.ZMESSAGETYPE = 8 AND m.ZMEDIALOCALPATH IS NOT NULL",
			[]blobCandidate{{3, "Media/chat/lease.pdf"}, {4, "Media/chat/sheet.docx"}}},
	}
	for _, c := range cases {
		got, err := selectBlobCandidates(db, c.where)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s[%d]: got %+v, want %+v", c.name, i, got[i], c.want[i])
			}
		}
	}
}

func TestMagicOK(t *testing.T) {
	cases := []struct {
		head string
		want bool
	}{
		{"%PDF-1.7 rest", true},
		{"%PDF", true},
		{"PK\x03\x04", false}, // a zip (docx/xlsx) named .pdf
		{"%PD", false},        // shorter than the signature
		{"", false},
	}
	for _, c := range cases {
		if got := magicOK([]byte(c.head), pdfMagic); got != c.want {
			t.Errorf("magicOK(%q, %%PDF) = %v, want %v", c.head, got, c.want)
		}
	}
	if !magicOK(nil, nil) {
		t.Error("nil magic must accept anything")
	}
}

func TestBlobCandidateManifestPath(t *testing.T) {
	c := blobCandidate{rowid: 7, localPath: "Media/chat/img.jpg"}
	if got := c.manifestPath(); got != "Message/Media/chat/img.jpg" {
		t.Errorf("manifestPath = %q", got)
	}
}
