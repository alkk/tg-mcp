# tg-mcp review profile

## What it is

- Telegram support gateway: a Bot API bot logs allowlisted customer groups into SQLite, an MCP server
  (streamable HTTP, bearer auth) lets Claude triage and reply, plus an opt-in unauthenticated
  `/public/{name}` for a website widget
- single Go binary, no CGO, one maintainer, one deployment (Docker behind a reverse proxy)
- real customers on the other end of `send_reply`; the MCP client is an LLM harness

## What a real failure looks like

- a telegram chat id, or any content of a chat outside the allowlist, reaching an MCP result, an error
  string, a `/public/` body or a download link
- a customer message lost: the getUpdates offset advancing past an uncommitted batch, a buffered
  message dropped when it should replay, a poison-skip on a database-wide error
- a reply reaching the customer as corrupted text: valid-but-wrong HTML from `pkg/tghtml`, or a retry
  that double-posts
- a signed `/files/` link that forges, outlives its ttl, or writes its signature into a log
- the FTS index diverging from `messages` (rowid churn, write outside the message transaction)
- ingest stalled while `/ping` still answers

## Blast radius

- ingest/store bugs lose history permanently: telegram does not redeliver after the offset moves
- `send_reply`/`tghtml` bugs are customer-visible and cannot be recalled
- server/auth bugs expose customer conversations to whoever holds a link or guesses an alias

## Reporting bar

- material, not merely true: would a maintainer fix it before merge?
- noise here: style the linter already enforces, missing comments (comments are deliberately rare),
  hypothetical scale (the chat map is tens of entries, one process), speculative interfaces
- a deviation from a documented rule in CLAUDE.md "Design constraints" is always worth reporting

## Where the rules live

- `CLAUDE.md` — layout, design constraints (each bullet is a decision with its reason), conventions
- `README.md` — tool and endpoint behaviour, must stay in sync with signatures
- `.golangci.yml` — golangci-lint v2, `build-tags: [e2e]`
- `docs/plans/completed/` — plans with the reasoning; frozen, never edited even when drifted

## Deliberate conventions (not defects)

- no persisted getUpdates offset; redelivery is the crash-safety mechanism
- `pending_messages` is a separate table, never a flag on `messages`
- `ON CONFLICT ... DO UPDATE`, never `INSERT OR REPLACE`; FTS5 standalone, maintained in Go
- consumer-side interfaces, moq mocks in `<pkg>/mocks/`, no DB mocks — store tests hit real SQLite
- one `_test.go` per source file, table-driven testify, `t.TempDir()` for fs work
- errors wrapped `fmt.Errorf("doing x: %w", err)`; store error text reaches MCP clients verbatim
- comments only where something is unexpected
- `vendor/` committed; `go` directive at minor granularity (`1.26`)
- every tghtml rule fails toward leaving text literal

## Languages

- Go for code; Markdown for README/CLAUDE.md/plans; YAML for the chat map and CI
