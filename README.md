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

Run all tests (repo root):

```
go test ./...
```

Build the binary:

```
go build -o whatskept ./cmd/whatskept
```
