-- reactions.sql
-- Emoji reactions. Separate from views.sql because live has to apply this
-- to a running workspace, where views.sql (which drops the FTS index)
-- must not run. Idempotent.

-- ----------------------------------------------------------------------------
-- wa_reaction: one row per person per message, as in WhatsApp itself — a new
-- emoji replaces the old one, withdrawing it removes the row.
--   - target_stanza: the reacted-to message (v_messages.stanza_id). A stanza
--                    ID rather than a rowid: rowids change on re-import, and
--                    a reaction can arrive before its message.
--   - sender_jid:    '' when the reaction is the user's own (the phone stores
--                    those without a sender).
--   - ts:            UTC ISO, comparable with v_messages.ts.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS wa_reaction (
    target_stanza  TEXT    NOT NULL,
    sender_jid     TEXT    NOT NULL,
    is_from_me     INTEGER NOT NULL,
    emoji          TEXT    NOT NULL,
    ts             TEXT    NOT NULL,
    PRIMARY KEY (target_stanza, sender_jid)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS wa_reaction_ts_idx ON wa_reaction(ts);

-- ----------------------------------------------------------------------------
-- v_reactions: each reaction with its message, chat and resolved sender.
-- rowid and the message_* columns are NULL when the message isn't stored.
-- ----------------------------------------------------------------------------
DROP VIEW IF EXISTS v_reactions;
CREATE VIEW v_reactions AS
SELECT
    m.rowid                 AS rowid,
    r.target_stanza         AS stanza_id,
    m.chat_id               AS chat_id,
    m.chat_title            AS chat_title,
    m.ts                    AS message_ts,
    m.sender_name           AS message_sender,
    m.message_type_name     AS message_type_name,
    m.text                  AS message_text,
    r.emoji                 AS emoji,
    r.ts                    AS ts,
    r.is_from_me            AS is_from_me,
    NULLIF(r.sender_jid, '') AS sender_jid,
    -- Same order as v_messages.sender_name: saved name, then push name.
    CASE
        WHEN r.is_from_me = 1 THEN 'me'
        ELSE COALESCE(
            (SELECT display_name FROM wa_contact WHERE jid = r.sender_jid),
            (SELECT wc.display_name
               FROM wa_contact wc
               JOIN wa_jid_alias a ON a.phone_jid = wc.jid
              WHERE a.lid_jid = r.sender_jid),
            (SELECT COALESCE(NULLIF(ZCONTACTNAME, ''), NULLIF(ZFIRSTNAME, ''))
               FROM ZWAGROUPMEMBER
              WHERE ZMEMBERJID = r.sender_jid
                AND COALESCE(NULLIF(ZCONTACTNAME, ''), NULLIF(ZFIRSTNAME, '')) IS NOT NULL
              LIMIT 1),
            (SELECT ZPUSHNAME FROM ZWAPROFILEPUSHNAME WHERE ZJID = r.sender_jid),
            (SELECT p.ZPUSHNAME
               FROM ZWAPROFILEPUSHNAME p
               JOIN wa_jid_alias a ON a.phone_jid = p.ZJID
              WHERE a.lid_jid = r.sender_jid),
            CASE WHEN m.chat_kind = 'dm' THEN m.chat_title END,
            r.sender_jid
        )
    END                     AS sender_name
FROM wa_reaction r
LEFT JOIN v_messages m ON m.stanza_id = r.target_stanza;
