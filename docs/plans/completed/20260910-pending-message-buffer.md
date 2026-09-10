# Pending message buffer for chats outside the allowlist

## Overview

When the bot is added to a new group, every message that arrives before the operator learns the
chat id, edits `chats.yml` and restarts is lost for good: `convert()` drops the update after
`getUpdates` already acknowledged it and the offset advanced past it. Telegram will not redeliver.

This change buffers those messages in a separate `pending_messages` table instead of dropping them,
and drains the buffer at startup for every chat that has since been added to the chat map. The
operator workflow stays exactly what it is today — see the log line, add the `chats.yml` entry,
restart — but the backlog survives it. A replayed chat has no `cursors` row, so `list_new` surfaces
the whole recovered backlog on the first triage after the restart.

Benefits:

- no lost history when onboarding a new customer group
- the startup log names every chat still waiting, with the id, title, type, count and oldest
  timestamp needed to write the `chats.yml` line
- bounded by a TTL sweep so a group nobody ever adds does not grow the database forever

## Context (from discovery)

- `cmd/tg-mcp/main.go:79` — `config.Load` runs exactly once; the `*config.Config` is shared by
  `store.SyncChats`, the MCP server and ingest. Nothing re-reads it, so a chat map edit already
  requires a restart. No reload machinery is wanted.
- `cmd/tg-mcp/main.go:26-39` — `options` struct, go-flags; `FileLinkTTL` is the style model for the
  new duration flag, including its zero-means-default convention.
- `cmd/tg-mcp/main.go:94` — `st.SyncChats(ctx, chats.All())`, where the drain hooks in.
- `pkg/store/store.go:94-127` — the `schema` const applied by `migrate()`.
- `pkg/store/store.go:188` — `upsertTx` handles the `messages_fts` delete+insert keyed on the
  surrogate id; `classify()` at `:211` tags `ErrBadMessage`.
- `pkg/store/store.go:452-486` — `scanMessage`, a 17-field `Scan` plus null unpacking. `pending.go`
  will need a near-twin, and `dupl` runs at threshold 100 with an exclusion for `_test.go` only.
- `pkg/ingest/ingest.go:86` `record`, `:136-157` the retry/`skipPoison` batch loop, `:178`
  `skipPoison`, `:196` `collect`, `:209` `convert` (the allowlist check lives at `:224`, its log
  line at `:225`).
- `pkg/server/server.go:32-44` — the server's `messageStore` is a consumer-side interface with no
  write methods, so adding store methods does not touch `pkg/server`. Verified, no task needed.
- `pkg/store/search.go:43-56` — `Search` falls back to a `LIKE` scan over `messages.text` when FTS
  returns nothing, so it is **not** a valid probe for "was the FTS row written". `ftsMatch`
  (`pkg/store/store_test.go:755`) queries `messages_fts MATCH` directly and is.
- `pkg/server/server.go:147-161` — the MCP server shuts its listener down on a goroutine, so a test
  that restarts the app on the same address has to await `run()`'s return, not just cancel.

Existing tests and fixtures this change disturbs — all verified, all easy to miss:

- `pkg/ingest/ingest_test.go:70` `collectingStore` builds `&mocks.MessageStore{UpsertBatchFunc: ...}`
  only. moq panics on a nil func, so it must learn `UpsertPendingBatchFunc` the moment the method
  joins the interface.
- `pkg/ingest/ingest_test.go:158` `TestServiceFilters` case `"chat outside allowlist dropped"`
  expects `nil` and asserts `assert.Empty(t, batches())` at `:186` — that message is now buffered.
- `pkg/ingest/ingest_test.go:243` `TestServiceBatchIsOneTransaction` mixes an `otherChat` update
  into the batch; its `assert.Len(t, batches()[0], 2)` stays true, but the mock must not panic.
- `pkg/ingest/ingest_test.go:502` `TestServiceFilteredBatchAdvancesOffset` asserts
  `assert.Empty(t, batches(), "nothing to store")` for a batch of one `otherChat` update.
- `pkg/store/store_test.go:24` enumerates `{"messages", "chats", "cursors", "messages_fts"}`.
- `cmd/tg-mcp/main_test.go:325` `clearEnv` enumerates every env var; a missing `PENDING_TTL` lets a
  developer's exported value leak into `TestParseArgs`, which asserts every flag across three
  subtests.
- `cmd/tg-mcp/main_test.go:247` `writeChats` allocates a fresh `t.TempDir()` per call, so the
  restart test must rewrite the same path with `os.WriteFile` rather than call it twice.
- `cmd/tg-mcp/e2e_test.go:101` `TestE2ESmoke` scripts a message from `e2eOtherChat` (`:608`) and
  asserts at `:174` that it is dropped, with a comment string that becomes half false.
- `cmd/tg-mcp/e2e_test.go:517` `fakeAPI.serveUpdates` serves every scripted update with
  `u.id >= req.Offset` and never remembers what was confirmed. Since the real offset lives only in
  memory, a second `run()` would be redelivered the whole batch — the replay test would pass with
  the replay code deleted. The fake must model Telegram forgetting acknowledged updates.
- `README.md:272-273` — Bot setup step 4 tells the operator to watch for "the dropped-message
  lines", which this change removes.

## Development Approach

- **testing approach**: Regular (code first, then tests within the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task, and that
  includes *updating existing tests* whose expectations this change invalidates
- **CRITICAL: all tests must pass before starting next task** — no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run `make test` after each change; run `make lint` at the end of task 1 as well as in task 9 —
  `pending.go` introduces the scan/upsert duplication `dupl` is most likely to flag, and finding
  that three tasks later is wasted work
- maintain backward compatibility: an existing database gains the new table through
  `CREATE TABLE IF NOT EXISTS`, no migration step and no data movement

## Testing Strategy

- **unit tests**: required for every task
  - `pkg/store` tests run against a real SQLite file in `t.TempDir()` — no DB mocks, per project
    convention
  - `pkg/ingest` tests use the generated moq store
  - table-driven with testify, one `_test.go` per source file
- **e2e tests**: `make e2e` (build tag `e2e`). This change needs a new `TestE2EPendingReplay` that
  restarts the app in-process against the same data dir; it is the only test that proves the whole
  path end to end, so it is treated with the same rigor as the unit tests — and it needs task 7's
  fake-API fix first or it proves nothing.
- there is no UI, so no browser e2e layer

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview

Ingest stops dropping messages from unknown chats and writes them to a **separate**
`pending_messages` table. At startup, `drainPending` replays every buffered chat that is now in the
chat map into `messages`, sweeps whatever has aged past the TTL, and logs a summary of what is
still waiting.

Key design decisions, all settled during the brainstorm:

1. **Restart-only drain.** No SIGHUP, no file watch, no ticker. `chats.yml` is already read once at
   startup, so a chat map edit already means a restart.
2. **Buffer the converted `store.Message`, not the raw update JSON.** Safe because `botID` and
   `botUsername` are fixed for the process run, so `IsMention` and `repliesToBot` computed at drop
   time equal what a replay would compute.
3. **A separate table, never a `pending` flag on `messages`.** FTS5 indexes on write — `upsertTx`
   populates `messages_fts` unconditionally inside the write — so a flagged-in-place design would
   expose non-allowlisted chat content through `search` the moment any one of the nine query sites
   over `messages`/`messages_fts` forgot `AND pending = 0`. The allowlist is a security boundary; a
   separate table makes the leak unrepresentable rather than merely guarded against.
4. **TTL sweep at startup only**, default 14 days (`336h` — go-flags parses through
   `time.ParseDuration`, which has no day unit). No per-chat cap, no ticker, no disable switch: a
   `--pending-ttl=0` meaning "off" would collide with the zero-means-default convention `FileLinkTTL`
   established, and a separate `--no-pending` flag is a knob for a problem that does not exist.
5. **Replay before sweep.** Adding a chat on day 15 still recovers its messages; sweeping first
   would delete exactly the rows the operator just asked for.
6. **The allowlist decision splits in `collect()`, two store calls in `persist()`** — pending
   first, then messages. Rejected alternatives: a two-slice `UpsertBatch` signature (atomicity buys
   nothing, both are idempotent upserts keyed on `(chat_id, message_id)` and redelivery converges,
   the same property the existing crash-safety design leans on); and letting the store route by its
   `chats` mirror table (contradicts the documented constraint that ingest filters on the chat map).

## Technical Details

### Schema — appended to the existing `schema` const in `pkg/store/store.go`

```sql
CREATE TABLE IF NOT EXISTS pending_messages (
  chat_id        INTEGER NOT NULL,
  message_id     INTEGER NOT NULL,
  chat_title     TEXT    NOT NULL DEFAULT '',
  chat_type      TEXT    NOT NULL DEFAULT '',
  received       TEXT    NOT NULL,
  thread_id      INTEGER,
  sent           TEXT    NOT NULL,
  sender_id      INTEGER NOT NULL DEFAULT 0,
  sender_name    TEXT    NOT NULL,
  from_bot       INTEGER NOT NULL DEFAULT 0,
  reply_to       INTEGER,
  text           TEXT    NOT NULL DEFAULT '',
  is_mention     INTEGER NOT NULL DEFAULT 0,
  edited_at      TEXT,
  media_type     TEXT,
  file_id        TEXT,
  file_unique_id TEXT,
  file_name      TEXT,
  file_size      INTEGER,
  PRIMARY KEY (chat_id, message_id)
);
```

No FTS row and no extra index: the only access patterns are by `chat_id` (the PK's left prefix
covers it) and a full scan for the sweep. There is **no `id` column** — the surrogate key exists to
be the FTS rowid and pending rows have no FTS row, so `Pending.ID` inherited from the embedded
`Message` is always 0 on read-back. That deserves a comment on the type; a replayed row gets its
`id` from `upsertTx` at replay time.

### Store API — new `pkg/store/pending.go`

```go
// Pending is a message from a chat outside the allowlist, held until the chat is added.
// The embedded Message.ID is always zero: pending rows carry no surrogate key, they get one
// from upsertTx when they are replayed.
type Pending struct {
    Message
    ChatTitle string
    ChatType  string
    Received  time.Time
}

type Replayed struct {
    ChatID   int64
    Customer string
    Count    int
}

type PendingChat struct {
    ChatID   int64
    Title    string
    Type     string
    Messages int
    Oldest   time.Time
}

func (s *Store) UpsertPendingBatch(ctx context.Context, msgs []Pending) error
func (s *Store) ReplayPending(ctx context.Context, chats []config.Chat) ([]Replayed, error)
func (s *Store) SweepPending(ctx context.Context, ttl time.Duration) (int, error)
func (s *Store) PendingChats(ctx context.Context) ([]PendingChat, error)
```

- `UpsertPendingBatch`: one transaction. `ON CONFLICT(chat_id, message_id) DO UPDATE` refreshes the
  message columns but deliberately **omits `received`, `chat_title` and `chat_type`**, so a row
  stays a consistent snapshot of the chat as it was when that message was first buffered: an edit
  refreshes the text without restarting the hold clock or rewriting the chat identity. Refreshing
  the title would silently break `PendingChats` below — edit the oldest buffered message and the
  "first seen" title it reports becomes the *current* one, which is neither contract. `Received` is
  set by the caller, so there is no clock to inject and the sweep tests can plant old rows directly.
  Errors go through `classify()` unchanged, so a poison pending row is skippable exactly like a
  normal one.

  The cost is that a group renamed while it sits unallowlisted keeps showing its old name in the
  startup summary. Accepted: the summary's job is to hand over a chat **id**, the rename window is
  hours to days, and the alternative — reporting the newest title — needs `COUNT` and `MIN` pushed
  into correlated subqueries so exactly one min/max aggregate is left to drive the bare columns.
  That is a lot of SQL to buy a fresher hint next to an authoritative id.
- `ReplayPending`: **one transaction per chat** — bounded blast radius and a meaningful per-chat
  count. Reads that chat's rows ordered by `sent, message_id`, calls the existing `upsertTx` per
  row (this is what keeps `messages_fts` correct; an `INSERT..SELECT` would have to reproduce the
  delete-then-insert on the FTS table by hand), then `DELETE FROM pending_messages WHERE chat_id = ?`
  inside the same transaction. Chats with nothing buffered produce no `Replayed` entry.
- `SweepPending`: `DELETE FROM pending_messages WHERE received < ?`; a zero `ttl` means the 14-day
  default, mirroring how a zero `FileLinkTTL` means five minutes at `pkg/server/server.go:56`.
  Returns rows affected.
- `PendingChats`: `SELECT chat_id, chat_title, chat_type, COUNT(*), MIN(received) FROM
  pending_messages GROUP BY chat_id ORDER BY MIN(received)`. The query has **exactly one min/max
  aggregate**, so SQLite's documented rule makes the bare columns come from that same row — the
  title as *first seen*. `COUNT(*)` is irrelevant to that rule; a second min/max aggregate would
  make the bare columns undefined. Say it that precisely in the code comment, so nobody "improves"
  it into a `MAX(received)` later. The contract holds **only because the upsert freezes
  `chat_title`/`chat_type`** — the two decisions are one decision, and the comment should say so or
  a later edit to the conflict clause will quietly falsify this query.

### Ingest — `pkg/ingest/ingest.go`

- The allowlist check moves out of `convert()` and into `collect()`. `convert()` goes back to one
  job: map an update to a `Message`, or report there is nothing to store.
- **`collect` must run the discovery check before it drops anything `convert` rejects**, and this
  is the subtlest part of the change. Today the allowlist log at `ingest.go:225` fires *before* the
  service-message drop at `:231`, so a group where the bot is added and nobody says anything still
  produces the line that tells the operator the chat id — that is exactly how this feature's
  workflow starts. If `collect` simply calls `convert` and `continue`s on `!ok`, that group goes
  completely silent: no log, no pending row, nothing in the startup summary, invisible forever.
  So `collect` takes over the update-level filtering and `convert` keeps only the mapping:

  ```go
  m := messageOf(u)                     // u.Message ?: u.EditedMessage
  if m == nil { continue }              // debug log as today
  if m.MigrateToChatID != 0 { warn; continue }   // still before the allowlist check
  var unknown *chatRef
  if _, allowed := s.chats.ByChat(m.Chat.ID); !allowed {
      unknown = &chatRef{title: m.Chat.Title, kind: m.Chat.Type}
      s.noteUnknown(m.Chat.ID, unknown) // fires even if convert rejects the update below
  }
  msg, ok := s.convert(u)               // service-message drop and field mapping only
  if !ok { continue }
  recs = append(recs, record{update: u, msg: msg, unknown: unknown})
  ```

  The migration warn stays ahead of the allowlist check, so a migrated chat is neither announced
  as unknown nor buffered under an id that no longer exists. The service-message drop stays inside
  `convert`, so the join event is logged but not buffered — there is nothing to replay in it.
- `record` gains `unknown *chatRef`, non-nil when the chat is outside the allowlist:

  ```go
  type chatRef struct{ title, kind string }
  ```

- A shared helper (`messageOf(u) *telegram.Message`) does the `u.Message ?: u.EditedMessage` pick
  for both `convert` and `collect`, rather than duplicating it.
- `noteUnknown` logs once per chat per process run, deduped through a `map[int64]struct{}` on
  `Service`. `Run` is single-goroutine, so no mutex — that needs a comment saying why. This line
  **replaces** the current per-message `slog.Info("message from a chat outside the allowlist
  dropped", ...)` at `ingest.go:225`; nothing is dropped any more, so the old wording would lie.
- `persist` splits the records and issues both calls, pending first, **each guarded on its own
  slice**:

  ```go
  msgs, pend := split(recs, time.Now())
  if len(pend) > 0 {
      if err := s.store.UpsertPendingBatch(ctx, pend); err != nil {
          return fmt.Errorf("buffer %d messages: %w", len(pend), err)
      }
  }
  if len(msgs) > 0 {
      if err := s.store.UpsertBatch(ctx, msgs); err != nil {
          return fmt.Errorf("store %d messages: %w", len(msgs), err)
      }
  }
  ```

  The guards matter: once the allowlist check lives in `collect()`, a batch of nothing but
  unknown-chat updates yields a **non-empty** `recs`, so the existing `len(recs) == 0` early return
  no longer keeps an empty slice away from the store. The pending call gets its own error text —
  `store %d messages` would misreport it. Both errors flow into the existing
  counter/backoff/`skipPoison` path untouched.
- `skipPoison` gains a routing switch: a record with `unknown != nil` replays through
  `UpsertPendingBatch`, everything else through `UpsertBatch`. The poison payload log line is
  unchanged.
- `messageStore` gains `UpsertPendingBatch`; `pkg/ingest/mocks/message_store.go` is regenerated.
- Note `slog.Error("store batch failed", ..., "messages", len(recs))` at `ingest.go:144` now counts
  buffered records too. That is correct — they are all records the batch failed to persist — but
  worth a glance when reading the log.

### Wiring — `cmd/tg-mcp/main.go`

```go
PendingTTL time.Duration `long:"pending-ttl" env:"PENDING_TTL" default:"336h" description:"how long messages from chats outside the allowlist are kept for replay"`
```

`validate()` rejects a negative value the way it rejects a negative `FileLinkTTL`.
`drainPending(ctx, st, chats, ttl)` is an unexported func called right after `SyncChats` and before
either goroutine starts; a failure is fatal, like `SyncChats` — it is the same database the whole
process depends on. It does replay, then sweep, then summary.

Log shapes — the first is ingest's, the rest are `drainPending`'s:

```
INFO  buffering messages from a chat outside the allowlist chat_id=-5220430040 title="Wegagen NetXMS Support" type=group
INFO  replayed buffered messages customer=wegagen chat_id=-5220430040 messages=12
INFO  expired buffered messages removed messages=48 ttl=336h0m0s
WARN  buffered chat not in the allowlist chat_id=-5220430040 title="Wegagen NetXMS Support" type=group messages=12 oldest=2026-09-08T11:03:22Z
```

The sweep line is silent when nothing expired. Logging chat ids here is correct: the "chat ids
never leave the server" constraint is about tool results and error text, and it explicitly makes
logging the caller's job.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): store layer, ingest, wiring, tests, docs
- **Post-Completion** (no checkboxes): deploying and verifying against a real group

## Implementation Steps

### Task 1: Pending table and buffered write

**Files:**
- Create: `pkg/store/pending.go`
- Create: `pkg/store/pending_test.go`
- Modify: `pkg/store/store.go`
- Modify: `pkg/store/store_test.go`

- [x] append the `pending_messages` table to the `schema` const in `pkg/store/store.go`
- [x] create `pkg/store/pending.go` with the `Pending` type (including the always-zero-`ID`
      comment) and `UpsertPendingBatch`: one transaction, `ON CONFLICT DO UPDATE` refreshing the
      message columns while omitting `received`, `chat_title` and `chat_type`, errors through
      `classify()`
- [x] add `pending_messages` to the table list at `pkg/store/store_test.go:24`
- [x] write tests: insert a batch and read the rows back; empty batch is a no-op
- [x] write test: re-upserting the same `(chat_id, message_id)` with new text and a new chat title
      updates the text and leaves `received`, `chat_title` and `chat_type` untouched
- [x] write test: a constraint-violating row returns an error wrapping `ErrBadMessage`
- [x] run `make test` and `make lint` — lint here because this is where the `scanMessage`/`upsertSQL`
      duplication `dupl` flags at threshold 100 first appears; must pass before task 2

### Task 2: ReplayPending

**Files:**
- Modify: `pkg/store/pending.go`
- Modify: `pkg/store/pending_test.go`

- [x] add `Replayed` and `ReplayPending`, one transaction per chat, rows ordered by
      `sent, message_id`, reusing `upsertTx` per row, then `DELETE ... WHERE chat_id = ?` in the
      same transaction
- [x] write test: only the chats passed in are moved; rows for other chats stay buffered
- [x] write test: **the replayed row is returned by a direct `messages_fts MATCH`**, using the
      existing `ftsMatch` helper (`pkg/store/store_test.go:755`) — **not** by calling `Search`.
      `Search` falls back to a `LIKE` scan over `messages.text` when FTS returns nothing
      (`pkg/store/search.go:43-56`), so it would happily find a replayed row that has no FTS row at
      all and the assertion would prove nothing. The direct MATCH is what an `INSERT..SELECT`
      implementation fails.
- [x] write test: replaying a pending row onto a message that already exists **stops the old text
      matching** — the FTS row must be replaced, not just added to. Replay lands on an existing
      `messages` row whenever a buffered message was edited or the chat was removed and re-added,
      and insertion-only coverage would miss a stale index entry left searchable
- [x] write test: replaying into a chat that already has messages (removed and re-added) upserts
      cleanly and returns the right count
- [x] write test: a chat with nothing buffered produces no `Replayed` entry
- [x] run `make test` — must pass before task 3

### Task 3: SweepPending and PendingChats

**Files:**
- Modify: `pkg/store/pending.go`
- Modify: `pkg/store/pending_test.go`

- [x] add `SweepPending` (zero ttl means the 14-day default) returning rows affected
- [x] add `PendingChat` and `PendingChats` with the single-min/max-aggregate query, plus the comment
      explaining precisely why a second min/max aggregate would break the bare columns
- [x] write test: sweep with planted old `received` values removes exactly the expired rows and
      returns their count; a zero ttl uses the default
- [x] write test: `PendingChats` reports the count, the oldest timestamp, and the **first-seen**
      title when the group was renamed mid-buffer (a newer row carries the new title, the reported
      one is the old)
- [x] write test for the exact trace that falsifies a refreshing upsert: buffer A with title "Old",
      buffer B with title "New", then re-upsert A carrying title "New". `PendingChats` must still
      report "Old" and A's original `received`. This is the regression test for the
      freeze-title/first-seen-title pair being one decision
- [x] write test: `PendingChats` on an empty table returns no rows, not an error
- [x] run `make test` — must pass before task 4

### Task 4: Ingest plumbing for the pending store call

Deliberately behaviour-free: the interface, the mock, and the split write path land first, with
`pend` always empty, so this task compiles and passes with the existing expectations intact.

**Files:**
- Modify: `pkg/ingest/ingest.go`
- Modify: `pkg/ingest/mocks/message_store.go` (regenerated)
- Modify: `pkg/ingest/ingest_test.go`

- [x] add `UpsertPendingBatch` to the `messageStore` interface and regenerate the mock with
      `go generate ./pkg/ingest/...` (moq must be on PATH)
- [x] teach `collectingStore` (`ingest_test.go:70`) an `UpsertPendingBatchFunc` recording pending
      batches, plus an accessor — without it moq panics on the nil func the moment `persist` calls
      the method
- [x] add `record.unknown *chatRef` and the `chatRef` type, unset for now
- [x] split `persist` into the pending call then the messages call, each guarded on its own slice,
      with the distinct `buffer %d messages` error text
- [x] add the routing switch in `skipPoison` so a pending record replays through the pending method
- [x] write test: with every chat allowlisted, `UpsertPendingBatch` is never called and the existing
      batching behaviour is unchanged
- [x] run `make test` — the whole existing suite must still pass untouched before task 5

### Task 5: Route non-allowlisted messages to the buffer

**Files:**
- Modify: `pkg/ingest/ingest.go`
- Modify: `pkg/ingest/ingest_test.go`

- [x] extract `messageOf(u)` and move the nil-message, `MigrateToChatID` and allowlist checks from
      `convert()` into `collect()`, leaving `convert()` with the service-message drop and the field
      mapping only
- [x] populate `record.unknown` in `collect` and add `noteUnknown` with its per-run dedup map, the
      new log wording, and the comment on why no mutex is needed; remove the old per-message
      `message from a chat outside the allowlist dropped` line
- [x] **call `noteUnknown` before the `convert` `!ok` guard** — see Technical Details; a group whose
      only update is the join event must still announce itself, or onboarding a quiet group breaks
- [x] write test: an unknown chat whose only update is a service message (a join event) still emits
      the discovery log at Info, and writes to neither table
- [x] update `TestServiceFilters`: the `"chat outside allowlist dropped"` case becomes a buffered
      case — the table needs a way to express "expected in the pending batch, not the message
      batch" (a second want field, or a separate test), and the `assert.Empty(t, batches())` branch
      at `:186` must not swallow it
- [x] update `TestServiceFilteredBatchAdvancesOffset` (`:502`): the batch is now buffered, not
      dropped — assert it lands in the pending batch while the offset still advances to 8
- [x] verify `TestServiceBatchIsOneTransaction` (`:243`) still passes: 2 messages stored, and now
      1 buffered
- [x] write test: a buffered message carries the chat title and type from the update
- [x] write test: a service message from an unknown chat is buffered nowhere
- [x] write test: a migrated chat still only warns and is neither stored nor buffered
- [x] write test: the unknown-chat log fires once for a batch of five messages from the same chat
- [x] write test: `skipPoison` routes a pending record to `UpsertPendingBatch` and a normal one to
      `UpsertBatch`
- [x] write test: a batch whose pending write fails keeps the offset pinned and retries
- [x] write **mixed-batch** tests — a batch carrying both kinds is the case the two-call split
      actually changes, and the routing tests above do not cover it. Four cases:
      (1) pending succeeds, the messages write fails transiently, the retry succeeds;
      (2) pending succeeds, the messages write is poison, and the eventual singleton replay skips
      only the poison record;
      (3) pending is poison, the normal records are good, and the singleton replay still writes the
      normal ones;
      (4) a database-wide outage hits *during* the singleton replay, so `skipPoison` returns false
      and the offset stays pinned despite the singleton writes that already committed.
      Assert in each: record order, the latest content winning for a repeated message key, the
      eventual offset, and `received` unchanged across duplicate pending writes. The retry counter
      must not reset just because the first of the two calls succeeded — resetting `storeFails`
      after every successful pending call would make it oscillate 0→1 forever and a consistently
      poisonous normal row would never reach `s.retries`
- [x] run `make test` — must pass before task 6

⚠️ `convert` takes the `*telegram.Message` rather than the update: `collect` has already picked it
with `messageOf`, and passing the update back would reintroduce a nil check for a state that can no
longer happen. `messageOf` is called in `collect` only.
⚠️ "received unchanged across duplicate pending writes" cannot hold at the ingest layer — `persist`
stamps `time.Now()` per call, so a redelivery carries a fresh one. Holding the clock is the store's
`ON CONFLICT` clause, covered by `TestStore_UpsertPendingBatch`; the mixed-batch test asserts the
stamp is taken once per batch and points at that test.
➕ `TestServiceFilteredBatchAdvancesOffset` renamed to `TestServiceBufferedBatchAdvancesOffset` —
the batch is no longer filtered.

### Task 6: Startup drain, sweep and summary

**Files:**
- Modify: `cmd/tg-mcp/main.go`
- Modify: `cmd/tg-mcp/main_test.go`
- Modify: `README.md` (flag table only; the prose lands in task 10)

- [x] add the `PendingTTL` flag and reject a negative value in `validate()`
- [x] add `drainPending` doing replay, then sweep, then summary, with its three log lines
- [x] call it in `run()` right after `SyncChats`, before either goroutine starts, failure fatal
- [x] add `PENDING_TTL` to `clearEnv` (`main_test.go:325`) and cover the new flag in all three
      `TestParseArgs` subtests (defaults, all flags set, env vars applied)
- [x] write test: `validate` rejects a negative `--pending-ttl`
- [x] write test: `drainPending` on a store with buffered rows for a now-allowlisted chat moves
      them, and leaves an unknown chat's rows in place
- [x] write test: replay happens before the sweep — a buffered row older than the TTL belonging to
      a now-allowlisted chat is recovered, not deleted
- [x] run `make test` — must pass before task 7

### Task 7: Make the e2e fake API forget confirmed updates

**Files:**
- Modify: `cmd/tg-mcp/e2e_test.go`

- [x] add a confirmed-offset high-water mark to `fakeAPI`, guarded by its existing `sync.Mutex`
- [x] in `serveUpdates`, raise the mark to `req.Offset` and serve only updates at or above it, so
      an acknowledged update is never redelivered — modelling what Telegram actually does
- [x] expose a `confirmedOffset()` accessor guarded by the same mutex; task 8 needs it to know the
      first run really confirmed the batch
- [x] fix the now-half-false comment at `:174` — the non-allowlisted chat is buffered, not dropped;
      the assertion itself stays as is
- [x] run `make e2e` — `TestE2ESmoke` must still pass before task 8

### Task 8: End-to-end restart replay test

**Files:**
- Modify: `cmd/tg-mcp/e2e_test.go`

- [x] add `TestE2EPendingReplay` as its own top-level test with its own fake API and data dir (the
      smoke test defers `stopApp` at the top, so a restart cannot live inside it)
- [x] first `run()` with a `chats.yml` holding only `acme`; **gate the cancel on
      `require.Eventually(... api.confirmedOffset() >= 8 ...)`**, not on a tool result — the mark
      only rises on the *next* `getUpdates` after `offset = last + 1` (`ingest.go:157`), so
      cancelling on `list_new` alone leaves it at 0, the batch is redelivered, and the test passes
      with `drainPending` deleted
- [x] close the first MCP session, cancel the first app context, and **await its `done` channel with
      a bounded timeout and `require.NoError`** before starting the second run — `srv.Run` shuts the
      listener down on a goroutine (`pkg/server/server.go:147-161`), so reusing the same
      `opts.Listen` without that barrier can bind-fail intermittently or let the readiness probe
      answer from the old server. `cmd/tg-mcp/main_test.go:225-231` is the pattern to copy, and
      **both** runs need that cleanup on the failure path, not just the first
- [x] rewrite the same `chats.yml` path with `os.WriteFile` to add `e2eOtherChat` under a second
      customer, then `run()` again on the same data dir and the same `opts.Listen`
- [x] open a fresh MCP session against the second run and assert `get_thread` for the new customer
      returns the message dropped on the first run, and `list_new` surfaces it as untriaged
- [x] sanity-check the test fails when `drainPending` is stubbed out — if it still passes, the fake
      API is redelivering and task 7 is incomplete
- [x] run `make e2e` — must pass before task 9

### Task 9: Verify acceptance criteria

- [x] verify all requirements from Overview are implemented — buffering (`UpsertPendingBatch`,
      routed in `collect`), restart drain (`drainPending` before either goroutine starts), the
      `WARN buffered chat not in the allowlist` line carrying id/title/type/count/oldest, and the
      TTL sweep behind `--pending-ttl` (`336h` default, zero means the 14-day store default)
- [x] verify edge cases: chat removed from the map after buffering
      (`ReplayPending/only the given chats are moved` — rows for chats not passed in stay
      buffered and keep showing up in `PendingChats`), chat re-added after messages already exist
      (`ReplayPending/replaying into a chat that already has messages`), buffered message edited
      before replay (`UpsertPendingBatch/edit refreshes the text and freezes the snapshot` plus
      `ReplayPending/replaying over an existing message replaces its index row`), poison pending
      row (`UpsertPendingBatch/row rejection is tagged`,
      `TestServiceMixedBatch/poison buffered row skipped while the messages land`,
      `TestServiceSkipPoisonRoutesEachRecordToItsTable`)
- [x] run the full test suite: `make test` — all 7 packages pass
- [x] run the e2e suite: `make e2e` — all pass, including `TestE2EPendingReplay`
- [x] run `make lint` (0 issues) and `make fmt` (no changes)
- [x] verify coverage did not regress — total 95.0% against 95.4% on `main`; `cmd/tg-mcp` rose
      65.1% → 68.6% and `pkg/ingest` 98.5% → 98.7%, while `pkg/store` sits at 93.5% against 94.2%.
      ➕ added the missing error-path subtests `pending.go` was short of the store convention:
      closed-store cases for all four entry points, planted `not-a-time` in `received`, `sent` and
      `edited_at`, and a `BEFORE DELETE` trigger proving a failed drop rolls the whole replay back.
      What is left uncovered in `pending.go` is `tx.Commit`, `RowsAffected` and `rows.Err` guards —
      the same class of line left uncovered in `store.go` on `main` (`upsertTx` 77.8%,
      `UpsertBatch` 92.3%), unreachable without fault injection the codebase does not do

### Task 10: [Final] Update documentation

**Files:**
- Modify: `README.md`
- Modify: `CLAUDE.md`

- [x] README: add `--pending-ttl` to the flag table
- [x] README: add an "adding a new group" note — the bot can be added before the chat map knows it;
      the `WARN buffered chat not in the allowlist` line carries the id, title and type; add the
      entry and restart and the backlog replays
- [x] README: rewrite Bot setup step 4 (`:272-273`), which tells the operator to watch for
      "the dropped-message lines" that no longer exist
- [x] README: update the chat map line reading "useful while collecting chat ids from the drop log"
      (`:149`) and the `### Restarts` section, which currently states the loss is unrecoverable
- [x] CLAUDE.md: add the design-constraint bullet — separate table rather than a flag on `messages`
      (FTS indexes on write, the allowlist is a security boundary, nine query sites), replay before
      sweep, why converting at drop time is safe, and why there is no disable switch
- [x] CLAUDE.md: note `pending.go` in the `pkg/store` layout line
- [x] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual verification:**

- add the bot to a scratch Telegram group that is not in `chats.yml`, send a few messages, confirm
  the `buffering messages from a chat outside the allowlist` line appears once, not per message
- restart, confirm the `WARN buffered chat not in the allowlist` summary names the group
- add the chat id to `chats.yml`, restart, confirm `replayed buffered messages` and that `list_new`
  shows the backlog

**Deployment:**

- no migration step: the new table is created by `CREATE TABLE IF NOT EXISTS` on first start
- `PENDING_TTL` is optional; the image does not need a new preset in `Dockerfile` or
  `docker-compose.yml` unless a non-default value is wanted
