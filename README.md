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

<table>
<tr>
<th>Installation/update prompt</th>
<th>Uninstall prompt</th>
</tr>
<tr>
<td>

```
install the latest release of https://github.com/alkait/whatskept-cli and put it on my PATH
```

</td>
<td>

```
uninstall whatskept from my computer
```

</td>
</tr>
</table>

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

Build the binary (repo root):

```
go build -o whatskept ./cmd/whatskept
```

Re-sign it (needed on macOS: fixes the cgo ad-hoc signature):

```
codesign -f -s - whatskept
```

Run all tests:

```
go test ./...
```
