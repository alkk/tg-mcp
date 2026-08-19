# Flat content-addressed attachment cache

## Overview

The attachment cache stores each file at `files/<sanitized-file_unique_id>/<sanitized-name>` — a
directory per attachment, with telegram's file name as the file inside it. That layout makes the
filesystem hold metadata, and it costs correctness three ways.

**The filesystem is the name store.** `Cached` returns *the first regular entry it finds* in the
directory. `file_unique_id` identifies a file, not a message, and the file name lives on the
message — so the same bytes resent under a different name yield the same id and a **second** file
in the same directory, after which which name `get_file` reports comes down to `ReadDir`
ordering.

**The case-fold hazard.** `file_unique_id` is base64url and case-significant, but APFS folds case,
so `AgADuQ` and `agaduq` share a directory on macOS. Combined with the first-entry rule, a
`/files/` GET can hand back a different customer's attachment. Latent, not live: a collision needs
two genuine ids equal under case folding, and `make test` passes today.

**`sanitize` serves two callers with one rule.** It guards both the directory (an id, from the
`/files/` URL) and the file (an arbitrary telegram name), so it carries traversal defence,
dot-stripping and a `Cached`-imposed no-leading-dot rule at once. The dot-stripping is why `.env`
reaches the customer as `env`.

Storing the bytes flat at `files/<hex(file_unique_id)>` and taking the display name from the
message row deletes all three. Traversal becomes unrepresentable rather than checked, the mapping
stays injective under case folding, `Cached` becomes an exact-path `os.Lstat`, and the entire
name-sanitizing half of the code disappears.

## Context (from discovery)

Files and components involved:

- `pkg/store/files.go` — `CachePath`, `Cached`, `SaveFile`, `sanitize`; the layout lives here
- `pkg/store/files_test.go` — table-driven tests against real temp dirs via `testStore(t)`
- `pkg/server/files.go` — `cachedFile`, `getFile`, `serveFile`; all three read the name off the path
- `pkg/server/server.go:41` — the `messageStore` interface, which carries `SaveFile`'s signature
- `pkg/server/mocks/message_store.go` — moq-generated, regenerates from the interface
- `pkg/server/files_test.go` — runs against a real `store.New(t.TempDir())`, not a mock
- `CLAUDE.md` — "Design constraints", where the layout decision belongs

Related patterns found:

- `Cached` is reached from two directions: `cachedFile` (`pkg/server/files.go:94`) on the tool
  path, and `serveFile` (`:233`) with `r.PathValue("id")` off the `GET /files/{id}` route
  registered at `pkg/server/server.go:116`
- the store owns its on-disk layout; nothing outside `pkg/store` enumerates `files/`
- `CachePath` is exported but has **no caller outside `pkg/store`** — only `SaveFile` and
  `files_test.go`, which is `package store`, so it can be unexported for free
- every message-listing tool already returns `FileName` (`pkg/server/tools.go:560`), so the
  harness knows the name before `get_file` is ever called

Dependencies identified:

- `encoding/hex` (stdlib, new import in `pkg/store/files.go`)
- `strconv` drops from `pkg/server/files.go`; `path/filepath` stays, still needed for `Ext` in
  `inlineContent`
- no new dependency, no schema change, no migration machinery

## Development Approach

- **testing approach**: TDD — the case-fold regression fails on the current code, so it goes first
  for a real red→green signal that the hazard is fixed rather than refactored around
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** — with exactly one exception, the
  deliberate red baseline in Task 1, which is required to fail at that task's end
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change

## Testing Strategy

- **unit tests**: `pkg/store/files_test.go` and `pkg/server/files_test.go`, table-driven with
  testify, `t.TempDir()` via the existing helpers
- **e2e tests**: `cmd/tg-mcp/e2e_test.go` passes **unchanged** — verified by implementing the
  whole change in a scratch copy and running `go test -tags=e2e ./cmd/...`. `:268` reads
  `core.dump` from the row seeded at `:510`, which is exactly what the new code reports, and the
  download subtest at `:274` asserts only body and status, never a header.
- **the load-bearing test** is the case-fold regression, and it must compare *bytes* through
  `Cached`, not paths through `CachePath`: `filepath.Join` is pure string work, so path
  comparison passes on any filesystem. Only writing both files and reading one back exposes it.
  Verified red on current code: `id "Ab" served the wrong bytes — expected "payload-Ab", actual
  "payload-aB"`.
- **that test is vacuous on CI.** `.github/workflows/ci.yml:12` runs `ubuntu-latest`, i.e. ext4,
  where it passes whether or not `fileKey` exists. It stays as the macOS reproduction, and a
  portable companion carries the invariant everywhere:
  `assert.False(t, strings.EqualFold(fileKey("Ab"), fileKey("aB")))` — green for hex (`4162` vs
  `6142`), red for any readable-key scheme a later change might revert to.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

**On disk**

```
files/<hex(file_unique_id)>     the bytes, one file, no extension
files/.tmp/partNNNN             half-written downloads
```

`Cached` becomes `os.Lstat` on the exact path with an `IsRegular` check — no `ReadDir`, no
first-entry heuristic, no dot filter, and no way for two names to occupy one slot.

`fileKey(id) = hex.EncodeToString([]byte(id))`. The output alphabet is `[0-9a-f]`, so a separator
or a dot is not merely rejected but inexpressible, and the mapping stays injective on
case-insensitive filesystems. The cost is that `ls files/` needs `xxd -r -p` to map an entry back
to an id — accepted, because the readable alternative (a base64url whitelist filter) leaves the
case hazard standing.

`sanitize` is deleted outright. No replacement: no telegram-supplied name reaches the filesystem
any more.

**Keeping `files/.tmp`** is optional now that lookups are exact — temps are invisible wherever
they sit. It stays because it preserves the invariant *everything directly in `files/` is a
complete cached attachment*, so a later GC or `ls`-based tool needs no prefix rule. That is the
rule this change is deleting; reintroducing it for temps would be a poor trade.

**Where the name comes from**

`get_file` reports `msg.FileName` — the store already has it, and `Media()` always synthesizes
one (`pkg/telegram/types.go:222-244`). Fallback is `msg.FileUniqueID`, not telegram's `FilePath`.

That fallback change matters: under flat storage the `FilePath` fallback becomes
**cache-dependent**, because `cachedFile` only calls `GetFile` on a miss. The same message would
report `f1` on first access and something else after. A deterministic fallback beats a prettier
nondeterministic one.

`serveFile` keeps `Content-Disposition: attachment` and `X-Content-Type-Options: nosniff` — those
are what stop an `.html` attachment rendering on this origin — but **drops the `filename=`
parameter**. The consumer is a harness that already has the name twice: from every listing tool
(`tools.go:560`) and from `fileResult.FileName` alongside the URL. The documented human flow,
`curl ... -O` (`README.md:288`), names files from the URL path and ignores the header entirely,
so `filename=` served browsers and `curl -OJ` and nothing else. If pretty curl downloads ever
matter, `/files/{id}/{name}` is a purely additive route change later.

### Declined: a `file_unique_id` lookup in the store

The obvious alternative for `serveFile` is an index on `messages(file_unique_id)` plus a narrow
name query. Declined because it is ambiguous by construction: one id can appear on many rows with
different names, since names belong to messages and not to files. Worse, two customers sending
byte-identical files share a cache entry, so `ORDER BY id DESC LIMIT 1` can hand customer A the
filename customer B chose — per-customer metadata crossing over, which is the class of thing the
chat-ids-never-leave-the-server constraint exists to prevent. It would also add an index and a
query to serve a consumer that already has the name.

## Technical Details

### `pkg/store/files.go`

```go
const (
	filesDir = "files"
	// tempDir collects half-written downloads; a hex key never starts with a dot, so it
	// cannot collide with a cached attachment.
	tempDir = ".tmp"
)

// fileKey encodes a telegram file_unique_id into one path element. Hex cannot express a
// separator or a dot, so no traversal is representable and none has to be checked for, and the
// mapping stays injective on case-insensitive filesystems, where two base64url ids differing
// only in case would otherwise collide and serve each other's bytes.
func fileKey(id string) string { return hex.EncodeToString([]byte(id)) }
```

`cachePath` (unexported, name parameter gone):

```go
func (s *Store) cachePath(fileUniqueID string) (string, error) {
	if fileUniqueID == "" {
		return "", errors.New("empty file id")
	}
	dir := filepath.Join(s.dir, filesDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	return filepath.Join(dir, fileKey(fileUniqueID)), nil
}
```

`errors.New`, not `fmt.Errorf` with `%w` — there is no inner error to wrap. The empty-id guard is
defensive: `GET /files/` 404s at the mux (`{id}` does not match an empty segment) and every media
branch in `pkg/telegram/types.go` carries a unique id. It is one line and testable, so it stays.

`Cached` and `SaveFile`:

```go
func (s *Store) Cached(fileUniqueID string) (path string, ok bool) {
	if fileUniqueID == "" {
		return "", false
	}
	path = filepath.Join(s.dir, filesDir, fileKey(fileUniqueID))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return path, true
}

func (s *Store) SaveFile(fileUniqueID string, write func(w io.Writer) error) (path string, err error)
```

The `IsRegular` check is what makes a leftover directory from the old layout a cache miss rather
than an error.

`os.Lstat`, not `os.Stat`, and deliberately: `Stat` follows symlinks, so a symlink planted at
`files/<hex>` pointing outside the data dir would be a cache **hit**, where the old `ReadDir` +
`e.Type().IsRegular()` used Lstat semantics and treated it as a miss. That needs write access to
`DATA_DIR` so it is not a live risk, but `Stat` would quietly narrow this change's central claim
— that the key makes escape unrepresentable rather than checked — for one saved character.

### `pkg/server`

| site | before | after |
| --- | --- | --- |
| `messageStore` (`server.go:41`) | `SaveFile(id, name string, …)` | `SaveFile(id string, …)` |
| `cachedFile` (`files.go:105-108`) | derives `name`, passes it to `SaveFile` | block deleted |
| `getFile` (`files.go:74`) | `FileName: filepath.Base(path)` | `FileName: msg.FileName`, falling back to `msg.FileUniqueID` |
| `serveFile` (`files.go:252-257`) | `filename=` + name to `ServeContent` | bare `attachment`, `""` to `ServeContent` |

Passing `""` to `ServeContent` **does** change the response Content-Type, and the plan should not
pretend otherwise. Today `serveFile` hands it the *file name* (`server.log`), and
`net/http/fs.go` does `mime.TypeByExtension(filepath.Ext(name))`, sniffing only when that comes
back empty — so Content-Type is currently extension-derived and becomes sniffed.

Accepted, because the consumer never reads it: `attachment` plus `nosniff` means the bytes are
saved either way, and `fileResult.MimeType` — the value the harness actually sees — is still
derived from `msg.FileName`'s extension in `inlineContent`. The visible cost is that a `.csv`
sniffs to `text/plain` and `.docx`/`.xlsx` to `application/zip` on the HTTP response only.

### Accepted consequences

- **Cache invalidation is total,** and it degrades safely — old `files/<id>/<name>` entries are
  *directories* where the new code expects files, so the `IsRegular` check makes them a miss and a
  re-download. `rm -rf $DATA_DIR/files/` on deploy is cleaner: it clears the dead weight and moots
  a theoretical collision where an old readable id that happened to be all-lowercase hex equalled
  a new key, which would make `SaveFile`'s rename fail with `EISDIR`.
- **Orphaned temps** in `files/.tmp` are not swept at startup. They leaked before too, inside the
  attachment directory where the dot rule hid them. Decided: no sweep, no cleanup code.
- **The cache is shared across customers by content.** Two customers sending identical bytes share
  one entry. That is inherent to content addressing and was already true; what changes is that no
  per-customer *name* is stored beside it any more, which removes the sideways-leak question the
  DB-lookup alternative would have introduced.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): the store rewrite, the server consumer update,
  their tests, and the CLAUDE.md bullet.
- **Post-Completion** (no checkboxes): the deploy-time cache wipe and the re-download burst.

## Implementation Steps

### Task 0 (prerequisite, not part of this change): land the in-flight work

The working tree carried an unrelated `X-Forwarded-Prefix` / `forwardedPrefix` change in
`pkg/server/files.go`, `files_test.go`, `CLAUDE.md` and `README.md`. Committing the cache work on
top would have mixed two changes in one commit.

- [x] commit or stash the in-flight prefix work before starting Task 1 — landed as `f13b484`
      (`make test` / `make lint` / `make e2e` all green beforehand). The tree is now clean apart
      from this plan file and an untracked `.revmux/`.

### Task 1: Case-fold regression test (red)

**Files:**
- Modify: `pkg/store/files_test.go`

- [ ] add `TestStore_CachedCaseFold`: write distinct content for ids `"Ab"` and `"aB"`, then
      assert `Cached("Ab")` reads back the `Ab` bytes and `Cached("aB")` the `aB` bytes — compare
      **bytes**, never paths. Comment that it reproduces only on a case-insensitive filesystem.
- [ ] write it against the *current* `CachePath(id, name)` signature; Task 3 re-points the call
      sites when the signature changes. Note that in the test so the churn is expected.
- [ ] run `go test ./pkg/store/ -run TestStore_CachedCaseFold -count=1` — MUST fail on this
      machine; record the output as the red baseline. Expect
      `id "Ab" served the wrong bytes — expected "payload-Ab", actual "payload-aB"`.
- [ ] confirm nothing else regressed: `go test ./pkg/store/ -count=1` fails only on the new test

### Task 2: Take the display name off the path (server only)

This goes **before** the store change, not after. The seam works because `getFile` can read
`msg.FileName` against the *old* store: the existing tests already seed rows whose `FileName`
matches what they assert, so nothing about the on-disk layout is involved. Verified end to end —
this intermediate state builds, passes all six packages, passes e2e, and lints clean.

**Files:**
- Modify: `pkg/server/files.go`, `pkg/server/files_test.go`

- [ ] `getFile`: `FileName: msg.FileName` falling back to `msg.FileUniqueID`, not
      `filepath.Base(path)` and not telegram's `FilePath`
- [ ] `serveFile`: bare `Content-Disposition: attachment`, `""` to `ServeContent`; keep `nosniff`;
      drop the now-unused `strconv` import
- [ ] update `pkg/server/files_test.go:264`: `Content-Disposition` is now bare `attachment` with
      no `filename=`
- [ ] update `TestFileNameFallback` (`files_test.go:430`) to expect `"u7"`, the unique id, and
      reword its comment away from "telegram file path is the last resort"
- [ ] run `go build ./... && make test && make e2e && make lint` — **everything** green, including
      `pkg/store`, which this task does not touch

### Task 3: Flip the store to the flat layout

The store change, the interface narrowing and the temp-dir move land together. Not for the reason
an earlier draft of this plan gave — a server-first seam does exist and is Task 2 — but because
`TestStore_SaveFile`'s two error-path subtests break the moment the layout changes and can only
be rewritten into their *final* form once temps live in `files/.tmp`. Splitting them would mean
writing the same two tests twice.

**Files:**
- Modify: `pkg/store/files.go`, `pkg/store/files_test.go`
- Modify: `pkg/server/files.go`, `pkg/server/server.go`
- Modify: `pkg/server/mocks/message_store.go` (regenerated)

- [ ] add `fileKey`; delete `sanitize`; unexport `CachePath` to `cachePath` and drop its `name`
      parameter, adding the empty-id `errors.New("empty file id")`
- [ ] rewrite `Cached` as exact-path `os.Lstat` + `IsRegular`, with its own empty-id guard
- [ ] drop `name` from `SaveFile`; add the `tempDir = ".tmp"` const and drop `tempPrefix`; create
      `files/.tmp` with `if err = os.MkdirAll(tmpDir, 0o750); err != nil` — **`=`, not `:=`**:
      `SaveFile` has a named return `err`, and `:=` trips `govet: shadow`
      (`.golangci.yml:77-79`), which would not surface until Task 4's `make lint`
- [ ] `os.CreateTemp(tmpDir, "part")`; keep the deferred `Close`+`Remove`
- [ ] narrow `SaveFile` in the `messageStore` interface (`server.go:41`) and regenerate the mock
      with moq
- [ ] delete the `name` derivation block in `cachedFile` (`files.go:105-108`); log the same
      `FileName`/`FileUniqueID` fallback `getFile` uses, so the line is not `file=""` for a row
      with no stored name
- [ ] re-point the Task 1 test at the new signatures; it MUST now pass — that is the proof
- [ ] add `TestFileKey` (not `Test_fileKey` — no test in the repo uses that prefix): the hex
      mapping (`hex("AgADuQ") == "416741447551"`, verified) as literal expectations, plus
      `assert.False(t, strings.EqualFold(fileKey("Ab"), fileKey("aB")))` so Linux CI guards the
      invariant too. Needs a new `strings` import in the test file.
- [ ] rewrite the `TestStore_CachePath` table to be keyed on **id alone**: every `want` becomes a
      hex string (`plain`: `AgADuQ/server.log` → `416741447551`), the `backslashes stripped` /
      `empty name` / `dot name` rows all key on `"x"` and collapse into one, every *name* row goes
      (`../../etc/passwd`, `c:\tmp\a.txt`, `""`, `"."`, `".."`, `". .."`), and the
      `empty unique id` row flips from a path expectation to an error assertion
- [ ] repoint `TestStore_CachePath/"undreadable cache root"` (`files_test.go:44-52`) — it plants a
      regular file at `files/uid`, which no longer blocks anything; plant at `$dir/files` itself
      and assert both `cachePath("uid")` and `SaveFile("uid", …)` fail with `create cache dir`.
      This is the only test covering that branch, so dropping it would regress the coverage Task 4
      checks.
- [ ] rename `TestStore_Cached/"sanitized id matches the one used for writing"`
      (`files_test.go:106`) — it still passes, but the name is the reason Task 4's
      `grep -rn 'sanitize' pkg/store/` gate would fail on this plan's own output
- [ ] fix `TestStore_SaveFile/"failed write leaves no cache hit"` (`files_test.go:147`) — it reads
      `files/uid`, which no longer exists; assert instead that `Cached` misses and `files/.tmp`
      holds no leftover
- [ ] replace `TestStore_SaveFile/"unusable cache directory"` (`files_test.go:169`), which plants
      `files/uid` and now succeeds silently, with the new failure branch: plant a regular file at
      `files/.tmp` and assert `create temp dir` errors
- [ ] delete `TestStore_Cached/"skips subdirectories"` — it still *passes* after the change while
      testing nothing, so no test run would catch it; and reword
      `TestStore_Cached/"miss on empty directory"` (`:73`), whose comment now describes `files/`
      rather than a per-attachment directory
- [ ] add a leftover-directory test: an old-layout directory at the new key's path is a `Cached`
      miss, not an error
- [ ] add empty-id tests: `cachePath("")` errors, `Cached("")` is false
- [ ] add a test that a successful `SaveFile` leaves `files/.tmp` empty
- [ ] run `go test ./pkg/store/ ./pkg/server/ -count=1` — both packages green before Task 4

### Task 4: Verify acceptance criteria

- [ ] verify all requirements from Overview are implemented
- [ ] verify edge cases: empty id, leftover directory at a key path, case-differing ids, a message
      row with no `file_name`, a resent file that previously produced two names in one directory
- [ ] confirm the encoding never leaked out of the store:
      `grep -rn 'fileKey\|encoding/hex' pkg/ cmd/ --exclude-dir=store` returns nothing
- [ ] confirm `sanitize` and `cacheName` are gone: `grep -rn 'sanitize' pkg/store/` returns
      nothing — this passes only if Task 3 renamed the `"sanitized id…"` subtest
- [ ] run full test suite: `make test`
- [ ] run e2e: `make e2e` — verified to pass unchanged; a failure at `:268` means the `FileName`
      source is wired wrong, not that the test needs updating
- [ ] run `make lint` and `make fmt`
- [ ] verify coverage on `pkg/store` and `pkg/server` has not regressed

### Task 5: [Final] Update documentation

**Files:**
- Modify: `CLAUDE.md`

- [ ] add one bullet to "Design constraints": attachments are stored flat and content-addressed at
      `files/<hex(file_unique_id)>`, never under their telegram name — the id keys *bytes*, not
      names, so a directory-per-id layout let a resent file put two names in one slot and left
      `Cached` picking by `ReadDir` order; hex also keeps the key injective under case folding,
      which a readable base64url key is not on APFS
- [ ] note in the same bullet that the download name comes from the message row, and that
      `/files/` deliberately sends no `filename=`: the consumer is a harness that already has the
      name from every listing tool, and a name lookup by `file_unique_id` would be ambiguous
      (many messages, one id) and could cross customers
- [ ] `README.md:288` documents `curl … /files/<id> -O`, which now writes a file named after the
      id; add a clause pointing at `file_name` in the `get_file` result
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual verification:**

- `rm -rf $DATA_DIR/files/` on deploy — the layout changed completely, nothing under the old one
  is readable by the new code, and clearing it avoids a re-download racing a stale directory
- expect a one-off burst of `attachment cached` log lines as previously cached attachments are
  re-fetched; confirm it does not recur on a second `get_file` for the same message

**External system updates:**

- none — no config, no flags, no schema change, no tool-schema change. `get_file`'s result keeps
  the same fields; only the value of `file_name` in the no-stored-name case changes, and only from
  a telegram-internal path fragment to the unique id.

**Related, out of scope:**

- [alkk/tg-mcp#1](https://github.com/alkk/tg-mcp/issues/1) — `media_group_id` is not captured, so
  albums arrive as N ungrouped messages
- stale `file_id`: telegram only guarantees a `file_id` for the receiving bot and it can change,
  so `get_file` on an old message can fail with a raw telegram error and no re-resolution path
  (`file_unique_id` cannot be traded back for a fresh `file_id`). No ticket filed — no clean fix
  exists short of a better error message.
