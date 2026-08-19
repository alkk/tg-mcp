# tg-mcp

[![build](https://github.com/alkk/tg-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/alkk/tg-mcp/actions/workflows/ci.yml)

Telegram support gateway. A Bot API bot silently logs allowlisted customer groups into SQLite,
and an MCP server (streamable HTTP, bearer auth) lets Claude Code / Claude Desktop triage that
history — read new messages, reconstruct threads, search, fetch attachments, and reply as the bot.

Single Go binary, no CGO. Chat ids never leave the server: every tool speaks in **customer slugs**.

## What it does

- **ingest** — long-polls `getUpdates`, keeps `message` and `edited_message` from allowlisted
  chats only, drops everything else (with a log line naming the unknown chat id). Service
  messages — joins, leaves, pins, title changes: no text and no attachment — are dropped too,
  quietly at debug level. The update offset advances only after the batch is committed, so a
  crash redelivers instead of losing messages.
- **store** — SQLite in WAL mode, FTS5 full-text search, lazy on-disk cache for attachments.
- **serve** — MCP over streamable HTTP at `/mcp`, `/ping` for health checks, and an
  authenticated `/files/<id>` endpoint for attachments too large to inline.

Accepted Bot API constraints: no history backfill (logging starts when the bot joins),
deletions are never delivered, and the cloud API caps `getFile` at 20 MB — a self-hosted
`telegram-bot-api` raises that to 2 GB.

## Tools

| tool | params | returns |
|---|---|---|
| `list_customers` | — | customers, their group labels, unread counts |
| `list_new` | `customer?, limit?` | messages above the triage cursor, oldest first (limit 100) |
| `get_thread` | `customer, message_id, label?` | the reply chain plus everything said around it |
| `get_history` | `customer, from?, to?, limit?, label?, cursor?` | chronological bulk, defaults to the last 24h (limit 200) |
| `search` | `query, customer?, from?, to?, limit?` | FTS5 hits with snippets (limit 50) |
| `get_file` | `customer, message_id, label?` | image/text inline, larger files as a download URL |
| `send_reply` | `customer, text, reply_to?, label?` | renders a markdown subset, sends as the bot and logs the sent message |
| `mark_handled` | `customer, message_id, label?` | advances the triage cursor |

Every tool that addresses a message takes an optional `label`; when a customer has several
groups and nothing else pins one down, the error lists the available labels.

### Behaviour worth knowing

- **`list_new`** returns the oldest untriaged messages first, so the limit cuts the newest ones —
  a backlog is worked through front to back.
- **`limit`** on `list_new`, `get_history` and `search` is capped at 1000 whatever is asked for;
  the result is held in memory whole, so one call cannot pull the entire store into the heap.
  Page through a larger range instead.
- **triage cursor** — `mark_handled` marks the given message *and everything before it*. The
  cursor only ever moves forward, so marking an older message cannot resurface newer ones —
  `marked_up_to` reports where the cursor ended up, not what was asked for. The bot's own replies
  never count as unread.
- **`get_thread`** walks the `reply_to` chain up to its root, adds everything that replies to any
  member of that chain, then fills in every message sent between the first and last of them
  (±5 minutes) — answers typed without hitting "reply" belong to the thread too. In a forum
  group that fill stays inside the anchor's topic. Capped at 500 messages around the anchor, with
  `truncated: true` when it cut.
- **`get_history`** takes both bounds inclusive and keeps the newest of the range. A full page
  comes with a `next_cursor`; pass it back as `cursor` to read what came before it, with nothing
  repeated and nothing skipped. Timestamps alone could not do that — Telegram stamps whole
  seconds, so a page boundary inside one second would hand back the same messages forever. The
  token is opaque, keep the other parameters as they were. Without a bound or a cursor it returns
  the last 24h.
- **`search`** runs FTS5 first and falls back to `LIKE` on an error or an empty result, so
  substring matches (`nxagentd`) still land. Every term is quoted, so `error:` or `-v` is not
  parsed as query syntax; a term carrying punctuation is also checked literally, because
  tokenization would throw it away and answer a different question (`50%` matching every `50`).
  Only when nothing carries the punctuation literally does the loose match get a turn. Quote a
  phrase to keep it together. Edits reindex — the pre-edit text stops matching.
- **`get_file`** returns images and text inline under 1 MiB; anything larger or binary comes back
  as a `/files/<id>` url plus metadata. Files are downloaded from Telegram on first request and
  cached on disk afterwards.
- **`send_reply`** renders a markdown subset to Telegram HTML before sending: fenced and inline
  code, `**bold**`, `*italic*`, `[links](url)` whose target carries an `https://`, `http://`,
  `tg://`, `mailto:` or `tel:` scheme, and `# ` headings as bold lines. Underscores never
  italicize — `file_unique_id` stays as typed — a single `*` only italicizes when a word character
  sits on both sides of the pair, so `rm /tmp/*.log /var/*.log` and `f(*args)` survive intact.
  `**` and `***` do not open behind punctuation glued to a word either, so `f(**kwargs) and
  g(**args)` keeps its stars while `see (**bold**)` still emphasizes. Tables and block quotes are
  not rendered. Link text may be bold or italic but keeps the backticks of a code span: Telegram
  refuses to nest a code entity in a link and drops one of the two, which can take the url with it.
  Anything the converter cannot match confidently
  stays literal, and if Telegram cannot parse the formatting the message is resent once as plain
  text; that rescue is a log line, not a `warning`. Any other failure — and a failing plain retry
  — comes back as an error. The 4096-character limit applies to the text as written and is
  rejected instead of silently split. It inherits the forum topic of `reply_to`, and a `reply_to`
  that exists in exactly one of a customer's groups pins the group by itself. The sent message is
  written to the log immediately — if delivery succeeded but logging failed you get the reply plus
  a `warning` rather than an error, because an error would invite a double post. What is logged is
  the message Telegram built, so searches over the bot's own replies match the words, not the `**`
  and the backticks.
- **mentions** are flagged both for an `@bot` mention (in text or media caption) and for a reply
  to one of the bot's own messages.

## Configuration

Flags and environment variables are equivalent (`--telegram.token` == `TELEGRAM_TOKEN`).

| flag | env | default | purpose |
|---|---|---|---|
| `--telegram.token` | `TELEGRAM_TOKEN` | — | bot token (required) |
| `--telegram.api-url` | `TELEGRAM_API_URL` | `https://api.telegram.org` | Bot API base url |
| `--telegram.local` | `TELEGRAM_LOCAL` | `false` | API server runs with `--local`: `getFile` returns filesystem paths |
| `--auth-token` | `AUTH_TOKEN` | — | bearer token for `/mcp` and `/files/` (required) |
| `--listen` | `LISTEN` | `:8080` | http listen address |
| `--data` | `DATA_DIR` | `./data` | data directory (SQLite db + file cache) |
| `--chats` | `CHATS_FILE` | `chats.yml` | chat map file |
| `--dbg` | `DEBUG` | `false` | debug logging |

`--telegram.local` is explicit rather than probed: a missing shared volume must fail loudly
instead of falling through to a confusing 404.

### Chat map

The allowlist works both directions — ingest filters on it, and `send_reply` resolves its
target through it. See `chats.example.yml`:

```yaml
chats:
  -1001234567890:
    customer: acme
  -1009876543210:
    customer: globex
    label: main          # required once a customer has more than one chat
  -1005555555555:
    customer: globex
    label: escalations
```

Customer slugs must be non-empty and labels unique within a customer. An empty file is valid —
useful while collecting chat ids from the drop log. Changes need a restart.

## Running

### Docker

```
cp chats.example.yml chats.yml   # edit it
docker compose up -d
```

Multi-arch (amd64/arm64) images are published to `ghcr.io/alkk/tg-mcp`: `latest` and `vX.Y.Z`
follow releases, `main` tracks the main branch. `docker compose build` builds locally instead.

`docker-compose.yml` mounts `chats.yml` read-only, keeps the data dir on a named volume, and
health-checks `/ping`. It takes `TELEGRAM_TOKEN` and `AUTH_TOKEN` from the environment (or an
`.env` file beside it) and refuses to start without them.

TLS is the reverse proxy's job — see [Behind a reverse proxy](#behind-a-reverse-proxy).

For `--telegram.local` mode, the volume the `telegram-bot-api` server writes to must be mounted
into this container at the same path it reports in `file_path`.

### Behind a reverse proxy

`get_file` builds its download urls from the request the MCP call arrived on, honoring
`X-Forwarded-Host`, `X-Forwarded-Proto` and `X-Forwarded-Prefix`, so there is no public-url
setting to keep in sync with the proxy. Forward all three and the links come out right; forward
none and they are built from the `Host` header of the incoming request over plain `http://` —
which points at the container as soon as the proxy rewrites `Host`, and stays `http://` once TLS
terminates in front. Every header below is load-bearing, not decoration.

Whole host, endpoints at their own paths:

```nginx
server {
    listen 443 ssl;
    server_name tg.example.com;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-Host  $host;
        proxy_set_header X-Forwarded-Proto $scheme;

        # streamable http keeps the response open
        proxy_buffering off;
        proxy_read_timeout 1h;
    }
}
```

Client url: `https://tg.example.com/mcp`.

Mounted under a prefix, sharing a host with other services — `X-Forwarded-Prefix` is what keeps
the download links inside the mount:

```nginx
location /tg-mcp/ {
    proxy_pass http://127.0.0.1:8080/;
    proxy_http_version 1.1;
    proxy_set_header Host               $host;
    proxy_set_header X-Forwarded-Host   $host;
    proxy_set_header X-Forwarded-Proto  $scheme;
    proxy_set_header X-Forwarded-Prefix /tg-mcp;

    proxy_buffering off;
    proxy_read_timeout 1h;
}
```

The trailing slash on `proxy_pass` strips the prefix, so `/tg-mcp/mcp` reaches `/mcp` and
`/tg-mcp/files/<id>` reaches `/files/<id>`; `X-Forwarded-Prefix` puts it back on the way out, so
`get_file` returns `https://tg.example.com/tg-mcp/files/<id>`. Client url:
`https://tg.example.com/tg-mcp/mcp`. `/ping` follows the prefix too
(`https://tg.example.com/tg-mcp/ping`) — it stays unauthenticated, so keep it off the public
listener if that matters.

Both the MCP endpoint and `/files/` sit behind the bearer token; the proxy does not need to add
anything, but it must pass the `Authorization` header through untouched. A prefix that is not a
plain absolute path (a full url, a query string) is ignored rather than trusted — links then fall
back to the host root.

Traefik and Caddy set `X-Forwarded-Host` / `-Proto` on their own. For a prefixed mount, Traefik's
`stripPrefix` middleware adds `X-Forwarded-Prefix` as well; in Caddy, `handle_path` strips the
prefix but the header is yours to add (`header_up X-Forwarded-Prefix /tg-mcp`).

### From source

```
make build
.bin/tg-mcp --telegram.token=... --auth-token=... --chats=chats.yml --data=./data
```

### Restarts

No update offset is stored on disk, by design. Telegram keeps unconfirmed updates for about 24
hours, so a restart re-receives whatever was not committed and the idempotent upserts absorb the
duplicates. Downtime longer than that loses the messages in between — there is no backfill.

The one deliberate loss path: if a batch still fails to store after three redeliveries, ingest
retries it message by message and drops the ones the database itself rejects (a constraint
violation, a type mismatch, an oversized value), so a single bad update cannot stall every chat.
Each drop is logged at error level with the full raw update — grep for
`update skipped after repeated store failures`.

Nothing is dropped when the database as a whole is unusable — a full disk, a read-only mount, an
I/O error. Those keep the offset pinned and retry indefinitely (logged as `store batch failed`),
so the messages stay in Telegram's queue until the store recovers.

Only one consumer may poll a bot token at a time: a second instance, or a leftover webhook, makes
Telegram answer 409 Conflict. tg-mcp calls `deleteWebhook` at startup and fails fast on a 409
instead of spinning.

## Bot setup

1. Create the bot with [@BotFather](https://t.me/BotFather).
2. Disable privacy mode (`/setprivacy` → Disable) — otherwise the bot only sees messages that
   mention it. Making the bot a group admin has the same effect.
3. Add the bot to each customer group.
4. Start `tg-mcp` with an empty chat map and watch the log for the dropped-message lines naming
   the unknown chat ids; put those ids in `chats.yml` and restart.

Pointing the bot at a self-hosted `telegram-bot-api` requires calling `logOut` on the cloud API
for that token first, otherwise the local server refuses to take over the bot.

If a basic group is upgraded to a supergroup its chat id changes. tg-mcp logs the migration
prominently with the old and new id — update `chats.yml` and restart.

## Client registration

Claude Code:

```
claude mcp add --transport http tg-mcp https://<host>/mcp \
  --header "Authorization: Bearer <auth-token>"
```

Claude Desktop has no native HTTP transport, so bridge it through `mcp-remote` in
`claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "tg-mcp": {
      "command": "npx",
      "args": [
        "-y", "mcp-remote", "https://<host>/mcp",
        "--header", "Authorization: Bearer <auth-token>"
      ]
    }
  }
}
```

Attachments served as urls are plain authenticated GETs:
`curl -H "Authorization: Bearer <auth-token>" https://<host>/files/<id> -O`.

## Backups

Everything is in the data dir: `tg-mcp.db` (plus its WAL sidecars) and `files/`. There is no
history backfill from Telegram, so a lost database is lost for good — copy the volume nightly.
SQLite is in WAL mode: use `sqlite3 tg-mcp.db ".backup out.db"` or stop the container before a
plain file copy.

## Development

```
make test      # go test -race with coverage, mocks stripped
make e2e       # end-to-end smoke test against a fake bot api
make lint      # golangci-lint
make fmt       # gofmt -s + goimports
make build     # .bin/tg-mcp with revision injected
```

Needs `golangci-lint`, `goimports` and `moq` on PATH. Design notes and the implementation plan
live in `docs/plans/`.

## License

MIT — see [LICENSE](LICENSE).
