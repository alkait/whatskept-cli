# whatskept

whatskept turns the WhatsApp history
into one searchable  database — with images, voice notes and PDFs
converted to text by AI — and serves it to AI agents over MCP. 

Your
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

## Uninstall prompt

```
uninstall whatskept from my computer
```

## Getting started

New workspace:

```
mkdir my-whatsapp && cd my-whatsapp && whatskept init && claude "Hi"
```

Existing workspace:

```
cd my-whatsapp && claude "Hi"
```

After saying "Hi", or really any thing the the agent takes over and guides you through the steps 😊

## Dev

Run all tests (repo root; no phone, key, or network needed):

```
go test ./...
```

Build the binary:

```
go build -o whatskept ./cmd/whatskept
```

### Real end-to-end tests

Gated behind the `e2e` build tag; they send real WhatsApp messages
between a dedicated tester number and the workspace's own account.
One-time setup — pair the tester's (second) number via QR:

```
go run ./cmd/whatsapp-tester pair
```

Run the suite, with `whatskept live` running in the workspace
(capture scenarios: texts, edits, revokes, replies, media):

```
go test -tags e2e ./e2e-test -workspace <dir>
```

Run the paid enrichment scenarios too, with live running with
`OPENROUTER_API_KEY` set (costs a fraction of a cent):

```
go test -tags e2e ./e2e-test -workspace <dir> -enrich
```
