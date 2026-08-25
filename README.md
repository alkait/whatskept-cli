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

Serve the workspace DB over MCP (endpoint: `http://<addr>/<token>/mcp`):

```
WHATSKEPT_MCP_TOKEN=... ./whatskept mcp [--addr host:port]
```

Run all tests:

```
go test ./...
```
