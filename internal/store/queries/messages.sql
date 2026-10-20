-- name: InsertMessage :one
INSERT INTO messages (
    id, account_id, mailbox_id, uid, pop3_uidl, modseq, message_id_header, provider_msg_id,
    dedupe_key, in_reply_to, references_list, thread_id, subject, from_addr, to_addrs,
    cc_addrs, bcc_addrs, sent_at, received_at, size, flags, keywords, local_flag,
    preserved_at, snippet, has_attachments, raw_sha256, body_state, attachments_state
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    $9, $10, $11, $12, $13, $14, $15,
    $16, $17, $18, $19, $20, $21, $22, $23,
    $24, $25, $26, $27, $28, $29
)
ON CONFLICT (mailbox_id, uid) WHERE uid IS NOT NULL DO UPDATE SET
    modseq = COALESCE(EXCLUDED.modseq, messages.modseq),
    message_id_header = CASE WHEN EXCLUDED.message_id_header <> '' THEN EXCLUDED.message_id_header ELSE messages.message_id_header END,
    provider_msg_id = CASE WHEN EXCLUDED.provider_msg_id <> '' THEN EXCLUDED.provider_msg_id ELSE messages.provider_msg_id END,
    dedupe_key = EXCLUDED.dedupe_key,
    in_reply_to = EXCLUDED.in_reply_to,
    references_list = EXCLUDED.references_list,
    subject = EXCLUDED.subject,
    from_addr = EXCLUDED.from_addr,
    to_addrs = EXCLUDED.to_addrs,
    cc_addrs = EXCLUDED.cc_addrs,
    bcc_addrs = EXCLUDED.bcc_addrs,
    sent_at = EXCLUDED.sent_at,
    size = EXCLUDED.size,
    flags = EXCLUDED.flags,
    keywords = EXCLUDED.keywords,
    snippet = EXCLUDED.snippet,
    has_attachments = EXCLUDED.has_attachments,
    updated_at = now()
RETURNING *;

-- name: UpsertPOP3Message :one
INSERT INTO messages (
    id, account_id, mailbox_id, pop3_uidl, message_id_header, dedupe_key,
    in_reply_to, references_list, subject, from_addr, to_addrs, cc_addrs, bcc_addrs,
    sent_at, received_at, size, flags, snippet, has_attachments, raw_sha256, body_state, attachments_state
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10, $11, $12, $13,
    $14, $15, $16, $17, $18, $19, $20, $21, $22
)
ON CONFLICT (mailbox_id, pop3_uidl) WHERE pop3_uidl IS NOT NULL DO UPDATE SET
    message_id_header = EXCLUDED.message_id_header,
    dedupe_key = EXCLUDED.dedupe_key,
    subject = EXCLUDED.subject,
    from_addr = EXCLUDED.from_addr,
    to_addrs = EXCLUDED.to_addrs,
    cc_addrs = EXCLUDED.cc_addrs,
    bcc_addrs = EXCLUDED.bcc_addrs,
    sent_at = EXCLUDED.sent_at,
    size = EXCLUDED.size,
    has_attachments = EXCLUDED.has_attachments,
    updated_at = now()
RETURNING *;

-- name: GetMessage :one
SELECT * FROM messages WHERE id = $1;

-- name: ListMailboxMessages :many
SELECT * FROM messages
WHERE mailbox_id = sqlc.arg(mailbox_id)
  AND (sqlc.narg(cursor_received)::timestamptz IS NULL OR (received_at, id) < (sqlc.narg(cursor_received)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY received_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListUnifiedMessages :many
WITH ranked AS (
    SELECT m.*, row_number() OVER (
        PARTITION BY m.account_id, m.dedupe_key
        ORDER BY CASE mb.role
            WHEN 'inbox' THEN 0 WHEN 'custom' THEN 1 WHEN 'junk' THEN 2
            WHEN 'archive' THEN 3 ELSE 4 END,
            m.received_at DESC, m.id DESC
    ) AS rn
    FROM messages m
    JOIN mailboxes mb ON mb.id = m.mailbox_id
    WHERE (sqlc.narg(account_id)::uuid IS NULL OR m.account_id = sqlc.narg(account_id)::uuid)
      AND (sqlc.narg(unread)::boolean IS NULL OR (NOT ('\Seen' = ANY(m.flags))) = sqlc.narg(unread)::boolean)
      AND (sqlc.narg(marked)::boolean IS NULL OR m.local_flag = sqlc.narg(marked)::boolean)
      AND (sqlc.narg(include_preserved)::boolean = true OR m.preserved_at IS NULL)
      AND (sqlc.narg(marked)::boolean = true OR mb.role NOT IN ('sent', 'drafts', 'trash'))
)
SELECT * FROM ranked
WHERE rn = 1
  AND (sqlc.narg(cursor_received)::timestamptz IS NULL OR (received_at, id) < (sqlc.narg(cursor_received)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY received_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListMessagesByIDs :many
SELECT * FROM messages WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ListMessageIDsByMailbox :many
SELECT id, uid FROM messages WHERE mailbox_id = $1;

-- name: ListMessageUIDsByMailbox :many
SELECT uid FROM messages WHERE mailbox_id = $1 AND uid IS NOT NULL;

-- name: UpdateMessageFlags :exec
UPDATE messages SET flags = $2, keywords = $3, modseq = COALESCE($4, modseq), updated_at = now() WHERE id = $1;

-- name: SetMessageLocalFlag :exec
UPDATE messages SET local_flag = $2, updated_at = now() WHERE id = $1;

-- name: SetMessagesLocalFlagByDedupe :exec
UPDATE messages SET local_flag = $3, updated_at = now() WHERE account_id = $1 AND dedupe_key = $2;

-- name: SetMessagePreservedAt :exec
UPDATE messages SET preserved_at = $2, updated_at = now() WHERE id = $1;

-- name: SetMessageBodyState :exec
UPDATE messages SET body_state = $2, snippet = $3, updated_at = now() WHERE id = $1;

-- name: SetMessageCacheState :exec
UPDATE messages SET body_state = $2, attachments_state = $3, snippet = $4, has_attachments = $5, updated_at = now() WHERE id = $1;

-- name: SetMessageRawInfo :exec
UPDATE messages SET raw_sha256 = $2, body_state = $3, updated_at = now() WHERE id = $1;

-- name: SetMessageDedupeKey :exec
UPDATE messages SET dedupe_key = $2, updated_at = now() WHERE id = $1;

-- name: MoveMessage :exec
UPDATE messages SET mailbox_id = $2, uid = $3, updated_at = now() WHERE id = $1;

-- name: DeleteMessage :exec
DELETE FROM messages WHERE id = $1;

-- name: DeleteMessagesByMailbox :exec
DELETE FROM messages WHERE mailbox_id = $1;

-- name: DeleteMessagesByAccountExceptPreserved :exec
DELETE FROM messages WHERE account_id = $1 AND preserved_at IS NULL;

-- name: ListMessagesForCleanup :many
SELECT * FROM messages WHERE account_id = $1 AND preserved_at IS NULL;

-- name: CountUnread :one
SELECT count(*) FROM messages m JOIN mailboxes mb ON mb.id = m.mailbox_id
WHERE (sqlc.narg(account_id)::uuid IS NULL OR m.account_id = sqlc.narg(account_id)::uuid)
  AND mb.role NOT IN ('sent', 'drafts', 'trash')
  AND NOT ('\Seen' = ANY(m.flags));

-- name: CountMarked :one
SELECT count(*) FROM messages WHERE local_flag;

-- name: CountMessagesInMailbox :one
SELECT count(*) FROM messages WHERE mailbox_id = $1;

-- name: ListMarkedForAccount :many
SELECT m.* FROM messages m WHERE m.account_id = $1 AND m.local_flag ORDER BY m.received_at DESC;

-- name: ListRecentMessagesForAccount :many
SELECT * FROM messages WHERE account_id = $1 ORDER BY received_at DESC LIMIT $2;

-- name: UpdateMessageSnippet :exec
UPDATE messages SET snippet = $2, updated_at = now() WHERE id = $1;

-- name: UpdateMessageThread :exec
UPDATE messages SET thread_id = $2, updated_at = now() WHERE id = $1;

-- name: FindMessageByHeader :one
SELECT * FROM messages
WHERE account_id = $1 AND lower(message_id_header) = lower($2) AND message_id_header <> ''
ORDER BY received_at DESC LIMIT 1;

-- name: ListMessageViewsForAccount :many
SELECT m.*, mb.role AS mailbox_role, mb.path AS mailbox_path
FROM messages m JOIN mailboxes mb ON mb.id = m.mailbox_id
WHERE m.account_id = $1
ORDER BY m.received_at DESC;

-- name: ListAllMessages :many
SELECT * FROM messages ORDER BY received_at;

-- name: ListSearchableMessages :many
SELECT * FROM messages ORDER BY received_at;

-- name: CountUnifiedAll :one
SELECT count(DISTINCT (m.account_id, m.dedupe_key))
FROM messages m JOIN mailboxes mb ON mb.id = m.mailbox_id
WHERE m.preserved_at IS NULL AND mb.role NOT IN ('sent', 'drafts', 'trash');

-- name: CountUnifiedUnread :one
SELECT count(DISTINCT (m.account_id, m.dedupe_key))
FROM messages m JOIN mailboxes mb ON mb.id = m.mailbox_id
WHERE m.preserved_at IS NULL AND mb.role NOT IN ('sent', 'drafts', 'trash')
  AND NOT ('\Seen' = ANY(m.flags));

-- name: CountDrafts :one
SELECT count(*) FROM drafts WHERE status <> 'sent';

-- name: CountOutboxActive :one
SELECT count(*) FROM outbox WHERE status IN ('scheduled', 'pending', 'sending', 'failed', 'copy_failed');

-- name: CountByMailboxRole :one
SELECT COALESCE(sum(mb.total), 0)::bigint FROM mailboxes mb WHERE mb.role = $1;

-- name: CountUnreadInMailbox :one
SELECT count(*) FROM messages WHERE mailbox_id = $1 AND NOT ('\Seen' = ANY(flags));

-- name: UpdateFlagsByDedupe :exec
UPDATE messages SET flags = $3, keywords = $4, updated_at = now()
WHERE account_id = $1 AND dedupe_key = $2;

-- name: GetMessagesByDedupe :many
SELECT * FROM messages WHERE account_id = $1 AND dedupe_key = $2;

-- name: FindMessageByUID :one
SELECT * FROM messages WHERE mailbox_id = $1 AND uid = $2;

-- name: ListPOP3Messages :many
SELECT id, pop3_uidl FROM messages WHERE mailbox_id = $1 AND pop3_uidl IS NOT NULL;

-- name: ClearMessageBodyCache :exec
UPDATE messages SET raw_sha256 = '', body_state = 'missing', attachments_state = 'missing', updated_at = now()
WHERE account_id = sqlc.arg(account_id) AND NOT local_flag AND preserved_at IS NULL
  AND (sqlc.narg(mailbox_id)::uuid IS NULL OR mailbox_id = sqlc.narg(mailbox_id)::uuid);

-- name: ClearMessageAttachmentCache :exec
UPDATE messages SET attachments_state = 'missing', updated_at = now()
WHERE account_id = sqlc.arg(account_id) AND NOT local_flag AND preserved_at IS NULL
  AND (sqlc.narg(mailbox_id)::uuid IS NULL OR mailbox_id = sqlc.narg(mailbox_id)::uuid);

-- name: ListMessagesOlderThan :many
SELECT * FROM messages
WHERE account_id = $1 AND received_at < $2 AND NOT local_flag AND preserved_at IS NULL;

-- name: ListMessagesByRole :many
WITH ranked AS (
    SELECT m.*, row_number() OVER (
        PARTITION BY m.account_id, m.dedupe_key ORDER BY m.received_at DESC, m.id DESC
    ) AS rn
    FROM messages m
    JOIN mailboxes mb ON mb.id = m.mailbox_id
    WHERE mb.role = sqlc.arg(role)
      AND (sqlc.narg(account_id)::uuid IS NULL OR m.account_id = sqlc.narg(account_id)::uuid)
)
SELECT * FROM ranked
WHERE rn = 1
  AND (sqlc.narg(cursor_received)::timestamptz IS NULL OR (received_at, id) < (sqlc.narg(cursor_received)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY received_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListMailboxUnreadMessages :many
SELECT * FROM messages
WHERE mailbox_id = sqlc.arg(mailbox_id) AND NOT ('\Seen' = ANY(flags))
  AND (sqlc.narg(cursor_received)::timestamptz IS NULL OR (received_at, id) < (sqlc.narg(cursor_received)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY received_at DESC, id DESC
LIMIT sqlc.arg(page_limit);
