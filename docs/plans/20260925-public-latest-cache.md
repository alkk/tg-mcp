# Serve /public/{name} from an in-memory latest-message cache

## Overview
- `GET /public/{name}` is unauthenticated and currently runs `store.History(..., 1)` on every
  request. The answer only changes when a message lands in that chat, so the per-request query is
  wasted work and an anonymous caller can make the server read SQLite as often as it likes.
- Load each public chat's newest message once at startup and update it whenever a message for that
  chat is committed. After that the endpoint never reads SQLite.
- The store owns the cache: every write to `messages` (ingest batch, ingest `skipPoison`,
  `send_reply`) already goes through `Store.UpsertBatch`, so a write path cannot bypass the cache.

## Context (from discovery)
- `pkg/server/public.go` — `servePublic` calls `s.store.History(ctx, []int64{chat.ID}, time.Time{},
  time.Time{}, nil, 1)` and has a `context.Canceled` branch and a 500 path
- `pkg/store/store.go` — `Store{db, dir}`, `New(dir)`, `UpsertMessage` → `s.UpsertBatch`,
  `UpsertBatch` commits one tx per batch; `History` orders by `sent DESC, id DESC`
- `pkg/server/server.go` — `messageStore` consumer interface (keeps `History`, `get_history` uses it)
- `pkg/server/mocks/` — moq-generated mock of `messageStore`
- `cmd/tg-mcp/main.go` — `run()`: `SyncChats` → `drainPending` → `GetMe` → `server.New` →
  `ingest.New` → goroutines
- `pkg/server/public_test.go` — `TestServePublic`, `TestServePublicErrors` (asserts `HistoryCalls`),
  `TestServePublicClientGone` (context.Canceled)
- `cmd/tg-mcp/e2e_test.go` — `TestE2ESmoke` already covers ingest → endpoint and "serves the bot
  reply once it is the newest"; `TestE2EPendingReplay` covers restart-time replay
- README "Public endpoint" section (around line 236–247) mentions the 500 and "one indexed
  single-row lookup"

## Development Approach
- **testing approach**: Regular (code first, then tests)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change
- maintain backward compatibility

## Testing Strategy
- **unit tests**: required for every task; store tests run against a real SQLite temp file, server
  tests against the moq mock
- **e2e tests**: `make e2e` (`//go:build e2e`, whole app in-process against a fake bot api)

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview
- **Store-owned cache.** `Store` holds the newest message of each *tracked* chat. Chats are tracked
  explicitly via `TrackLatest` (only public chats are passed), so memory is one `Message` per public
  chat and non-public chats are never cached.
- **Newest means highest `message_id`**, both at startup and live, so a restart never changes the
  answer. `History` orders by `sent, id` and would disagree when a bot reply (id 11) is stored
  before a customer message (id 10) from the same second, so the startup load has its own query.
- **Commit, then max by `message_id`.** The cache is updated only after `tx.Commit()` succeeds,
  and a message replaces the cached one only when `m.MessageID >= cur.MessageID`. An edit of the
  newest message (same id) replaces it but keeps the cached `Sent`, mirroring `upsertSQL`, which
  never updates `sent` on conflict. An edit of an older message is ignored.
- **Startup order.** `TrackLatest` runs after `drainPending` and before the writers start.
  `replayChat` writes through `upsertTx` directly, not `UpsertBatch`, so loading after it is what
  makes replayed backlog count.
- Concurrency is not a goal: this feeds a decorative ticker, and a rare stale value until the next
  message is acceptable.
- **Endpoint can no longer fail.** `servePublic` becomes `ByPublic` → `Latest` → `publicView` →
  encode. The `History` call, the `context.Canceled` branch and the 500 path go away.
- Accepted as before: deletions are not reflected (the Bot API never delivers them). New and
  accepted: a row changed out-of-band (sqlite3 CLI) stays stale until restart.

## Technical Details
- `pkg/store/latest.go`:
  - fields added to `Store`: `latestMu sync.RWMutex`, `tracked map[int64]struct{}`,
    `latest map[int64]Message` (maps initialized in `New`)
  - `TrackLatest(ctx context.Context, chatIDs []int64) error` — for each id: `SELECT
    messageColumns FROM messages m WHERE m.chat_id = ? ORDER BY m.message_id DESC LIMIT 1` via
    `queryMessages`; mark tracked; store the row if one came back. Error:
    `fmt.Errorf("load latest message: %w", err)`
  - `Latest(chatID int64) (Message, bool)` — RLock, map lookup
  - `noteLatest(msgs []Message)` — Lock; for each `m`: skip if not tracked; skip if an entry exists
    and `m.MessageID < cur.MessageID`; if `m.MessageID == cur.MessageID` set `m.Sent = cur.Sent`;
    store `m`
- `UpsertBatch`: call `s.noteLatest(msgs)` after a successful `tx.Commit()`
- `cmd/tg-mcp/main.go` `run()`: after `drainPending`, collect `c.ID` for `c.Public != ""` from
  `chats.All()` and call `st.TrackLatest(ctx, ids)`; on error return
  `fmt.Errorf("load latest public messages: %w", err)`
- `pkg/server/server.go` `messageStore`: add `Latest(chatID int64) (store.Message, bool)`;
  regenerate the moq mock
- `pkg/server/public.go` `servePublic`: `msg, ok := s.store.Latest(chat.ID)`; `if ok { res.Message =
  publicView(msg, chat.Username) }`; drop `context`/`errors`/`slog` imports if unused

## What Goes Where
- **Implementation Steps**: store cache, wiring, endpoint, tests, README/CLAUDE.md
- **Post-Completion**: deploy and spot-check a live widget

## Implementation Steps

### Task 1: Add the latest-message cache to the store

**Files:**
- Create: `pkg/store/latest.go`
- Modify: `pkg/store/store.go`
- Create: `pkg/store/latest_test.go`

- [ ] add `latestMu`, `tracked`, `latest` to `Store` and initialize the maps in `New`
- [ ] implement `TrackLatest`, `Latest`, `noteLatest` in `pkg/store/latest.go`
- [ ] call `s.noteLatest(msgs)` in `UpsertBatch` after a successful commit
- [ ] write tests for `TrackLatest`: picks the highest `message_id` per chat across several chats;
      a tracked chat with no rows returns `false`, and its first upserted message then fills it
- [ ] write test: ids 11 then 10 upserted with the same `Sent`, then a fresh `TrackLatest` (the
      restart) still returns 11, the same as the live cache did
- [ ] write test: an edit of the cached message with a moved `Sent` updates the text and keeps the
      original `Sent`, matching `MessageByID`
- [ ] write tests for `noteLatest` via `UpsertBatch`/`UpsertMessage` (table-driven): an untracked
      chat is never cached; a higher id replaces; a lower id is ignored; the same id (an edit)
      replaces; `UpsertMessage` updates the cache
- [ ] write test: a batch failing with `ErrBadMessage` (e.g. a valid message followed by one that
      violates a constraint) leaves the cache untouched, including for the valid one
- [ ] run `make test` - must pass before task 2

### Task 2: Serve /public/{name} from the cache

**Files:**
- Modify: `pkg/server/server.go`
- Modify: `pkg/server/mocks/` (regenerated)
- Modify: `pkg/server/public.go`
- Modify: `pkg/server/public_test.go`

- [ ] add `Latest(chatID int64) (store.Message, bool)` to `messageStore` and regenerate the mock
- [ ] rewrite `servePublic` to use `s.store.Latest`; drop the `History` call, the
      `context.Canceled` branch and the 500 path
- [ ] update `TestServePublic` to drive `LatestFunc` and assert `HistoryCalls()` is empty
- [ ] update `TestServePublicErrors`: unknown alias → 404 with neither `Latest` nor `History`
      called; drop the store-error case
- [ ] remove `TestServePublicClientGone` (no store call left to cancel)
- [ ] run `make test` - must pass before task 3

### Task 3: Load the cache at startup

**Files:**
- Modify: `cmd/tg-mcp/main.go`
- Modify: `cmd/tg-mcp/main_test.go`
- Modify: `cmd/tg-mcp/e2e_test.go`

- [ ] in `run()`, after `drainPending`, call `st.TrackLatest` with the ids of chats whose `Public`
      is set; wrap the error as `load latest public messages: %w`
- [ ] extend `TestRunServesAndShutsDown` (or add a focused test) so a public chat seeded with a
      message before `run()` answers `/public/<alias>` with it: the startup load path
- [ ] extend `TestE2EPendingReplay` so the replayed chat carries a `public` alias and
      `/public/<alias>` serves the replayed newest message. This checks that `TrackLatest` runs after
      `drainPending`
- [ ] confirm `TestE2ESmoke`'s existing "serves the newest message" and "serves the bot reply once
      it is the newest" cases pass unchanged (the live update path from ingest and `send_reply`)
- [ ] run `make test` and `make e2e` - must pass before task 4

### Task 4: Verify acceptance criteria
- [ ] verify `servePublic` no longer calls into SQLite (`grep` for `History` in `public.go`)
- [ ] verify edge cases: edit of newest, edit of older, rolled-back batch, untracked chat
- [ ] run full test suite: `make test`
- [ ] run e2e tests: `make e2e`
- [ ] run `make lint`
- [ ] verify coverage of `pkg/store/latest.go` is complete

### Task 5: [Final] Update documentation
- [ ] README "Public endpoint": drop "A store failure is a bare `500`…" and the `500` in the
      headers sentence; replace "a request is one indexed single-row lookup" with an in-memory
      lookup, loaded at startup and updated as messages arrive
- [ ] CLAUDE.md `/public/{name}` bullet: replace "a store error is a bare 500 with the detail in
      the log only" with the cache rule (loaded after `drainPending`, updated in `UpsertBatch`
      after commit, max by `message_id`, the endpoint never touches SQLite)
- [ ] CLAUDE.md layout: add `latest.go` to the `pkg/store` line
- [ ] leave `docs/plans/completed/20260925-public-latest-message.md` untouched
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- deploy, open the website widget, post in the public group and confirm the teaser updates within
  the 60s `Cache-Control` window without a restart
- restart the container and confirm the teaser still shows the last message
