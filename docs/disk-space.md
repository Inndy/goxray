# Proposal: getting blob storage from O(N²) to O(N)

Status: **superseded by [blob-storage.md](blob-storage.md)**, which measured
these candidates against the real store and reached a different conclusion —
CDC dedup wins on space *and* speed *and* read depth, and the delta-chain
recommended below is 2–4× larger. Kept for the problem statement and the
"why the obvious shortcuts don't work" section, both of which still hold; the
measured candidates table is synthetic and should not be trusted.

Status: proposal, not implemented. 2026-07-27.

## The problem

Every Claude Code request re-sends the entire conversation, so within one
session the request blobs are 1, 2, 3, … units long and a session of N turns
stores **O(N²)** raw bytes. Per-file zstd does not help with this: each blob is
compressed against itself, so the shared conversation prefix is paid for again
in every file. Compression only divides the quadratic constant (~4×); it does
not change the exponent.

Concretely: a 150-turn session whose final request is ~1.2 MB holds ~115 MB of
raw request bodies ≈ 27 MB on disk today. A hundred such sessions is ~2.7 GB,
almost all of it the same text stored dozens of times. Response blobs are not
part of the problem — each response contains only its own turn's output, so
they sum to O(total output). Manifests and the index are small and O(N).

The redundancy is *across* files, so the fix has to see across files.

## Why the obvious shortcuts don't work here

- **"Store only the new suffix of each request."** Requests are not
  prefix-extensions of their predecessors. As measured in CLAUDE.md, Claude
  Code edits the echo between turns: `caller` fields stripped (17/31 turns),
  `cache_control` markers moved every turn, at least one tool input rewritten.
  Suffix-sharing would either corrupt the record or need parse-dependent diffing
  — and blob storage must never depend on parse correctness (verbatim capture
  is the point of the project).
- **"Recompress a whole session as one long-window zstd stream."** Random
  access then requires decompressing the prefix; making the stream seekable
  (independent frames) destroys exactly the cross-blob matching we came for.
- **"Just raise the zstd level."** Measured below: 1.2×. Also too slow for the
  hot path. Not a fix, though usable inside an offline pass.

## Measured candidates

No store exists in this dev environment, so the numbers come from a synthetic
benchmark that models the documented growth shape: two sessions (150 + 60
turns) sharing a 30 KB system+tools prefix, per-turn user/assistant/tool_result
messages, and the echo edits above (caller strips, moving cache breakpoints,
rare tool-input rewrites). Round-trips were verified byte-identical. Treat the
ratios as indicative; the absolute compressibility of real JSON will differ.

| Strategy | Stored | vs current |
|---|---|---|
| per-file zstd, default (today) | 26.98 MB | 1.0× |
| per-file zstd, best level | 23.01 MB | 1.2× |
| **delta-chain** (dict = previous request, checkpoint every 16, default level) | 2.87 MB | **9.4×** |
| **delta-chain** (checkpoint every 32, best level) | 1.17 MB | **23×** |
| CDC dedup (content-defined ~8 KB chunks, sha256, per-chunk zstd, incl. recipe overhead) | 2.10 MB | 12.8× |

**Delta-chain**: compress request *k* with request *k−1* of the same session as
a raw zstd dictionary (`klauspost/compress` supports this via
`WithEncoderDictRaw` / `WithDecoderDictRaw`, already at v1.19.1 in go.mod).
Every K-th blob stays self-contained ("checkpoint") to bound read cost and
corruption blast radius. Growth becomes O(new content per turn) — O(N) total.

**CDC/CAS**: the "content-addressed blob dedup" backend already seamed for in
CLAUDE.md. Same order of win, plus cross-session dedup — but the shared
cross-session content is only the ~30 KB system+tools prefix, which is noise
next to the intra-session redundancy. It costs much more machinery: a chunk
store (~14k chunk files for the 115 MB above, or packfiles to avoid that),
recipes, and — the day deletion exists — refcount GC. Verdict: **defer**; the
delta-chain captures ~the whole win with a fraction of the moving parts, and
the opaque-ref seam keeps CAS possible later with no contract change.

## Recommended plan

### Phase 1 — offline compaction: `goxray compact`

A new writing subcommand in the mold of `goxray migrate-usage` (dry-run by
default, `--apply` to write, safe against a running proxy). It recovers the
disk already spent, which write-time changes never can.

- Scan manifests (source of truth), group exchanges by `session_id`, order by
  `started_at` / uuidv7 id. Skip inflight and sub-minute-old manifests, skip
  empty session ids (subagent heuristic turns), skip response blobs entirely.
- Walk each session in order keeping the previous request uncompressed in
  memory. For each request blob past a checkpoint: write `<ref>.zd` — a small
  self-describing header (`magic, dict_ref, chain_depth`) followed by a normal
  zstd frame encoded with the parent as raw dictionary (frame CRC on, as
  today) — via the existing `writeFileSync` temp+rename+fsync, **then** unlink
  `<ref>.zst`.
- **The ref never changes.** `.zd` lives at the same `shardPath` under a
  different extension, so no manifest is rewritten and invariant 3 is untouched
  — this is strictly less invasive than migrate-usage.
- `localBlobs.Get`: try `.zst`; on ENOENT try `.zd` and resolve the dictionary
  recursively (depth-capped at K, plus a defensive hard cap); on ENOENT again
  retry `.zst` once to close the race with a concurrent compactor. Add a small
  LRU of uncompressed blobs so serially browsing a session in the dashboard
  resolves each chain step from cache instead of re-walking it.
- Offline, so it can afford best-level encoding and a generous window
  (window ≥ dict + payload; multi-MB requests need this or matches fall outside
  the window). Idempotent: already-`.zd` blobs are skipped or, optionally,
  re-tightened.
- Dry run prints projected savings per session — which also serves as the
  validation step: run it on the real store *before* trusting the synthetic
  ratios above.
- Reversible: a `--decompact` mode can rematerialize `.zst` files, since every
  chain resolves to exact bytes.

Read-cost budget: worst case is K−1 dictionary resolutions ≈ K × final-request
size of zstd decode (~GB/s), i.e. tens of milliseconds at K=16 even for
multi-MB requests, amortized to ~0 by the LRU. Corruption blast radius: losing
one delta file breaks its descendants up to the next checkpoint — bounded by K,
and each frame's CRC makes the damage detectable rather than silent. That
trade (K files at risk instead of 1, for ~10–20× less disk) is the essence of
any dedup scheme; K is the knob.

### Phase 2 — write-time deltas in the recorder

Compaction alone lets disk balloon between runs. Small extension to
`PersistRequest`: the `Recorder` keeps an LRU of `session_id → last raw request
bytes` (bounded, e.g. 8 sessions / ~64 MB). On a hit with chain depth < K,
encode the new blob as a `.zd` against the in-memory parent — the parent is
already durable from the previous turn, so the chain never dangles. On any
miss (first turn, cache evicted, empty session id, restart), write a plain
`.zst` checkpoint exactly as today.

Fail-closed posture is unchanged: still exactly one fsync'd file write before
forwarding, still 502 on failure (invariants 1–2). Concurrent turns in one
session just branch the chain (it's a DAG; depth is carried in the header, so
checkpointing stays bounded per branch). Delta encoding at default level costs
one dictionary-primed EncodeAll over data the process is already holding — no
extra I/O. An occasional periodic compact (or `--compact-interval` in `serve`)
can re-tighten write-time deltas at best level if the extra ~2× matters.

### Explicitly deferred / rejected

- **CAS/CDC backend** — deferred as above; the `BlobStore` ref stays opaque so
  it remains a drop-in later.
- **Retention (`goxray gc --keep-days N`)** — orthogonal policy, not a fix for
  the exponent, and it deletes the audit record. If ever added: opt-in, whole
  sessions at a time (manifests + blobs), then index rebuild.
- **Compressing manifests** — they are O(N) and small; not worth a format
  change.
- **Best-level zstd on the hot path** — 1.2× for hundreds of ms of added
  time-to-first-upstream-byte on multi-MB bodies. No.

## Invariant compliance

1. *Nothing forwarded before PersistRequest commits* — unchanged; still one
   durable write, fail-closed.
2. *Terminal frames held for PersistResponse* — untouched (responses are not
   delta-encoded at all).
3. *Manifests are the source of truth; index rebuildable* — refs are unchanged
   and resolution lives inside `BlobStore`; rebuild and reconcile never touch
   blob internals.
4. *No credentials persisted* — untouched.
5. *Observer never blocks* — untouched.
6. *Dashboard read-only* — untouched; `web.Reader` still goes through `Get`.
7. *Verbatim capture* — every chain resolves to byte-identical originals
   (verified in the benchmark round-trip); zstd frame CRCs guard each link.

## Suggested order of work

1. `localBlobs.Get` `.zd` resolution + LRU, with unit tests for chains,
   branches, depth cap, and the `.zst`/`.zd` race.
2. `goxray compact` (dry-run first) + run against the real store to replace
   the synthetic savings estimate with a measured one.
3. Write-time deltas in `PersistRequest` behind the session LRU.
4. Revisit CAS only if cross-session dedup or global dedup ever becomes the
   bottleneck.
