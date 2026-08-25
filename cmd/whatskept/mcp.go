package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"whatskept/internal/backup"
	"whatskept/internal/mcpserve"
	"whatskept/internal/workspace"
)

// runMCP serves the workspace database over MCP (streamable HTTP)
// until the process is signalled. The auth token comes from the
// environment (never a flag — secrets don't belong in process lists).
func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root, err := workspace.Find()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return mcpserve.Serve(ctx,
		filepath.Join(root, backup.ChatStorageName),
		*addr,
		os.Getenv(mcpserve.TokenEnv),
	)
}
