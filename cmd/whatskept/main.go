package main

import (
	"fmt"
	"os"
	"path/filepath"

	"whatskept/internal/backup"
)

const usage = `whatskept — WhatsApp history, kept.

Usage:
  whatskept init [dir]               initialize a workspace (default: current directory)
  whatskept import <ios-backup-path> import history from an iOS backup
  whatskept import --list            list iOS backups on this machine
  whatskept enrich                   turn queued media into searchable text via OpenRouter [--concurrency n]
  whatskept mcp --database <file>    serve a database file over MCP (HTTP) [--addr host:port]
  whatskept -h | --help              show this help
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		fmt.Print(usage)
	case "init":
		dir := "."
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		if err := runInit(filepath.Clean(dir)); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "import":
		if len(os.Args) < 3 {
			fmt.Fprint(os.Stderr, "import requires the path to an iOS backup, or --list\n\n"+usage)
			os.Exit(2)
		}
		if os.Args[2] == "--list" {
			root := backup.DefaultRoot()
			if len(os.Args) > 3 {
				root = os.Args[3]
			}
			if err := runList(root); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		}
		if err := runImport(filepath.Clean(os.Args[2])); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "enrich":
		if err := runEnrich(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "mcp":
		if err := runMCP(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}
