# Recording Codex

Design for recording OpenAI Codex (the CLI, and the desktop app, which reads the
same `~/.codex/config.toml`) alongside Claude Code. Every fact about Codex's
behaviour below was read out of its source (`openai/codex`, `codex-rs/`, the tree
checked out next to this repo, CLI 0.164.0); file references are to that tree.

## What Codex sends

Pointing Codex at goxray is one setting:

```toml
openai_base_url = "http://127.0.0.1:5599/backend-api/codex"   # optionally /<slug>/backend-api/codex
```

The path has to end in `/backend-api/codex`: that keeps the built-in `openai`
provider's `supports_codex_backend_routes()` true
(`model-provider-info/src/lib.rs:669`), so Codex behaves exactly as it does
against `chatgpt.com`. A custom `model_providers` entry would flip `is_openai()`
off and change behaviour — a recorder must not do that.

Under that base, three things arrive:

- **Model turns over WebSocket** — `ws://…/backend-api/codex/responses`, the
  default transport for the built-in provider (`supports_websockets: true`).
  Text frames only. Client→server: `response.create` (and rarely
  `response.interrupt`). Server→client: the Responses streaming events, ending in
  `response.completed` / `response.failed` / `response.incomplete` / `error`.
  `permessage-deflate` is offered.
- **Model turns over HTTP** — `POST …/responses` with an SSE reply. Used after a
  426 on the handshake or once WS retries are exhausted (then permanently for
  that client), and always for background (async) compaction. Under ChatGPT auth the
  body is `Content-Encoding: zstd` (`enable_request_compression`, on by default).
- **Side calls** — `GET …/models`, `POST …/alpha/search`, `…/images/*`,
  `…/realtime/calls`. Account, usage and analytics calls go to `chatgpt_base_url`,
  not here, and are out of scope.

### The property everything below is built around

**Over WS a request is not self-contained.** One `ModelClient` exists per thread
and caches its socket across turns (`core/src/client.rs:753`). When the new
request extends the previous one — same model/tools/settings, input = previous
input + previous output + new items — Codex sends only
`previous_response_id` and the new items (`client.rs:2214-2287`). The full
prompt for turn N exists nowhere on the wire; it is the *fold* of everything
earlier on that socket.

Two facts bound that fold to the socket:

- the server keeps the chain state per connection (`store:false`; an unknown id
  is `previous_response_not_found`, `responses_websocket.rs:166`);
- every way a socket can end — a stream error, a retry, an auth refresh, a
  change of handshake headers, a resume — makes the next request **full**, with
  no `previous_response_id` (`client.rs:1806-1850`, `2228`).

So a connection is the complete and only unit of reconstruction. Nothing ever
chains across two connections. The converse does not hold: a full request can
also arrive mid-connection, whenever the new input does not extend the last one
— after compaction, which runs in the foreground on the turn's own socket
(`compact_remote_v2.rs:322-344`), or after any settings change. The fold simply
restarts there.

An interrupted response is a continuation base too. On steering or mailbox
preemption Codex sends `response.interrupt` (`discard_partial_items`) and keeps
the socket; the response ends `response.incomplete` with reason `interrupted`,
which Codex promotes to completed, and the next request continues it by id
(`sse/responses.rs:421-474`, `scenarios_mailbox_preemption_tests.rs:314-331`).
The terminal event can also arrive *before* `response.interrupt.accepted`, which
Codex waits for before releasing the turn (`responses_websocket.rs:800-825`). In
0.164 the waiting kind of interrupt (`ResponseInterrupt::Cancel`) is sent only by
guardian-v2's sampler. A plain Esc is different — verified live, mid-stream: it
drops the socket without a close frame, and the next turn starts a new
connection.

### Concurrency inside one session

`session-id` (header, hyphenated) is the **root** session id, shared by every
descendant thread; `thread-id` is per thread. One session routinely has several
sockets open at once: forked and fresh sub-agents (`spawn_agent`), `/review`,
the guardian reviewer, and guardian-v2's classifier pool (eight sockets
prewarmed, up to sixteen concurrent; `async_scorer/sampler.rs:39-44`). Memory
consolidation is not one of them: it starts its own thread with its own root
session id (`memories/write/src/runtime.rs:399-416`). Per-request identity —
`thread_id`, `turn_id`, `x-openai-subagent`, parent thread, and
`x-codex-turn-metadata` (request kind `turn|prewarm|compaction|memory`,
`thread_source`, `agent_name`, …) — rides in each frame's `client_metadata`.
The handshake headers describe only the first request on the socket.

## The recording unit: a connection log

Three shapes were considered for a WS turn.

1. **One blob pair per turn, chained by `previous_response_id`.** Reading turn N
   means reading turns 1..N — N manifest reads plus 2N blob reads, every time.
   The chain is real, but the store would be the one walking it.
2. **Materialise the full request at capture** and store it as an ordinary
   request blob. Reads become free and nothing downstream changes, but the
   store would hold bytes that never crossed the wire, presented in the slot
   reserved for what did.
3. **Record the connection as what it is: an append-only stream.** Turns are
   ranges in it. Reading turn N is one read of one blob and a replay of its
   prefix.

(3) is what the protocol is. The connection is where the state lives, the frames
are already ordered, and the fold that rebuilds a turn is exactly the fold Codex
and the server do. It is also the cheaper read. This design takes (3).

### Log content

Decoded, a log is JSON Lines, one record per event:

```json
{"n":1,"t":1761530000123,"d":"c","p":{"type":"response.create",...}}
{"n":2,"t":1761530000301,"d":"s","p":{"type":"response.created",...}}
{"n":9,"t":1761530004410,"d":"s","op":"close","code":1000,"reason":""}
```

- `n` is a per-connection sequence number from 1; `t` is unix ms; `d` is the
  direction (`c` client→upstream, `s` upstream→client).
- `p` holds the frame payload **verbatim**: it is used only when the payload is
  valid JSON with no CR/LF and no leading or trailing whitespace, so the raw
  bytes round-trip exactly through `json.RawMessage`. Any other text frame goes
  in `s` as a JSON string, a binary frame in `b` as base64, and a text frame that
  is not valid UTF-8 in `x` as base64 — a JSON string would replace those bytes
  with U+FFFD, and the log itself stays valid UTF-8. Codex sends compact serde
  JSON, so in practice every frame is `p`.
- `op` is absent for a data frame. `close` records a close frame (code, reason)
  in either direction; `refused` an upgrade upstream answered with something
  other than 101 (its status in `code`, its body as the payload).
- Two ops are goxray's own annotations, never wire data, and labelled as such:
  `turn` directly precedes each `response.create` — the two are appended as one
  unit, and a turn record names a create only at `n+1` — and carries the turn
  exchange's id in `id`, which makes a log self-describing: recovery and `fsck`
  find every turn of a connection from the log alone. `end` records how goxray
  saw the connection finish (`clean`, `client_gone`, `upstream_gone`,
  `persist_failed`, `refused`, `rotated`, `shutdown`).
- Pings and pongs are not recorded; each leg answers its own.

`permessage-deflate` is per-leg and context-dependent, so the wire bytes of a
compressed frame cannot be recorded meaningfully. The log records the message
payloads, which is the same stance the HTTP path takes by forcing
`accept-encoding: identity`: the record is the entity, not the transfer coding.

The payload is byte-exact. Lines are assembled by hand rather than with
`json.Marshal` (which compacts and HTML-escapes a `RawMessage`), and decoding
into a `json.RawMessage` hands back the value's literal bytes, so `<`, `&` and
the sender's own whitespace survive; anything that could not sit on one line
unchanged goes in `s` instead.

### On disk

The log's ref *is* the connection exchange's id, so a log file names the
manifest that owns it. While the connection is open the log is
`blobs/live/<ref>.gxl`: a sequence of segments, each `uint32le length` + one
complete zstd frame. A segment is
written and fsync'd at every durability barrier (below), so it holds whatever
accumulated since the previous one — typically one `response.create`, or one
turn's worth of events, which compresses well within its own frame.

The reader is strict, because stored bytes can be wrong. A segment length of
zero or above a fixed bound, a segment that does not decode, a decoded segment
above its bound, a total above the log bound, a malformed record, or a gap or
repeat in `n` is an integrity error. The single exception is a final segment
whose declared length runs past end of file: that is a torn tail from a crash,
and it is dropped.

**After a failed append nothing is appended again.** A write that landed but
whose fsync failed leaves the file's tail unknown: retrying the segment would
duplicate records, and writing past a partial frame would bury everything after
it as a torn tail. So the writer is poisoned, the barrier fails, and the close
skips sealing and leaves the `.gxl` for recovery, which reads it strictly.

When the connection ends the log is **sealed** into one ordinary
`blobs/<shard>/<ref>.zst`, streamed segment by segment into the encoder rather
than decoded whole and checked by the same rules as it goes, so a log that
would not read back is never installed, with the usual tmp+fsync+rename. From then on it is an
ordinary blob for `Get`, archiving, `--unarchive` and the manifest pack. `Get`
gains one step: `.zst`, then `live/<ref>.gxl`, then the archive.

**A log is bounded by construction.** An archive refuses any one body over 256
MiB, and a session holding one is never archived. So once a log passes 128 MiB
decoded, the proxy closes both legs (1001, `end: rotated`) at the first point
Codex also sees as between turns: a turn has committed its terminal event *and*
no interrupt acknowledgement is still owed, since Codex keeps reading for one
after the terminal event. Codex treats a closed socket as a reason to reconnect
and sends the next request in full, onto a fresh log. A create that races the
close is refused before anything is recorded or forwarded, and the socket is
closed the same clean way. Nothing is lost and no log needs another to be read.

The hard limit, 192 MiB, aborts a turn that would cross it. Close and end
records are exempt, so a full log can always be finished, and a 1 MiB slack
bounds what they add; the reader's limit is the sum, still under the archive's.

Why not keep it compressed and append-only as `.zst`: a zstd file that is still
being written cannot be decoded by the existing strict reader, and telling a
torn tail from corruption would need frame offsets the decoder does not expose.
Length-prefixed segments make the live tier tolerant and the sealed tier
strict, which is the split the rest of the store already has.

## Exchanges

The manifest stays the source of truth and the `Exchange` stays the unit the
index, dashboard and CLI see. A WS connection produces two kinds.

**The connection exchange** is the HTTP upgrade itself: `GET …/responses`,
status 101, curated handshake headers on both sides, `ResRef` = the log's ref.
It is `inflight` while the socket is open and takes a terminal state when it
closes. It is marked noise, so turn lists hide it, and it is what the archive
folds the log through — exactly like any response body.

**A turn exchange** is one `response.create` and the events answering it:
`method: "WS"`, the connection's URL, and

```json
"transport": "ws",
"ws": {"conn": "<connection exchange id>", "log": "<log ref>", "first": 12, "last": 57}
```

`ReqRef` and `ResRef` are empty: the bytes are in the log, once. `first` is the
`response.create` record and `last` the turn's last record. The turn runs from
`first` to the next `response.create` or the end of the log, so an event that
follows the terminal one — `response.interrupt.accepted` — still belongs to it.
The manifest is first written final at the terminal event, with the range so
far, and written again when the turn ends, so once the log is sealed `last` is
exactly the turn's end; `fsck` holds it to that. `complete` means the API's
terminal event is recorded, not that Codex has released the turn; a turn with
no terminal event stays `inflight` until it ends, even across an
acknowledgement that arrives first, because recovery keeps a final manifest's
outcome and a guess written early would outlive a crash. A turn's session is always its
connection's session, so the log resolves through the same archive.

`WS.Log` is a body reference in its own right: the archiver folds it and
`fsck` checks it for every turn, rather than relying on the connection exchange
to carry it there. A turn whose log is still live is left alone by the archiver
the way an inflight one is.

Parsed metadata (`req`/`res`) is computed exactly as for HTTP — through the
`WireParser` — from the turn's *reconstructed* request and its own events, so a
listing row needs no log read.

## Fail-closed on a socket

Invariants 1 and 2 carry over as statements about frames:

1. **Every client→server frame is durable before it is forwarded.** It is
   appended, its segment is written and fsync'd, and for a `response.create` the
   turn's inflight manifest is written — then, and only then, the frame goes
   upstream. The handshake follows the same rule twice: the connection
   exchange's inflight manifest and the empty live log exist before goxray dials
   upstream, and the manifest is rewritten with the upstream's 101 and headers
   before the 101 reaches the client.
2. **A terminal event is held until the turn is committed.** Non-terminal
   events are forwarded as they arrive and buffered into the pending segment,
   as SSE deltas are today. `response.completed`, `response.failed`,
   `response.incomplete`, `error`, and the interrupt acknowledgements
   (`response.interrupt.accepted` / `.failed`) are held; the segment is fsync'd,
   the turn's final manifest is written, then the event is released. Holding an
   `error` that turns out not to be terminal costs one extra fsync and nothing
   else, since it no longer ends the turn's range.

A close frame from either peer is a frame like any other: it is made durable
before it is passed on, and if that fails the connection ends as a failed
barrier rather than a clean close. goxray's own 1001 (rotation, shutdown) is not
a peer's frame and is not recorded as one; rotation sends it only once every
turn has committed, and the `end` record is the account of it. Whoever ends a
connection claims the outcome before sending any close frame, so a peer's reply
cannot race in and record the end as its own.

A failure at either barrier cannot be reported in-band — a close frame is a
clean finish. Both sockets are torn down without a close frame, which Codex
treats as a stream error: it reconnects and resends the whole conversation, so
nothing is lost and nothing unrecorded reaches the API. The log gets an `end`
record of `persist_failed` if it can still take one.

Ordering within a barrier is log first, manifest second: a manifest never names a
record that is not durable.

## Closing and crash recovery

Closing a connection is five steps in a fixed order:

1. append `end`, write and fsync the last segment;
2. seal: stream the `.gxl` into `<ref>.zst`, installed by atomic rename;
3. settle every turn manifest still inflight;
4. settle the connection manifest;
5. unlink the `.gxl`.

A connection and the turn open on it end separately. A clean close — a close
frame from either side, goxray's own rotation or shutdown — completes the
*connection*, but a turn still waiting for its terminal event did not complete:
it settles as `client_aborted` when the client closed, `upstream_error`
otherwise, the same state crash recovery gives it.

The `.gxl` is the marker that the close is not finished: it exists until every
manifest is final. A process that dies leaves `.gxl` files whose connections
died with it — goxray terminates both legs, so no socket survives a restart —
and `Store.Open` runs steps 2–5 for each, idempotently: an existing `.zst` is
rebuilt over, a final manifest keeps its outcome but has its range extended to
the turn's end if it stopped short, and turns are found through the log's own
`turn` records, not the index. Recovery reads and checks everything before it
writes anything, and only rebuilds a manifest that is *missing*: one that cannot
be read, one that describes another turn (id, connection, log, first record or
session) or has no `ws` at all, a turn with no `turn` record or named twice, or
a connection manifest that is missing or is not this log's connection (id,
`ResRef`, kind, transport) is reported, and the `.gxl` is left exactly as found rather than sealed into a
blob nothing can account for.

Shutdown closes sockets alongside the HTTP drain, not after it, since
`http.Server.Shutdown` does not wait for hijacked connections. Every WS handler
is counted from its first line, before it hijacks anything, and none starts once
shutdown has begun, so none is missed; each socket gets its 1001 concurrently.
The same close runs if the listener dies on its own.

## Reconstruction

`wire.Replay` folds a decoded log, once, front to back:

- each `response.create` starts a turn. If it carries `previous_response_id` and
  that id names a response earlier in this log, the turn's full input is that
  response's full input + its output items + the frame's `input`. With no
  `previous_response_id` the frame's `input` is the full input. An id that is not
  in the log (cannot happen, see above) makes the turn's context *incomplete*,
  and the view says so instead of showing a truncated prompt as if whole;
- output items are the `item`s of `response.output_item.done`, in order, falling
  back to `response.completed.response.output` if a stream carried none;
- every response is indexed by id whatever its terminal state: Codex chains on
  an interrupted `response.incomplete` as readily as on a completed one;
- a turn ends at the next `response.create` or the end of the log, not at its
  terminal event, so trailing acknowledgements stay with it.

The full request handed to the parser and the dashboard is the turn's own frame
with `input` replaced by the full input. Nothing else is inherited: in the
Responses API `previous_response_id` carries conversation state only, not tools
or settings, and Codex resends those in every frame.

The prewarm (`generate:false`) is the base of the first real turn, which is one
more reason it is recorded as an ordinary turn and only *flagged* as noise:
filtering is a view concern, and the replay sees everything.

The recorder runs the same `Replay` incrementally on the live connection (it
already has every frame in order), so capture-time metadata and read-time views
come from one code path.

## Classification

| Traffic | Provider | Noise | Subagent |
|---|---|---|---|
| WS connection exchange | openai | yes | — |
| `generate:false` prewarm | openai | yes | per metadata |
| `thread_source` = `thread_title` / `system` (title, recap: their own throwaway root sessions) | openai | yes | no |
| `GET …/models` | openai | yes | no |
| turn with `x-openai-subagent` (sub-agent, review, guardian, memory) | openai | no | yes |
| compaction (`request_kind: compaction`) | openai | no | as metadata |

Session id: `client_metadata.session_id`, else the `session-id` header; a turn
always takes its connection's. Thread, parent thread, agent name and request
kind are kept in `req` and projected into the index so the dashboard can label
a sub-agent's turns without opening them. cwd is the last `<cwd>` inside an
`<environment_context>` message in the full input (later turns send it only as
a diff, which is why it has to come from the reconstructed input).

## HTTP path

Routing is by path, after the source slug is stripped: `/backend-api/…` goes to
`chatgpt.com` (`--chatgpt-upstream`), `/v1/responses…` to `api.openai.com`
(`--openai-upstream`, for API-key auth), everything else to Anthropic as before.

The SSE commit point needs the Responses terminal events added — without that,
an HTTP-fallback turn's `response.completed` would reach Codex before its
record is durable.

A `Content-Encoding: zstd` request body is stored exactly as received and
forwarded byte-for-byte; the parser and the dashboard decode a copy. That costs
the archive its cross-turn dedup on those bodies, which is accepted: HTTP is the
fallback transport, and a record that differs from the wire would be a worse
trade than a larger one.

Credentials stay out: `authorization` and `chatgpt-account-id` are never in the
curated header set.

## Usage and cost

Responses usage counts cached input and cache writes *inside* `input_tokens`,
and reasoning *inside* `output_tokens`. The parser normalises at capture into
goxray's shape — `input = input_tokens − cached_tokens − cache_write_tokens`,
`cache_read = cached_tokens`, `cache_create = cache_write_tokens`,
`output = output_tokens`, `thinking = reasoning_tokens` — so `pricing.Calc` and
every context-percentage computation work unchanged. Writes bill by lifetime,
1.25× input for 30 minutes and 2.5× for 12 hours, so
`cache_write_tokens_details.{ttl_30m_tokens,ttl_12h_tokens}` go into the two
slots Anthropic's 5m/1h split uses, where the rate table prices OpenAI's
lifetimes. Service tiers scale the whole request: flex and batch 0.5×, fast
(formerly priority) 2×, ultrafast 6×.

Cost is the API-equivalent figure, as it is for Claude Code on a subscription.
OpenAI rates join the table under the existing rules: explicit keys per model
family, longest prefix, and no rate rather than a guessed one.

The context window is not in a Responses request. It is recorded as 0
(unknown) rather than guessed; `GET …/models`, which goxray records, reports it
and is the obvious later source.

## Index

`schemaVer` goes to 3 (rebuild from manifests, as every bump does) for new
columns `transport`, `conn`, `thread`, `agent`, `kind`. `CaptureVersion` goes to
2: a v2 manifest can describe a turn with no body refs of its own, which a v1
reader would misread as "not recorded".

## Dashboard

Go turns a Responses request into the shapes the existing views already
consume — the developer message becomes system blocks, `message` items become
messages, `function_call`/`custom_tool_call` become `tool_use`,
`*_output` become `tool_result`, `reasoning` summaries become `thinking` — and
assembles the response the same way. The raw tab stays verbatim: for a WS turn
it serves the turn's records from the log, and the connection's whole log is
one more raw view. A settled turn is replayed only up to its manifest's `last`,
and only if the turn record at `first` names it; an inflight turn replays to the
end of the live log, since its events are what the reader is watching for.

## Not covered

- **The realtime voice socket.** It needs API-key auth and its URL handling under
  a `/backend-api/codex` base is unverified.
- **Calls under `chatgpt_base_url`** (usage, accounts, cloud tasks, file
  upload, analytics). Not model traffic.
- **Linking title and recap threads to the session they describe.** Codex gives
  them no parent link.
- **Memory consolidation as part of its session.** It runs as its own root
  session, so it appears as one; nothing in its traffic links it back.

## Test plan

- Proxy: a close frame that cannot be made durable ends the connection as
  `persist_failed`; a refusal that cannot be recorded is answered 502, not
  relayed.
- Unit: log encode/decode round-trips every payload byte-exactly; torn tails
  are dropped while interior damage, oversized lengths and `n` gaps are errors;
  replay matches hand-built chains, including prewarm, an interrupted
  `response.incomplete` continued by id, an acknowledgement after the terminal
  event, a full request mid-connection, and an unknown `previous_response_id`.
- Proxy, against a fake WS upstream: no client frame reaches upstream when the
  log or manifest write fails; no terminal event reaches the client before its
  manifest; both legs drop without a close frame on persist failure; a refused
  upgrade (426) is relayed and recorded; rotation past the size bound.
- Store: recovery at every close boundary (before the `.zst`, after it, midway
  through settling, after the unlink) converges; an orphan `.gxl` is reported;
  `fsck` catches a turn whose range does not start at its own `turn` record.
  Added after Codex's code review: a failed append leaves the `.gxl` and
  recovery settles it; a repeated record is never sealed; an interrupted turn
  neither rotates nor closes its range before its acknowledgement; a clean close
  does not complete an open turn; recovery extends a stale range and leaves an
  unreadable manifest alone; a full log still takes its end record;
  `CloseStreams` settles open sockets and refuses new ones.
- Live, against the real Codex in a herdr pane, on a scratch `--home` and a port
  other than 5599: a multi-tool turn over WS, `/compact`, a spawned sub-agent,
  an Esc mid-turn, `resume`, and a forced HTTP fallback — each checked in the
  dashboard and against Codex's own rollout file.
