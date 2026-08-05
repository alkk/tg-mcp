# Render send_reply markdown as Telegram HTML

## Overview

`send_reply` posts the model's text verbatim: `SendMessage` (`pkg/telegram/client.go:118`) sends
`{chat_id, text}` with no `parse_mode`, so the markdown Claude naturally writes — ``` fences,
`` `inline code` ``, `**bold**` — reaches customers as raw syntax (screenshot-confirmed in a
production support group). Fix: convert a practical markdown subset to Telegram HTML server-side
and send with `parse_mode=HTML`, falling back to today's plain send if Telegram rejects the
message. Support replies full of shell commands render with real monospace blocks; a reply never
bounces back to the MCP client.

Rejected alternatives (from the brainstorm, for the record):

- **`parse_mode=MarkdownV2` pass-through** — ~18 reserved characters must be escaped outside
  entities; virtually every English sentence 400s.
- **MarkdownV2 as converter output** — same parsing work as HTML, worse target: more escapes to
  get wrong, mistakes bounce the message instead of rendering slightly off.
- **`entities` array** — zero escaping but UTF-16 code-unit offset arithmetic for every entity.
- **Bot API 10.2 `sendRichMessage`** — block AST is more converter code for no gain on our subset,
  and the API is too new to rely on customers' clients. It is the future path for embedded media
  (`InputRichMessage.media`) and real tables (`InputRichBlockTable`) — see Post-Completion.

Guiding safety rule (from plan review): the fallback only rescues messages Telegram *rejects*.
Anything the converter renders as valid-but-wrong HTML reaches the customer corrupted — worse
than today's lossless raw markdown. So every conversion rule must fail toward "leave it literal",
never toward "guess an entity".

## Context (from discovery)

- `pkg/telegram/client.go:118` — `SendMessage`, gains a parse-mode parameter; doc comment at
  `client.go:116` says "plain text" and must change with it (`godoclint` is enabled)
- `pkg/server/server.go:47` — consumer-side `telegramAPI` interface, moq mock in
  `pkg/server/mocks/telegram_api.go` (`go:generate` at `server.go:28`); `SendMessageFunc` is also
  implemented by hand at `pkg/server/tools_test.go:497` and `:640` (`echoAPI`)
- `pkg/server/tools.go:111-116` — `sendReplyParams`; `Text` jsonschema tag currently promises
  "plain text of the reply, no markdown"
- `pkg/server/tools.go:197-` — `sendReply`: trim → rune-count check → `replyTarget` → send → log →
  persist the returned message. Persistence code needs no change, but the *fakes* do: real
  Telegram responds with rendered plain text + separate entities, while the e2e fake (`serveSend`
  in `cmd/tg-mcp/e2e_test.go`) and `echoAPI` echo the posted text back verbatim — they must mimic
  Telegram or the tests assert the wrong contract
- `cmd/tg-mcp/e2e_test.go` — one sequential scenario; `assert.Len(... sentMessages(), 1)` and
  `require.Len(t, res.Messages, 4)` mean markdown goes into the *existing* reply subtest, not a
  new flow
- `README.md:36` (tool table) and `README.md:73` ("sends plain text only") — sync rule with tool
  descriptions per CLAUDE.md
- Only `pkg/server` consumes `SendMessage`; ingest does not send

## Development Approach

- **testing approach**: Regular (code first, then tests in the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - table-driven with testify, one `_test.go` per source file (project convention)
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change
- no new dependencies, no vendor churn

## Testing Strategy

- **unit tests**: required for every task; telegram tests hit `httptest` fakes, server tests use
  moq mocks (existing conventions)
- **e2e tests**: `make e2e` (`//go:build e2e`) — extend the fake bot api and the existing reply
  subtest in the same task as the fake change

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope

## Solution Overview

1. **`pkg/tghtml`** — one exported function, `Render(text string) string`: model markdown in,
   Telegram-safe HTML out. Hand-rolled line scanner, no dependencies; construction guarantees
   balanced tags and escaped text, so output is valid Telegram HTML by design.
2. **`pkg/telegram`** — `SendMessage` gains a `parseMode` string parameter placed right after
   `text` (`""` = plain, `telegram.ParseModeHTML` = `"HTML"`); the client stays dumb about
   markdown.
3. **`pkg/server`** — `sendReply` renders before sending; on **any 400** from the HTML attempt it
   logs a warning and retries once with the raw text and no parse mode. A 400 means nothing was
   delivered, so the retry can never double-post; at worst a non-parse 400 (chat not found) costs
   one extra failed call and returns the same error. Non-400 errors (403, timeouts, flood wait)
   propagate unchanged with no retry — the first attempt may have landed. Error text stays
   chat-id-free (project constraint).

## Technical Details

**Telegram HTML mode, per the official docs (core.telegram.org/bots/api#html-style) — the spec
the converter targets:**

- Supported tags: `b`/`strong`, `i`/`em`, `u`/`ins`, `s`/`strike`/`del`, `tg-spoiler`,
  `a href`, `tg-emoji`, `tg-time`, `code`, `pre`, `blockquote` (+ `expandable`). Only these;
  everything we emit (`b`, `i`, `code`, `pre`, `a`) is on the list.
- Every `<`, `>`, `&` not part of a tag or entity **must** be escaped. Only four named entities
  exist: `&lt;` `&gt;` `&amp;` `&quot;` (numeric entities also allowed) — so href escaping uses
  exactly `&amp;`/`&quot;`, nothing fancier.
- A language is declared via nested `<pre><code class="language-x">`; a standalone `<code>`
  cannot carry one — matches the fence mapping below.
- Nesting is legal (`<b><i>…</i></b>`) and the converter uses it — see the nesting rules below;
  `blockquote` support means that v1 skip is preference, not impossibility.

Converter subset (block pass, then inline pass per plain line):

| markdown | HTML |
|---|---|
| ```` ```lang ```` fence | `<pre><code class="language-lang">…</code></pre>` (`<pre>…</pre>` without tag); content HTML-escaped, no inline markdown processing; unclosed fence at EOF treated as closed; language token restricted to `[A-Za-z0-9+#._-]`, otherwise dropped |
| `` `code` `` | `<code>…</code>` |
| `**bold**` | `<b>…</b>` (flanking rules below) |
| `*italic*` | `<i>…</i>` (flanking rules below) |
| `[text](url)` | `<a href="url">text</a>`; `&` and `"` in url escaped as `&amp;`/`&quot;` |
| `#`–`######` + **space** heading line | `<b>…</b>` line (Telegram has no heading entity); no space, no heading — `#!/bin/sh` stays literal |
| everything else | escaped (`&` `<` `>`) and passed through; `- ` list markers stay literal |

**Dropped from v1 after review: `_italic_`.** Underscores are everywhere in identifiers
(`file_unique_id`, `chat_id`, `message_thread_id`) and a false match renders *valid* HTML the
fallback cannot catch — corruption, not degradation. Models overwhelmingly emit `*`/`**` anyway.

**Flanking rules for `*` delimiters** (CommonMark-style, prevents `SELECT * FROM t`,
`chmod 755 * && chown *`, `2 * 3 * 4` from italicising): an opener must be preceded by
start-of-line, space, or punctuation and followed by non-space; a closer must be preceded by
non-space. Flanking applies at every recursion level.

**Delimiter-run semantics** (`*` is never read character-by-character):

1. At every recursion level, backtick code spans are tokenized **before** `*` runs — a character
   inside backticks can never be a delimiter.
2. Consecutive `*` form an **atomic run**; a closer is never taken from the middle of a run.
   Run of 1 = `<i>`, run of 2 = `<b>`, run of 3 = composite `<b><i>`, run ≥ 4 = always literal.
3. An opener run matches the next flanking-valid closer run of **equal length**; no match by end
   of segment → the run and everything it would have wrapped stay literal (fail toward literal).
4. Single fused-close exception: while an outer `**` and an inner `*` are both open (either
   order), a flanking-valid run of 3 closes both, innermost first. No other run splitting.
5. Same-type exclusion: a run inside an open context of the same length is never an opener; the
   excluded markers render as literal text.

**Decision table — pin each as a named test row in Task 1:**

| input | output |
|---|---|
| `***both***` | `<b><i>both</i></b>` (composite run 3 both ends) |
| `**bold *italic***` | `<b>bold <i>italic</i></b>` (fused-close exception) |
| `*a **b** c*` | `<i>a <b>b</b> c</i>` (runs atomic — final `*` closes italic, never the middle of `**`) |
| `**a **b** c**` | `<b>a **b</b> c**` — accepted residual of the kwargs class, decided not accidental |
| `***x**`, `**x***`, `****x****` | literal (rules 3/4: no other splitting) |
| `use **kwargs and **args` | literal (second run is never a flanking-valid closer) |
| `x = a**b**c` | literal (opener preceded by alphanumeric) |
| ``**see `a**b` here**`` | ``<b>see <code>a**b</code> here</b>`` (rule 1: backticks tokenized first) |

**Nesting** (Telegram HTML allows it; the converter renders it recursively):

- code spans are terminal — content escaped, never processed for markdown, but a span may sit
  *inside* bold/italic/link text: ``**run `jcmd`**`` → ``<b>run <code>jcmd</code></b>``.
  ⚠️ this assumes Telegram accepts `<code>` *contained in* `b`/`i`/`a` (the docs only forbid
  entities *inside* `code`/`pre`) — verify against the live API early in Task 1; if rejected,
  emit the code span adjacent instead of nested (`</b><code>…</code><b>` splitting the wrapper)
- `**bold**` inner content recursively rendered for code spans, italic, links
- `*italic*` inner content recursively rendered for code spans, bold, links
- `[text](url)` — text recursively rendered for code/bold/italic; url stays literal
  (attribute-escaped only)

**Link boundaries**: link text runs to the first `]`, which must be immediately followed by `(`;
the url runs to the first `)`. Anything else — `]` deeper in the text, a url containing `)`
(the tail past the first `)` goes back to literal), unterminated `[a](b` — stays literal.

Skipped for v1 (YAGNI until seen in real replies): `_italic_`, blockquotes, strikethrough,
spoilers, tables.

Send flow in `sendReply`: length-check the **raw** text exactly as today (post-parse text is only
ever shorter than the raw markdown, so the check stays valid and conservative) → render → send
HTML → on `*telegram.APIError` with code 400, warn and retry once plain. If the plain retry
*also* fails, the retry's error is the one returned — it describes the raw send the caller asked
for; the HTML error lives in the warn log only. Exactly two calls, never more. A *successful*
fallback is invisible to the MCP client — log only, `sendReplyResult.Warning` stays reserved for
its existing store-failure meaning. The existing `slog.Info("reply sent", …)` keeps logging the
**raw** text; add `parse_mode` and `fallback` fields so a production parse rejection is
greppable — that is the only signal the Post-Completion manual verification has.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests, docs in this repo
- **Post-Completion** (no checkboxes): live verification in a real support group, future
  `sendRichMessage` note

## Implementation Steps

### Task 1: pkg/tghtml converter package

**Files:**
- Create: `pkg/tghtml/tghtml.go`
- Create: `pkg/tghtml/tghtml_test.go`

- [ ] block pass: line scanner handling fences (with/without language tag, invalid language token
      dropped, unclosed at EOF, empty fence), `# `-space heading lines, plain lines; escape
      `&` `<` `>`; CRLF input normalised
- [ ] write block-pass tests: escaping in text and inside fences, fences ± language, invalid
      language token, unclosed fence, empty fence, heading → bold, `#!/bin/sh` stays literal,
      plain text passes through untouched, CRLF
- [ ] run block-pass tests - must pass before inline pass
- [ ] inline pass: recursive renderer per the delimiter-run semantics and nesting rules above;
      href `&`/`"` escaping; link boundaries per spec; unmatched markers literal
- [ ] sanity-check against the live Bot API (any throwaway chat) that `<code>` nested inside
      `<b>`/`<i>`/`<a>` is accepted; if not, switch to the adjacent-emit fallback and update the
      nesting section here
- [ ] write inline-pass tests: **every row of the decision table as a named case**, plus
      precedence (code span beats bold; `**x**` not `<i>*x</i>*`), nesting
      (``**run `jcmd`**``, `[**docs**](url)`), flanking negatives (`file_unique_id` untouched —
      no `_` rule at all, `SELECT * FROM t`, `chmod 755 * && chown *`, `2 * 3 * 4`), unmatched
      markers, link boundaries (`)` in url, `]` in text, unterminated `[a](b`), links incl.
      query-string url (`?a=1&b=2`) and a `"` in url
- [ ] adversarial row: ~200 alternating `*[` `` ` `` characters renders (all literal) without
      hanging — catches accidental quadratic run-matching
- [ ] add the real screenshot reply (jcmd instructions with fence + inline code) as a test case
- [ ] run tests - must pass before task 2

### Task 2: parse-mode plumbing through client and interface

**Files:**
- Modify: `pkg/telegram/client.go`
- Modify: `pkg/telegram/client_test.go`
- Modify: `pkg/server/server.go`
- Modify: `pkg/server/tools.go` (call site only, passes `""`)
- Modify: `pkg/server/tools_test.go` (hand-written `SendMessageFunc` at `:497` and `:640`)
- Modify: `pkg/server/mocks/telegram_api.go` (regenerated)

- [ ] add `ParseModeHTML` const; `SendMessage(ctx, chatID, text, parseMode, replyTo, threadID)` —
      `parseMode` right after `text` — sets `parse_mode` in the payload only when non-empty;
      update the doc comment at `client.go:116` ("plain text" no longer true)
- [ ] update `telegramAPI` interface (`pkg/server/server.go:47`) and **all call sites first**
      (`tools.go` passes `""`, both `tools_test.go` mock funcs), *then*
      `go generate ./pkg/server` — moq type-checks the package, so it must compile before regen;
      behavior unchanged in this task
- [ ] write tests: payload carries `parse_mode:"HTML"` when set, omits the key when empty
      (httptest fake)
- [ ] run tests - must pass before task 3

### Task 3: render + fallback in sendReply

**Files:**
- Modify: `pkg/server/tools.go`
- Modify: `pkg/server/tools_test.go`

- [ ] `sendReply`: render trimmed text via `tghtml.Render`, send with `telegram.ParseModeHTML`
- [ ] fallback: on `*telegram.APIError` code 400 only, `slog.Warn` (server-side fields may name
      the chat id, returned error text must not) and retry once with raw text, no parse mode; add
      `parse_mode`/`fallback` fields to the "reply sent" log, raw text logged as today
- [ ] `echoAPI` (`tools_test.go:640`) mimics Telegram: response text is the *posted* text with
      tags stripped and entities unescaped, so `res.Message.Text` asserts the clean-text contract
- [ ] update tool `Description` and `Text` jsonschema tag: markdown subset is rendered (fences,
      inline code, bold, `*italic*`, links, `# ` headings become bold lines); underscores never
      italicise; tables/quotes are not rendered — README wording (Task 6) must match
- [ ] write tests: mock captures rendered HTML + mode; 400 on first call → second call has raw
      text and empty mode; both attempts fail → returned error is the *second* (plain) failure
      and exactly two calls were made; 403 → no second call, error propagates; a raw text just
      under 4096 whose HTML is well past it is still accepted
- [ ] run tests - must pass before task 4

### Task 4: e2e coverage

**Files:**
- Modify: `cmd/tg-mcp/e2e_test.go`

- [ ] `serveSend` mimics Telegram: records the posted `text` and `parse_mode`, but the returned
      Message carries entity-stripped plain text — a small tag strip plus `html.UnescapeString`
      (stdlib), not a parser
- [ ] extend the existing "send_reply posts and logs the answer" subtest with markdown text:
      assert the recorded payload is HTML with `parse_mode=HTML`, and the tool result / persisted
      history show plain text — existing `sentMessages()==1` and `Messages==4` counts stay intact.
      ⚠️ the scenario is sequential and later subtests search the persisted reply: the new text
      must keep the phrase "please attach the agent log" (`get_thread` asserts it) and must not
      contain "upgrade" or "connecting" (search-count assertions at `e2e_test.go:236-243`)
- [ ] run `make e2e` - must pass before task 5

### Task 5: Verify acceptance criteria

- [ ] a markdown reply renders as HTML on the wire; a plain reply still works; a 400 rejection
      degrades to plain instead of erroring; snake_case identifiers survive untouched
- [ ] no chat id appears in any error text returned to the MCP client
- [ ] run full suite: `make fmt && make lint && make test && make e2e`

### Task 6: [Final] Update documentation

- [ ] README.md: tool table row (`README.md:36`) and the plain-text bullet (`README.md:73`) now
      describe the rendered subset — keep wording in sync with the tool description (CLAUDE.md
      rule)
- [ ] CLAUDE.md: add `tghtml` to the Layout package list, and add a **design-constraints bullet**:
      replies render server-side to `parse_mode=HTML` (not MarkdownV2, not `entities`), the
      telegram client stays markdown-dumb, conversion rules fail toward literal text (valid-but-
      wrong HTML is uncatchable corruption), and any 400 on the HTML attempt falls back to one
      plain send (a 400 delivered nothing, so no double-post)
- [ ] completed plans (`docs/plans/completed/20260730-tg-mcp.md:204,:416`) still say "plain text,
      no parse_mode" — historical record, deliberately left stale
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- send a markdown-heavy reply through Claude into a test group; check rendering on mobile and
  desktop clients (code block monospace, links, bold); grep logs for `fallback` to confirm the
  HTML path held
- include a ``**bolded `command`**`` in that reply — confirms live Telegram accepts `<code>`
  nested inside `<b>` (mocks accept everything, only the real API can prove this)

**Future path (recorded, not planned):**
- Bot API 10.2 `sendRichMessage`: `InputRichMessage.media` for attaching screenshots/files with
  formatted captions (a future `send_file`-style tool), `InputRichBlockTable` if markdown tables
  start appearing in real replies. `tghtml` staying its own package keeps a rich-block renderer
  additive. Re-verify exact field names against the Bot API docs before building on them.
