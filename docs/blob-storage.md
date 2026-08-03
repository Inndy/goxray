# Blob storage: measured design

Status: **implemented**. Design measured 2026-07-27, shipped 2026-07-28.
Supersedes the delta-chain plan in [disk-space.md](disk-space.md), whose numbers
came from a synthetic corpus.

Code: `internal/store/{chunk,archive,compact,sweeper,fsck}.go`, `goxray archive`,
`goxray fsck`, `serve --archive-after`. Measured on the real store by
`TestArchiveFixtureRoundTrip` (opt-in, see Reproducing): **131.4 MiB → 4.6 MiB,
28.7×**, all 1384 bodies byte-identical after both archive and unarchive, fsck
clean, 2.3 s to archive the whole store.

**Recommendation: leave the write path exactly as it is, and archive each
session into one self-contained file after it goes quiet.** The archive holds
the session's request *and* response bodies as content-defined chunks,
deduplicated within the session, with the unique chunks compressed as a single
solid zstd stream. No shared dictionary. No object in one archive ever
references another.

Measured end to end against `~/.goxray`, real files, both read paths, every one
of the 1384 bodies verified byte-identical:

| | today | archived |
|---|---|---|
| whole store on disk | 131.4 MiB | **4.65 MiB (28.3×)** |
| files | 1384 | 14 + index |
| read, session resident | 894 MB/s | **11120 MB/s**, 0.38 ms worst |
| read, cold | 2.3 ms worst | 10.7 ms worst |
| build | — | 0.81 s for the whole store, offline |

---

## What the real store looks like

692 exchanges, 14 sessions, 8 hours of one working day.

| | raw | on disk | ratio |
|---|---|---|---|
| requests | 327.1 MiB | 129.5 MiB | 2.53× |
| responses | 13.3 MiB | 1.8 MiB | 7.26× |

Three facts shape everything below:

- **Requests are 98.6% of the store and grow monotonically.** 633 of 676
  turn-to-turn transitions grow; the 6% that shrink are context compactions. A
  171-turn session sent 100.7 MiB of request bodies whose *final* request was
  1.71 MiB.
- **Per-file zstd is stuck at 2.53× because the redundancy is across files.**
  Level 4 buys 1.06× for 5× the CPU. In the largest request, `tool_result`
  content is 54.6% of bytes and `thinking.signature` — encrypted, therefore
  incompressible — is another 10.4%. That ciphertext is re-sent verbatim every
  turn: free to dedup, impossible to compress.
- **The overlap is not a prefix.** A turn shares 63.7% of its bytes as a literal
  prefix with the previous turn but **0.2%** with its session's first turn.
  Claude Code rewrites the echo in place, so anything assuming append-only
  suffixes is wrong.

## Why session scope, and why no universal dictionary

A universal dictionary was measured, and it is the wrong instrument:

- Trained honestly — built from the first 20% of traffic, applied to the unseen
  80% — a 512 KiB dictionary was worth 21% on a global chunk store. On a
  *small* store it is a net loss: break-even is around 1.5 MiB of stored chunks,
  below which the fixed 512 KiB costs more than the ~31% it saves.
- It ages. It is a single file every blob in the store depends on, so it can
  never be rebuilt or retuned without rewriting everything, and losing it loses
  100% of the store — the worst blast radius of any option measured.
- `zstd.BuildDict` in klauspost/compress v1.19.1 panicked twice while
  measuring it (`slice bounds out of range`; `invalid offset in dictionary`).

A **solid pool inside one session archive does the same job better**. Chunks
compress against each other rather than against a global corpus, which is
strictly more relevant content, and it introduces no cross-archive coupling at
all. The comparison, requests only:

| | stored |
|---|---|
| global chunk store, per-chunk frames + 512 KiB trained dictionary | 4.22 MiB |
| **per-session archives, solid pool, no dictionary** | **3.92 MiB** |

Session scope is not a compromise here — it wins outright. Cross-session sharing
was only ever 29.7% of the dedup win; **70.3% is intra-session**, measured by
re-running with the chunk index reset at every session boundary. And unique
content holds at **2.9% of traffic** across the whole capture (5.1% at 33 MiB
in, 2.9% at 327 MiB in), so storage is O(unique content), not O(traffic).

Per session, unique content is **1.3×–2.8× that session's final request**, not
the sum of its requests. That is the floor for a store that keeps every turn.

## The design

### Two tiers

**Live tier — unchanged.** A turn of an active session is written exactly as it
is today: one zstd file per body, `writeFileSync`, fail-closed. `PersistRequest`
and `PersistResponse` are not touched, so invariants 1 and 2 cannot regress.
There is no chunking, hashing, or dependency on any other object on the hot
path.

**Archive tier.** When a session has been quiet for a threshold (say 30 min), or
on an explicit `goxray archive`, its bodies are rewritten into one file and the
individual `.zst` files are unlinked.

```
archives/<shard>/s-<sha256(session id)[:16]>.gxa
    header       magic, version, session length, chunk/frame/turn counts, offsets
    chunk table  per chunk id: hash16 | frame | offset | length   (zstd'd)
    recipes      per ref: zstd(delta-varint chunk ids)
    frame dir    per frame: offset | compressed length | raw length
    pool         solid zstd frame(s) over unique chunk content, first-use order
    session      the session id, at EOF
```

No index maps a ref or a session to a file; the path is recomputed on each read
(see [Finding an archive](#finding-an-archive-no-index-just-a-name)).

**Parameters**, each measured below: gear-hash CDC at **8 KiB average**
(2 KiB/32 KiB bounds), SHA-256 truncated to 128 bits in the chunk table,
zstd **level 4** with a 16 MiB window, **one solid pool** per archive holding
both request and response bodies.

`BlobStore.Put`/`Get` keep their signatures and **the ref never changes** — a
uuidv7 as today. `Get` tries `.zst`, then the archive index. No manifest is
rewritten, so invariant 3 is untouched.

### Dependency tracking

The dependency graph is recorded explicitly and is **a forest of depth one**:

```
turn ref ──recipe──▶ chunk ids ──chunk table──▶ pool offsets    (all inside one archive)
turn ref ──archive-index──▶ session archive                     (derived, rebuildable)
```

Every edge lives inside the archive that owns both endpoints. There are no
cross-archive edges, so:

- **Integrity is local.** `fsck` opens one archive, decodes its pool, and
  verifies every chunk against the hash in its own chunk table. A verdict on one
  session needs nothing else on disk.
- **Deletion is local.** Dropping a session is `rm` of one file plus an index
  rebuild. No refcounts, no GC, no tracing — which is the single biggest
  operational cost a global dedup store would have imposed.
- **Blast radius is one session, and it is a session the operator can name.**

### The one case that wants a cross-archive edge, and why it doesn't get one

A session that is archived and then resumed produces a second batch of turns
sharing most of their content with the first. Three ways to handle it, measured
by simulating a resume every 50 turns:

| | stored | cross-archive edges |
|---|---|---|
| independent generations (store shared content again) | 6.08 MiB | 0 |
| dependent generations (gen N reads gen N−1's pool) | 4.05 MiB | 10 |
| **rebuild the whole session archive** | **3.87 MiB** | **0** |

Rebuilding beats dependency-chaining on size *and* keeps the graph edge-free,
because a rebuilt archive dedups across the whole session instead of only
forward. It costs **59 ms per resumed session** — archives build at 440 MB/s,
and the largest session in this store rebuilds in about 200 ms.

So: on resume, discard the archive and rebuild it from the live `.zst` files
plus the archive's own contents. Write the new archive, fsync, rename over the
old, then unlink. Never two writers on one session — archiving takes the session
as its unit of exclusion.

### Read paths

Resolving a turn is: read its recipe, walk zigzag-varint deltas to chunk ids,
copy each chunk's range out of the materialised pool. Depth 1. No per-read codec
construction — one shared decoder process-wide.

- **Session resident** (the dashboard case: list a session, open its turns):
  materialise the pool once, then every turn in that session is memcpy.
  **11120 MB/s, 0.38 ms worst.** The largest session's pool is 10.70 MiB
  resident; most are under 5 MiB.
- **Cold** (one turn, nothing cached): materialise only the frames the recipe
  touches, then resolve. **10.7 ms worst**, 5.6 MiB decompressed on average.

Cold is ~8 ms worse than today's 2.3 ms. For a local dashboard opening one turn
that is imperceptible, and the moment a second turn of the same session is
opened the archive is ahead.

## Measured

All configurations round-trip verified byte-identical.

### Chunk size and level (requests only, one pool, no dictionary)

| chunk | level 4 | level 2 |
|---|---|---|
| 2 KiB | 4.03 MiB | 4.29 MiB |
| 4 KiB | 3.97 MiB | 4.23 MiB |
| **8 KiB** | **3.92 MiB** | — |
| 16 KiB | 3.91 MiB | — |

Chunk size barely matters once the pool is solid — the compressor recovers
whatever the chunker leaves. 8 KiB is chosen for the smallest chunk table and
the best cold read; 16 KiB is 0.01 MiB better and reads worse.

Splitting the pool into fixed frames costs compression and buys nothing, because
pools are already small: 256 KiB frames → 4.44 MiB, 1 MiB frames → 4.17 MiB,
versus 3.97 MiB solid.

### Responses belong in the same pool

| | stored |
|---|---|
| requests archived, responses left per-file | 3.92 + 1.80 = 5.72 MiB |
| both, separate request and response pools | 5.28 MiB |
| **both, one shared pool** | **4.65 MiB** |

Responses have 0.0% duplicate chunks, so this is not dedup — it is the
compressor matching a response against the request that echoes it back a turn
later. Splitting the pools destroys exactly that. (Note this reverses the answer
under a *global* per-chunk-frame store, where responses got 17% **worse**: the
mechanism is solid-pool locality, which only exists inside an archive.)

### Alternatives, requests only, 327.1 MiB raw

`amp` is bytes decompressed per byte returned, `depth` is chained codec
invocations, `blast` is worst-case exchanges lost with one file.

| strategy | stored | vs today | enc | dec worst | amp | depth | blast |
|---|---|---|---|---|---|---|---|
| per-file zstd default (today) | 129.5M | 1.00× | 219 MB/s | 2.5 ms | 1.00 | 1 | 1 |
| per-file zstd best | 122.0M | 1.06× | 44 MB/s | 2.2 ms | 1.00 | 1 | 1 |
| universal dictionary, per-file | 92.8M | 1.40× | 401 MB/s | 2.6 ms | 1.00 | 1 | all |
| delta-anchor K=8 | 33.9M | 3.82× | 337 MB/s | 2.8 ms | 1.79 | 1 | 8 |
| delta-chain K=16 | 16.8M | 7.69× | 393 MB/s | 9.7 ms | 7.73 | 15 | 16 |
| delta-chain K=32 best | 10.7M | 12.09× | 56 MB/s | 16.2 ms | 14.05 | 31 | 32 |
| global CDC + 512K dictionary | 4.7M | 27.6× | 871 MB/s | 3.3 ms | 1.00 | 1 | 89% of store |
| **per-session archive** | **3.9M** | **33.0×** | 440 MB/s | 0.38 ms* | 1.00 | 1 | one session |

\* session-resident; 4.24 ms cold.

Delta chains are the option this supersedes: 2–4× larger, up to 31 chained
decodes, a per-blob decoder that cannot be pooled, and they need a reliable
session *ordering* — which subagent turns (`session_id == ""`) and concurrent
branches do not provide. Chunk dedup inside an archive needs no ordering at all.

## Robustness

- **Loss.** One damaged archive costs one session — nothing else, because
  nothing else points into it. The store is 4.65 MiB total, so a second copy of
  every archive costs 4.65 MiB and is still 14× smaller than today. At this
  ratio redundancy stops being a budget question.
- **Detection, not silence.** Chunks are verified against the hashes in the
  archive's own chunk table, and zstd frame CRCs cover the pool. A corrupted
  archive reports as corrupt rather than serving wrong bytes — the property that
  matters most for a tool whose purpose is a faithful record.
- **Archiving is never destructive-in-place.** Build the new archive to a temp
  file, fsync, rename, fsync the dir, and only then unlink the `.zst` files. A
  crash at any point leaves either the live files or a complete archive, and
  `Get` tries `.zst` first, so the overlap window is safe rather than racy.
- **Hash collisions.** SHA-256 truncated to 128 bits in the chunk table:
  ~10⁻²¹ at 10⁹ chunks, and the scope is one session's few thousand chunks.
- **Lost archive-index.** Derived. Rebuilt by scanning archive turn directories,
  like `index.db`.
- **Never archive a live session.** Skip any session with a turn newer than the
  quiet threshold and any inflight manifest, the same guard `migrate-usage`
  already uses.

## Invariant compliance

1. **Nothing forwarded before `PersistRequest` commits** — the write path is not
   modified at all.
2. **Terminal frames held for `PersistResponse`** — likewise untouched.
3. **Manifests are the source of truth** — refs are unchanged, no manifest is
   rewritten, and `archive-index` is derived and rebuildable exactly like
   `index.db`. The archive is reachable only through `BlobStore.Get`.
4. **No credentials persisted** — untouched.
5. **Observer never blocks** — archiving is offline and out of the request
   goroutine entirely.
6. **Dashboard read-only** — `web.Reader` still goes through `Get`.
7. **Verbatim capture** — all 1384 bodies verified byte-identical after a real
   write/read round trip, and archiving never parses a body: CDC boundaries are
   content-defined, not JSON-aware.

## Operations: what actually triggers archiving

`goxray archive` is the primitive, in the mould of `migrate-usage`: dry run by
default, `--apply` to write, idempotent, and reversible — `--unarchive`
rematerialises `.zst` files exactly, since every recipe resolves to exact bytes.

`serve --archive-after 30m` runs the same code as a background sweep. Both
exist: the subcommand because it is testable and scriptable, the sweep because
disk does not stay flat if a person has to remember.

### The sweep is a cold-session scan, and it reads the index, not the hot path

Every few minutes the worker asks the index which sessions are cold —
`MAX(started_at) < now - quiet` — takes at most a few of them, archives them,
and sleeps. Bounded work per tick, so shutdown never waits on a long build.

Deriving coldness from the **index** rather than from the recorder matters. The
obvious alternative is to have the archiver observe exchanges and keep its own
`lastSeen` map, but the observer slot belongs to the dashboard `Hub`, and
invariant 5 exists precisely to keep extra work off the request goroutine. The
index already knows the last turn of every session, it is queried off the hot
path, and a stale answer only delays an archive by one tick. Nothing about
archiving should be able to touch a request.

A session that never goes cold — days long, still active — is archived anyway
once it has more than a few hundred unarchived turns, covering the snapshot and
leaving newer turns live. The next cold rebuild merges them.

### Rules that keep it safe

1. **Never archive a session with an inflight manifest**, or with a turn newer
   than the quiet threshold. Same guard `migrate-usage` already uses.
2. **Snapshot the ref set before building**, and unlink only the `.zst` files
   that were in that snapshot *and* are covered by the archive just written.
   Turns that arrive mid-build stay live and are picked up next sweep — so the
   archiver never needs to lock against the proxy.
3. **Install, then delete.** Temp file, fsync, rename, fsync dir, and only then
   unlink. `Get` tries `.zst` before the archive, so the overlap window serves
   correct bytes either way; a crash mid-unlink leaves duplicates, which are
   harmless and swept later.

   *And nothing may hold the target open while it is renamed over.* Go opens
   files on Windows without `FILE_SHARE_DELETE`, so there the rename fails with
   "Access is denied" rather than replacing anything, and both holders are this
   process's own: `verifyArchive` reads the session's already-archived bodies
   through `blobs`, which caches the very file about to be replaced, and in
   `serve` the dashboard's cache keeps the last two archives anyone viewed. The
   install drops its own (`localBlobs.forget`) and asks the rest of the process
   to drop its (`ArchiveOptions.Release`, wired to `Store.ReleaseArchive`)
   immediately before the rename. Without it a resumed session on Windows could
   never be archived at all: rebuild, verify, fail to install, and the same log
   line every tick. It is the same rule as the lock's close-before-unlink below,
   one step earlier. What remains is a race rather than a leak — a dashboard read
   landing between the release and the rename reopens the archive and costs that
   session one deferred retry.
4. **One archiver at a time, across processes.** `serve`'s sweep and a
   hand-run `goxray archive --apply` can otherwise rebuild the same session
   concurrently and unlink each other's inputs. This is the one place the design
   needs a real lock — an flock on a file in the store root, taken for the
   duration of a session's rebuild.

   *What shipped:* `archives/.lock`, created `O_EXCL`, because Go's `syscall`
   package has no flock on Windows and this repo ships a Windows binary. The
   weaker primitive needs three things flock gives for free, and the first
   version had none of them:

   - The file carries a **random token** identifying its owner, so a pass that
     was declared abandoned and taken over cannot delete its successor's lock on
     the way out — which was a way for two passes to end up running at once even
     without a crash.
   - The holder **refreshes it every 30 s**, so a lock is only stolen after five
     minutes of genuine silence. Before that, a merely *slow* pass could be
     stolen from, and its stale snapshot could then rename over a newer archive
     and drop refs whose live blobs the newer pass had already unlinked.
   - Every step that deletes anything **re-reads the token first**, and the
     install re-stats the archive it verified against immediately before the
     rename. A pass that has lost the lock, or whose target changed while it was
     building, aborts instead of installing.

   - Release is **close, then unlink**, in that order. Go opens files on Windows
     without `FILE_SHARE_DELETE`, so removing a file this process still holds
     open fails there; the first version removed first and so left its lock
     behind on every successful pass, blocking the next one until the TTL.

   What remains is a stat-to-rename window of microseconds, reachable only by a
   process that was silent for five minutes and then resumed mid-pass. `fsck`
   reports the outcome if it ever happens; the archive itself cannot be corrupted
   by it, only replaced by an older complete one.

   The honest reading is that a lease is not a lock and no amount of checking
   makes it one — the fix is `syscall.Flock` on unix and `LockFileEx` (in the
   `x/sys` already in the build) on Windows, held on `archives/.lock` for the
   pass, which deletes the token, the heartbeat, the TTL and the stealing
   outright. It costs nothing at the install: the lock is on the lock file, not
   on the archive, so `os.Rename` is untouched. It is not done because the
   remaining window needs a process stopped for five minutes in the middle of a
   pass.

5. **A session id never becomes a filename.** `metadata.session_id` is whatever
   the recorded request said, and finding an archive by name (below) makes it a
   path. When the name *was* the id, `../../../x` as a session id pointed an
   applied sweep at a `.gxa` path outside the store.

   The first fix normalized the id once, where it enters the store, and had
   `archiveFile` refuse anything that was still not a name. That worked, and it
   cost three things: a predicate that had to be correct, a boundary that had to
   be the only way in, and a class of session — pre-boundary manifests holding raw
   ids — that could not be archived at all.

   The name is now `s-<sha256(session id)[:16]>`, which is defined for **every**
   byte string. Nothing about a recorded id is checked, trusted, or rejected
   before it becomes a path, because no part of it reaches the path. The escape is
   not blocked; it is not expressible. `archiveFile` is total apart from the empty
   session, and that one case is about grouping rather than safety: a turn with no
   session id has nothing to be folded in with.

   The cost is a filename no one can read, which the archive pays back by
   recording its own session id in a trailer. That is what lets `fsck` check the
   file against where it is filed and name the session that would put a misfiled
   one right — a case that used to be diagnosable but not fixable. `--unarchive`
   still restores from the files it finds on disk rather than from session ids, so
   recovery reads a directory and never takes a path from a manifest.

   Existing stores are moved by `tools/migrate-archive-names.py`, not by Go. The
   trailer sits after every section, so no offset depends on it and the upgrade is
   an append plus a four-byte header patch — small enough to be a script, which is
   better than a compatibility path the store would carry forever. A v1 archive
   still reads; it just cannot say whose it is.

6. **Every length in an archive header is checked before it is allocated.** The
   file is the thing whose damage `fsck` exists to find, so it cannot be trusted
   to size a buffer: counts and offsets are validated against the file size and
   against limits the builder itself enforces. A flipped `nrefs` used to ask for
   a four-billion-entry map and take the reading process down with it.

Rebuild costs 59 ms for a typical session and about 200 ms for the largest in
this store, so there is no need for throttling beyond the per-tick cap.

### It is compaction, not GC

The framing matters for what the worker is allowed to do. Nothing here is
reclaimed by reachability, and no object's liveness depends on another's: the
worker rewrites a session and deletes only inputs it has just proven
reproducible. There are no refcounts to get wrong, which is the main thing
session-scoped archives buy over a global chunk store.

### One thing the implementation added

`Max` bounds the sessions a sweep *archives*, not the ones it looks at. Capping
the candidate list instead starves the tail: the list is ordered
most-recent-first, so already-archived sessions would fill the budget on every
tick and older ones would never be reached. Candidates are also filtered by one
`stat` — an archive newer than the session's last turn already covers it — so a
store full of archived sessions does not re-examine all of them every tick.

Attempts are capped separately, at twice `Max`, because bounding only successes
lets sessions that fail every time make one tick unboundedly long. A session is
counted as archived — and its bytes as reclaimed — only after the archive is
installed and the blobs are gone.

Neither cap is fairness, though, and that took a second look to see. Candidates
come newest-first, so whatever fails is the *same* set next tick: eight sessions
that cannot be archived at all — an archive path occupied by something else, a
body over the format limit — exhaust the attempt budget on every tick forever,
and nothing older is ever reached. So the pass reports which sessions failed, and
the sweep holds each one back for a doubling cooldown (capped at an hour, and
forgotten after two). A broken session costs one attempt and then gets out of the
way; a transient failure is retried within a tick or two. A hand-run `goxray
archive` defers nothing — someone who asked for it wants everything tried now.

### Finding an archive: no index, just a name

There is no ref → archive index, persisted or otherwise. An archive is found by
name — `archives/<shard>/s-<sha256(session id)[:16]>.gxa`, sharded on the last
two characters by the same `shardPath` helper blobs and manifests use.

That works because every blob read in the tree already holds its
`*model.Exchange` and therefore its session id: `cmdRaw`, both dashboard detail
reads, the dashboard raw endpoint, and `MigrateUsage`. Not one caller has a bare
ref. So `Get` takes the session:

```go
Get(session, ref string) ([]byte, error)   // BlobStore, store.Reader, web.Reader
```

`Put` is unchanged — it always writes `.zst`. `Get` tries `.zst` first, then the
computed archive path. An empty session means "cannot be archived", which is
true by construction: sessionless turns are skipped by the archiver.

The alternatives were all permanent complexity for a lookup the caller can
already do: scanning archive headers into an in-memory locator (a per-invocation
cost for the CLI, plus rescan-on-miss to survive a concurrent `goxray archive`);
a persisted locator file (derived state with invalidation to get wrong); a table
in `index.db` (worst — `rm ~/.goxray/index.db*` is a *documented* repricing
procedure, and it would break blob reads until the rebuild finished).

Because the path is computed rather than remembered, an archive appearing
mid-run needs no invalidation. Two archives stay open with their pools decoded
(`archiveCacheMax`), revalidated with one `stat` before reuse so a rebuild that
renamed over the path is not served from a stale inode — and dropped outright
just before such a rebuild is installed, because on Windows a cached handle
blocks the rename (rule 3 above).

A digest name keeps all of that. What it gives up is a minted name — a uuid per
archive, say, which would make the filename unconditionally safe without hashing
anything. That was considered and rejected: a minted name has no relationship to
the session, so it reintroduces exactly the lookup this section exists to avoid,
and it costs two further properties. Installing a rebuilt archive stops being one
atomic rename over a known path and becomes write-new, update-mapping,
unlink-old, so a crash leaves an orphan file or a dangling entry and something
has to collect them. And "one session, one archive" stops being structural,
which is what `verifyArchive`'s refusal to drop a ref the current archive holds
depends on. Deriving the name from the session costs a digest; not deriving it
costs a mapping, a GC, and an invariant.

## Deliberately not done

- **Write-time chunking.** Measured and it works — 30.7× with a *faster* write
  path (worst turn 16.4 ms vs today's 31.9 ms, because 97.3% of chunks are
  already stored so the compressor sees 2.9% of the bytes). Rejected anyway:
  it puts a shared mutable structure behind the fail-closed barrier for a
  benefit archiving already delivers.
- **A universal dictionary**, for the reasons above.
- **Cross-archive dependencies**, measured as worse than rebuilding.
- **Retention/GC** — orthogonal, and it deletes the audit record. Note the
  archive makes it trivial if it ever arrives: whole sessions, one file.

## Open risks

- **One capture: 8 hours, one user, one repo, arm64.** The session-isolated
  bound (70.3% of the win is intra-session) is the number to trust if a
  different store looks different. Re-run `archive` before believing any ratio
  here elsewhere.
- **Pool residency scales with session length.** 10.70 MiB for a 342-body
  session. A pathological session — thousands of turns, no compaction — would
  want the pool split; the split variants are measured above and cost ~5%.
- **Do not use `SpeedFastest` with dictionaries** if one is ever reintroduced:
  measured 899 KB versus 85 KB at `SpeedDefault` on the same delta.

## Reproducing

The shipped path is verified against a real store by an opt-in test, which
copies the store first so it never archives someone's actual data:

```bash
GOXRAY_STORE_FIXTURE=~/.goxray go test ./internal/store/ -run Fixture -v
```

It hashes every body through `Reader.Blob`, archives, re-hashes, runs `fsck`,
checks idempotence, unarchives, and re-hashes again.

The design numbers come from `docs/lab`, a nested module (so `go build ./...`
and `go test ./...` ignore it):

```bash
cd docs/lab
GOXRAY_DIR=~/.goxray go run . archive 8k best both   # the recommendation, verified
GOXRAY_DIR=~/.goxray go run . generations 50         # the resume comparison
GOXRAY_DIR=~/.goxray go run . baseline               # today's path, same accounting
GOXRAY_DIR=~/.goxray go run . bench req              # the alternatives table
```

Also `stats`, `content`, `shape`, `scale`, `holdout`, `blast`, `micro`,
`recipe`, `design` (the rejected write-time variant). `archive` materialises a
real store under `$TMPDIR/goxray-lab` and verifies every body byte-for-byte.
