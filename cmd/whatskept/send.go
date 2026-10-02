package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"whatskept/internal/live"
	"whatskept/internal/workspace"
)

// runSend hands one text message to the `whatskept live` process
// running in this workspace — live owns the WhatsApp connection, so it
// does the sending and records the message in the database. A non-empty
// replyTo is the stanza ID of the message to quote.
func runSend(chat, text, replyTo string) error {
	root, err := workspace.Find()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sent, err := live.Send(ctx, root, chat, text, replyTo)
	if err != nil {
		return err
	}
	fmt.Printf("sent id=%s chat=%s ts=%s\n", sent.ID, sent.Chat, sent.Ts)
	if sent.Warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", sent.Warning)
	}
	return nil
}
