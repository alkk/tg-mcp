# Public latest-message endpoint

## Overview

netxms.org wants a "latest in Telegram" teaser showing the newest message from the public community
groups. Today every route except `/ping` sits behind the bearer token or a signed link, so the site
has no way to read it.

This change adds an unauthenticated `GET /public/{name}` returning the newest message of one chat
with a little metadata (sent time, sender name, media indicator, `t.me` link). Exposure is opt-in
per chat through a new `public:` alias in `chats.yml`; a chat without one has no public name and so
cannot be reached — not enabled by default is structural, not a flag check.

Benefits:

- the website widget reads JSON straight from the browser (CORS `*`), cacheable by a CDN
- internal customer slugs never reach the website: the alias is its own namespace
- chat ids still never leave the server

## Context (from discovery)

- `pkg/config/config.go` — `ChatInfo{Customer, Label}` decoded with `KnownFields(true)`;
  `validate()` walks `ids()` in sorted order so errors are deterministic; accessors `ByChat`,
  `ByCustomer`, `Customers`, `All`.
- `pkg/config/config_test.go` — `TestLoad` table: yaml in, `map[int64]ChatInfo` or error substring
  out.
- `pkg/server/server.go:123` — `Handler()`: `/ping` open, `/mcp` behind `auth`, `/files/` behind
  `fileAuth`. Package doc at the top lists the routes.
- `pkg/server/server.go:32` — `messageStore` already has `History(ctx, chatIDs, from, to, before,
  limit)`; `pkg/store/store.go:408` orders `sent DESC, id DESC` and keeps the newest `limit`, so
  `History(ctx, []int64{id}, time.Time{}, time.Time{}, nil, 1)` is the latest message. It reads
  `messages` only, never `pending_messages`. No store, interface, mock or schema change.
- `pkg/server/tools.go:560` — timestamps go out as `m.Sent.Format(time.RFC3339)`; log lines carry
  `"chat_id", chat.ID` alongside customer/label.
- `cmd/tg-mcp/e2e_test.go:109` — `TestE2ESmoke`, single chat `acme` at `e2eChatID`; the scripted
  batch ends with message 105 (a photo) before any `send_reply` subtest runs.
- `README.md` — `### Chat map` (line 133), `### Behind a reverse proxy` (line 214);
  `chats.example.yml` documents every chat map field.

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
- maintain backward compatibility: an existing `chats.yml` without the new fields loads unchanged

## Testing Strategy

- **unit tests**: `config_test.go` table cases, new `public_test.go` with the moq store and
  `httptest`
- **e2e tests**: one check in `TestE2ESmoke` (`make e2e`), no UI in this project

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

- **opt-in by alias**: `public: netxms-en` on a chat both enables the endpoint and names it. The
  server resolves only through `config.ByPublic`, so a chat without the field is unreachable by
  construction.
- **global alias namespace**: aliases are unique across the whole map, not per customer — they are
  one URL namespace. They are restricted to `^[a-z0-9][a-z0-9-]*$`, so the path segment needs no
  escaping and traversal is unrepresentable.
- **`t.me` link from config**: `username: netxms_en` (the group's public @username) enables
  `https://t.me/<username>/<message_id>`. Config over learning `chat.username` from updates: no
  schema or ingest change for one link, and a rename is rare and harmless. A private group has no
  username and gets no link — `t.me/c/<id>/…` would leak the chat id.
- **the server is dumb**: raw, untruncated text; the widget trims. Text is untrusted customer input
  and plain text, not HTML — escaping is the consumer's job (`textContent`, never `innerHTML`).
- **opaque failures**: an unknown alias is the same plain 404 as an unmatched path, so the endpoint
  never hints that private chats exist; a store error is a bare 500 with the detail only in the log.
- **deletions are accepted**: the Bot API delivers no deletion events, so spam admins remove stays
  the latest message until the next one arrives (plus up to 60s of cache). Documented, not fixed —
  filtering new senders would hide a genuine newcomer's first question and put policy in the server.

## Technical Details

`chats.yml`:

```yaml
chats:
  -1001234567890:
    customer: community
    label: en
    public: netxms-en    # opt-in; absent = not exposed
    username: netxms_en  # optional, requires public; enables the t.me link
```

`ChatInfo` gains `Public string \`yaml:"public"\`` and `Username string \`yaml:"username"\``.

Validation, added to `validate()` in the existing sorted walk:

- `public` non-empty must match `^[a-z0-9][a-z0-9-]*$`
- `public` unique across all chats: `public alias "x" used by both chat A and B`
- `username` non-empty must match `^[A-Za-z][A-Za-z0-9_]{3,31}$` — 4–32 chars, one below
  telegram's usual 5 so the 4-char collectible usernames load too
- `username` set while `public` is empty is rejected — a dead setting

`ByPublic(name string) (Chat, bool)` — an empty name never matches.

Response, `200 application/json`:

```json
{
  "message": {
    "sent": "2026-09-25T10:04:12Z",
    "sender": "John D.",
    "text": "has anyone tried 5.2 with ...",
    "media": {"type": "photo", "file_name": "screenshot.png"},
    "link": "https://t.me/netxms_en/48213"
  }
}
```

- `sent` is `m.Sent.UTC().Format(time.RFC3339)`
- `media` omitted when `!m.HasMedia()`; no download url, `/files/` stays authenticated
- `file_name` only when it is a name the sender gave: ingest synthesizes `<file_unique_id><ext>`
  for every photo, voice, video note and sticker, and as the fallback for nameless documents,
  audio, video and animations (`pkg/telegram/types.go:218-246`, `fileName` at `:307`). A name with
  `m.FileUniqueID` as its prefix is omitted — otherwise the endpoint would hand out the key the
  private `/files/` cache is stored under, and the widget would show meaningless names
- `link` omitted when the chat has no `username`
- a known alias with no messages yet: `{"message": null}`, so the widget can tell "quiet" from "not
  configured"
- nothing else leaves: no chat id, sender id, message id field, reply_to, file ids
- the bot's own replies are messages like any other

Headers on every 200: `Content-Type: application/json`, `Access-Control-Allow-Origin: *`,
`Cache-Control: public, max-age=60`, `X-Content-Type-Options: nosniff`. A simple GET, so no preflight
handling. No in-process rate limiter — an indexed single-row lookup, and a CDN or proxy absorbs load.

Flow of `servePublic`:

1. `chat, ok := s.chats.ByPublic(r.PathValue("name"))`; `!ok` → `http.NotFound`
2. `msgs, err := s.store.History(ctx, []int64{chat.ID}, time.Time{}, time.Time{}, nil, 1)`;
   error → `slog.Error` with alias, customer, label, `chat_id`, err, then `http.Error(w, "internal
   error", 500)`
3. build the response from `msgs[0]` when present, encode

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): config fields and validation, the handler and route,
  tests, e2e check, README/CLAUDE.md/`chats.example.yml`
- **Post-Completion** (no checkboxes): the `chats.yml` edit on the deployment, the proxy exposing
  `/public/`, the widget on netxms.org

## Implementation Steps

### Task 1: Add `public` and `username` to the chat map

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `pkg/config/config_test.go`

- [x] add `Public` and `Username` to `ChatInfo` with yaml tags `public` / `username`
- [x] extend `validate()`: alias pattern, alias uniqueness across the whole map (track
  `map[string]int64` alias → chat id, error names both chats), username pattern, username without
  public rejected; compile both regexps once at package level
- [x] add `ByPublic(name string) (Chat, bool)`, empty name never matches
- [x] write `TestLoad` cases for success: both fields round-trip, `public` without `username`,
  existing files without the fields load unchanged
- [x] write `TestLoad` cases for errors: one alias on chats of two different customers; bad aliases
  `Netxms`, `../x`, `-x`, `a_b`; bad usernames `abc` (too short), `1netxms` (leading digit),
  `netxms-en` (contains `-`); `username` without `public`
- [x] write `TestConfig_ByPublic`: hit returns the chat with id, miss, empty name misses even when
  chats without an alias exist
- [x] run `make test` - must pass before task 2

### Task 2: Serve `GET /public/{name}`

**Files:**
- Create: `pkg/server/public.go`
- Create: `pkg/server/public_test.go`
- Modify: `pkg/server/server.go`

- [x] create `public.go` with the response structs (`publicResult{Message *publicMessage}`,
  `publicMessage`, `publicMedia`) and `servePublic` following the flow in Technical Details
- [x] set the four response headers; build `link` only when `chat.Username != ""`; set
  `file_name` only when `!strings.HasPrefix(m.FileName, m.FileUniqueID)`
- [x] register `mux.HandleFunc("GET /public/{name}", s.servePublic)` in `Handler()`, outside `auth`,
  and add the route to the package doc and the `Handler` doc comment
- [x] write table-driven `TestServePublic` success cases: full JSON of a text message with link;
  no `link` without username; a document with a real `file_name`; a photo whose `FileName` is
  `FileUniqueID+".jpg"` and a document falling back to `FileUniqueID` — both carry `media.type`
  only and the body contains no file unique id; `Text` with `<script>&` decodes back unchanged
  (no HTML processing server-side); empty chat →
  `{"message":null}`; headers asserted; no `Authorization` header sent; the mock asserts `History`
  got exactly `[]int64{chatID}`, zero bounds, nil cursor and limit 1
- [x] write error cases: unknown alias → 404 and `History` never called; an allowlisted chat
  without `public` unreachable under its customer slug and its label; store error → 500 whose body
  does not contain the error text
- [x] assert no response body contains the chat id
- [x] run `make test` and `make lint` - must pass before task 3

### Task 3: Cover the endpoint end to end

**Files:**
- Modify: `cmd/tg-mcp/e2e_test.go`

- [x] give the `acme` chat in `TestE2ESmoke` `public: acme-public` — deliberately not the slug, so
  the 404 below proves something — and a `username`
- [x] add a subtest right after the ingest `Eventually`, before any `send_reply` subtest: `GET
  /public/acme-public` without auth returns message 105 as a photo with the expected `t.me` link
  and no `file_name`
- [x] assert the raw body contains neither the chat id (`1001234567890`) nor the photo's file unique
  id (`uniq-2`) — the only place the real encoding of a real ingested row is checked
- [x] in the same subtest, `GET /public/acme` (the slug, not an alias) is 404
- [x] run `make e2e` - must pass before task 4

### Task 4: Verify acceptance criteria

- [x] verify all requirements from Overview are implemented
- [x] verify edge cases: empty chat, missing username, store failure, chat without alias
- [x] run full test suite: `make test`
- [x] run e2e tests: `make e2e`
- [x] run `make lint`
- [x] verify coverage of `public.go` and the new `config.go` branches is complete

### Task 5: [Final] Update documentation

**Files:**
- Modify: `README.md`
- Modify: `chats.example.yml`
- Modify: `pkg/config/config.go`
- Modify: `CLAUDE.md`

- [x] README `### Chat map`: the two fields and their validation
- [x] README: a `### Public endpoint` section with the url, the JSON, the headers, `{"message":
  null}` vs 404, the warning that `text` is untrusted customer input to insert via `textContent`
  never `innerHTML`, and the deletion limitation
- [x] README `### Behind a reverse proxy`: `/public/` is unauthenticated like `/ping`, and follows
  the prefix mount
- [x] `chats.example.yml`: add `public:` and `username:` *uncommented* to the existing `acme`
  entry, with a comment explaining them — `TestLoad_exampleFile` loads that file, so the example is
  validated rather than decorative (and it asserts `Customers() == [acme, globex]`, so no new
  customer)
- [x] `pkg/config/config.go` package doc: chats also resolve to an optional public alias
- [x] CLAUDE.md: layout entry for `pkg/server/public.go`; extend the "chat ids never leave the
  server" bullet — `ByPublic` is a third resolution path next to `chatIDs`/`singleChat`; a new
  design-constraint bullet recording opt-in by alias, global alias namespace, opaque 404/500, no
  chat id, synthesized file names withheld, raw text with escaping as the consumer's job, deletions
  accepted and why sender filtering was rejected
- [x] run `make test` — the example file is under test
- [x] move this plan to `docs/plans/completed/` (deferred to orchestrator completion step)

## Post-Completion

*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Deployment:**

- add `public:` (and `username:` where the group is public) to the community chats in the
  production `chats.yml`, restart
- make sure the reverse proxy forwards `/public/` on the hostname the website will call
- curl the endpoint from outside and confirm the CORS header survives the proxy

**Consuming project:**

- netxms.org widget: fetch `/public/<alias>`, render with `textContent`, truncate client-side,
  handle `{"message": null}` and a failed fetch by hiding the teaser
