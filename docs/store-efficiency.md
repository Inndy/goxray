# Where the bytes are: a store-wide audit

Status: **findings 1 and 3 implemented 2026-08-04** (manifest packs, and the
window/pool-frame reconciliation). Finding 2 was measured and deliberately left
alone; finding 4 stands as the measured cost of invariant 8. Audited against a
live `~/.goxray` (7 archived sessions across 6 projects, several days, amd64).

What shipped, measured on the real store: **792 manifests that cost 3.09 MiB of
4 KiB clusters now cost 113 KiB — 27.9× physical, 13.9× logical** — in 15
per-session `.gxm` packs. `TestArchiveFixtureRoundTrip` proves all 2655 bodies
*and* every manifest come back byte-identical after both archive and unarchive,
with `fsck` clean.

This does not supersede [blob-storage.md](blob-storage.md). That document designs
the archive tier and every number in it still holds; this one audits the *whole*
store and finds that the archive tier is now the part that works. The remaining
cost is in a tier with no compression at all, and in the live tier.

Reproduce with `python3 tools/store-audit.py` (read-only).

---

## The headline

178.0 MB of recorded request and response bodies live in 1.53 MiB of archives.
The archive tier is not where the remaining bytes are:

| tier | logical | on disk | compressed? |
|---|---|---|---|
| archives (7 files) | 178.0 MB of bodies | **1.53 MiB** | yes — solid zstd, best |
| live blobs (125 files) | 12.29 MiB raw | **4.47 MiB** | yes — per-blob zstd, `SpeedDefault` |
| manifests (560 files) | 1.10 MiB | **2.19 MiB** | **no** |
| `index.db` + `-wal` + `-shm` | — | 2.54 MiB | rebuildable projection |

**Two tiers hold more than every archive combined, and one of them is not
compressed at all.** The live tier was mid-session when measured, so it is the
high-water mark of one active session rather than a steady state — but that is
the point: a store that is *recording* spends most of its disk outside the
archives.

The three safe fixes below are worth more than the one that would cost an
invariant.

## Why a compression ratio is the wrong number

The archive tier reports 110.8× against the sum of body sizes. That number is
mostly a statement about session length, not about the compressor.

Claude Code re-sends the whole conversation every turn, so for a session of N
turns `Σ|body|` grows as O(N²) while novel content grows as O(N). The ratio
therefore rises with N on its own. Decomposed:

| factor | value | what it measures |
|---|---|---|
| chunk dedup | 6.70× | protocol redundancy — **scales with N** |
| zstd on unique chunks | 17.20× | the compressor's actual work |
| product | 110.8× | mostly how long the sessions were |

The decisive demonstration that the dedup factor measures nothing usable:
halving `chunkAvg` from 8 KiB to 4 KiB **more than doubles it, 6.70× → 14.21×,
and moves final stored bytes by −0.6%.** zstd was already recovering everything
the coarser chunker left behind. This is the same conclusion blob-storage.md
reached from the other direction ("chunk size barely matters once the pool is
solid"), restated as a warning about the metric.

**Use stored bytes per novel byte instead.** It is O(1) in session length, and it
immediately exposes a fixed per-session cost:

| session | novel pool | stored | bytes per KiB of novel content |
|---|---|---|---|
| 226 turns | 14.80 MiB | 456 KB | **30** |
| 6 shorter ones | 1.06–3.07 MiB | 111–265 KB | **77–128** |

Short sessions cost 3–4× more per novel byte, because each archive stores the
system prompt and tool definitions in full. That floor is what
[finding 4](#finding-4-what-invariant-8-costs-30-and-it-is-inherent) is about.

## Finding 1: manifests were never compressed — fixed

`internal/store/manifest.go` was a bare `json.Marshal` followed by a plain write,
and archiving never touched one: the archive tier folds blob refs, and manifests
are not blobs.

**Fixed** by `internal/store/mpack.go`: the sweep now folds a cold session's
manifests into one `m-<sha256(session)[:16]>.gxm` beside its archive — a directory
of lengths plus one solid zstd frame — and deletes the loose copies once it has
reopened the pack from disk and byte-compared every one of them. Reads try the
loose file first, exactly as a blob read tries `.zst` before the archive, so the
overlap window and `--unarchive` are correct by construction.

The measurements that shaped it are below.

| | bytes |
|---|---|
| 560 manifests, logical | 1,150,717 |
| the same, physical at 4 KiB clusters | 2,293,760 |
| per-file zstd L10 | 537,957 (2.1×) |
| **one solid zstd L19 stream** | **64,716 (17.8× logical, 35× physical)** |

Two things make this the largest and cheapest win in the store:

- **Half the physical cost is slack.** A manifest averages 2054 bytes and gets a
  4 KiB cluster. The 560 files are also spread across 228 shard directories —
  2.5 files each — so directory entries cost nearly as much as the data does.
- **The redundancy is across files, not inside them**, which is why per-file
  compression only reaches 2.1×. `res_headers` alone is **39.0%** of all
  manifest bytes and is near-identical `anthropic-ratelimit-*` boilerplate on
  every turn; `req_headers` is another 22.1%. Nearly two thirds of the tier is
  the same headers written 560 times.

Grouping matters, and per-session is close to optimal:

| grouping | packed | ratio | files |
|---|---|---|---|
| per-file zstd | 592,623 | 2.1× | 618 |
| per-shard (last 2 hex of the id) | 305,417 + 966 KB slack | 4.1× | 236 |
| **per-session** | **78,193** | **16.2×** | **11** |
| one global pack | 71,249 | 17.8× | 1 |

Per-session lands within 9% of a single global pack while keeping blast radius,
integrity and deletion per-session, and it reuses `archiveName`-style digest
naming, the `archives/.lock` pass lock, the quiet/inflight gates and
verify-then-delete unchanged. Per-shard is ruled out twice over: the shard is the
*tail* of a uuidv7, so a shard groups ~2.5 manifests today, and 236 files of 4 KiB
slack costs more than the compression saves.

Two things this deliberately does not do.

**It does not compress at write time.** That would put a codec behind the
fail-closed barrier for a benefit the sweep can deliver later. Manifests are still
written individually, immutably, and uncompressed on the hot path, and are still
the source of truth, so invariant 3 holds unchanged — `index.db` rebuilds from
them whether they are loose or packed, which
`TestIndexRebuildAfterPackingIsIdentical` pins row for row.

**It does not put them in the session's `.gxa`.** That was the original plan, and
the tests rejected it: co-locating them makes the record share fate with the data.
A flipped byte in the pool, a misfiled archive, or a v1 downgrade then destroys the
manifests too, and `fsck` degrades from "these six exchanges are unreadable" to
"this archive is broken" — precisely the diagnosis it exists to give. A sibling
file also keeps `migrate-usage`, which rewrites manifests, from having to rebuild a
14.8 MiB body pool to change one JSON field. The cost is a second file kind and
~45 KB of cluster slack.

## Finding 2: the live tier is an order of magnitude off

`internal/store/blob.go:55` compresses each body on its own at
`zstd.SpeedDefault`. Per-blob compression cannot see the redundancy that matters,
because consecutive turns of a session share almost everything:

| 125 live blobs | bytes |
|---|---|
| raw | 12,881,360 |
| per-blob zstd, as shipped | 4,684,152 (2.75×) |
| **the same bytes as one solid stream** | **351,328 (36.7×)** |

A 13.3× gap, and at this moment the live tier alone holds 3× the bytes of all
seven archives. The absolute figures move with whatever is unarchived — four runs
over a few hours saw 56, 125, 141 and 158 blobs — but the shipped ratio stayed in
2.68–2.75× and the gap in 11–13.4× every time, so **the gap is the durable
finding and the byte counts are a snapshot.**

This is by design: the live tier is deliberately dumb so the write path stays
fail-closed and fast. But the *size* of the gap is worth knowing, because two
cheap changes narrow it without touching that design:

- raise the blob level from `SpeedDefault`; blob-storage.md measured only 1.06×
  from `SpeedBest` on *whole requests*, so verify before believing much here.
- shorten `--archive-after` (default 30m). The gap is a function of how long
  bodies sit in the live tier, and nothing else.

Neither is a substitute for archiving, which already reaches 110×.

## Finding 3: the solid-pool window was nearly binding — fixed

blob-storage.md lists "pool residency scales with session length" as an open
risk, at 10.70 MiB for a 342-body session. It is now closer than that:
`archiveWindow` is `1<<24` (16 MiB) and **the largest session's unique pool is
14.80 MiB — 92% of the window.**

Sweeping the window on that exact pool shows what happens past the edge:

| window | stored | vs 16 MiB |
|---|---|---|
| 1 MiB | 667,242 | +46.2% |
| 2 MiB | 644,617 | +41.2% |
| 4 MiB | 609,203 | +33.5% |
| 8 MiB | 554,009 | +21.4% |
| **16 MiB** | **456,478** | — |
| 32 MiB | 456,478 | 0.0% |

The penalty is steep and it is not a cliff you notice: a session ~10% longer
than today's largest simply starts compressing worse, with no error and no log
line.

**Fixed** by making the two constants one decision. `poolFlushAt` and
`archiveWindow` are now both `1<<25` (32 MiB), so the window always spans a whole
frame — which is what `archiveWindow`'s own comment always claimed and the code
never did. Past 32 MiB the builder splits into frames instead, which the earlier
measurements put at ~5%, so what used to be a silent 21% truncation is now a
gradual and documented ~5%.

`archiveMaxFrameRaw` deliberately did **not** move down with `poolFlushAt`. It is a
*format limit*, and a build from before the change could have flushed a 64 MiB
frame; a limit that rejected it would be rejecting a file goxray wrote. Limits only
ever move up.

**`chunkAvg` stays at 8 KiB.** Dropping it to 4 KiB was in the plan — it would take
the largest pool from 14.80 MiB to 5.20 MiB for −0.6% stored bytes — but its whole
purpose was window headroom, and equalising the window achieves that directly and
by construction. blob-storage.md chose 8 KiB partly on cold-read latency, which has
not been re-measured, so changing it now would trade a measured property for an
unmeasured one to solve a problem already solved.

Chunk sizing is also coarser than configured, which is what makes the pool large:
mean chunk is 13,280 bytes against a nominal 8 KiB, and **8.8% of chunk
instances are pinned at `chunkMax` (32 KiB), carrying 21.8% of all bytes.**

| chunk size | instances | share | bytes | share |
|---|---|---|---|---|
| < 4 KiB | 1,664 | 12.4% | 4,761,330 | 2.7% |
| 4–8 KiB | 4,026 | 30.0% | 25,260,835 | 14.2% |
| 8–16 KiB | 3,609 | 26.9% | 40,556,078 | 22.8% |
| 16–32 KiB | 2,919 | 21.8% | 68,561,128 | 38.5% |
| = 32 KiB (`chunkMax`) | 1,186 | 8.8% | 38,862,848 | 21.8% |

## Finding 4: what invariant 8 costs — ~30%, and it is inherent

Invariant 8 makes every archive self-contained. Measured on this store, that
costs about 30% of archived bytes, and **no cheap trick recovers it.** All
figures net of storing whatever is shared:

| approach | vs per-session | coupling introduced |
|---|---|---|
| trained 110 KiB dictionary | **−7.6%** (a loss) | one shared file |
| shared 256 KiB raw-content prefix | +6.8% | one shared file |
| grouping archives per project (`cwd`) | +6.7% | none — only a grouping change |
| shared 1 MiB raw-content prefix | +27.3% | one shared file |
| shared 4 MiB raw-content prefix | +30.1% | one shared file |
| one global pool | +30.4% | total |

Three things this settles:

- **A trained dictionary is the wrong instrument, and the reason is not
  distribution shift.** It is that the shared material is *megabytes of
  long-range literal matches* — identical tool definitions, identical system
  prompts, the same files re-read in different sessions. A 110 KiB COVER
  dictionary cannot hold that at any level of statistical fit: it saves 693 bytes
  across the whole store and then costs 110,592 to store, a net loss of 7.6%,
  while a 1 MiB *raw* prefix of the same content wins 27.3%. This is
  blob-storage.md's conclusion — "on a small store it is a net loss" — arrived at
  independently, with the mechanism identified: the instrument is too small for
  the redundancy, not mistuned to it.
- **Per-project grouping is not the shortcut it looks like.** It recovers 6.7% of
  the available 30% here, because the redundancy is cross-project boilerplate
  rather than project content. (Weak evidence: only one `cwd` in this store has
  two sessions.)
- **The 30% is a real ceiling, reachable only by real sharing.** Every mechanism
  that gets near it introduces a file that every archive depends on — exactly the
  forest-of-depth-one property invariant 8 exists to keep, and the blast radius
  blob-storage.md measured as the worst of any option.

**Recommendation: keep invariant 8.** Per-session deletability and per-session
integrity are worth more than 30% of the smallest tier, and findings 1–3 are
worth more than that 30% anyway.

## Not a finding: side-turns already fold correctly

Suspected, measured, and false: side-requests are not escaping their session.
Of 517 turns, **150 (29%) are no-tool side calls** — Sonnet-5 title and summary
generation, some Opus-4.6 — and **every one carries a `session_id`**, so each is
already folded into its parent session's archive.

Only 8 turns are sessionless, and therefore unarchivable by construction
(invariant 9: `archiveFile` is total except for `session == ""`):

| turns | url | note |
|---|---|---|
| 6 | `/api/hello` | dashboard probe, a few hundred bytes |
| 1 | `/` | ditto |
| 1 | `/v1/messages/count_tokens?beta=true` | **carries a full conversation body** |

Irrelevant at 8 turns. Worth remembering that `count_tokens` is the one shape
that is both large and permanently unarchivable, so a client that starts calling
it per keystroke would accumulate live blobs nothing can fold.

Two related observations from the same pass, neither actionable:

- **Requests interleave two streams** (~116 KB and ~280 KB bodies in the largest
  session). Neighbours *in time* often share a large suffix and almost no
  prefix, while neighbours within a stream share **99.5%** of their prefix. Chunk
  dedup is order-independent, so this costs nothing — but it is why any
  prefix- or ordering-based scheme would misread this store, as
  blob-storage.md's rejection of delta chains already concluded.
- Archive metadata overhead is 3.9% of archive bytes (table 40,343, refs 21,711,
  frame dir 112), or 23.9 bytes per unique chunk and 22 bytes per body. Nothing
  to win here.

## What was done

| # | change | result |
|---|---|---|
| 1 | fold manifests into a per-session `.gxm` at sweep time | **27.9× physical on 792 manifests** (3.09 MiB → 113 KiB) |
| 3 | `poolFlushAt` = `archiveWindow` = 32 MiB | removes a silent 21% cliff; `chunkAvg` left at 8 KiB |
| 2 | nothing | measured: −6% for 3.3× slower. Not worth it |
| 4 | nothing | ~30% available, costs invariant 8. **Not recommended** |

Upgrading an existing store needs no migration and no flag: a session whose
manifests are still loose is not "already archived" any more, so the next sweep
folds them. It does not rebuild the body archive to do it — with nothing live to
fold, the archive already holds every body, so only the small pack is written.

## Operational notes

- `goxray fsck` gained a pass over packs: it verifies each one, resolves every
  manifest in it, and reports a pack filed where nothing can read it. Output now
  carries `N manifest packs checked, M manifests resolved`.
- `goxray archive` reports manifests alongside bodies.
- `goxray archive --unarchive --apply` restores manifests as loose `.json` files
  before removing either file, so a failure mid-restore cannot cost the record.
- Sessionless turns keep loose manifests forever, for the same reason they keep
  live blobs: there is no group to fold them into. 16 turns in this store —
  `/api/hello`, `/`, and one `count_tokens`.

## Open risks in this audit

- **One store, one user, one week, amd64.** blob-storage.md's warning applies
  here too: re-measure before believing any ratio elsewhere.
- **The store was live throughout.** Manifest and live-blob counts grew during
  measurement (56 → 125 blobs); the archive figures are stable because archives
  are immutable once written, but the live-tier numbers are one session's
  high-water mark.
- **Compression figures here come from libzstd via python-zstandard, while the
  shipped path uses klauspost/compress.** Absolute byte counts differ by a few
  percent at the same nominal level — the per-session `avg8K/w24` configuration
  measures 1,463,685 here against 1,544,261 actually on disk. Every comparison
  in this document is between configurations measured the same way, so the
  deltas hold even though the absolutes are not the shipped encoder's.
- **`chunkAvg` 4 KiB has not been re-measured for cold read latency**, which is
  the axis blob-storage.md chose 8 KiB on. This is why it was left alone.
- **Encoder memory at the new 32 MiB window is not measured.** It is 2× the old
  window and the builder is the only thing that pays it, once per pass, but the
  number is an assumption rather than a measurement.
- **The two fixture tests were broken before this work and are now fixed.** They
  copied `blobs` and `manifests` but not `archives`, so on any store that had
  actually been archived every already-archived body read as missing — 5790 errors
  on the real store. They are the repo's headline "verified against real data"
  check, and they had been silently measuring nothing since archiving shipped.

## Reproducing

```bash
python3 tools/store-audit.py               # tier table, manifest and live-tier findings
python3 tools/store-audit.py --deep        # also: chunk histogram, window sweep, sharing variants
python3 tools/store-audit.py --home X      # audit a store other than ~/.goxray
```

The script is read-only: it opens no lock, writes nothing, and binds no port, so
it is safe against a running `serve`. `--deep` re-chunks every archived body in
Python and takes a few minutes; it parallelises across archives.

Requires `zstandard` (as `tools/migrate-archive-names.py` does).
