# Signed file links and thread image inlining

## Overview

`get_file` hands back a `/files/` url for every attachment it cannot inline, but that url sits behind
the same bearer token as `/mcp`. An MCP harness only speaks MCP — it never sees the token — so the
url points at bytes the caller structurally cannot fetch. Every non-inlined attachment is a dead end
for the primary consumer.

This adds a signed, time-boxed download link: the url carries an expiry and an HMAC, so it works
without any credential and stops working shortly after it is minted. The bearer path stays, so
curl workflows are untouched.

Two related changes ride along:

- `get_file` currently emits `mcp.ImageContent` for anything matching `image/`, including `heic`,
  `svg`, `bmp`, `tiff` and `avif` — types the vision API rejects, so the call errors instead of
  degrading to a link (github issue #2). Narrowing the filter is a prerequisite: those types need
  the link path, and the link path now works.
- `get_thread` inlines up to 5 images, so a support thread's screenshots arrive with the
  conversation instead of costing a round trip each.

## Context (from discovery)

- files/components involved: `pkg/server/files.go` (`getFile`, `inlineContent`, `fileURL`,
  `serveFile`, `requestBase`), `pkg/server/tools.go` (`registerTools`, `getThread`, `messageView`,
  `view`), `pkg/server/server.go` (`Params`, `New`, `Handler`, `auth`), `cmd/tg-mcp/main.go`
  (`options`, `validate`, `run`)
- related patterns found: `trackBase` already pins the request base onto the `/mcp` request via
  `X-Tg-Mcp-Base` rather than shared state; `fileURL` reads it back off `CallToolRequest.Extra.Header`.
  The same request-scoped approach carries the TTL-derived expiry — no new shared state.
- `pkg/store/files.go` is read-only for this work: `Cached` takes the raw `file_unique_id` and hexes
  it internally (`fileKey`), so the url path keeps carrying the raw id and the signature covers that
  same string.
- tests: `t.TempDir()` for fs work, `httptest` fakes for telegram, real SQLite temp files for the
  store, table-driven with testify. `cmd/tg-mcp/e2e_test.go` runs the whole app against a fake bot api.
- `seededServer` (`pkg/server/tools_test.go:28`) hands every test a bare `&mocks.TelegramAPI{}`, and
  its acme thread already carries a qualifying image (`shot.png`, `pkg/server/tools_test.go:48-50`).
  moq panics on a nil `GetFileFunc`, so the moment `get_thread` fetches, that fixture takes down the
  test binary — the fixture has to be scripted before the inlining lands.
- `cachedFile` logs `chat_id`, `message_id`, `media` and the display name (`pkg/server/files.go:120`)
  — it does not log `file_unique_id`. The rule here is only that the signed url never reaches a logger.

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

- **unit tests**: required for every task (see Development Approach above)
- **e2e tests**: the project has no UI. `cmd/tg-mcp/e2e_test.go` (`//go:build e2e`) runs the whole
  binary in-process against a fake bot api and is the right place for the end-to-end proof that a
  signed link works with no `Authorization` header at all. Run with `make e2e`.
- **linting**: `.golangci.yml` sets `build-tags: [e2e]`, so the tagged file is linted too — `make lint`
  must pass alongside the tests.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

**Signature scheme.** A link key is derived once in `server.New`:

```
linkKey = HMAC-SHA256(authToken, "tg-mcp/files-url/v1")
```

No new secret to configure, domain-separated so it can never be confused with the auth token itself,
and it rotates when the token does.

The url gains an expiry and a signature over the id already in the path:

```
<base>/files/<file_unique_id>?exp=<unix>&sig=<32 hex chars>
sig = HMAC-SHA256(linkKey, id + "\n" + exp)[:16]
```

**Only the id and expiry are signed — never host, scheme or prefix.** The base is reconstructed per
request from `X-Forwarded-*`, so a signature covering the host would break the moment a download went
through a different path than the `/mcp` call did, and it would quietly reintroduce the public-url
coupling the current design avoids. The id is Telegram's raw `file_unique_id` (base64url charset), so
it cannot contain the `\n` separator and the MAC input stays injective. 128 bits truncated is far past
what a 5-minute window needs and keeps the url manageable.

**Route auth.** `s.auth` on `/files/` is replaced by a `fileAuth` middleware accepting either
credential: a valid bearer serves as today; otherwise the signature is verified with `hmac.Equal`.
A bad or malformed signature is a `401` with the same opaque body as today. An authentic but expired
signature is a `410 Gone` naming the expiry and telling the caller to call `get_file` again — the
difference between the model self-healing and the model concluding the file is missing. **The
signature is verified before the expiry is checked**, so a forged link never receives a `410` and the
distinct status leaks nothing about which ids exist. `/mcp` keeps `s.auth` untouched.

**Why stateless.** A random-token map would buy single-use links at the cost of a mutex, a sweeper,
and links dying on restart. This codebase already refuses shared mutable state for exactly this shape
of problem — no persisted `getUpdates` offset, no public-url setting, `trackBase` pinning the base
onto the request rather than a field. Single-use is also a thin prize: a harness retrying a failed
download is legitimate, and a 5-minute window makes replay near-worthless.

**Why 5 minutes.** Nothing bounds the gap between the tool result landing and the fetch happening —
the model may make other calls first, and a `curl` to a new host triggers a permission prompt whose
approval latency is unbounded. The leak vector that matters is proxy access logs, which persist
indefinitely; against those a 60s link and a 5-minute link are equally dead by the time anyone reads
them. TTL only defends against a link leaking live, where the difference is marginal.

## Technical Details

**`server.Params`** gains `FileLinkTTL time.Duration`. `Server` gains `linkKey []byte` and
`linkTTL time.Duration`, both set in `New`. **A zero TTL defaults to 5 minutes in `New`**, exactly the
way `Version` defaults to `"dev"` (`pkg/server/server.go:91-94`) — every existing `New(Params{...})`
call site in `files_test.go` and `tools_test.go` keeps compiling and stays green, and the flag's own
`default:"5m"` means the zero case only ever arises programmatically. `validate()` rejects a
*negative* duration only; zero means "use the default" everywhere.

**`fileURL`** takes the expiry and key, appending `?exp=&sig=`. It is called only from `getFile`.

**`fileResult`** is unchanged. `Inline` and `URL` stay mutually exclusive: two routes to the same
bytes invites the model to pick badly, and an image that inlines is already in the form you want it.

**`inlineContent`** gains a shared `isInlineImage(mimeType string) bool` predicate — exactly
`image/jpeg`, `image/png`, `image/gif`, `image/webp` — used by both `getFile` and the thread helper so
the allowlist exists once.

The switch needs a new arm, not just a narrowed one. `isTextual` matches on the substring `"xml"`
(`pkg/server/files.go:155`), so narrowing the image branch alone would drop `image/svg+xml` straight
into the *text* branch and inline it as `TextContent` — issue #2 half-fixed, and the svg case at
`files_test.go:237-247` changes behaviour silently. The order becomes:

```go
switch {
case isInlineImage(mimeType):                     // model can read it
    return &mcp.ImageContent{...}
case strings.HasPrefix(mimeType, "image/"):       // heic, svg, bmp, tiff, avif — link path
    return nil, mimeType, nil
case isTextual(mimeType) && utf8.Valid(data):
    return &mcp.TextContent{...}
}
```

**`get_file` description** becomes a contract with no numbers — images and text come back in the
result, everything else as a short-lived download link. The caller branches on what it got, so the
threshold is ours to change and does not belong in the schema.

**`messageView`** gains `Inlined bool \`json:"inlined,omitempty"\``. Content blocks are a flat list
with no link back to a message; five flagged messages and five image blocks in the same chronological
order let the caller pair them without text markers, and it stays inside "result shaping is limited
to snippets, timestamps and media indicators".

**`getThread`** returns a `*mcp.CallToolResult` carrying the first 5 qualifying images in
chronological order. **When nothing qualified it must return a nil result, not an empty one**: the
go-sdk only synthesizes the serialized-JSON `TextContent` block when `res.Content == nil`
(`vendor/.../mcp/server.go:435-443`), and `messagesResult` is an object so the `!isObjectJSON` branch
does not fire either. A non-nil empty slice would silently strip the conversation out of `content` for
every thread without images — the common case.

Even on the happy path the messages move to `structuredContent` only. That is a deliberate, recorded
consequence: `get_file` already has this shape, and a client reading `content` rather than
`structuredContent` sees images instead of JSON.

Qualifying means: has media, `MediaType != "sticker"` (static stickers are named `<id>.webp` by
`stickerExt`, `pkg/telegram/types.go:294-303`, so five 👍 reactions would otherwise burn the whole cap
and trigger five downloads per read), `isInlineImage` on the same mime resolution `getFile` uses, and
under the inline size limit **as re-stated after download** — `msg.FileSize` is optional in the Bot
API and can be 0, which is why `getFile` re-stats at `pkg/server/files.go:74-76` before handing a size
to `inlineContent`. Non-image media does not consume a cap slot.

Cache misses fetch on demand through the existing `cachedFile` under a 10s `context.WithTimeout`
derived from the request context. At most 5 tasks ever exist, so a `sync.WaitGroup` writing into a
preallocated indexed slice is enough — that also makes chronological order structural rather than
something the assembly step has to remember. Every failure degrades that one image to metadata; an
attachment problem must never fail a conversation read. `s.telegram == nil` skips inlining rather than
erroring, unlike `get_file` where no client is a genuine failure.

**Logging.** No logger ever receives the signed url. The live risk is `fileAuth`'s 401 path, which
mirrors `pkg/server/server.go:170` — that logs `r.URL.Path`, and reaching for `r.URL.String()` or
`r.RequestURI` there would write a valid signature into the log for its whole TTL.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, README and CLAUDE.md updates
- **Post-Completion** (no checkboxes): reverse-proxy verification against a real deployment, closing
  issue #2

## Implementation Steps

### Task 1: Add the link signing helpers

**Files:**
- Modify: `pkg/server/files.go`
- Modify: `pkg/server/files_test.go`

- [x] add `deriveLinkKey(authToken string) []byte` computing `HMAC-SHA256(authToken, "tg-mcp/files-url/v1")`
- [x] add `signFileID(key []byte, id string, exp int64) string` returning 32 hex chars from the first
      16 bytes of `HMAC-SHA256(key, id + "\n" + strconv.FormatInt(exp, 10))`
- [x] add `verifyFileSig(key []byte, id, expRaw, sig string) (expired bool, ok bool)` — parses `exp`,
      recomputes, compares with `hmac.Equal`, reports authenticity separately from expiry
- [x] write tests for `signFileID` (stable output for the same inputs, different output for a
      different id, expiry, or key)
- [x] document that `expired` is only meaningful when `ok` is true, so the 401-before-410 ordering
      cannot be miswired at the call site
- [x] write tests for `verifyFileSig` (valid and unexpired, valid but expired, tampered sig, tampered
      id, tampered exp, non-numeric exp, empty sig)
- [x] run tests - must pass before task 2

### Task 2: Thread the TTL and link key through the server

**Files:**
- Modify: `pkg/server/server.go`
- Modify: `cmd/tg-mcp/main.go`
- Modify: `pkg/server/server_test.go`
- Modify: `cmd/tg-mcp/main_test.go`

- [x] add `FileLinkTTL time.Duration` to `server.Params`; store `linkKey` and `linkTTL` on `Server`,
      deriving the key in `New` from `p.AuthToken`
- [x] **default a zero `FileLinkTTL` to 5m in `New`**, mirroring the `Version` → `"dev"` default at
      `pkg/server/server.go:91-94` — this is what keeps the six existing `New(Params{...})` call sites
      in `files_test.go` and `tools_test.go` compiling and green without touching them
- [x] add `FileLinkTTL time.Duration \`long:"file-link-ttl" env:"FILE_LINK_TTL" default:"5m" description:"lifetime of get_file download links"\``
      to `options` in `cmd/tg-mcp/main.go`
- [x] reject only a *negative* `FileLinkTTL` in `validate()`, naming both the flag and the env var in
      the message; zero means "use the default" so `e2e_test.go`'s options literal stays valid
- [x] pass `FileLinkTTL: opts.FileLinkTTL` in the `server.New(server.Params{...})` call in `run`
- [x] add `FILE_LINK_TTL` to `clearEnv` (`cmd/tg-mcp/main_test.go:289-299`) so the defaults subtest
      stays independent of the ambient environment
- [x] write tests for `New` defaulting a zero TTL and for the derived key being stable for a token
- [x] write tests for `parseArgs` picking up the flag and defaulting to 5m, and for `validate`
      rejecting a negative value while accepting zero
- [x] run tests - must pass before task 3

### Task 3: Mint signed urls in get_file

**Files:**
- Modify: `pkg/server/files.go`
- Modify: `pkg/server/files_test.go`

- [x] change `fileURL` to take the expiry and link key and append `?exp=&sig=`, keeping the existing
      `url.PathEscape` on the id and the empty-base fallback to a listener-relative path
- [x] compute the expiry in `getFile` as `time.Now().Add(s.linkTTL).Unix()` and pass it through
- [x] write tests for `fileURL` (base from the header, expiry and signature present and verifiable,
      empty base degrading to a relative path, id needing escaping)
- [x] write tests for `getFile` returning a url whose signature verifies against the server's key
- [x] run tests - must pass before task 4

### Task 4: Accept signatures on the /files/ route

**Files:**
- Modify: `pkg/server/server.go`
- Modify: `pkg/server/files.go`
- Modify: `pkg/server/server_test.go`
- Modify: `pkg/server/files_test.go`
- Modify: `cmd/tg-mcp/e2e_test.go`

- [x] add a `fileAuth` middleware: valid bearer serves; otherwise read `exp`/`sig` from the query and
      verify against the **decoded** `r.PathValue("id")` — the same string `store.Cached` receives at
      `pkg/server/files.go:237`
- [x] return `401` with the existing opaque body and `WWW-Authenticate: Bearer` for a missing,
      malformed or tampered signature; log `r.URL.Path` only, never `r.URL.String()` or `r.RequestURI`
- [x] return `410 Gone` for an authentic but expired signature, with a body telling the caller to
      call `get_file` again — verify the signature **before** checking the expiry
- [x] swap `s.auth` for `s.fileAuth` on the `/files/{id}` route in `Handler`, leaving `/mcp` on `s.auth`
- [x] add `Cache-Control: private, no-store` to `serveFile` — under bearer-only the `Authorization`
      header suppressed shared caching, and a credential-in-the-query url does not, so a caching proxy
      could otherwise keep serving attachments long past the TTL
- [x] update the two doc comments that assert bearer-only access: `serveFile`'s
      (`pkg/server/files.go:234-235`) and `Handler`'s routing comment (`pkg/server/server.go:107`)
- [x] **invert the existing e2e subtest** `"the download url needs the token"`
      (`cmd/tg-mcp/e2e_test.go:280-283`) — that url is now signed and returns 200; rename it to say
      the url carries its own credential
- [x] write tests: valid sig without bearer → 200; bearer without sig → 200; tampered sig → 401;
      authentic but expired → 410; forged **and** expired → 401 not 410; no credential at all → 401
- [x] write a test that an unknown id with a valid signature is still a 404, and one that an id
      needing escaping round-trips through the route
- [x] run tests and `make e2e` - must pass before task 5

### Task 5: Narrow the inline image types

**Files:**
- Modify: `pkg/server/files.go`
- Modify: `pkg/server/files_test.go`

- [x] extract `isInlineImage(mimeType string) bool` — exactly `image/jpeg`, `image/png`, `image/gif`,
      `image/webp` — as the single place the allowlist is written; Task 6 reuses it
- [x] restructure the `inlineContent` switch to three arms: `isInlineImage` inlines, any other
      `image/` prefix returns nil for the link path, then the existing text arm. **The middle arm is
      the fix, not an extra** — without it `image/svg+xml` reaches `isTextual`, which matches the
      substring `"xml"` (`pkg/server/files.go:155`), and svg keeps inlining
- [x] leave the size threshold untouched
- [x] update the code comment to say why the set is closed — these are the types a vision model
      accepts, and an unusable `ImageContent` block fails the call where a link would have worked
- [x] update the existing svg expectation in `TestServeFile` (`files_test.go:237-247`), whose
      behaviour changes here
- [x] write table-driven tests: `.heic`, `.svg`, `.bmp`, `.tif`, `.avif`, `.ico` → `inline=false` plus
      a url; `.jpg`, `.png`, `.gif`, `.webp` → inlined `ImageContent`. Assert the *branch taken*, never
      the mime subtype — commit 32eee61 removed exactly that kind of platform-dependent pin
- [x] use non-UTF-8 fixture bytes for the rejected types, so an extension the system mime table does
      not know cannot sniff into `text/plain` and inline through the text arm
- [x] write a test for a sniffed `image/bmp` (no extension) taking the link path
- [x] run tests - must pass before task 6

### Task 6: Inline thread images

**Files:**
- Modify: `pkg/server/tools.go`
- Modify: `pkg/server/files.go`
- Modify: `pkg/server/files_test.go`
- Modify: `pkg/server/tools_test.go`

- [x] **first**, script `seededServer`'s mock with a `GetFileFunc` and `DownloadFunc`
      (`pkg/server/tools_test.go:28-31`) — its acme thread already holds `shot.png`
      (`tools_test.go:48-50`), and moq **panics** on a nil `GetFileFunc`
      (`pkg/server/mocks/telegram_api.go:126-128`). In a fetch goroutine that panic is unrecoverable
      and kills the whole `go test -race` binary, taking `TestToolsGetThread` and the `get_thread`
      call inside the send_reply test (`tools_test.go:556`) with it
- [x] add `Inlined bool` to `messageView` and a `threadImageCap = 5` constant
- [x] add a helper in `pkg/server/files.go` (tests in `files_test.go`, per the one-`_test.go`-per-file
      convention) that picks the first N qualifying images and fetches them via `cachedFile` under a
      10s `context.WithTimeout`, using a `sync.WaitGroup` over a preallocated indexed slice so
      chronological order is structural
- [x] qualification: has media, `MediaType != "sticker"`, `isInlineImage` from Task 5, and the size
      re-stated after download rather than trusted from `msg.FileSize`, which the Bot API may omit
- [x] have the helper return the content blocks plus the set of message ids that were inlined, with
      every per-image failure degrading to metadata instead of propagating
- [x] wire it into `getThread`: build the views, mark the inlined ones, return the blocks in a
      `*mcp.CallToolResult`; skip entirely when `s.telegram == nil`
- [x] **return a nil result, never an empty one, when no block was produced** — the go-sdk only
      synthesizes the JSON `TextContent` when `Content == nil` (`vendor/.../mcp/server.go:435-443`),
      so an empty slice strips the conversation from every image-free thread
- [x] write tests: 7 qualifying images → exactly 5 blocks and 5 `inlined` flags in chronological
      order; one download failing → 4 blocks **and 4 flags** and the thread intact; `telegram == nil`
      → no blocks and the thread intact; a thread with no images → nil result and the JSON block still
      present; stickers and non-image media do not consume cap slots; an oversized image is skipped;
      a photo whose `file_size` is 0 is still gated by the re-stat
- [x] run tests - must pass before task 7

### Task 7: Update the tool descriptions

**Files:**
- Modify: `pkg/server/tools.go`
- Modify: `pkg/server/tools_test.go`

- [x] rewrite the `get_file` description as a contract with no numbers: images and text come back in
      the result, everything else as a short-lived download link that needs no credential
- [x] extend the `get_thread` description to say the first few images come back inline and the rest
      carry metadata for `get_file`
- [x] add a clause to `get_file` that the link carries its own credential and must not be sent to a
      customer — the model holds both the url and `send_reply`, and one sentence is the cheap guard
- [x] write a test asserting the registered descriptions mention neither a byte threshold nor the
      bearer token
- [x] run tests - must pass before task 8

### Task 8: End-to-end proof without a credential

**Files:**
- Modify: `cmd/tg-mcp/e2e_test.go`

- [x] make the fake bot api's `getFile` and download routes **file_id-keyed** — both currently answer
      with one hardcoded payload regardless of what was asked for (`cmd/tg-mcp/e2e_test.go:401-403`
      and `:414-416`), so a second attachment is not serveable as-is — then add an image message.
      The non-inlinable attachment already exists (message 103 / `core.dump`, `e2e_test.go:507-512`,
      fetched at `:263-272`), so nothing needs adding for that half
- [x] call `get_file` through the running server and read the url off the result
- [x] fetch that url off the listener with **no** `Authorization` header and assert 200 plus the
      expected bytes and `Content-Disposition: attachment`
- [x] assert the same url with a mangled `sig` gives 401
- [x] call `get_thread` on a message with images and assert image content blocks come back
- [x] run `make e2e` - must pass before task 9

### Task 9: Verify acceptance criteria

- [x] verify all requirements from Overview are implemented
- [x] verify edge cases are handled: expired link, forged link, unknown id, missing query string,
      empty base url, `telegram == nil`
- [x] run full test suite: `make test`
- [x] run e2e tests: `make e2e`
- [x] run `make lint` — `.golangci.yml` lints the e2e-tagged file too
- [x] verify test coverage meets project standard

### Task 10: [Final] Update documentation

- [x] update the `get_file` row in README.md's tool table and its bullet under "Behaviour worth
      knowing" — inline images and text, everything else a short-lived link needing no bearer token
- [x] update the `get_thread` bullet to cover the 5-image inline cap
- [x] add `--file-link-ttl` to README.md's flag/env table (`README.md:99-104`) and extend
      `--auth-token`'s description, which now also covers link signing
- [x] fix the three other README claims that go stale: "authenticated `/files/<id>` endpoint"
      (`README.md:20`), "Both the MCP endpoint and `/files/` sit behind the bearer token"
      (`README.md:210`), and the curl instructions (`README.md:288-290`)
- [x] add a line to README.md's reverse-proxy section that the query string must reach us — a proxy
      that strips it breaks every link, and the failure looks like a forged signature (401), not a
      proxy problem
- [x] update the package doc at `pkg/server/server.go:1-4`, which describes the bearer-guarded surface
- [x] rewrite CLAUDE.md's `/files/` design-constraint bullet: it currently documents bearer-only
      access and the `image/*` mime rationale, and both change here
- [x] add the signed-link reasoning to CLAUDE.md: stateless HMAC over id and expiry only, why the
      host is not signed, why signature is checked before expiry
- [x] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**:
- fetch a signed link through the real reverse proxy and confirm the query string survives; an nginx
  `location` with a rewrite that drops arguments will break every link and the failure looks like a
  forged signature (401), not a proxy problem
- confirm the proxy's access log retention is acceptable given that the log line contains a link that
  is valid for the TTL window

**External updates**:
- close github issue #2 — the mime narrowing in Task 5 is its fix, and the description half of that
  ticket is resolved by Task 7 dropping the numbers rather than naming them
