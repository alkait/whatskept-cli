# whatskept workspace — operator guide

You are operating a **whatskept workspace**: a directory holding one
searchable copy of the user's WhatsApp history, extracted from an
encrypted iOS backup. Your job here is to drive the `whatskept` CLI on
the user's behalf — import their backup, and serve the result over MCP.
This file is (re)written by `whatskept init` and always describes what
the installed binary can do.

## What lives here

```
./ChatStorage.sqlite     # the database — messages, chats, SQL views, FTS index
./.unenriched/           # decrypted media awaiting enrichment (media/, voice/, documents/)
./.whatskept/settings.json   # portable workspace configuration (see below)
./CLAUDE.md, ./AGENTS.md     # this guide (same content, two filenames)
```

The text in the database is the record: media files in `.unenriched/`
exist only until enrichment turns them into text, then they are deleted.
(Enrichment is not implemented in this binary version yet — the files
simply wait.)

## settings.json

```json
{
  "created_at": "…",
  "udid": "00008120-…",              // absent until first import
  "whatsapp_number": "+9715…"        // absent until first import
}
```

`udid` + `whatsapp_number` **bind** the workspace to one device and one
WhatsApp account. Both start absent. The first import stamps the
device's UDID before decryption and the account number after; from then
on, a backup from a different device or account is refused. That guard
protects the data — never work around it. If the user genuinely wants
to switch device/account, the escape hatch is a fresh workspace (or
they can hand-edit settings.json themselves; don't do it for them
unprompted).

## First: scan the workspace state

Look at `settings.json` and the files, then pick the path:

1. **Clean slate** — no `udid`, no `ChatStorage.sqlite` → offer to
   import an encrypted iOS backup (below).
2. **Bound, but no `ChatStorage.sqlite`** — a previous import bound the
   workspace but extraction didn't finish → suggest re-importing, from
   the bound device only.
3. **`ChatStorage.sqlite` present** — the history is imported → offer
   to serve it over MCP (below). Re-importing from a newer backup of
   the same device is also fine at any time; it replaces the database
   wholesale.

## Importing a backup

1. List the backups on this machine:

   ```
   whatskept import --list
   ```

   Only **encrypted** backups can be imported (WhatsApp data is only
   present in encrypted ones). If several encrypted backups exist, show
   the user the list (date, device name) and ask which to use. If the
   workspace is already bound, use the backup whose directory name
   equals the bound `udid` — any other will be refused.

2. Ask the user for their iOS backup password. Pass it inline as an
   environment variable — never store it, never echo it back:

   ```
   WHATSKEPT_BACKUP_PASSWORD=<password> whatskept import <backup-path>
   ```

3. The import binds (or verifies) the device and account, extracts
   `ChatStorage.sqlite`, downloads images/voice notes/PDFs into
   `.unenriched/`, and applies the SQL views + full-text index. It logs
   each stage and ends with a count of indexed messages. "missing"
   counts in the media stats are normal (iOS skips blobs not recently
   viewed on the device) — not errors.

## Serving over MCP

The database is queried through MCP only — do not open
`ChatStorage.sqlite` directly; the MCP server carries its own query
instructions for whichever agent connects.

1. Generate a token and start the server (from this directory):

   ```
   WHATSKEPT_MCP_TOKEN=$(openssl rand -hex 16) whatskept mcp --database ./ChatStorage.sqlite
   ```

   Run it in the background; it prints the endpoint, which embeds the
   token: `http://127.0.0.1:8787/<token>/mcp`. The unguessable path is
   the only credential — treat the URL as a secret.

2. To expose it for testing (e.g. a claude.ai custom connector), offer
   a Cloudflare quick tunnel (https://trycloudflare.com — no account
   needed; `brew install cloudflared` if missing):

   ```
   cloudflared tunnel --url http://127.0.0.1:8787
   ```

   Hand the user the combined URL to paste into their MCP client:

   ```
   https://<random>.trycloudflare.com/<token>/mcp
   ```

## Hard rules

- **Never** read `.env`, or echo/store the backup password or MCP token.
- **Never** read `.unenriched/**` file contents into your context — it
  is an enrichment queue holding tens of thousands of media files, not
  reading material.
- **Never** open or modify `ChatStorage.sqlite` directly; MCP is the
  query surface.
- Don't edit `settings.json` unless the user explicitly asks.
