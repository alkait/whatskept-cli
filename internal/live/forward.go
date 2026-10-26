package live

// Forwarding. A forward is a new message carrying the original's
// content, marked forwarded. For an attachment that content is not the
// file but its download details — the CDN path, the media key and the
// encrypted file's hash — which the phone keeps in ZWAMEDIAITEM, so a
// forward never needs the bytes we deleted after enrichment.
//
// Two catches, both verified against a real account:
//
//   - The CDN drops files after a few weeks. The phone still has them
//     and re-uploads on request (a media retry receipt), answering with
//     a fresh path under the same key.
//   - The plaintext SHA-256 a message must carry is not stored anywhere
//     in the row, so the file is downloaded once, in memory, to compute
//     it. Nothing is written to disk.
//
// Media captured by live itself has no stored download details yet;
// those rows become forwardable once the next import replaces them with
// the phone's own.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// forwardSource is the original message a forward copies.
type forwardSource struct {
	PK     int64
	Stanza string
	Type   int    // ZMESSAGETYPE: 0 text, 7 link, 1 image, 3 voice note, 8 document
	Text   string // text and link messages

	// Where the original sits — what a re-upload request names.
	Chat   types.JID
	FromMe bool
	Sender types.JID // group messages from others

	Media *storedMedia // attachments only
}

// storedMedia is an attachment's download details as the phone keeps
// them, plus what the recipient's client shows.
type storedMedia struct {
	MediaKey      []byte
	FileEncSHA256 []byte
	KeyTimestamp  int64
	DirectPath    string
	Mimetype      string
	Size          uint64
	Caption       string
	FileName      string
	Seconds       uint32
}

// sourceFunc reads the message to forward by stanza ID.
type sourceFunc func(ctx context.Context, stanzaID string) (forwardSource, error)

// fetchFunc readies a stored attachment for sending: a current download
// path and the plaintext hash.
type fetchFunc func(ctx context.Context, src forwardSource) (directPath string, fileSHA256 []byte, err error)

// forwardSource reads the message with the given stanza ID, from any
// chat. Refuses what cannot be forwarded with the reason.
func (w *Writer) forwardSource(ctx context.Context, stanzaID string) (forwardSource, error) {
	db, err := openRW(w.dbPath)
	if err != nil {
		return forwardSource{}, err
	}
	defer db.Close()

	var src forwardSource
	var fromMe int
	var text, chat, member, mime, caption, author, localPath sql.NullString
	var key, meta []byte
	var size, seconds sql.NullInt64
	err = db.QueryRowContext(ctx, `
		SELECT m.Z_PK, COALESCE(m.ZMESSAGETYPE, 0), m.ZTEXT, COALESCE(m.ZISFROMME, 0),
		       cs.ZCONTACTJID, gm.ZMEMBERJID,
		       mi.ZMEDIAKEY, mi.ZMETADATA, mi.ZVCARDSTRING, mi.ZFILESIZE,
		       mi.ZTITLE, mi.ZAUTHORNAME, mi.ZMOVIEDURATION, mi.ZMEDIALOCALPATH
		FROM   ZWAMESSAGE m
		LEFT JOIN ZWACHATSESSION cs ON cs.Z_PK = m.ZCHATSESSION
		LEFT JOIN ZWAGROUPMEMBER gm ON gm.Z_PK = m.ZGROUPMEMBER
		LEFT JOIN ZWAMEDIAITEM   mi ON mi.ZMESSAGE = m.Z_PK
		WHERE  m.ZSTANZAID = ? LIMIT 1`, stanzaID).Scan(
		&src.PK, &src.Type, &text, &fromMe, &chat, &member,
		&key, &meta, &mime, &size, &caption, &author, &seconds, &localPath)
	if errors.Is(err, sql.ErrNoRows) {
		return forwardSource{}, fmt.Errorf("no message with stanza ID %q in the database", stanzaID)
	}
	if err != nil {
		return forwardSource{}, err
	}
	src.Stanza, src.FromMe = stanzaID, fromMe != 0
	if chat.String != "" {
		if src.Chat, err = types.ParseJID(chat.String); err != nil {
			return forwardSource{}, fmt.Errorf("message %s: bad chat JID %q", stanzaID, chat.String)
		}
	}
	if member.String != "" {
		src.Sender, _ = types.ParseJID(member.String)
	}

	switch src.Type {
	case 0, 7:
		if text.String == "" {
			return forwardSource{}, fmt.Errorf("message %s has no text", stanzaID)
		}
		src.Text = text.String
		return src, nil
	case 14:
		return forwardSource{}, fmt.Errorf("message %s was deleted", stanzaID)
	case 3:
		// Voice notes only; other audio is a shared music or sound file.
		if !strings.HasSuffix(localPath.String, ".opus") {
			return forwardSource{}, fmt.Errorf("message %s is an audio file — only voice notes can be forwarded", stanzaID)
		}
	case 1, 8:
	default:
		return forwardSource{}, fmt.Errorf("message %s is of a type that cannot be forwarded yet (ZMESSAGETYPE %d) — text, links, images, voice notes and documents can", stanzaID, src.Type)
	}

	if len(key) == 0 || len(meta) == 0 {
		return forwardSource{}, fmt.Errorf("message %s: its attachment's download details are not stored (media captured by `whatskept live`) — it becomes forwardable after the next import", stanzaID)
	}
	m, err := decodeStoredMedia(key, meta)
	if err != nil {
		return forwardSource{}, fmt.Errorf("message %s: %w", stanzaID, err)
	}
	m.Mimetype = mime.String
	if m.Mimetype == "" {
		m.Mimetype = map[int]string{1: "image/jpeg", 3: "audio/ogg; codecs=opus", 8: "application/octet-stream"}[src.Type]
	}
	m.Size = uint64(size.Int64)
	m.Caption = caption.String
	m.FileName = author.String
	m.Seconds = uint32(seconds.Int64)
	src.Media = &m
	return src, nil
}

// decodeStoredMedia reads the two protobuf blobs the phone stores per
// attachment. Their schema is unpublished; the fields used here were
// established against a real account, where every one verified:
//
//	ZMEDIAKEY  f1 media key, f2 SHA-256 of the encrypted file, f3 key timestamp
//	ZMETADATA  f4 CDN direct path
func decodeStoredMedia(mediaKey, metadata []byte) (storedMedia, error) {
	var m storedMedia
	k, err := protoFields(mediaKey)
	if err != nil {
		return m, fmt.Errorf("decode ZMEDIAKEY: %w", err)
	}
	md, err := protoFields(metadata)
	if err != nil {
		return m, fmt.Errorf("decode ZMETADATA: %w", err)
	}
	m.MediaKey, m.FileEncSHA256 = k.bytes[1], k.bytes[2]
	m.KeyTimestamp = int64(k.varints[3])
	m.DirectPath = string(md.bytes[4])
	if len(m.MediaKey) != 32 || len(m.FileEncSHA256) != 32 || !strings.HasPrefix(m.DirectPath, "/") {
		return m, errors.New("stored download details are incomplete")
	}
	return m, nil
}

type wireFields struct {
	bytes   map[protowire.Number][]byte
	varints map[protowire.Number]uint64
}

// protoFields flattens one level of a protobuf message.
func protoFields(b []byte) (wireFields, error) {
	f := wireFields{map[protowire.Number][]byte{}, map[protowire.Number]uint64{}}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return f, protowire.ParseError(n)
		}
		b = b[n:]
		switch typ {
		case protowire.BytesType:
			v, n := protowire.ConsumeBytes(b)
			if n < 0 {
				return f, protowire.ParseError(n)
			}
			f.bytes[num] = v
			b = b[n:]
		case protowire.VarintType:
			v, n := protowire.ConsumeVarint(b)
			if n < 0 {
				return f, protowire.ParseError(n)
			}
			f.varints[num] = v
			b = b[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, b)
			if n < 0 {
				return f, protowire.ParseError(n)
			}
			b = b[n:]
		}
	}
	return f, nil
}

// forwardPlan marks the recording of a forward we sent, so the writer
// copies the original's derived text instead of downloading the file.
func forwardPlan(p plan, forwardOf int64) plan {
	if forwardOf != 0 && p.Media != nil {
		p.Media.CopyFrom = forwardOf
	}
	return p
}

// forwardMessage builds the message a forward sends: the original's
// content, marked forwarded. For an attachment, directPath and
// fileSHA256 come from fetchFunc.
func forwardMessage(src forwardSource, directPath string, fileSHA256 []byte) *waE2E.Message {
	ci := &waE2E.ContextInfo{IsForwarded: proto.Bool(true), ForwardingScore: proto.Uint32(1)}
	m := src.Media
	if m == nil {
		return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(src.Text), ContextInfo: ci}}
	}
	url := proto.String("https://mmg.whatsapp.net" + directPath)
	switch src.Type {
	case 1:
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			URL: url, DirectPath: &directPath, MediaKey: m.MediaKey, MediaKeyTimestamp: &m.KeyTimestamp,
			FileEncSHA256: m.FileEncSHA256, FileSHA256: fileSHA256, FileLength: &m.Size,
			Mimetype: &m.Mimetype, Caption: nilIfEmpty(m.Caption), ContextInfo: ci}}
	case 3:
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			URL: url, DirectPath: &directPath, MediaKey: m.MediaKey, MediaKeyTimestamp: &m.KeyTimestamp,
			FileEncSHA256: m.FileEncSHA256, FileSHA256: fileSHA256, FileLength: &m.Size,
			Mimetype: &m.Mimetype, Seconds: &m.Seconds, PTT: proto.Bool(true), ContextInfo: ci}}
	default:
		return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			URL: url, DirectPath: &directPath, MediaKey: m.MediaKey, MediaKeyTimestamp: &m.KeyTimestamp,
			FileEncSHA256: m.FileEncSHA256, FileSHA256: fileSHA256, FileLength: &m.Size,
			Mimetype: &m.Mimetype, FileName: nilIfEmpty(m.FileName), Title: nilIfEmpty(m.FileName),
			Caption: nilIfEmpty(m.Caption), ContextInfo: ci}}
	}
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// mediaFetcher implements fetchFunc over the live connection. Re-upload
// answers arrive as events, which the event handler hands to handle.
type mediaFetcher struct {
	client  *whatsmeow.Client
	mu      sync.Mutex
	waiting map[types.MessageID]chan *events.MediaRetry
}

func newMediaFetcher(client *whatsmeow.Client) *mediaFetcher {
	return &mediaFetcher{client: client, waiting: map[types.MessageID]chan *events.MediaRetry{}}
}

func (f *mediaFetcher) handle(evt *events.MediaRetry) {
	f.mu.Lock()
	ch := f.waiting[evt.MessageID]
	f.mu.Unlock()
	if ch != nil {
		select {
		case ch <- evt:
		default:
		}
	}
}

func (f *mediaFetcher) fetch(ctx context.Context, src forwardSource) (string, []byte, error) {
	path := src.Media.DirectPath
	data, err := f.download(ctx, src, path)
	if errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410) {
		logf("forward: media of pk=%d expired on the CDN — asking the phone to re-upload", src.PK)
		if path, err = f.reupload(ctx, src); err != nil {
			return "", nil, err
		}
		data, err = f.download(ctx, src, path)
	}
	if err != nil {
		return "", nil, fmt.Errorf("download media: %w", err)
	}
	sum := sha256.Sum256(data)
	return path, sum[:], nil
}

// download fetches and decrypts the attachment at path. The plaintext
// hash is unknown (that is what this computes), so whatsmeow's final
// check fails by design — but only after the ciphertext hash and the
// MAC have verified the bytes, and it returns them regardless.
func (f *mediaFetcher) download(ctx context.Context, src forwardSource, path string) ([]byte, error) {
	mt := map[int]whatsmeow.MediaType{1: whatsmeow.MediaImage, 3: whatsmeow.MediaAudio, 8: whatsmeow.MediaDocument}[src.Type]
	data, err := f.client.DownloadMediaWithPath(ctx, path, src.Media.FileEncSHA256, nil, src.Media.MediaKey, mt, "", true)
	if errors.Is(err, whatsmeow.ErrInvalidMediaSHA256) && len(data) > 0 {
		return data, nil
	}
	return data, err
}

// reupload asks the user's phone to put the attachment back on the CDN
// and waits for the new path.
func (f *mediaFetcher) reupload(ctx context.Context, src forwardSource) (string, error) {
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat: src.Chat, IsFromMe: src.FromMe, IsGroup: src.Chat.Server == types.GroupServer, Sender: src.Sender},
		ID: types.MessageID(src.Stanza),
	}
	if info.IsGroup && src.FromMe {
		if id := f.client.Store.ID; id != nil {
			info.Sender = id.ToNonAD()
		}
	}
	ch := make(chan *events.MediaRetry, 1)
	f.mu.Lock()
	f.waiting[info.ID] = ch
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.waiting, info.ID)
		f.mu.Unlock()
	}()

	if err := f.client.SendMediaRetryReceipt(ctx, info, src.Media.MediaKey); err != nil {
		return "", fmt.Errorf("ask the phone to re-upload: %w", err)
	}
	select {
	case evt := <-ch:
		if evt.Error != nil {
			return "", fmt.Errorf("the phone could not re-upload the media (error %d)", evt.Error.Code)
		}
		n, err := whatsmeow.DecryptMediaRetryNotification(evt, src.Media.MediaKey)
		if err != nil {
			return "", fmt.Errorf("re-upload answer: %w", err)
		}
		if n.GetResult() != waMmsRetry.MediaRetryNotification_SUCCESS || n.GetDirectPath() == "" {
			return "", fmt.Errorf("media no longer available — the phone answered %s", n.GetResult())
		}
		return n.GetDirectPath(), nil
	case <-ctx.Done():
		return "", errors.New("media no longer available on WhatsApp's servers and the phone did not re-upload it in time (is it online?)")
	}
}
