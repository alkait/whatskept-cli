# whatskept workspace — operator guide

You are operating a **whatskept workspace**: a directory holding one
searchable copy of the user's WhatsApp history, extracted from an
encrypted iOS backup. Your job here is to drive the `whatskept` CLI on
the user's behalf — import their backup, and serve the result over MCP.
This file is (re)written by `whatskept init` and always describes what
the installed binary can do.

## What lives here

```
./ChatStorage.sqlite         # the database — messages, chats, SQL views, FTS index
./.whatskept/settings.json   # portable workspace configuration (see below)
./AGENTS.md                  # this guide (the source of truth)
./CLAUDE.md                  # one-line stub importing AGENTS.md
./.unenriched/               # the enrichment queue:
    media/<rowid>.<ext>      #   images awaiting OCR + description
    voice/<rowid>.opus       #   voice notes awaiting transcription
    documents/<rowid>.pdf    #   PDFs awaiting text extraction
    */failed/                #   permanently-failed files (flagged/unreadable) — quarantined
    enrich.log               #   append-only history of every enrichment attempt
```

The text in the database is the record: files in `.unenriched/` exist
only until `whatskept enrich` turns them into text, then they are
deleted. The filename stem is the message rowid — it joins directly to
`ZWAMESSAGE.Z_PK` and the `wa_image_text` / `wa_voice_text` /
`wa_document_text` result tables. So the queue state reads directly:
an empty `.unenriched/` means fully enriched; files still in the kind
directories are pending; files under `failed/` are permanently rejected.

`enrich.log` is your history across runs — one timestamped line per
event (`run start`, `<kind> <rowid> enriched`, `… transient <reason>
(still queued)`, `… failed <reason> -> failed/`, `run done
enriched=… failed=… remaining=…`). Use it to understand what previous
runs did, why files are still queued, and to spot patterns (e.g. one
rowid failing transiently run after run → suggest moving it to
`failed/` manually). It can grow to tens of thousands of lines — always
`grep`/`tail` it, never read it whole.

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

## This guide vs. the binary

Run `whatskept -h` (and `<command> --help`) to see the installed
binary's actual commands and flags — trust that over this file. If they
disagree, this guide is stale: run `whatskept init` here to refresh it,
then re-read it.

## First: scan the workspace state

Check whether a `.env` file exists and which variables it defines —
names only, never the values:

```
cut -d= -f1 .env
```

The commands read `WHATSKEPT_BACKUP_PASSWORD` (import),
`OPENROUTER_API_KEY` (enrich), and `WHATSKEPT_MCP_TOKEN` (mcp) from
the environment. For any of those already in `.env`, don't ask the
user for the secret again — source the file into the command's
environment instead:

```
set -a; source .env; set +a; whatskept enrich
```

Then look at `settings.json` and the files, and pick the path:

1. **Clean slate** — no `udid`, no `ChatStorage.sqlite` → offer to
   import an encrypted iOS backup (below).
2. **Bound, but no `ChatStorage.sqlite`** — a previous import bound the
   workspace but extraction didn't finish → suggest re-importing, from
   the bound device only.
3. **`ChatStorage.sqlite` present, files in `.unenriched/`** — imported
   but not fully enriched → check `enrich.log` for what earlier runs
   did (hard failures? transient outage? never run at all?), then offer
   to run enrichment (below). Read-only SQL (`sqlite3 -readonly`)
   against the `wa_*_text` tables vs. the queue counts tells you the
   coverage gap precisely.
4. **`ChatStorage.sqlite` present, `.unenriched/` empty** — fully
   processed → offer to serve it over MCP (below). Re-importing from a
   newer backup of the same device is also fine at any time; it
   replaces the database wholesale, but enrichment results are carried
   forward automatically and already-enriched media is not re-queued.

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

## Enriching

Enrichment sends the queued media through OpenRouter (paid API) and
indexes the resulting text. Ask the user for their OpenRouter API key
first — inline env var, never stored, never echoed.

Once you have the key, fetch the credit balance and report it to the
user BEFORE starting:

```
curl -s https://openrouter.ai/api/v1/credits -H "Authorization: Bearer $OPENROUTER_API_KEY"
```

The remaining balance is `total_credits - total_usage` (USD). Tell the
user the balance and the queue size so they can decide whether to
proceed. All costs and balances are reported in USD, exactly as
OpenRouter states them — never convert to another currency. Then run:

```
OPENROUTER_API_KEY=<key> whatskept enrich
```

`--concurrency n` overrides the in-flight request cap (default 48).
Lower it if the log shows sustained rate-limit pushback; raising it
rarely helps beyond the provider's limits.

AFTER each run, fetch the balance again and report the difference —
that is the run's estimated credit spend.

**Project cost and time to finish.** From any completed (or safely
interrupted) run you have everything needed:

- cost per file  = balance delta ÷ files enriched this run
- rate           = files enriched ÷ run duration (the `run start` and
                   `run done` timestamps in `enrich.log`, or the
                   per-line timestamps for a partial run)
- remaining      = files still in the queue directories

Report: estimated credit to finish = remaining × cost per file, and
estimated time = remaining ÷ rate. Costs differ by kind (voice notes
and PDFs cost more than images), so when the mix allows, compute per
kind from the log's per-kind lines and sum.

On a first-ever run with a big queue, offer the user a measured sample
first: start `whatskept enrich`, let it process a few hundred files,
interrupt it (Ctrl-C is safe — everything committed stays, the run
resumes later), then present balance spent, rate, and the projections
above before continuing with the full queue.

It is fully resumable: interrupt or re-run any time, it continues where
it left off. Exit 0 means the queue fully drained. A non-zero exit with
"still queued" counts means transient API failures — just re-run later.
Files reported in `failed/` were rejected permanently (content flagged
or unreadable); they need no attention unless the user asks. Every
attempt is appended to `.unenriched/enrich.log` — grep it to diagnose
what happened and to advise the user on the next action.

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

- **Never** read, echo, or store secret values — not from `.env`, not
  from the user's messages. Listing `.env`'s variable NAMES
  (`cut -d= -f1 .env`) is fine; `cat .env` or printing any value is
  not. Secrets reach commands only via the environment.
- **Never start `whatskept enrich` — or any paid API call — without
  the user's explicit go-ahead in the current conversation.** Wanting
  to "check if it works" is not a reason; `whatskept enrich --help`
  and this guide answer that for free.
- **Never modify the database.** Read-only inspection is fine and
  encouraged for supervising enrichment (`sqlite3 -readonly
  ./ChatStorage.sqlite "..."`), but no INSERT/UPDATE/DELETE/DROP, ever
  — this is the user's only copy. End-user questions about the chat
  history still go through MCP, not direct SQL.
- **Never read media file contents into your context.** Listing
  `.unenriched/` files, counting them, and checking names/sizes is fine
  (that's how you assess progress); piping the bytes of a
  `.jpg`/`.opus`/`.pdf` into a model is not — enrichment exists so you
  never need to.
- Don't edit `settings.json` unless the user explicitly asks.
