# Handoff — recover download details for media `live` captured (temporary)

Delete this file once the steps below are done.

## Background

Until the commit before this one, `live` stored captured media without the
download details `forward` needs (`ZWAMEDIAITEM.ZMEDIAKEY`, `ZMETADATA`), so
`send --forward` refused every image, voice note and document that `live`
had captured:

    cannot forward: message 3A2DB14FEACB6763D7EB: its attachment's download details are not stored …

That commit fixes this for new captures: `insertMediaItem` now writes
`ZMEDIAKEY`, `ZMETADATA` and `ZVCARDSTRING` (the mimetype) through
`encodeStoredMedia`, the counterpart of `decodeStoredMedia`. The rows
captured before the fix still have none. On 2026-10-26 that was 850 rows:
634 images, 156 voice notes and 60 documents.

## Steps on the host running `live`

1. `git pull`, `go test ./...`, `go build -o whatskept ./cmd/whatskept`,
   then restart `whatskept live` on the new binary, so no further rows
   are captured without details.

2. Write a one-off recovery program. Keep it out of the repo, or put it
   in an untracked directory and delete it afterwards. Run it while
   `live` is stopped: it uses `.whatskept/session.db`, and two connections
   on one session knock each other off. WhatsApp queues incoming
   messages for the device, so nothing is lost while `live` is down.
   - **Select:** rows in `wa_live_pk` joined to `ZWAMESSAGE`
     (`ZMESSAGETYPE IN (1,3,8)`) and `ZWAMEDIAITEM`, where `ZMEDIAKEY` or
     `ZMETADATA` is NULL. Newest first. For each row take the stanza ID,
     the chat JID (`ZWACHATSESSION.ZCONTACTJID`), `ZISFROMME`, and in
     groups the sender (`ZWAGROUPMEMBER.ZMEMBERJID`).
   - **Connect** a whatsmeow client on `session.db` with no handler that
     writes messages, so nothing new reaches the database or enrichment.
   - **Ask the phone to resend:**
     `client.SendPeerMessage(ctx, client.BuildUnavailableMessageRequest(chat, sender, id))`.
     The answer comes back as an `*events.Message` with
     `UnavailableRequestID` set. Send a few requests at a time, wait about
     20 seconds for each, and don't retry.
   - **Take only** the media key, `FileEncSHA256`, `MediaKeyTimestamp`,
     `DirectPath` and `Mimetype` from the image, audio or document message.
     Check them: a 32-byte key, a 32-byte hash, and a path starting
     with `/`.
   - **Write** them into that row's `ZWAMEDIAITEM` in the layout
     `encodeStoredMedia` uses (`internal/live/forward.go`): ZMEDIAKEY has
     f1 key, f2 enc-SHA256 and f3 timestamp as a varint; ZMETADATA has
     f4 direct path; `ZVCARDSTRING` holds the mimetype, written only if
     the column is NULL. Each row in its own transaction, and only while
     the columns are still NULL, so a second run is safe and resumes
     where the first stopped.
   - **Report** one line per row, `recovered|missing <pk> <stanza> <chat> <type>`,
     then totals. Exit non-zero if any row is missing.
   - **Go slow at first:** run on about 5 rows, including pk 638663
     (stanza `3A2DB14FEACB6763D7EB`). Check the phone really returns the
     media key, then run the rest. If it doesn't, stop and report to
     the user. Restart `live` between chunks if a run is slow.

3. Restart `live`, then forward the image the user asked for, the latest
   group image: Muna in "Cloud Leaders", 2026-10-26 05:24 UTC, stanza
   `3A2DB14FEACB6763D7EB`, to the user's own number +971504320432:
   `whatskept send +971504320432 --forward 3A2DB14FEACB6763D7EB`.

4. Delete the recovery program and this file. Commit the deletion only
   when the user asks.
