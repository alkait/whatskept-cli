package live

// Media capture.
//
// A live-captured attachment has to be indistinguishable from one a
// backup produced, because the views, queue selection and enrichment
// read the backup's shape and nothing written here gets to be a special
// case. Every rule below was read off a real workspace, not assumed:
//
//   - ZTEXT is NOT the caption. It is NULL on every image row in the
//     workspace. The caption lives in ZWAMEDIAITEM.ZTITLE, which the
//     FTS rebuild folds into messages_fts — so writing it anywhere
//     else makes it unsearchable.
//   - ZMEDIALOCALPATH's EXTENSION is load-bearing. It is the phone's
//     own path, and the queue selection filters on it
//     (`ZMEDIALOCALPATH LIKE '%.opus'`). A voice note whose media item
//     carries the wrong suffix is invisible even with the file sitting
//     in the queue.
//   - ZAUTHORNAME is the document's original filename. views.sql turns
//     it into the searchable wa_document table.
//   - Files land at .unenriched/<dir>/<Z_PK><ext>, the layout import's
//     extractors write and enrichment reads.
//
// Video and stickers are deliberately not captured: video is the
// storage hog and whatskept indexes neither, so the next backup is the
// right place for both.

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

	"whatskept/internal/backup"
)

// Core Data entity ID for WAMediaItem, read from a real workspace's
// Z_PRIMARYKEY alongside the three in writer.go.
const entMediaItem = 8

// downloadable mirrors whatsmeow.DownloadableMessage. Redeclared here so
// classification stays a pure function over the protobuf types, and so
// tests can drive the writer without a WhatsApp connection. The method
// set is identical, so a value of this type satisfies whatsmeow's
// interface directly.
type downloadable interface {
	GetDirectPath() string
	GetMediaKey() []byte
	GetFileSHA256() []byte
	GetFileEncSHA256() []byte
}

// mediaPlan is everything needed to store one attachment. Produced by
// classifyMedia (pure), then completed by the writer, which fills Data
// before the transaction opens.
type mediaPlan struct {
	Source      downloadable
	MessageType int    // ZMESSAGETYPE: 1 image, 3 audio, 8 document
	Dir         string // queue subdirectory: media / voice / documents
	Ext         string // file extension, including the dot
	LocalPath   string // synthesized ZMEDIALOCALPATH
	Caption     string // ZWAMEDIAITEM.ZTITLE
	FileName    string // ZWAMEDIAITEM.ZAUTHORNAME (documents)
	Duration    uint32 // ZWAMEDIAITEM.ZMOVIEDURATION (voice notes)
	Size        uint64 // ZWAMEDIAITEM.ZFILESIZE

	Data []byte // downloaded bytes; filled by Writer.Apply
}

// classifyMedia returns a plan for the attachments worth capturing, or
// nil for a message carrying none we store.
func classifyMedia(msg *waE2E.Message, chat types.JID) *mediaPlan {
	switch {
	case msg.GetImageMessage() != nil:
		img := msg.GetImageMessage()
		// Always .jpg: WhatsApp stores the path that way regardless of
		// the actual encoding, and import's image extractor mirrors it.
		return newMediaPlan(img, chat, 1, "media", ".jpg", &mediaPlan{
			Caption: img.GetCaption(),
			Size:    img.GetFileLength(),
		})

	case msg.GetAudioMessage() != nil:
		aud := msg.GetAudioMessage()
		// Voice notes only. A non-PTT audio message is a shared music or
		// sound file, which the voice pipeline does not transcribe.
		if !aud.GetPTT() {
			return nil
		}
		return newMediaPlan(aud, chat, 3, "voice", ".opus", &mediaPlan{
			Duration: aud.GetSeconds(),
			Size:     aud.GetFileLength(),
		})

	case msg.GetDocumentMessage() != nil:
		doc := msg.GetDocumentMessage()
		// The extension comes from the sender's own filename so
		// ZMEDIALOCALPATH — and therefore wa_document.ext — matches what
		// a backup would record. Only PDFs get text extracted later, but
		// every document is downloaded: the blob expires, the filename
		// does not.
		ext := strings.ToLower(filepath.Ext(doc.GetFileName()))
		if ext == "" {
			ext = ".bin"
		}
		return newMediaPlan(doc, chat, 8, "documents", ext, &mediaPlan{
			Caption:  doc.GetCaption(),
			FileName: doc.GetFileName(),
			Size:     doc.GetFileLength(),
		})
	}
	return nil
}

// newMediaPlan fills in the fields common to every kind, so each case
// above states only what is specific to it.
func newMediaPlan(src downloadable, chat types.JID, msgType int, dir, ext string, p *mediaPlan) *mediaPlan {
	// No direct path means nothing to fetch — an attachment we could
	// record but never download, which is the half-row this avoids.
	if src.GetDirectPath() == "" {
		return nil
	}
	p.Source = src
	p.MessageType = msgType
	p.Dir = dir
	p.Ext = ext
	p.LocalPath = mediaLocalPath(chat, ext)
	return p
}

// mediaLocalPath synthesizes the path the phone itself would have
// stored: Media/<chat jid>/<u0>/<u1>/<uuid><ext>, where the two
// single-character directories are the first two characters of the
// UUID. Verified against every sampled row in a real workspace
// (…/c/a/caf0dab2-….jpg, …/9/8/985c8a90-….opus).
func mediaLocalPath(chat types.JID, ext string) string {
	u := uuid.NewString()
	return fmt.Sprintf("Media/%s/%c/%c/%s%s", chat.String(), u[0], u[1], u, ext)
}

// mediaPathFor rebuilds where capture stored a message's attachment,
// relative to the workspace root, from the message type and the media
// item's local path. Returns "" for a message carrying no attachment.
//
// The extension has to come from ZMEDIALOCALPATH for documents, because
// that is the only place the sender's original suffix survives — the
// file on disk is named <Z_PK><ext>, not <Z_PK>.pdf.
func mediaPathFor(pk int64, msgType int, localPath string) string {
	var dir, ext string
	switch msgType {
	case 1:
		dir, ext = "media", ".jpg"
	case 3:
		dir, ext = "voice", ".opus"
	case 8:
		dir, ext = "documents", strings.ToLower(filepath.Ext(localPath))
		if ext == "" {
			ext = ".pdf"
		}
	default:
		return ""
	}
	return filepath.Join(backup.UnenrichedDir, dir, fmt.Sprintf("%d%s", pk, ext))
}

// quotedIDOf pulls the quoted stanza ID off a media message, so a reply
// carrying an attachment resolves its ZPARENTMESSAGE the same way a
// text reply does.
func quotedIDOf(msg *waE2E.Message) string {
	switch {
	case msg.GetImageMessage() != nil:
		return msg.GetImageMessage().GetContextInfo().GetStanzaID()
	case msg.GetAudioMessage() != nil:
		return msg.GetAudioMessage().GetContextInfo().GetStanzaID()
	case msg.GetDocumentMessage() != nil:
		return msg.GetDocumentMessage().GetContextInfo().GetStanzaID()
	}
	return ""
}
