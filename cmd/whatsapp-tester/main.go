// whatsapp-tester — dev tool driving the real end-to-end tests.
//
// It pairs to a SECOND WhatsApp number (never the workspace's own
// account) and sends real messages that `whatskept live` must capture;
// the e2e suite (go test -tags e2e ./e2e-test) drives it and asserts
// against the workspace database. Not part of any release: releases
// build only ./cmd/whatskept.
//
// Every send prints the message's stanza ID — that is the key the e2e
// tests (and later edit/revoke scenarios) use to find the row.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

const usage = `whatsapp-tester — send real WhatsApp messages for e2e tests.

Usage:
  whatsapp-tester pair                     link the tester's (second) number via QR
  whatsapp-tester send-text <jid> <text>   send a text message, print its stanza ID

Flags:
  --store <file>   session store (default e2e-test/session.db)
`

var storePath = flag.String("store", filepath.Join("e2e-test", "session.db"),
	"SQLite file holding the tester's WhatsApp session")

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch args[0] {
	case "pair":
		err = runPair(ctx)
	case "send-text":
		if len(args) != 3 {
			fmt.Fprint(os.Stderr, "send-text requires <jid> <text>\n\n"+usage)
			os.Exit(2)
		}
		err = runSendText(ctx, args[1], args[2])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", args[0], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newClient(ctx context.Context) (*whatsmeow.Client, error) {
	// A distinct device name so the tester is recognizable — and
	// deletable — in the second number's Linked Devices list.
	store.SetOSInfo("whatskept-tester", [3]uint32{0, 1, 0})
	if err := os.MkdirAll(filepath.Dir(*storePath), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on&_busy_timeout=5000", *storePath)
	container, err := sqlstore.New(ctx, "sqlite3", dsn, waLog.Stdout("store", "ERROR", false))
	if err != nil {
		return nil, fmt.Errorf("open session store %s: %w", *storePath, err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("read device from store: %w", err)
	}
	return whatsmeow.NewClient(device, waLog.Stdout("client", "ERROR", false)), nil
}

func runPair(ctx context.Context) error {
	client, err := newClient(ctx)
	if err != nil {
		return err
	}
	defer client.Disconnect()
	if client.Store.ID != nil {
		fmt.Printf("already paired as %s — to re-pair, delete %s\n", client.Store.ID, *storePath)
		return nil
	}

	// The QR "success" event is NOT the end of pairing: the phone still
	// syncs keys and app state with this device, over a connection
	// whatsmeow drops and re-establishes after the handshake.
	// Disconnecting at "success" leaves a half-paired device that never
	// appears in Linked Devices — so wait for the post-pair login
	// (events.Connected) and then let the initial sync settle.
	connected := make(chan struct{}, 1)
	client.AddEventHandler(func(evt any) {
		if _, ok := evt.(*events.Connected); ok {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})

	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("open QR channel: %w", err)
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	for item := range qrChan {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			fmt.Println()
			fmt.Println("── Scan with the TESTER number's WhatsApp → Linked Devices ──")
			qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stdout)
			fmt.Printf("\n(expires in %s)\n\n", item.Timeout.Round(time.Second))
		case "success":
			fmt.Println("QR accepted — completing pairing, keep the phone online…")
			select {
			case <-connected:
			case <-time.After(60 * time.Second):
				return fmt.Errorf("pairing never completed — delete %s and try again", *storePath)
			}
			fmt.Println("connected — letting the initial sync settle (15s)…")
			time.Sleep(15 * time.Second)
			fmt.Printf("paired as %s\n", client.Store.ID)
			return nil
		case "timeout":
			return errors.New("QR expired — run pair again for a fresh code")
		default:
			if item.Error != nil {
				return fmt.Errorf("pairing failed (%s): %w", item.Event, item.Error)
			}
		}
	}
	return nil
}

// connect brings up an already-paired session and waits until it is
// usable for sending.
func connect(ctx context.Context) (*whatsmeow.Client, error) {
	client, err := newClient(ctx)
	if err != nil {
		return nil, err
	}
	if client.Store.ID == nil {
		return nil, errors.New("not paired — run `whatsapp-tester pair` first")
	}
	if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if !client.WaitForConnection(20 * time.Second) {
		client.Disconnect()
		return nil, errors.New("connection not established within 20s")
	}
	return client, nil
}

func runSendText(ctx context.Context, jid, text string) error {
	to, err := types.ParseJID(jid)
	if err != nil {
		return fmt.Errorf("parse jid %q: %w", jid, err)
	}
	client, err := connect(ctx)
	if err != nil {
		return err
	}
	defer client.Disconnect()
	resp, err := client.SendMessage(ctx, to, &waE2E.Message{Conversation: proto.String(text)})
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	// The stanza ID is the contract with the e2e suite — everything
	// asserts (and later edits/revokes) by this key.
	fmt.Printf("sent id=%s ts=%s\n", resp.ID, resp.Timestamp.UTC().Format(time.RFC3339))
	return nil
}
