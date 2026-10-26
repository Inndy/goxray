# goxray

A **fail-closed recording proxy** for Claude Code — a ground-up Go rewrite of
[ccxray](https://github.com/lis186/ccxray)'s recording core, built for
security-audit compliance.

goxray sits between Claude Code and the Anthropic API, records every
request/response verbatim, and refuses to let a request succeed if it cannot be
durably recorded first.

## Why a rewrite

Three ccxray (Node.js) weaknesses this fixes:

1. **CPU never stalls IO.** Node's single event loop lets a big JSON/SSE parse
   block all proxying. Go runs parsing/compression on worker goroutines across
   `GOMAXPROCS` threads; one request never head-of-line-blocks another.
2. **Startup is O(1).** ccxray reloads all history into RAM at boot. goxray
   never does — the SQLite index is queried on demand; the hot path only appends.
3. **Fail-closed, not best-effort.** If the request can't be durably recorded,
   goxray returns 502 and never forwards it. If the response can't be recorded,
   the stream is aborted before the client sees a clean finish.

## Usage

```bash
make build          # builds the dashboard bundle, then the binary
# or, without a Node toolchain (the bundle is committed):
go build -o goxray ./cmd/goxray

# run the proxy (default command)
./goxray --port 5599

# ...with the read-only dashboard on a second port
./goxray --port 5599 --ui-port 5600      # → http://localhost:5600

# point Claude Code at it
export ANTHROPIC_BASE_URL=http://localhost:5599
claude

# query recorded exchanges
./goxray ls                     # recent turns (newest first)
./goxray ls --session <id>      # filter by session
./goxray ls --noise             # include count_tokens/quota probes
./goxray show <id>              # full parsed metadata (manifest)
./goxray raw  <id> [req|res]    # verbatim recorded bytes
```

Config (flags or env): `--host`/`PROXY_HOST` (default `127.0.0.1`),
`--port`/`PROXY_PORT` (default 5599), `--ui-port`/`GOXRAY_UI_PORT` (default 0 =
dashboard off), `GOXRAY_HOME` (default `~/.goxray`), `--max-body-mb` (default
50), `--upstream`/`GOXRAY_UPSTREAM`. `--host` moves only the proxy; the
dashboard always binds `127.0.0.1`. `GET /healthz` for liveness.

## Dashboard

`--ui-port N` starts a second, read-only HTTP server beside the proxy. Sessions →
turns → detail, with new turns streaming in live over SSE.

Per turn: the **recording state** (`complete` / `inflight` / `client_aborted` /
`upstream_error` / `persist_failed` / `timeout`), the cache/input composition of
its context, the idle gap since the previous turn (the cheapest cache-miss
diagnosis there is), and four detail tabs:

| Tab | Shows |
|---|---|
| **Timeline** | `messages[]` flattened into steps — each `tool_use` joined to its `tool_result`, parallel calls grouped, thinking attached, injected context separated from text a human actually typed, `role:"system"` messages surfaced |
| **System** | the system prompt split into named sections, each tagged `anthropic` (shipped by Claude Code) or `user` (contributed by your machine and config) |
| **Tools** | core and MCP definitions with per-turn call counts, and what share of context they cost |
| **Raw** | headers, request options, SSE events, and links to the verbatim recorded bytes |

Everything the dashboard shows besides the raw bytes is *derived*: token counts
are a ~4-chars-per-token estimate that runs below the API's real figure, and the
system-prompt split is a heuristic over prose Anthropic can reshape. The verbatim
blob is always one click away.

**It is off by default and must stay on loopback.** The store holds every
conversation in full, so the dashboard is opt-in per run and its handlers reach
the store through a read-only interface that cannot write blobs, manifests, or
the index. Requests are rejected unless the `Host` header is loopback on the
dashboard's own port — that check, not the loopback bind, is what defeats DNS
rebinding, since a rebound request still carries the attacker's domain in `Host`.
`Origin` must also be loopback, no CORS headers are ever sent, and recorded
bodies are served as `text/plain` unless they are JSON or SSE so they can never
be interpreted as markup. None of this is authentication: exposing the port off
loopback would need real auth first.

Frontend development: `make ui-dev` runs vite on :5173 proxying `/api` to :5600.
`make ui` writes the production bundle into `internal/web/dist`, which is
committed and embedded.

## How it records (fail-closed)

```
request  → buffer → PersistRequest (blob + inflight manifest, fsync)
             │           └─ on failure: 502, DO NOT forward
             ▼
          forward upstream (accept-encoding: identity → plaintext capture)
             ▼
response → stream live, tee every byte, HOLD BACK terminal SSE frames
             ▼
          PersistResponse (res blob + final manifest, fsync)  ← commit point
             │  success → release held terminal frames, clean close
             │  failure → abort connection (no clean EOF)
             ▼
          async → SQLite Upsert (rebuildable projection)
```

Every request that reaches the API is represented on disk as exactly one state:
`inflight → complete | client_aborted | upstream_error | timeout | persist_failed`.

## On-disk layout (`$GOXRAY_HOME`)

```
blobs/<shard>/<uuid>.zst        # raw request/response bytes, zstd (verbatim)
manifests/<shard>/<id>.json     # immutable audit record — the SOURCE OF TRUTH
index.db                        # SQLite (WAL) — a rebuildable projection
```

- **Manifests are authoritative.** Drop `index.db` and it rebuilds from
  manifests (free schema migrations, no in-memory restore). After an unclean
  shutdown, startup reconciles any manifests missing from the index.
- **Raw is verbatim**, so an Anthropic API shape change can only null a parsed
  metadata field (`parse_ok=0`) — never lose data.
- **Credentials are never persisted.** The auth token lives only in a request
  header, which goxray forwards upstream but does not capture and does not derive
  any stored identity from. (Anthropic's stable `anthropic-organization-id` *is*
  captured — in `res_headers` — for audit correlation; it's not a secret.)
- **Source slug** (`source`): a caller-declared origin label taken from a leading
  URL path segment — see below. Explicit and trusted, never inferred.
- **Noise** (`is_noise=1`, hidden from `ls` by default): `/v1/messages/count_tokens`,
  quota probes, and Claude Code's `/api/hello` connectivity preflight. Recorded,
  not dropped.
- `capture_ver` is stamped in every manifest for future format migration.

## Source slug — separating traffic

Put a slug in the base URL and goxray strips it, tags every exchange with that
`source`, and forwards the real path upstream:

```bash
export ANTHROPIC_BASE_URL=http://localhost:5599/proj-alpha
# → goxray records source="proj-alpha", forwards /v1/messages to Anthropic

./goxray ls --source proj-alpha    # filter by source
```

The rule: a leading path segment that isn't a known Anthropic root (`v1`, `api`)
is treated as the slug. Useful for one user separating projects/agent-groups, or
(centralized) tagging per-user/tenant. Note: a slug is a *label*, not
authentication — in a shared deployment it must be paired with real auth to be
audit-grade (see below).

## Architecture (seams for later)

| Package | Role |
|---|---|
| `internal/proxy` | HTTP server, `Classify` routing, `httpTransport` (SSE tee + hold-back) |
| `internal/store` | `BlobStore` (zstd), `ManifestStore`, `Index` (SQLite), `Recorder`, `Observer` |
| `internal/wire` | `WireParser` + Anthropic impl (usage/session/cwd/stop/title), SSE frame splitter |
| `internal/view` | read-only derivations: system-prompt sections, token estimates, tool summary |
| `internal/web` | dashboard JSON API + embedded SPA, behind a read-only `Reader` interface |
| `internal/pricing` | token → USD cost (fallback table) |
| `internal/model` | `Exchange`, `ReqMeta`, `ResMeta` |
| `ui/` | Vue 3 + naive-ui frontend; builds into `internal/web/dist` |

The dashboard observes the recorder through `store.Observer`, which fires only
after a durability barrier has already committed and is non-blocking with
drop-on-full — so a stalled or absent browser can never hold up the proxy.

Deferred but seamed for: OpenAI HTTP (new `WireParser` + `Classify` rule), Codex
WebSocket (new `Transport` emitting the same `Exchange`), content-addressed dedup
(new `BlobStore` backend, opaque ref unchanged), hub, intercept. A `goxray ui`
subcommand is nearly free: `*store.Reader` already satisfies `web.Reader`.

## Tests

```bash
go test ./...
```

Covers: wire parsing (SSE/CRLF/non-SSE/schema-drift/noise), blob byte-exact
round-trip, recorder→manifest→index flow, index rebuild + reconcile, and
end-to-end proxy behavior (live streaming, fail-closed 502 with zero upstream
hits, response-abort withholding terminal frames, non-SSE commit-before-write).
