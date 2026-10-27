# goxray — project notes

Working notes for anyone (human or agent) picking this repo up. The README is
the user-facing description of *what goxray is*; this file is *where the project
stands, why it is shaped this way, and what is deliberately missing*.

Read the README first for architecture and the recording protocol. Do not
duplicate it here.

---

## Status

| | |
|---|---|
| **Stage** | v0 + dashboard MVP + derived pricing + Codex (WebSocket and HTTP), working end to end |
| **Branch** | `codex` — Codex support, on top of `main`; every commit verified to build and test green in isolation |
| **Go** | 1.26, module `go.inndy.tw/goxray` |
| **Tests** | `go test ./...` — all green. `config`, `model`, `cmd` have no tests |
| **Size** | ~5k lines Go, ~2.6k lines Vue/JS |

---

## What works today

**Proxy (v0).** Fail-closed recording of every Claude Code exchange. Blobs
(zstd) + immutable JSON manifests + a rebuildable SQLite projection. Source-slug
tagging, noise classification, unclean-shutdown reconcile. Verbatim capture, so
an API shape change can only null a parsed field (`parse_ok=0`), never lose data.

**CLI.** `goxray ls | show | raw`, all read-only through `store.Reader`, plus
the writing subcommands `goxray migrate-usage`, `goxray archive`, and
`goxray fsck`.

**Session archiving.** Once a session has been idle for `--archive-after`
(default 30m), its request and response bodies are folded into one
self-contained file: content-defined chunks, deduplicated within the session,
unique chunks compressed as a solid zstd pool. Measured on the real store,
**131.4 MiB → 4.6 MiB (28.7×)**. The write path is untouched. See
[docs/blob-storage.md](docs/blob-storage.md) for the measurements behind every
parameter.

**Manifest packing (this cycle).** The same sweep folds the session's manifests
into a sibling `m-<digest>.gxm` — a directory of lengths plus one solid zstd frame,
no chunking — and deletes the loose `.json` files only after reopening the pack
from disk and byte-comparing every one. Measured on the real store, **792
manifests: 3.09 MiB of 4 KiB clusters → 113 KiB, 27.9×**. Manifests were the only
uncompressed tier and had grown to outweigh every archive combined. Reads try the
loose file first, exactly as a blob read tries `.zst` before the archive. See
[internal/store/mpack.go](internal/store/mpack.go) and
[docs/store-efficiency.md](docs/store-efficiency.md); reproduce the audit with
`python3 tools/store-audit.py`.

**Dashboard (this cycle).** `--ui-port N` starts a second, read-only HTTP server
inside the `serve` process. Sessions → turns → detail with four tabs (Timeline /
System / Tools / Raw), live turn streaming over SSE, inflight rows visible while
a turn is still running.

**Codex (this cycle).** Codex CLI is recorded with
`openai_base_url = "http://host:port[/slug]/backend-api/codex"`, over its
default WebSocket transport and over the HTTP fallback. A WebSocket connection
is recorded as one append-only log (`blobs/live/<ref>.gxl` while open, sealed
to an ordinary `.zst` on close); each `response.create` is a turn exchange
naming its range in that log, and its full request is rebuilt by replaying the
`previous_response_id` chain. Live-tested against Codex 0.164.0: prewarm,
incremental turns, forked sub-agents, `/compact`, Esc, hard-kill recovery,
graceful shutdown, archive and fsck. Design, protocol facts and the review
history are in [docs/codex.md](docs/codex.md) — read it before touching
`internal/proxy/ws.go`, `internal/store/{livelog,stream}.go` or
`internal/wire/{wslog,replay,openai}.go`.

**Context window per turn (this cycle).** `req.ctx_window` is parsed at capture
time and projected into the index, so a row — including the inflight one the SSE
frame carries — shows its context percentage against the right denominator with
no detail fetch. Older captures have no such field; `goxray migrate-usage`
recovers it from the stored request blobs.

---

## Invariants — do not break these

These are the properties the project exists to have. A change that violates one
is a bug even if every test passes.

1. **Nothing is forwarded upstream before `PersistRequest` commits.** Failure
   means 502 and zero upstream hits.
2. **Terminal SSE frames are held back until `PersistResponse` commits.** A
   client must never see a clean finish for an unrecorded response; on failure
   the connection is aborted without a clean EOF.

   On a WebSocket both become statements about frames: every client frame's
   segment is fsync'd (and a create's inflight manifest written) before it goes
   upstream, the 101 is recorded before the client sees it, and terminal events
   and interrupt acknowledgements are held until the turn commits. A barrier
   failure closes both legs *without* a close frame. After a failed append the
   log is never appended to or sealed again; the `.gxl` is left for recovery.
3. **Manifests are the source of truth.** `index.db` is a projection and must
   stay deletable-and-rebuildable. Never put data only in SQLite. A manifest is
   still authoritative once the sweep folds it into a `.gxm` pack — `EachID` and
   `Read` see both tiers, so a rebuild is identical either way
   (`TestIndexRebuildAfterPackingIsIdentical`). Manifests are never compressed on
   the write path; folding happens only in the sweep.
4. **Credentials are never persisted.** The auth header is forwarded, not
   captured, and no stored identity is derived from it.
5. **The observer must never block the proxy.** `Hub.OnExchange` runs on the
   request goroutine; it drops on a full client buffer rather than waiting.
   Observer panics are recovered in `Recorder.notify`.
6. **The dashboard is read-only and opt-in.** Its handlers reach the store only
   through the `web.Reader` interface — no blob, manifest, or index writes. Off
   unless `--ui-port` is passed.
7. **Archiving never deletes what it has not just proven.** Every pass writes
   the archive, reopens it *from disk*, and byte-compares every body against the
   live file before unlinking anything — and refuses to install a replacement
   that would drop a ref the current archive holds. Blob reads try `.zst` before
   the archive, so the overlap window and `--unarchive` are correct by
   construction. Two passes must never overlap: the lock carries an ownership
   token, is refreshed while held, and is re-read before every install and every
   unlink, so a pass that lost it stops rather than racing its successor.
8. **An archive is self-contained.** Nothing in one references anything outside
   it: no shared dictionary, no global chunk pool, no other archive. The store's
   dependency graph stays a forest of depth one, which is why deletion and
   integrity are per-session and there are no refcounts. A resumed session is
   *rebuilt*, never chained onto — measured smaller than chaining, and it keeps
   the graph edge-free. A session's `.gxm` manifest pack is self-contained on the
   same terms; it is a *sibling* of the `.gxa` rather than a section inside it
   precisely so that damage to the bodies cannot also destroy the record of what
   they were, which is what `fsck` needs to name the affected exchanges.

9. **An archive is found by computing its name, never by looking it up.**
   `archiveFile` is `s-<sha256(session id)[:16]>` and nothing maps a session to a
   file. That is what keeps blob reads working with no `index.db` at all (the
   documented repricing procedure deletes it), and what makes installing a rebuilt
   archive exactly one atomic rename over a known path, with "one session, one
   archive" true structurally rather than by bookkeeping. Do not add a mapping —
   see the alternatives in [docs/blob-storage.md](docs/blob-storage.md#finding-an-archive-no-index-just-a-name).
10. **`internal/web/dist` is committed on purpose.** It is embedded via
    `//go:embed all:dist` so `go install` works without Node. Rebuild it (`make
    ui`) whenever anything under `ui/` changes, or the binary silently ships a
    stale UI.

---

## Decisions and why

### Dashboard shape

- **Second `http.Server` on `--ui-port`, inside the `serve` process** — not a
  separate `goxray ui` command. Chosen because it is the only way to show
  inflight turns and push live SSE. The cost is shared fate: killing the proxy
  kills the dashboard. Accepted knowingly.
- **Hybrid parse split.** Go derives system-prompt sections, token estimates,
  SSE event parsing, and the tool summary (`internal/view`). Vue does step
  merging and tool previews (`ui/src/lib/`). Rule of thumb: anything another
  client would need goes in Go; anything that is purely presentation stays in JS.
- **Middle depth.** Step timeline + system blocks + tool chips + raw viewer.
  Deliberately cut from the MVP: Edit OLD/NEW diff rendering, tool-output
  collapse, a stacked context bar in the detail pane.
- **JS, not TS.** With naive-ui, dark theme only. Matches the "not fancy"
  brief; a TS migration is not planned.

### Frontend dependency constraints

`pnpm` is configured globally with `minimumReleaseAge: 10080` (7 days). Two
packages were dropped to satisfy it, **not** by weakening the policy:

- `vue-router` pinned to `^4.5.0` (5.x pulled a too-new transitive dep)
- `vite-plugin-vue-devtools` removed entirely

If `pnpm install` fails with `ERR_PNPM_NO_MATURE_MATCHING_VERSION`, find an
older-but-adequate version. Do not lower `minimumReleaseAge`.

### System-prompt splitting

ccxray splits on a table of B2 marker strings. That matches **nothing** on Claude
Code 2.1.220 — configuration moved out of the system prompt into `messages[]` as
`<system-reminder>` blocks. `internal/view` splits on `^# ` H1 headings instead,
which also sidesteps Go's RE2 having no lookbehind. Blocks are tagged
`anthropic` or `user` via `userOwnedKeys`; a few legacy markers are still matched
by regex for older captures.

Three other divergences from ccxray's assumptions, found by reading a real
capture rather than porting blind:

- `role:"system"` messages exist in `messages[]` and are large.
- Untagged `[SYSTEM NOTIFICATION - NOT USER INPUT]` messages up to 46k chars
  would otherwise render as human input.
- The `[1m]` context marker lives in the **system prompt text**, not the model
  id — hence `wire.CtxWindow(model string, system json.RawMessage)`, and hence
  the window being recorded per turn (below) rather than derived on read.

### Pricing

- Lookup is **longest-prefix**. This once caused a live 3× overcharge: the
  `claude-opus-4` entry was catching `claude-opus-4-8`. Every generation now has
  its own explicit key; never add a short generic prefix back.
- The table covers only what is actually in use: the 4.6 generation and newer,
  plus Haiku 4.5. Everything older was **removed on purpose**. An unpriced model
  yields `$0` plus an explicit "no price on file" warning, which is better than a
  stale rate rendered as a confident dollar figure.
- **Cost is derived, never stored truth.** `localManifests.Read` reprices from
  the stored token counts on every read, so the index, dashboard and CLI cannot
  disagree. The number in the manifest is only what the table said at record
  time. Editing a rate therefore needs no manifest rewrite: change the table,
  delete `index.db`, restart. This replaced a real incident — 200 turns recorded
  before Opus 5 was in the table were stuck at `$0` and had to be repaired by
  hand.
- **Cache writes bill by TTL**, 1.25x input for 5m and 2x for 1h, so
  `usage.cache_creation.{ephemeral_5m,ephemeral_1h}` is parsed and priced
  separately. Measured on a real capture, 44% of cache-write tokens were 1h;
  charging the flat total at the 5m rate underpriced cache writes by 26%.
  `cacheWriteCost` keys on *which fields are populated*, not on a capture
  version, so a pre-split capture still prices at the flat 5m rate instead of
  falling to zero.
- Pricing prefers the model id the **response** reported over the request's
  alias, and halves the rate for `service_tier: batch`. Priority tier is
  account-negotiated and deliberately left at 1x.

### The timeline shows the response, not the next turn's echo

A request contains the conversation only up to the point the model was asked to
speak, so a turn's own reply is not in it. `wire.AssembleSSE` / `AssembleJSON`
rebuild the reply from the recorded response body and the timeline appends it
below a "this turn's response" rule.

It would have been cheaper to let the reader click the *next* turn, where the
reply appears as the trailing assistant message. That is wrong twice over: the
newest turn has no next turn, and — measured across 31 consecutive turns —
**Claude Code edits the echo before sending it back**:

- the `caller` field the API returns on `tool_use` is stripped (17 of 31 turns)
- `cache_control` is added that the API never sent (4 of 31)
- at least one `Bash` input was rewritten, dropping a `cd <dir> && ` prefix
- side-turns (title generation, summaries, `explain_command`) never re-enter the
  conversation at all, so their replies appear in no later request

So the assembled response is strictly more faithful than the echo, for every
turn and not just the newest.

Blocks are kept as raw JSON and non-streaming bodies pass through untouched, so
unmodelled block types survive. A stream that never reached `message_stop` is
marked `truncated` and its blocks are still emitted; a `tool_use` whose argument
JSON was cut mid-stream keeps the fragment in `partial_input` rather than
dropping the only record of the call.

### The context window is recorded, unlike the cost

Cost is derived on every read because the rate table moves under it; the context
window is written into the manifest at capture time, because the thing it is
derived *from* is not always in reach. The `[1m]` marker usually appears only in
the system prompt, so deriving the window means reading a request blob — and a
listing has 500 manifests and no bodies. That is why a row used to render
composition-only until some turn of the same (session, model) had been opened,
and why the window is now `req.ctx_window`, parsed once by `ParseRequest` — which
already has the system prompt in hand — and projected into the index like any
other request field. `wire.CtxWindow` lives beside `ParseRequest` rather than in
`internal/view` for the same reason: it is consulted at capture, not at render.

Rows and the detail pane must never disagree about it, so the detail seeds from
the recorded value and lets the body it fetched correct it, rather than starting
from the 200K default. The dashboard keeps its learn-from-detail cache purely for
manifests written before the field existed; a store that has run
`goxray migrate-usage` never consults it.

Two properties fall out of recording it rather than deriving it per row: the
inflight SSE frame already carries the window, so a turn is correct the moment it
appears rather than once it finishes, and a turn whose request body was
unparseable reports `0` — rendered as unknown — instead of a confident 200K that
would show a 1M turn at five times its real fill.

### Security model

Loopback bind is not the defence — the **`Host` header allowlist** is. A
DNS-rebound request still carries the attacker's domain in `Host`, so it is
rejected. `Origin` must also be loopback, no CORS headers are ever emitted, and
recorded bodies are served as `text/plain` unless JSON or SSE. None of this is
authentication; exposing the port off loopback needs real auth first.

Verified empirically: 403 for a foreign `Host`, wrong port, or remote `Origin`;
200 for a loopback `Origin`; zero CORS headers in any response.

The **proxy** bind address is the operator's choice: `--host`/`PROXY_HOST`,
default `127.0.0.1`. Listening on another NIC (say, to record a Claude Code on
another machine) is a supported configuration, not a hole to warn about — the
default is loopback, so exposure only ever happens because someone asked for
it. `--host` deliberately does not move the dashboard, which stays hard-wired
to `127.0.0.1` for the reasons above.

### Nothing recorded is trusted as a filename or a length

Both halves of the archive path are attacker-shaped data, and both were treating
it as ours:

- **A session id comes from the request body**, and the proxy port — unlike the
  dashboard — has no `Host` guard, so a CORS-simple `POST` from a web page is
  recorded like anything else. `metadata.session_id` reached `shardPath`
  verbatim, so `"../../../outsideAA"` escaped `ArchivesDir` and an applied sweep
  would create or replace a `.gxa` file outside the store.

  The archive name is now `s-<sha256(session id)[:16]>` — **total**, so no part
  of a recorded id reaches the path and there is nothing to check, trust, or
  reject. The escape is not blocked, it is unexpressible. This replaced a
  working but partial rule (normalize at the boundary, and have `archiveFile`
  refuse whatever was still not a name), whose real cost was not the check but
  the leftovers: a session whose id predated the boundary could not be archived
  at all, and an archive could exist that no session id resolved to. Both are
  gone. `archiveFile` is total apart from `session == ""`, and that case is about
  grouping — a sessionless turn has nothing to be folded in with — not safety.

  `normalizeSessionID` stays, no longer load-bearing: it bounds a 46 KB id down
  to something an index column and a dashboard URL can hold, and keeps
  `IsSubagent`'s `sid == ""` test meaning what it says. Dropping it, and keeping
  raw ids in the record, is now a free choice rather than a filesystem question.

  Refs restored by `--unarchive` are still checked with `isPlainToken`: they come
  out of an archive's ref table, i.e. off disk, and unlike a session id a ref is
  used as a filename directly.

- **An archive records its own session id**, in a trailer at EOF (format v2).
  A digest filename is not reversible, so without this an archive would be
  anonymous — and the pre-digest scheme's one real diagnostic, "this file is not
  where its session would look for it", would have been lost with it. Instead it
  got better: `fsck` reads the session out of the file, compares it against the
  path, and for a misfiled archive names where it belongs. A v1 archive has no
  trailer, still reads, and can only be identified by its filename; `fsck` says
  so and points at the migration script.

  The trailer is placed after every section, so no offset depends on it. That is
  deliberate: upgrading a v1 file is an append plus a four-byte header patch, so
  the migration out of session-id filenames is
  `tools/migrate-archive-names.py` — a standalone script run once — rather than a
  compatibility path in the store. `internal/store/migrate_names_test.go` runs
  the script against a legacy-shaped store and checks `fsck` comes back clean.
- **An archive header is a file, and files rot.** Every count and offset in it
  was allocated on trust; a flipped `nrefs` asked for a four-billion-entry map
  and OOM-killed whatever process read it, `fsck` included. Everything is now
  bounded by the file size and by limits the builder also enforces, so a limit
  can only reject a file goxray did not write.

The general shape: `fsck` exists because stored bytes can be wrong, so no code
path may need them to be right in order to fail safely.

### Small ones worth remembering

- `Session.Sources` is always `[]string{}`, never nil — a Go nil slice marshals
  to JSON `null` and blanked the sessions pane. Vue also guards with `?.length`.
- **`/api/sessions` overlays inflight turns too**, for the same reason
  `/api/exchanges` does: the rollup is a `GROUP BY` over the index, and an index
  row appears when a *response* commits, so a brand-new session was absent from
  the pane for the whole duration of its first turn — while that turn was already
  in the turn list. Usage stays at what the index reported (zero for a first
  turn); the row corrects itself on commit. Together with the client half — the
  session refetch used to be gated on `!selectedId`, so a reader who had clicked
  any turn never saw a new session until reloading the page — this is what makes
  new sessions appear on their own. The refetch is coalesced (500 ms): a turn
  arrives twice and a busy proxy streams several a second.
- **A late-arriving session title needs no mechanism of its own.** A title-gen
  turn carries the real `session_id` and is *not* noise, so it reaches the hub
  like any turn and the refetch above picks the title up within 500 ms. Measured
  across the 13 titled sessions in the real store, 6 are titled by their very
  first turn and the rest by their second, 15 s to 128 s in. Until then the pane
  shows `basename(cwd)`, not a blank — see `label()` in `SessionList.vue`. The
  `title` subquery in `querySessions` deliberately has no noise predicate, so the
  title still resolves for a session viewed with noise hidden.
- `querySessions` uses correlated subqueries for model/cwd/title: a bare column
  alongside two aggregates is not guaranteed by SQLite.
- The router uses `NO_SESSION = '-'` as a path sentinel, decoded back to `''`.
  Applying `'-'` literally as a session filter matches nothing.
- Turn rows show the URL only when it is *not* the standard `/v1/messages` call,
  so 97 identical `?beta=true` lines do not drown the list.
- `internal/view/fixture_test.go` is opt-in via `GOXRAY_VIEW_FIXTURE=<path>` so
  no real conversation content is ever committed. Same for the two store tests
  that want a real store, via `GOXRAY_STORE_FIXTURE=<goxray home>`: the archive
  round-trip and the archive-name migration. Both copy the store to a `t.TempDir()`
  before touching anything, so they are safe to point at a live `~/.goxray`.
- Subcommands take no `--home`: `runCLI` calls `config.Load(nil)`, so `goxray ls |
  show | raw | archive | fsck | migrate-usage` operate on `$GOXRAY_HOME`, default
  `~/.goxray`. `--home` only works on `serve`, and `goxray --home X archive`
  silently *starts a server* (an unrecognised first arg falls through to serve).
  Use `GOXRAY_HOME=<scratch home> goxray fsck` for a scratch store.

---

## TODOs

### Known gaps in what shipped

- **Codex: the interrupt-acknowledgement path is unit-tested only.** Codex 0.164
  sends the waiting kind of `response.interrupt` only from guardian-v2's
  sampler; a user's Esc drops the socket instead. Rotation, ranges and holds
  around `response.interrupt.accepted` are covered by
  `TestStreamInterruptAckExtendsTheTurn`, not by live traffic.
- **Codex: HTTP fallback and `codex resume` were not exercised live.** No
  config forces the fallback; the 426 relay and the HTTP terminal-frame hold are
  unit-tested.
- **Codex: no context window.** A Responses request does not say which window
  the client works against, so `ctx_window` is 0 (unknown). The recorded
  `GET …/models` response reports it and is the obvious source.
- **Codex: a turn recorded before the range fix fails `fsck`** with "range stops
  short". Only stores that ran the pre-review build have one; the bytes are all
  there, only the manifest's `last` is low.

- **Turn titles are mostly empty.** Only Claude Code's title-generation turns
  populate `res.title`. A first-user-message excerpt would be a better fallback.
- **Sonnet 5 introductory pricing expires 2026-08-31.** On 2026-09-01 change
  `claude-sonnet-5` to the 4.6 tier `{3, 15, 0.30, 3.75, 6}`. There is no
  timestamp available in `Calc` to switch on automatically. Since costs are
  derived on read, the whole procedure is: edit the table, delete `index.db`,
  restart.
- **Pre-migration captures still price cache writes flat.** Anything recorded
  before the TTL split was parsed has no `cache_creation_5m/1h`, so it bills at
  the 5m rate. `goxray migrate-usage --apply` recovers the split from the stored
  response blobs; until it is run on a store, those turns read ~26% low.
- **A turn recorded before `req.ctx_window` existed still needs a click.** The
  window is now captured per turn (see below), but no older manifest has it, so
  the dashboard keeps the fallback that learns it per (session, model) from
  loaded details. `goxray migrate-usage --apply` fills the field in and the
  fallback stops mattering.
- **`wire.CtxWindow`'s 200K fallback is the client's window, not the model's.** The
  `[1m]` marker records what Claude Code will *use*; on the current generation
  Opus, Sonnet, and Fable are all 1M by default and only Haiku 4.5 is really
  200K. The fallback is therefore right for a Haiku side-turn and low for a bare
  Opus id. `GET /v1/models/{id}` reports `max_input_tokens` per model and would
  replace the heuristic.
- **No SSE replay buffer.** A dashboard that reconnects misses whatever streamed
  while it was away; it refetches the list instead. Fine for a local tool.
- **`IsSubagent` is a heuristic** (`cwd == "" && sid == ""`) in
  `internal/wire/anthropic.go`. It has not been validated against a real subagent
  turn.
- **Undisclosed thinking renders as nothing.** This account receives encrypted
  thinking: 48 `thinking_delta` events carrying zero characters, plus a 21 KB
  `signature`. The block is faithfully empty, but the timeline should say
  "thinking not disclosed" rather than leave a silent gap.
- **No auth on the dashboard**, by design. Loopback only.
- **The archive lock is a lease, not a lock — accepted knowingly.** `O_EXCL` plus
  an owner token, refreshed every 30 s, stealable after 5 minutes of silence.
  Every destructive step re-reads the token and the install re-stats its target,
  which narrows the loss window to the microseconds between the last check and
  the rename; a holder suspended past the TTL and then resumed can still land in
  it. Closing it properly means a real file lock — `syscall.Flock` on unix,
  `LockFileEx` (already in the `x/sys` we depend on) on Windows — held on
  `archives/.lock` for the pass, which would also delete the token, the
  heartbeat and the TTL. It does **not** require touching `os.Rename`: the lock
  is on the lock file, not on the archive. Judged not worth it for a risk that
  needs a process suspended for five minutes mid-pass; `fsck` reports the
  outcome if it ever happens.
- **Never hold a file open that is about to be unlinked or renamed over.** Go
  opens files on Windows without `FILE_SHARE_DELETE`, so both `os.Remove` and a
  rename *onto* a path fail there with "Access is denied" while this process has
  that file open. Three places obey it: the lock is released `close` then
  `remove` (removing first left every pass's lock on disk and blocked the next
  one until the TTL); `--unarchive` closes the archive before unlinking it; and
  the install drops every cached reader on its target before the rename — its
  own, because `verifyArchive` reads a resumed session's already-archived bodies
  through `blobs` and so caches the file it is about to replace, and the rest of
  the process's through `ArchiveOptions.Release` (wired in `serve` to
  `Store.ReleaseArchive`, since the dashboard's cache keeps the last two archives
  anyone viewed). Missing that, **on Windows no resumed session could ever be
  archived**: every sweep rebuilt it, verified it, failed to install it, and
  logged the same line five minutes later, forever. Nothing here can fail on
  unix, so both regression tests assert the handle rather than the rename.
- **A dashboard read can still lose one install a race.** If it lands between the
  release above and the rename, the archive is reopened and that rename fails. The
  session is deferred and retried, so it costs a tick rather than the archive;
  closing it properly would mean holding the blob cache's lock across the install,
  which is a cross-instance lock for a window of microseconds.

- **The live tier is the largest remaining cost, and there is no cheap fix.**
  Per-blob zstd gets 2.68–2.75× where the same bytes as one solid stream get
  ~31–36×, but the gap is not reachable per-blob: measured, raising the level buys
  −6.0% at L7 for 3.3× slower and −10.9% at L19 for 57× slower, on the fail-closed
  write path. Do not raise `SpeedDefault` in `blob.go`. The only real lever is
  `--archive-after`, and shortening it trades disk against rebuilding resumed
  sessions. Left at 30m deliberately.
- **Cross-session sharing is worth ~30% of the archive tier and costs invariant 8.**
  A trained 110 KiB dictionary is a *net loss* (−7.6%); a 1 MiB shared raw prefix
  wins 27.3%, because the shared material is megabytes of long-range literal
  matches, not a distribution. Grouping per project recovers only 6.7% — the
  redundancy is cross-project boilerplate, not project code. Not recommended; see
  [docs/store-efficiency.md](docs/store-efficiency.md).
- **Two known-flaky tests on Windows, both pre-existing and both test-only.**
  `TestGetSeesRebuiltArchive` and `TestArchiveLockSurvivesBeingTakenOver` fail when
  run in a filtered subset because their helpers bypass the real install path and
  so never call `forget`, and Windows refuses `os.Rename`/`os.Remove` on a file this
  process holds open. `internal/web`'s `TestExchangesMergesInflight` fails on an
  ordering assertion. `go test ./...` is therefore **not** green on this platform.
  Opening archives with `FILE_SHARE_DELETE` would dissolve the whole class — and
  most of invariant 7's machinery with it — but Go's `os.Open` does not.

### Deferred features, already seamed for

- `goxray ui` subcommand for browsing an existing store without running the
  proxy — nearly free, `*store.Reader` already satisfies `web.Reader`. The only
  thing it loses is inflight rows.
- Live pricing (ccxray fetches LiteLLM with a 24h cache). The fallback table has
  the same shape, so callers would not change.
- Content-addressed blob dedup: a new `BlobStore` backend, ref stays opaque.
- Hub / intercept modes.

### UI polish not done

Search or filter box, export, Edit OLD/NEW diff rendering, tool-output collapse,
light theme.

---

## Working in this repo

```bash
make build        # pnpm build into internal/web/dist, then go build
make ui           # bundle only
make ui-dev       # vite :5173, proxies /api to a dashboard on :5600
make test         # go test ./...
go build ./cmd/goxray             # no Node needed; dist is committed

pnpm -C ui lint    # oxlint + eslint, both with --fix
pnpm -C ui format  # oxfmt src/
```

Run the dashboard: `./goxray --port 5599 --ui-port 5600`

**Repairing a store after a pricing change.** Costs are derived on read, so a
rate edit only needs `rm ~/.goxray/index.db*` and a restart. Recovering detail an
older capture never parsed is the separate, one-time step:

```bash
goxray migrate-usage            # dry run: what would change, and the cost delta
goxray migrate-usage --apply    # rewrite manifests, then delete index.db
```

It re-parses stored blobs through the live `WireParser`, deliberately breaking the
"blobs are never re-parsed" rule that holds for index rebuilds, and recovers two
things the manifest could not hold at the time: the cache TTL split, from the
response blob, and `req.ctx_window`, from the request blob. Reusing the parser is
not optional on either side: a stream reports usage in several events, so anything
that sums instead of assigning double-counts, and the window is exactly what
`ParseRequest` now decides at capture time. The two fail independently — a turn
whose response was never recorded still gets a window, which is the row most in
need of a denominator. Safe against a running proxy (inflight and sub-minute-old
manifests are skipped, writes are atomic renames) and idempotent.

**Reclaiming disk.** Archiving runs on its own in `serve`; these are for doing
it by hand or checking it.

```bash
goxray archive                  # dry run: sessions eligible and bytes at stake
goxray archive --apply          # fold idle sessions into archives + manifest packs
goxray fsck                     # verify every chunk and every packed manifest
goxray archive --unarchive --apply   # put it all back, byte for byte
```

A store archived before manifest packing needs no migration: a session whose
manifests are still loose no longer counts as "already archived", so the next
sweep folds them. It does not rebuild the body archive to do that — with nothing
live to fold in, the archive already holds every body, so only the small pack is
written.

`--archive-after 0` disables the background sweep. A hand-run `archive` and the
sweep exclude each other through a lock file in `archives/`.

**Migrating a store off session-id filenames.** One-time, for any store archived
before the digest naming. `fsck` reports such archives and names this script;
until it runs, their bodies read as missing (nothing is lost — the files are
intact and untouched).

```bash
python3 tools/migrate-archive-names.py            # dry run
python3 tools/migrate-archive-names.py --apply    # then: goxray fsck
```

It moves each archive to its digest path and records the session id — taken from
the legacy filename, which is where the old naming put it — in the file. Stop
`serve` first, or the lock check will refuse; every rewrite is verified by
reparsing it from disk before the original is unlinked.

If `fsck` reports an archive as misfiled and the script cannot place it (two
archives claiming one session, say), `goxray archive --unarchive --apply` is the
fallback: it restores every archive it finds on disk, including that one, and the
next sweep re-files what it can under a name reads resolve to.

A session that fails to archive is logged, left completely alone, and held back
by the sweep for a doubling cooldown (capped at an hour) so it cannot spend every
tick's budget. A hand-run `archive` never defers — it retries everything.

**Vite dev proxy needs `changeOrigin: true`** or the guard rejects the
dev-proxied `Host`. Already set in `ui/vite.config.js`; do not remove it.

**Testing against a fake upstream.** `ANTHROPIC_TEST_HOST` / `ANTHROPIC_TEST_PORT`
/ `ANTHROPIC_TEST_PROTOCOL` override the upstream without touching flags, so a
local SSE stand-in can exercise the whole record → observe → SSE → UI path with
no outbound traffic. Point `--upstream` at it, or set the env vars.

**Do not disturb a running proxy.** The user runs a live goxray on **:5599**.
Use other ports for anything experimental, and wait for a port to actually free
before rebinding — killing and restarting in one command races.

**Rebuild before testing a UI change.** A stale `goxray` binary serves the old
embedded bundle and the old flags; more than one confusing debugging session has
started this way. Two Windows-specific traps sit on top of that, and both look
exactly like "my code change did nothing":

- **Never pass `-o` to the build.** An explicit `-o` suppresses the platform
  suffix, so the Makefile's original `-o goxray` wrote an extensionless `goxray`
  on Windows while the binary anyone actually launches is `goxray.exe`. `make`
  exited 0, the UI stayed stale, and nothing anywhere failed. Bare
  `go build ./cmd/goxray` names the output after the package directory *with*
  the suffix, on every platform.
- **Rebuilding is not restarting.** Windows does let you build over a running
  `goxray.exe` — Go renames the in-use image to `goxray.exe~` and installs the
  new one — so the build succeeds while the live process keeps serving the old
  code from memory. Confirm what is actually running by comparing the asset hash
  the dashboard serves against `internal/web/dist/assets/`:
  `curl -s http://127.0.0.1:5600/ | grep -o 'index-[^.]*\.js'`. The leftover
  `goxray.exe~` is deletable once the old process exits.

### Conventions

- Comments explain *why*, never *what*. No comment that restates the code.
- Commits: separate logical changes, ordered so every commit builds and tests
  green on its own. Verify with a throwaway `git worktree` when there are more
  than two or three.
