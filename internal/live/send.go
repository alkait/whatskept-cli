package live

// Sending. The linked session allows one connection and `whatskept
// live` owns it, so nothing else can put a message on the wire: `send`
// and the MCP send tool hand the text to the running live process over
// a loopback HTTP endpoint. The address and a random token are
// advertised in .whatskept/live.json while live runs — the token is
// the credential, because any local process can reach a loopback port.
//
// WhatsApp never echoes a device's own sends back to it, so a sent
// message is recorded here, through the same decide → Apply path every
// captured message takes.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"whatskept/internal/backup"
	"whatskept/internal/workspace"
)

const sendTimeout = 30 * time.Second

// ErrNotRunning is what Send returns when no live process is listening
// in the workspace.
var ErrNotRunning = errors.New("`whatskept live` is not running in this workspace — start it, then send again")

// Sent describes a delivered message.
type Sent struct {
	ID   string `json:"id" jsonschema:"the message's stanza ID (ZSTANZAID in the database)"`
	Chat string `json:"chat" jsonschema:"the chat JID the message went to"`
	Ts   string `json:"ts" jsonschema:"send time, UTC ISO"`
	// Warning is set when the message went out but could not be written
	// to the database. Never retry on it — the recipient has the message.
	Warning string `json:"warning,omitempty" jsonschema:"set when the message was sent but not recorded in the database; do not resend"`
}

type sendRequest struct {
	To      string `json:"to"`
	Text    string `json:"text"`
	ReplyTo string `json:"reply_to,omitempty"` // stanza ID of the message to quote
}

type sendReply struct {
	Sent
	Error string `json:"error,omitempty"`
}

type endpoint struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
}

// deliverFunc puts a message on the wire and returns the stanza ID and
// timestamp WhatsApp assigned. Injected so the endpoint is testable
// without a WhatsApp connection.
type deliverFunc func(ctx context.Context, to types.JID, msg *waE2E.Message) (types.MessageID, time.Time, error)

// quoteFunc builds the reply context for answering the message with the
// given stanza ID in chat `to` (see Writer.quote).
type quoteFunc func(ctx context.Context, to types.JID, stanzaID string) (*waE2E.ContextInfo, error)

// ParseChat turns what a caller typed into a chat JID: a phone number
// ("+971 50 000 0001") or a full JID (…@s.whatsapp.net, …@g.us, …@lid).
func ParseChat(s string) (types.JID, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "@") {
		jid, err := types.ParseJID(s)
		if err != nil || jid.User == "" {
			return types.JID{}, fmt.Errorf("invalid chat JID %q", s)
		}
		switch jid.Server {
		case types.DefaultUserServer, types.GroupServer, types.HiddenUserServer:
			return jid.ToNonAD(), nil
		}
		return types.JID{}, fmt.Errorf("cannot send to %q — only chats (@%s, @%s, @%s)",
			s, types.DefaultUserServer, types.GroupServer, types.HiddenUserServer)
	}
	digits := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(strings.TrimPrefix(s, "+"))
	if len(digits) < 7 || len(digits) > 15 || strings.Trim(digits, "0123456789") != "" {
		return types.JID{}, fmt.Errorf("invalid chat %q — want a phone number in international format or a full JID", s)
	}
	return types.NewJID(digits, types.DefaultUserServer), nil
}

// sentEvent is the message event WhatsApp would have delivered had the
// send come from another of the user's devices.
func sentEvent(to types.JID, id types.MessageID, ts time.Time, msg *waE2E.Message) *events.Message {
	v := &events.Message{Message: msg}
	v.Info.ID = id
	v.Info.Chat = to
	v.Info.IsFromMe = true
	v.Info.IsGroup = to.Server == types.GroupServer
	v.Info.Timestamp = ts
	return v
}

// startSend serves the send endpoint for the workspace at root and
// advertises it in .whatskept/live.json. The returned stop closes the
// listener and removes the advertisement.
func startSend(root string, deliver deliverFunc, quote quoteFunc, record func(*events.Message) error) (stop func(), err error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(raw)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for sends: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /"+token+"/send", func(rw http.ResponseWriter, r *http.Request) {
		reply := func(status int, v sendReply) {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(status)
			json.NewEncoder(rw).Encode(v)
		}
		var req sendRequest
		if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1<<20)).Decode(&req); err != nil {
			reply(http.StatusBadRequest, sendReply{Error: "bad request: " + err.Error()})
			return
		}
		to, err := ParseChat(req.To)
		if err != nil {
			reply(http.StatusBadRequest, sendReply{Error: err.Error()})
			return
		}
		if strings.TrimSpace(req.Text) == "" {
			reply(http.StatusBadRequest, sendReply{Error: "empty message text"})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), sendTimeout)
		defer cancel()
		// A reply is the same text carrying a quote of its target — the
		// shape textOf reads back, so recording links ZPARENTMESSAGE.
		msg := &waE2E.Message{Conversation: proto.String(req.Text)}
		if req.ReplyTo != "" {
			quoted, err := quote(ctx, to, req.ReplyTo)
			if err != nil {
				reply(http.StatusBadRequest, sendReply{Error: "cannot reply: " + err.Error()})
				return
			}
			msg = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(req.Text), ContextInfo: quoted}}
		}
		id, ts, err := deliver(ctx, to, msg)
		if err != nil {
			logf("send to %s FAILED: %v", to, err)
			reply(http.StatusBadGateway, sendReply{Error: "send failed: " + err.Error()})
			return
		}
		out := sendReply{Sent: Sent{ID: string(id), Chat: to.String(), Ts: ts.UTC().Format(time.RFC3339)}}
		if err := record(sentEvent(to, id, ts, msg)); err != nil {
			out.Warning = "sent, but not recorded in the database: " + err.Error()
		}
		reply(http.StatusOK, out)
	})

	data, err := json.Marshal(endpoint{Addr: ln.Addr().String(), Token: token})
	if err != nil {
		ln.Close()
		return nil, err
	}
	// Removed first: WriteFile keeps the mode of a file left by a crash.
	path := workspace.LiveEndpointPath(root)
	os.Remove(path)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("advertise send endpoint: %w", err)
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return func() {
		srv.Close()
		os.Remove(path)
	}, nil
}

// StartFakeSend runs the real send endpoint for root with a stand-in
// for WhatsApp: nothing reaches the wire, stanza IDs are FAKE1, FAKE2…,
// and messages are recorded in the workspace database as usual. Test
// support for the packages that drive sends from outside — the CLI and
// the MCP server.
func StartFakeSend(root string) (stop func(), err error) {
	w := &Writer{dbPath: filepath.Join(root, backup.ChatStorageName), root: root}
	if err := w.EnsureReady(context.Background()); err != nil {
		return nil, err
	}
	var n atomic.Int64
	return startSend(root,
		func(context.Context, types.JID, *waE2E.Message) (types.MessageID, time.Time, error) {
			return types.MessageID(fmt.Sprintf("FAKE%d", n.Add(1))), time.Now(), nil
		},
		w.quote,
		func(v *events.Message) error {
			_, err := w.Apply(context.Background(), decide(v, nil))
			return err
		})
}

// Send asks the live process running in the workspace at root to send
// text to a chat (see ParseChat for the accepted forms). A non-empty
// replyTo is the stanza ID of a message in that chat to quote.
func Send(ctx context.Context, root, to, text, replyTo string) (Sent, error) {
	data, err := os.ReadFile(workspace.LiveEndpointPath(root))
	if err != nil {
		return Sent{}, ErrNotRunning
	}
	var ep endpoint
	if err := json.Unmarshal(data, &ep); err != nil {
		return Sent{}, fmt.Errorf("parse %s: %w", workspace.LiveEndpointPath(root), err)
	}
	body, err := json.Marshal(sendRequest{To: to, Text: text, ReplyTo: replyTo})
	if err != nil {
		return Sent{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+ep.Addr+"/"+ep.Token+"/send", bytes.NewReader(body))
	if err != nil {
		return Sent{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: sendTimeout + 10*time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// A refused dial is a live.json left behind by a crash.
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return Sent{}, ErrNotRunning
		}
		return Sent{}, err
	}
	defer resp.Body.Close()
	var reply sendReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return Sent{}, fmt.Errorf("live answered %s", resp.Status)
	}
	if reply.Error != "" {
		return Sent{}, errors.New(reply.Error)
	}
	return reply.Sent, nil
}
