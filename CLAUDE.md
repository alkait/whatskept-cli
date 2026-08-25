# whatskept

One lean Go repo, one binary, consolidating whatskept, whatskept-mcp and
whatskept-live. No GUI. No backup/restore features. No deployment concerns.

## Shape

Everything lives in a workspace — a directory marked by a `.whatskept/`
directory, created with `whatskept init [dir]`. Every command works
inside one, except `mcp`, which addresses the database file explicitly.
`.whatskept/` contains only `settings.json`: portable configuration,
including the device/account binding. All other state (the shared
SQLite database, etc.) sits at the workspace root. Secrets come from
`.env` or the environment, never from settings:

- `whatskept import <ios-backup-path>` — seed history from an iOS backup,
  enrich everything, resumable. First import binds the workspace to that
  device and WhatsApp account; a different device's or account's backup is
  refused thereafter.
- `whatskept live` — capture new messages as they arrive, enriched through
  the same pipeline.
- `whatskept mcp --database <file>` — the only query surface; serves the
  unified DB over HTTP at a token-in-path endpoint.

Enrichment (image descriptions/OCR, voice transcripts, PDF text) runs via
OpenRouter on both import and live paths. Media, PDFs and voice files are
deleted once enriched — the text in the DB is the record.

## Philosophy

- **Less is more.** Minimal code, minimal commands, minimal README.
- **Small increments.** Build one small, agreed slice at a time; after every
  change, run `go test ./...` and `go build` so the tests and the binary at
  the repo root always reflect the current code.
- **No implementation without explicit approval.** A question, a design
  discussion, or an invitation to suggest options is never a green light to
  write code. Answer, propose, and wait for an explicit go-ahead.
- **Agent-first.** The CLI is operated by AI agents: non-interactive, clear
  parseable logs, honest exit codes, resumable. No interactive prompts.
- **One DB, one truth.** Import seeds it, live appends to it, mcp reads it.
- **Tested for real.** `whatsapp-tester` (separate dev tool, paired to a
  second WhatsApp number) drives end-to-end tests against the real account —
  no asking a human to send messages.

## Testing pattern

Every feature is covered the same way, all under one `go test ./...`:

- **Function tests** — call the Go functions directly against temp dirs.
- **Binary tests** — build the real binary once (in `TestMain`), run it as
  a subprocess, assert exit codes, output wording, and disk state.
- **Fake the external world** — features that depend on it get fakes:
  fixture backups in `testdata/`, a local `httptest` stand-in for the
  enrichment API. No test needs a phone, a key, or the network.
- **Real e2e is gated** — `whatsapp-tester` runs only when explicitly
  enabled; plain `go test ./...` stays green on any machine.
