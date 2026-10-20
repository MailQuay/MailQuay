-- +goose Up
CREATE TABLE users (
    id uuid PRIMARY KEY,
    username text NOT NULL,
    display_name text NOT NULL DEFAULT '',
    password_hash text NOT NULL,
    locale text NOT NULL DEFAULT 'en',
    theme text NOT NULL DEFAULT 'system',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_username_key ON users (lower(username));
CREATE UNIQUE INDEX users_singleton ON users ((true));

CREATE TABLE sessions (
    id uuid PRIMARY KEY,
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash bytea NOT NULL UNIQUE,
    expires_at timestamptz NOT NULL,
    user_agent text NOT NULL DEFAULT '',
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

CREATE TABLE accounts (
    id uuid PRIMARY KEY,
    name text NOT NULL,
    email text NOT NULL,
    color text NOT NULL DEFAULT '#f97316',
    provider text NOT NULL DEFAULT 'custom',
    username text NOT NULL,
    auth_type text NOT NULL DEFAULT 'password',
    password_enc bytea,
    oauth_provider text,
    oauth_scopes text[],
    oauth_refresh_enc bytea,
    receive_protocol text NOT NULL DEFAULT 'imap',
    imap_addr text,
    imap_security text NOT NULL DEFAULT 'implicit_tls',
    pop3_addr text,
    pop3_security text NOT NULL DEFAULT 'implicit_tls',
    smtp_addr text,
    smtp_security text NOT NULL DEFAULT 'starttls',
    smtp_helo_hostname text NOT NULL DEFAULT '',
    sync_enabled boolean NOT NULL DEFAULT true,
    sync_state text NOT NULL DEFAULT 'idle',
    last_error text NOT NULL DEFAULT '',
    initial_sync_range text NOT NULL DEFAULT 'all',
    download_scope text NOT NULL DEFAULT 'full_no_attachments',
    initial_sync_cutoff_at timestamptz,
    sent_copy_mode text NOT NULL DEFAULT 'auto',
    pop3_leave_on_server boolean NOT NULL DEFAULT true,
    pop3_delete_on_server boolean NOT NULL DEFAULT false,
    deleted_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT accounts_auth_type_check CHECK (auth_type IN ('password', 'oauth2')),
    CONSTRAINT accounts_receive_protocol_check CHECK (receive_protocol IN ('imap', 'pop3')),
    CONSTRAINT accounts_imap_security_check CHECK (imap_security IN ('implicit_tls', 'starttls', 'none')),
    CONSTRAINT accounts_pop3_security_check CHECK (pop3_security IN ('implicit_tls', 'starttls', 'none')),
    CONSTRAINT accounts_smtp_security_check CHECK (smtp_security IN ('implicit_tls', 'starttls', 'none')),
    CONSTRAINT accounts_initial_sync_range_check CHECK (initial_sync_range IN ('all', '1d', '3d', '1w', '1m', '1y')),
    CONSTRAINT accounts_download_scope_check CHECK (download_scope IN ('headers', 'full_no_attachments', 'full_with_attachments')),
    CONSTRAINT accounts_sent_copy_mode_check CHECK (sent_copy_mode IN ('auto', 'append', 'none'))
);
CREATE UNIQUE INDEX accounts_email_key ON accounts (lower(email)) WHERE deleted_at IS NULL;
CREATE INDEX accounts_active_idx ON accounts (created_at) WHERE deleted_at IS NULL;

CREATE TABLE identities (
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    email text NOT NULL,
    name text NOT NULL DEFAULT '',
    organization text NOT NULL DEFAULT '',
    reply_to text NOT NULL DEFAULT '',
    bcc text[],
    signature_html text NOT NULL DEFAULT '',
    signature_text text NOT NULL DEFAULT '',
    signature_mode text NOT NULL DEFAULT 'all',
    is_default boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, email),
    CONSTRAINT identities_signature_mode_check CHECK (signature_mode IN ('all', 'new', 'reply', 'forward', 'none'))
);
CREATE UNIQUE INDEX identities_default_key ON identities (account_id) WHERE is_default;

CREATE TABLE mailboxes (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    path text NOT NULL,
    name text NOT NULL,
    delimiter text NOT NULL DEFAULT '/',
    role text NOT NULL DEFAULT 'custom',
    attributes jsonb NOT NULL DEFAULT '[]'::jsonb,
    local_only boolean NOT NULL DEFAULT false,
    sync_mode text NOT NULL DEFAULT 'full',
    uidvalidity bigint,
    uidnext bigint,
    highestmodseq bigint,
    total integer NOT NULL DEFAULT 0,
    unread integer NOT NULL DEFAULT 0,
    sync_state text NOT NULL DEFAULT 'idle',
    last_synced_at timestamptz,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT mailboxes_role_check CHECK (role IN ('inbox', 'sent', 'drafts', 'trash', 'junk', 'archive', 'custom')),
    CONSTRAINT mailboxes_sync_mode_check CHECK (sync_mode IN ('full', 'pull', 'off')),
    CONSTRAINT mailboxes_sync_state_check CHECK (sync_state IN ('idle', 'syncing', 'error'))
);
CREATE UNIQUE INDEX mailboxes_account_path_key ON mailboxes (account_id, path);
CREATE INDEX mailboxes_account_role_idx ON mailboxes (account_id, role);

CREATE TABLE threads (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    subject_norm text NOT NULL,
    last_message_at timestamptz NOT NULL,
    message_count integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX threads_account_last_idx ON threads (account_id, last_message_at DESC);

CREATE TABLE messages (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    mailbox_id uuid NOT NULL REFERENCES mailboxes (id) ON DELETE CASCADE,
    uid bigint,
    pop3_uidl text,
    modseq bigint,
    message_id_header text NOT NULL DEFAULT '',
    provider_msg_id text NOT NULL DEFAULT '',
    dedupe_key text NOT NULL,
    in_reply_to text NOT NULL DEFAULT '',
    references_list text[],
    thread_id uuid REFERENCES threads (id) ON DELETE SET NULL,
    subject text NOT NULL DEFAULT '',
    from_addr jsonb,
    to_addrs jsonb NOT NULL DEFAULT '[]'::jsonb,
    cc_addrs jsonb NOT NULL DEFAULT '[]'::jsonb,
    bcc_addrs jsonb NOT NULL DEFAULT '[]'::jsonb,
    sent_at timestamptz,
    received_at timestamptz NOT NULL,
    size bigint NOT NULL DEFAULT 0,
    flags text[] NOT NULL DEFAULT '{}',
    keywords text[] NOT NULL DEFAULT '{}',
    local_flag boolean NOT NULL DEFAULT false,
    preserved_at timestamptz,
    snippet text NOT NULL DEFAULT '',
    has_attachments boolean NOT NULL DEFAULT false,
    raw_sha256 text NOT NULL DEFAULT '',
    body_state text NOT NULL DEFAULT 'missing',
    attachments_state text NOT NULL DEFAULT 'missing',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT messages_body_state_check CHECK (body_state IN ('missing', 'fetching', 'cached', 'unavailable')),
    CONSTRAINT messages_attachments_state_check CHECK (attachments_state IN ('missing', 'fetching', 'cached', 'unavailable'))
);
CREATE UNIQUE INDEX messages_mailbox_uid_key ON messages (mailbox_id, uid) WHERE uid IS NOT NULL;
CREATE UNIQUE INDEX messages_mailbox_uidl_key ON messages (mailbox_id, pop3_uidl) WHERE pop3_uidl IS NOT NULL;
CREATE INDEX messages_account_received_idx ON messages (account_id, received_at DESC, id DESC);
CREATE INDEX messages_mailbox_received_idx ON messages (mailbox_id, received_at DESC, id DESC);
CREATE INDEX messages_unread_idx ON messages (mailbox_id, received_at DESC) WHERE NOT ('\Seen' = ANY(flags));
CREATE INDEX messages_account_dedupe_idx ON messages (account_id, dedupe_key);
CREATE INDEX messages_account_marked_idx ON messages (account_id, local_flag) WHERE local_flag;
CREATE INDEX messages_thread_idx ON messages (thread_id, sent_at);

CREATE TABLE attachments (
    id uuid PRIMARY KEY,
    message_id uuid NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
    part_path text NOT NULL,
    filename text NOT NULL DEFAULT '',
    content_type text NOT NULL DEFAULT 'application/octet-stream',
    size bigint NOT NULL DEFAULT 0,
    sha256 text NOT NULL DEFAULT '',
    disposition text NOT NULL DEFAULT '',
    content_id text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX attachments_message_idx ON attachments (message_id);
CREATE INDEX attachments_sha256_idx ON attachments (sha256);


CREATE TABLE local_archives (
    id uuid PRIMARY KEY,
    origin_account_id uuid,
    origin_account_email text NOT NULL DEFAULT '',
    origin_mailbox_path text NOT NULL DEFAULT '',
    message_id_header text NOT NULL DEFAULT '',
    subject text NOT NULL DEFAULT '',
    from_addr jsonb,
    to_addrs jsonb NOT NULL DEFAULT '[]'::jsonb,
    cc_addrs jsonb NOT NULL DEFAULT '[]'::jsonb,
    bcc_addrs jsonb NOT NULL DEFAULT '[]'::jsonb,
    sent_at timestamptz,
    received_at timestamptz NOT NULL,
    size bigint NOT NULL DEFAULT 0,
    raw_sha256 text NOT NULL,
    text_sha256 text NOT NULL DEFAULT '',
    has_attachments boolean NOT NULL DEFAULT false,
    attachments jsonb NOT NULL DEFAULT '[]'::jsonb,
    archived_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX local_archives_message_key ON local_archives (origin_account_id, message_id_header) WHERE message_id_header <> '';
CREATE UNIQUE INDEX local_archives_raw_key ON local_archives (raw_sha256);
CREATE INDEX local_archives_archived_idx ON local_archives (archived_at DESC);

CREATE TABLE delete_queue (
    id uuid PRIMARY KEY,
    message_id uuid NOT NULL,
    account_id uuid NOT NULL,
    mailbox_id uuid NOT NULL,
    uid bigint,
    delete_after timestamptz NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT delete_queue_status_check CHECK (status IN ('pending', 'deleted', 'failed'))
);
CREATE INDEX delete_queue_due_idx ON delete_queue (status, delete_after);
CREATE UNIQUE INDEX delete_queue_message_key ON delete_queue (message_id) WHERE status = 'pending';

CREATE TABLE drafts (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    identity_account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    identity_email text NOT NULL DEFAULT '',
    in_reply_to text NOT NULL DEFAULT '',
    headers jsonb NOT NULL DEFAULT '{}'::jsonb,
    body_text text NOT NULL DEFAULT '',
    body_html text NOT NULL DEFAULT '',
    attachments jsonb NOT NULL DEFAULT '[]'::jsonb,
    revision integer NOT NULL DEFAULT 1,
    content_hash text NOT NULL DEFAULT '',
    origin text NOT NULL DEFAULT 'new',
    remote_uid bigint,
    remote_uidvalidity bigint,
    sync_state text NOT NULL DEFAULT 'local',
    conflict_of uuid,
    status text NOT NULL DEFAULT 'editing',
    sent_message_id uuid,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT drafts_status_check CHECK (status IN ('editing', 'sending', 'failed', 'copy_failed', 'sent')),
    CONSTRAINT drafts_sync_state_check CHECK (sync_state IN ('local', 'pulled', 'pushed', 'conflict'))
);
CREATE INDEX drafts_account_updated_idx ON drafts (account_id, updated_at DESC);
CREATE INDEX drafts_status_idx ON drafts (status, updated_at DESC);

CREATE TABLE outbox (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    identity_account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    identity_email text NOT NULL,
    draft_id uuid NOT NULL REFERENCES drafts (id) ON DELETE CASCADE,
    message_id_header text NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    scheduled_at timestamptz,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_until timestamptz,
    last_error text NOT NULL DEFAULT '',
    sent_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT outbox_status_check CHECK (status IN ('scheduled', 'pending', 'sending', 'sent', 'failed', 'copy_failed', 'cancelled'))
);
CREATE INDEX outbox_due_idx ON outbox (status, next_attempt_at);
CREATE INDEX outbox_scheduled_idx ON outbox (status, scheduled_at);
CREATE INDEX outbox_draft_idx ON outbox (draft_id);

CREATE TABLE pending_ops (
    id uuid PRIMARY KEY,
    account_id uuid NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    mailbox_id uuid REFERENCES mailboxes (id) ON DELETE CASCADE,
    message_id uuid REFERENCES messages (id) ON DELETE CASCADE,
    uid bigint,
    kind text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    status text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT pending_ops_kind_check CHECK (kind IN ('flags', 'move', 'delete', 'append', 'pop3_delete')),
    CONSTRAINT pending_ops_status_check CHECK (status IN ('pending', 'processing', 'done', 'failed'))
);
CREATE INDEX pending_ops_account_idx ON pending_ops (account_id, status, next_attempt_at);

CREATE TABLE search_jobs (
    id uuid PRIMARY KEY,
    message_id uuid NOT NULL,
    op text NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_until timestamptz,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT search_jobs_op_check CHECK (op IN ('index', 'delete')),
    CONSTRAINT search_jobs_status_check CHECK (status IN ('pending', 'processing', 'done', 'failed'))
);
CREATE UNIQUE INDEX search_jobs_pending_message_key ON search_jobs (message_id) WHERE status IN ('pending', 'processing');
CREATE INDEX search_jobs_due_idx ON search_jobs (status, next_attempt_at);

CREATE TABLE tasks (
    id uuid PRIMARY KEY,
    type text NOT NULL,
    account_id uuid,
    status text NOT NULL DEFAULT 'pending',
    done integer NOT NULL DEFAULT 0,
    total integer NOT NULL DEFAULT 0,
    unit text NOT NULL DEFAULT 'messages',
    message text NOT NULL DEFAULT '',
    error text NOT NULL DEFAULT '',
    started_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    dismissed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tasks_status_check CHECK (status IN ('pending', 'running', 'completed', 'failed', 'cancelled'))
);
CREATE UNIQUE INDEX tasks_active_key ON tasks (type, account_id) WHERE status IN ('pending', 'running');
CREATE INDEX tasks_status_updated_idx ON tasks (status, updated_at DESC);

CREATE TABLE oauth_states (
    state text PRIMARY KEY,
    provider text NOT NULL,
    verifier_enc bytea NOT NULL,
    redirect_after text NOT NULL DEFAULT '',
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX oauth_states_expires_idx ON oauth_states (expires_at);

CREATE TABLE settings (
    key text PRIMARY KEY,
    value jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS settings;
DROP TABLE IF EXISTS oauth_states;
DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS search_jobs;
DROP TABLE IF EXISTS pending_ops;
DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS drafts;
DROP TABLE IF EXISTS delete_queue;
DROP TABLE IF EXISTS local_archives;
DROP TABLE IF EXISTS attachments;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS threads;
DROP TABLE IF EXISTS mailboxes;
DROP TABLE IF EXISTS identities;
DROP TABLE IF EXISTS accounts;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
