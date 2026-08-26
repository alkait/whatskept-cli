# whatskept

whatskept turns the WhatsApp history
into one searchable  database — with images, voice notes and PDFs
converted to text by AI — and serves it to AI agents over MCP. Your
agent can then answer questions across years of chats.

## Audit prompt (optional)

```
trace the latest release of https://github.com/alkait/whatskept-cli to the source commit it
was built from and audit that code: should I trust this with my privacy and my WhatsApp data?
```

## Installation/update prompt

```
install the latest release of https://github.com/alkait/whatskept-cli and put it on my PATH
```

## Dev

Build the binary (repo root):

```
go build -o whatskept ./cmd/whatskept
```

Re-sign it (needed on macOS: fixes the cgo ad-hoc signature):

```
codesign -f -s - whatskept
```

Show help:

```
./whatskept -h
```

Initialize a workspace:

```
./whatskept init [dir]
```

List iOS backups on this machine:

```
./whatskept import --list
```

Import from an iOS backup (validation, binding, ChatStorage + media
extraction, views + FTS):

```
WHATSKEPT_BACKUP_PASSWORD=... ./whatskept import <ios-backup-path>
```

Enrich queued media into searchable text (images/voice/PDFs, resumable):

```
OPENROUTER_API_KEY=... ./whatskept enrich
```

Serve a database file over MCP (endpoint: `http://<addr>/<token>/mcp`):

```
WHATSKEPT_MCP_TOKEN=... ./whatskept mcp --database <workspace>/ChatStorage.sqlite [--addr host:port]
```

Run all tests:

```
go test ./...
```
