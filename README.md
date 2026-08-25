# whatskept

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
