package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"whatskept/internal/enrich"
	"whatskept/internal/views"
	"whatskept/internal/workspace"
)

// runEnrich drains the workspace's .unenriched/ queue through
// OpenRouter. Exit contract: nil (exit 0) only when the queue fully
// drained; anything left failing or queued is an error the caller
// should see.
func runEnrich() error {
	root, err := workspace.Find()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stats, err := enrich.Run(ctx, enrich.Options{
		Root:       root,
		Client:     &enrich.Client{APIKey: os.Getenv(enrich.APIKeyEnv)},
		Log:        func(line string) { fmt.Println(line) },
		RebuildFTS: views.Apply,
	})
	printKind := func(name string, k enrich.KindStats) {
		fmt.Printf("%s: %d enriched, %d failed, %d still queued, %d orphaned\n",
			name, k.Enriched, k.Failed, k.Remaining, k.Orphaned)
	}
	printKind("images", stats.Images)
	printKind("voice notes", stats.Voice)
	printKind("documents", stats.Documents)
	if stats.FTSRows > 0 {
		fmt.Printf("full-text index rebuilt: %d messages\n", stats.FTSRows)
	}
	if err != nil {
		return err
	}
	if !stats.Drained() {
		return fmt.Errorf("queue not fully enriched — failed files are in .unenriched/*/failed/, queued files retry on the next run")
	}
	fmt.Println("queue fully enriched")
	return nil
}
