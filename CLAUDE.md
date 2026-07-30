# tg-mcp

Telegram support gateway: a Bot API bot logs allowlisted customer groups into SQLite, and an MCP
server (streamable HTTP, bearer auth) lets Claude triage and reply. Single Go binary, no CGO.

## Layout

- `cmd/tg-mcp/main.go` — entrypoint, `options` (go-flags), `run()` composition root
- `cmd/tg-mcp/e2e_test.go` — `//go:build e2e`, whole app in-process against a fake bot api
- `pkg/config` — chat map (YAML): chat id → customer slug + optional label
- `pkg/telegram` — minimal Bot API client (getMe, getUpdates, deleteWebhook, sendMessage, getFile, download)
- `pkg/ingest` — long-poll loop, allowlist filter, store writes
- `pkg/store` — SQLite (WAL): `store.go` schema/upsert/cursors/history, `thread.go`, `search.go`, `files.go` cache
- `pkg/server` — `server.go` transport/auth/slug resolution, `tools.go` the 8 MCP tools, `files.go` `/files/`
- `Dockerfile` / `docker-compose.yml` / `init.sh` — multi-stage build on `ghcr.io/alkk/baseimage`;
  the image presets `DATA_DIR=/srv/data`, `CHATS_FILE=/srv/chats.yml`, `LISTEN=:8080` on top of the
  flag defaults

## Commands

```
make test      # go test -race with coverage, mocks stripped
make e2e       # go test -tags=e2e -race
make lint      # golangci-lint v2
make fmt       # gofmt -s + goimports
make build     # .bin/tg-mcp with revision injected
make version   # print computed revision
```

Needs `golangci-lint`, `goimports`, `moq` on PATH.

## Tool surface

`list_customers`, `list_new`, `get_thread`, `get_history`, `search`, `get_file`, `send_reply`,
`mark_handled` — registered with the go-sdk's typed `mcp.AddTool`, so params and results carry
inferred JSON schemas; keep the `jsonschema` tags descriptive, they are the tool's real docs.
Behaviour of each tool is documented in README.md — keep the two in sync when signatures change.

## Design constraints

These are decisions, not accidents — changing one needs a reason.

- **chat ids never leave the server.** Tools speak customer slugs plus an optional group label;
  no result struct carries a chat id, and neither does any error text — store errors reach the
  client verbatim, so they name message ids only and callers log the chat id themselves. The chat
  map is the allowlist in both directions: ingest filters on it, and every addressed tool resolves
  through `chatIDs`/`singleChat`.
- **the server is dumb.** Tools return raw messages, never summaries — intelligence lives in the
  model. Result shaping is limited to snippets, timestamps and media indicators.
- **no persisted getUpdates offset.** Redelivery after a crash *is* the crash-safety mechanism;
  the offset advances only after the batch is committed. Do not add an offsets table. The single
  exception is `skipPoison`: after `storeRetries` failed rounds the batch is retried message by
  message and whatever still fails is logged with its raw payload and dropped. Its retry counter
  is keyed on the *first* update id — a redelivery repeats that one while the tail grows.
  Dropping is gated on `store.ErrBadMessage`, which the store tags onto row-level sqlite codes
  (constraint, mismatch, too big, range). A database-wide failure — full disk, read-only mount,
  I/O error — is retried forever with the offset pinned, and it also aborts a replay already in
  progress: skipping cannot fix it, so losing the batch would be loss for nothing.
- **`ON CONFLICT ... DO UPDATE`, never `INSERT OR REPLACE`** — the latter changes the surrogate
  `id` (the FTS rowid) and skips triggers. Edits keep the original `sent` and set `edited_at`.
- **FTS5 is standalone, not external-content**, and maintained explicitly in Go inside the same
  transaction as the message write. Composite-PK tables have unstable rowids; an external-content
  index would corrupt silently on edits.
- **own replies are persisted by `send_reply`.** Bot API does not echo the bot's own messages
  through `getUpdates`, so without the upsert threads would lose their answers.
- **`--telegram.local` is an explicit flag, not path probing.** A missing shared volume must fail
  loudly instead of falling through to a confusing HTTP 404.
- **`/files/` urls are derived from the request host** (`X-Forwarded-Host`/`-Proto` honoured), so
  there is no public-url setting to keep in sync with the reverse proxy. `trackBase` pins the base
  onto the `/mcp` request itself (`X-Tg-Mcp-Base`, always set or cleared, so a client cannot supply
  one) and the tool call reads it back from `CallToolRequest.Extra.Header`: no shared state, so a
  concurrent call on another hostname cannot repoint the links of this one, and a download curl'd
  off the listener never touches them.
- **`get_history` pages backwards on an opaque cursor**, `sent` plus the surrogate row id, not on
  `to` alone: telegram stamps whole seconds, so a page boundary inside one second would return the
  same rows forever. The token carries no chat id.
- **the data dir is prepared by `init.sh`**, which the baseimage runs as root before dropping to
  `app`: it mkdirs `$DATA_DIR` and chowns it to `${APP_UID:-101}:${APP_GID:-990}` so bind mounts
  work as well as named volumes. Moving `DATA_DIR` means updating `init.sh` too.
- **one poller per token.** `deleteWebhook` at startup, fail fast on 409 Conflict — and on the
  codes that mean the token itself is dead (401/403/404): retrying those forever would leave
  ingestion stalled while `/ping` keeps answering.

## Conventions

- go-flags for CLI/env, one `options` struct, one `parseArgs`
- consumer-side interfaces, mocks generated with moq into `<pkg>/mocks/`
- one `_test.go` per source file, table-driven with testify, `t.TempDir()` for all fs work
- store tests run against a real SQLite temp file, telegram/ingest tests against `httptest` fakes —
  no DB mocks
- errors wrapped with `fmt.Errorf("doing x: %w", err)`
- `go mod vendor` after every dependency change; vendor is committed
- the `go` directive stays at minor granularity (`1.26`): the `builder-go` base image runs with
  `GOTOOLCHAIN=local`, a patch-level pin would force a toolchain download at image build
- `.golangci.yml` sets `build-tags: [e2e]`, so the tagged e2e file is linted too
- see `docs/plans/completed/` for the implementation plan and the reasoning behind the above
