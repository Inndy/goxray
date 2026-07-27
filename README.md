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
go build -o goxray ./cmd/goxray

# run the proxy (default command)
./goxray --port 5599

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

Config (flags or env): `--port`/`PROXY_PORT` (default 5599), `GOXRAY_HOME`
(default `~/.goxray`), `--max-body-mb` (default 50), `--upstream`/`GOXRAY_UPSTREAM`.
Binds `127.0.0.1` only. `GET /healthz` for liveness.

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
| `internal/store` | `BlobStore` (zstd), `ManifestStore`, `Index` (SQLite), `Recorder` |
| `internal/wire` | `WireParser` + Anthropic impl (usage/session/cwd/stop/title) |
| `internal/pricing` | token → USD cost (fallback table) |
| `internal/model` | `Exchange`, `ReqMeta`, `ResMeta` |

Deferred but seamed for: OpenAI HTTP (new `WireParser` + `Classify` rule), Codex
WebSocket (new `Transport` emitting the same `Exchange`), content-addressed dedup
(new `BlobStore` backend, opaque ref unchanged), dashboard, hub, intercept.

## Tests

```bash
go test ./...
```

Covers: wire parsing (SSE/CRLF/non-SSE/schema-drift/noise), blob byte-exact
round-trip, recorder→manifest→index flow, index rebuild + reconcile, and
end-to-end proxy behavior (live streaming, fail-closed 502 with zero upstream
hits, response-abort withholding terminal frames, non-SSE commit-before-write).
