# MailQuay

[简体中文](README_CN.md) | English

MailQuay is a self-hosted webmail client for mailboxes that already exist. It connects to providers over IMAP, POP3, and SMTP, keeps a synchronized copy of mail on your own server, and serves a React single-page frontend to any browser.

MailQuay is not a mail server. It does not run an MTA, does not own a domain, and does not deliver mail between servers. It is the client layer for mainstream hosted mailboxes, self-hosted IMAP or POP3 servers, and any other standards-compliant mailbox.

> **Status:** implementation. The backend, database migrations, API, synchronization workers and single-page frontend are implemented. Run `make build-full` after building the frontend, or use the Compose stack.

## Goals

- **Server-side sync.** Mail is fetched, indexed, and cached by the backend, so every browser sees the same state without a desktop client running.
- **One inbox, many accounts.** Unified views across accounts while preserving per-account folders and provider semantics.
- **Standards first.** IMAP, POP3, and SMTP only. No provider-specific SDK is required to read or send mail. Every protocol accepts a custom host and port.
- **Self-hosted and private.** Credentials are encrypted at rest, mail bodies stay on your server, and remote content in HTML mail is blocked by default.
- **Boring operations.** One Go binary, one database, one data directory, and Docker Compose.

## Planned features

- Multi-account unified inbox, folder tree, and unread counts.
- IMAP synchronization of folders, flags, and message metadata, with background body prefetch and on-demand fetch for the rest.
- POP3 compatibility mode for providers that only expose POP3: one virtual inbox, polling, UIDL tracking, and local-only flags.
- Per-account sync range (all time by default, or 1 day to 1 year) and download scope (headers, full without attachments, or full with attachments), fetched and indexed in the background.
- Per-account protocol configuration with custom host, port, and TLS mode for IMAP, POP3, and SMTP.
- Runtime settings (sync intervals, limits, CORS, trusted proxies, OAuth apps, log level) stored in PostgreSQL and edited in the web UI.
- Automatic sync and search concurrency derived from container CPU and memory limits, with manual overrides.
- Compose, reply, reply-all, forward, drafts, attachments, scheduled send, and a retrying outbox with a configurable retry limit.
- Multiple sender identities per mailbox account, each with its own signature and reply settings.
- Local marked mail keeps full bodies and attachments and survives account removal by default.
- Local archive stores an independent full copy that survives remote deletion and account removal.
- Delete either moves mail to Junk for provider cleanup or uses a local retention queue with a configurable number of days.
- Structured search combines subject/all mode with account, folder, recipient, sender, and attachment filters.
- Unified views deduplicate the same message across folders with a stable provider ID, Message-ID, or content hash fallback.
- Non-HTTPS access outside loopback and private ranges shows a permanent red warning above the app.
- Two backend builds: `mailquay` with embedded frontend assets, and `mailquay-core` as API-only.
- Centered settings dialog over the mail shell with dedicated navigation, account settings, identities, sync, and appearance.
- OAuth2 (XOAUTH2) for providers that require it, plus app passwords for generic IMAP or POP3 servers.
- Server-sent events for new mail, flag changes, task progress, and outbox state.
- A top-right task ring shows running, queued, and failed work with per-task colors and a hover panel; failures stay red until handled.
- Embedded full-text search with Bleve, including a CJK analyzer and IMAP server-side search as a fallback.
- Sandboxed HTML rendering, remote images blocked by default, and attachment download hardening.
- English and Simplified Chinese UI.

## Non-goals

- Acting as an SMTP server or MTA.
- POP3 folder or flag synchronization. POP3 has no folder or server-side flag model; MailQuay keeps those locally.
- Multi-tenant hosting. The data model is single-owner with multiple mail accounts.
- Calendar, contacts, or team collaboration in the first release.

## Architecture

```mermaid
flowchart LR
    B[Browser<br/>React SPA] -->|HTTPS| W[nginx<br/>static + reverse proxy]
    W -->|REST + SSE<br/>/api/v1| A[Go backend]
    A -->|IMAP / POP3 / SMTP| P[(Mail providers)]
    A -->|SQL| D[(PostgreSQL)]
    A -->|blobs + Bleve index| S[(Host data directory)]
    A -.->|SSE events| W
```

The browser only talks to the backend HTTP API. The backend owns IMAP, POP3, and SMTP connections, credential encryption, synchronization state, MIME parsing, search indexing, and outbox delivery. The frontend is stateless and can be served from any static host.

## Technology

| Layer | Choice | Reason |
| --- | --- | --- |
| Backend | Go 1.27+ | Predictable concurrency for long-lived IMAP connections and a single static binary. |
| HTTP | `net/http` + chi | Standard library semantics, small surface, middleware-friendly. |
| Database | PostgreSQL 18 | Concurrent sync workers, JSONB metadata, and transactional state in one store. |
| DB access | `pgx/v5` + `sqlc` | Native PostgreSQL types and typed queries without an ORM. |
| Search | Bleve v2 (scorch) | Embedded full-text index, CJK bigram analyzer, no external search service. |
| IMAP | `go-imap` v2 | Streaming parser, IDLE, QRESYNC, and context-aware operations. |
| POP3 | `net` + `bufio` | UIDL, TOP, RETR, DELE, STARTTLS and QUIT semantics without a second client library. |
| SMTP | `github.com/emersion/go-smtp` | Submission, STARTTLS, and XOAUTH2 support. |
| MIME | `go-message` | Standards-compliant message parsing without hand-rolled boundary handling. |
| Migrations | `goose` | Embedded PostgreSQL migrations with versioned upgrades. |
| Frontend | React 19 + TypeScript + Vite | Client-side rendering with a fast development loop. |
| Routing | react-router | Client-side routing over one `index.html`; deep links fall back to the app shell. |
| Server state | TanStack Query v5 | Cache, optimistic updates, and SSE-driven invalidation. |
| UI | Tailwind CSS v4 + shadcn/ui | Accessible Radix primitives with full local control of styles. |
| Realtime | Server-sent events | One-way server push with automatic reconnect and simple cookie auth. |

## Repositories

| Repository | Contents |
| --- | --- |
| [`MailQuay`](https://github.com/MailQuay/MailQuay) | Go backend, database migrations, deployment files, API contract, documentation. |
| [`frontend`](https://github.com/MailQuay/frontend) | React single-page frontend and its container image. |

## Documentation

- [Architecture](docs/architecture.md): system design, synchronization model, realtime transport, security, deployment.
- [Project structure](docs/project-structure.md): repository layout, module responsibilities, data model, API surface.
- [Development](docs/development.md): local setup, environment variables, migrations, testing, Git workflow.
- [Open questions](docs/questions/README.md): numbered decisions that still need an answer.
- [AGENTS.md](AGENTS.md): repository rules for coding agents and contributors.

## Roadmap

| Milestone | Scope |
| --- | --- |
| M0 | Architecture, structure, and development documentation. |
| M1 | Backend skeleton, configuration, migrations, session auth, account CRUD. |
| M2 | IMAP initial and incremental sync, POP3 polling compatibility, folder tree, message list and reader. |
| M3 | Compose, drafts, SMTP delivery, outbox retry, Sent folder append for IMAP and local Sent copies for POP3. |
| M4 | SSE event stream, Bleve full-text index, attachment streaming. |
| M5 | OAuth2 providers, HTML mail hardening, keyboard shortcuts, i18n polish. |
| M6 | Multi-platform binaries, multi-arch images, Compose stack, backup and upgrade documentation. |

## License

[MIT](LICENSE)
