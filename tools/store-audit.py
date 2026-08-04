#!/usr/bin/env python3
"""Audit where a goxray store's bytes actually are.

Regenerates the measurements in docs/store-efficiency.md. Read-only: it takes no
lock, writes nothing, and binds no port, so it is safe to run against a live
`serve`.

    python3 tools/store-audit.py                 # tier table + manifest/live-tier findings
    python3 tools/store-audit.py --deep          # + chunk histogram, window sweep, sharing
    python3 tools/store-audit.py --home /path    # a store other than ~/.goxray

`--deep` re-chunks every archived body in Python, which takes a few minutes; it
parallelises across archives. It exists because the interesting questions are
about configurations the store was *not* written with.

Absolute byte counts from this script are libzstd's, while goxray writes with
klauspost/compress. The two differ by a few percent at the same nominal level, so
trust the comparisons between rows rather than any single number.

Requires the `zstandard` package.
"""

from __future__ import annotations

import argparse
import collections
import glob
import hashlib
import json
import os
import struct
import sys
from concurrent.futures import ThreadPoolExecutor
from multiprocessing import Pool
from pathlib import Path

try:
    import zstandard as zstd
except ImportError:
    sys.exit("this script needs the zstandard package: pip install zstandard")

CLUSTER = 4096  # NTFS/ext4 default; only affects the "physical" column

# Mirrors internal/store/chunk.go. Kept as literals rather than derived so a
# change there shows up as a diff here instead of silently retuning the audit.
CHUNK_AVG = 8 << 10
GEAR_SEED = b"goxray/gear/1"
M64 = (1 << 64) - 1


def human(n: float) -> str:
    for unit in ("B", "KiB", "MiB", "GiB"):
        if abs(n) < 1024 or unit == "GiB":
            return f"{n:,.2f} {unit}" if unit != "B" else f"{int(n):,} B"
        n /= 1024
    return ""


# --- archive parsing (format documented in internal/store/archive.go) --------


def _dctx() -> zstd.ZstdDecompressor:
    return zstd.ZstdDecompressor()


def read_archive(path: str) -> tuple[str, dict[str, bytes], int]:
    """Return (session id, {ref: body}, file size). Reassembles every body."""
    raw = Path(path).read_bytes()
    if raw[:4] != b"GXA1":
        raise ValueError(f"{path}: bad magic {raw[:4]!r}")
    _ver, slen, nchunks, nframes, nrefs = struct.unpack_from("<HHIII", raw, 4)
    table_off, table_len = struct.unpack_from("<QI", raw, 20)
    refs_off, refs_len = struct.unpack_from("<QI", raw, 32)
    dir_off, _dir_len = struct.unpack_from("<QI", raw, 44)

    dec = _dctx().decompressobj
    table = dec().decompress(raw[table_off : table_off + table_len])
    refs = dec().decompress(raw[refs_off : refs_off + refs_len])

    frames = []
    for i in range(nframes):
        off, comp_len, _raw_len = struct.unpack_from("<QII", raw, dir_off + i * 16)
        frames.append(dec().decompress(raw[off : off + comp_len]))
    # chunk table entry: hash16, frame u32, off u32, len u32
    locs = [struct.unpack_from("<III", table, i * 28 + 16) for i in range(nchunks)]

    pos = 0

    def varint() -> int:
        nonlocal pos
        val = shift = 0
        while True:
            byte = refs[pos]
            pos += 1
            val |= (byte & 0x7F) << shift
            if not byte & 0x80:
                return val
            shift += 7

    bodies: dict[str, bytes] = {}
    for _ in range(nrefs):
        (rlen,) = struct.unpack_from("<H", refs, pos)
        pos += 2
        ref = refs[pos : pos + rlen].decode()
        pos += rlen
        (plen,) = struct.unpack_from("<I", refs, pos)
        pos += 4
        end = pos + plen
        ids, cur = [], 0
        while pos < end:
            zig = varint()
            cur += (zig >> 1) ^ -(zig & 1)  # zigzag delta
            ids.append(cur)
        bodies[ref] = b"".join(
            frames[locs[i][0]][locs[i][1] : locs[i][1] + locs[i][2]] for i in ids
        )
    session = raw[len(raw) - slen :].decode() if slen else ""
    return session, bodies, len(raw)


# --- content-defined chunking, a port of nextChunk ---------------------------


def _gear() -> list[int]:
    return [
        int.from_bytes(hashlib.sha256(GEAR_SEED + bytes([i])).digest()[:8], "little")
        for i in range(256)
    ]


GEAR = _gear()


def chunk_lengths(data: bytes, avg: int = CHUNK_AVG) -> list[int]:
    lo, hi, mask = avg // 4, avg * 4, avg - 1
    gear, out, pos, n = GEAR, [], 0, len(data)
    while pos < n:
        rem = n - pos
        if rem <= lo:
            out.append(rem)
            break
        limit = min(rem, hi)
        h = 0
        cut = limit
        for i in range(lo, limit):
            h = ((h << 1) + gear[data[pos + i]]) & M64
            if h & mask == 0:
                cut = i + 1
                break
        out.append(cut)
        pos += cut
    return out


def _chunk_job(args: tuple[str, int]) -> dict:
    """Worker: re-chunk one archive at one chunkAvg. Loads its own file."""
    path, avg = args
    session, bodies, size = read_archive(path)
    uniq: dict[bytes, bytes] = {}
    hist: collections.Counter[int] = collections.Counter()
    raw_total = instances = 0
    for body in bodies.values():
        raw_total += len(body)
        off = 0
        for ln in chunk_lengths(body, avg):
            piece = body[off : off + ln]
            off += ln
            instances += 1
            hist[ln] += 1
            key = hashlib.blake2b(piece, digest_size=16).digest()
            if key not in uniq:
                uniq[key] = piece
    return {
        "session": session,
        "avg": avg,
        "size": size,
        "raw": raw_total,
        "instances": instances,
        "hist": dict(hist),
        "pool": b"".join(uniq.values()),
        "nchunks": len(uniq),
    }


# --- compression helpers ----------------------------------------------------


def comp(data: bytes, level: int = 19, window_log: int | None = 24, ldm: bool = False) -> int:
    kw: dict = {}
    if window_log:
        kw["window_log"] = window_log
    if ldm:
        kw["enable_ldm"] = True
    params = zstd.ZstdCompressionParameters.from_level(level, **kw)
    return len(zstd.ZstdCompressor(compression_params=params).compress(data))


def physical(paths: list[str]) -> int:
    return sum(((os.path.getsize(p) + CLUSTER - 1) // CLUSTER) * CLUSTER for p in paths)


# --- the audit --------------------------------------------------------------


def tier_table(home: Path) -> tuple[list[str], list[str], list[str], list[str]]:
    archives = sorted(glob.glob(str(home / "archives" / "*" / "*.gxa")))
    packs = sorted(glob.glob(str(home / "archives" / "*" / "*.gxm")))
    blobs = sorted(glob.glob(str(home / "blobs" / "*" / "*.zst")))
    manifests = sorted(glob.glob(str(home / "manifests" / "*" / "*.json")))
    print(f"store: {home}\n")
    print("=== tiers ===")
    a_bytes = sum(os.path.getsize(p) for p in archives)
    p_bytes = sum(os.path.getsize(p) for p in packs)
    b_bytes = sum(os.path.getsize(p) for p in blobs)
    m_log = sum(os.path.getsize(p) for p in manifests)
    idx = [
        str(home / n)
        for n in ("index.db", "index.db-shm", "index.db-wal")
        if (home / n).exists()
    ]
    print(f"  archives  {len(archives):5} files  on disk {human(a_bytes):>14}   bodies")
    print(f"  packs     {len(packs):5} files  on disk {human(p_bytes):>14}   manifests, folded")
    print(f"  blobs     {len(blobs):5} files  on disk {human(b_bytes):>14}")
    print(
        f"  manifests {len(manifests):5} files  on disk {human(physical(manifests)):>14}"
        f"   (logical {human(m_log)})  still loose"
    )
    print(
        f"  index     {len(idx):5} files  on disk "
        f"{human(sum(os.path.getsize(p) for p in idx)):>14}   rebuildable"
    )
    if packs and manifests:
        print(
            f"\n  loose manifests are the hot tier: {len(manifests)} not yet folded."
            f" Run `goxray archive --apply` to fold the cold ones."
        )
    return archives, packs, blobs, manifests


def pack_finding(packs: list[str], manifests: list[str]) -> None:
    """What folding the manifests actually bought, once some are folded."""
    print("\n=== manifest packs ===")
    if not packs:
        print("  none yet: no session has been archived since manifest packing shipped")
        return
    n = 0
    for p in packs:
        try:
            n += mpack_count(p)
        except Exception as exc:
            print(f"  ! {Path(p).name}: {exc}")
    on_disk = sum(os.path.getsize(p) for p in packs)
    print(f"  {len(packs)} packs holding {n} manifests in {human(on_disk)}")
    if n:
        # Loose manifests average ~2 KB of JSON in a 4 KiB cluster, so that is what
        # the packed ones would have cost had they stayed loose.
        mean = 2045
        was_logical = n * mean
        was_physical = n * CLUSTER
        print(f"  as loose files that would be ~{human(was_logical)} logical, {human(was_physical)} physical")
        print(f"  => {was_logical/on_disk:.1f}x logical, {was_physical/on_disk:.1f}x physical")
    print(f"  {len(manifests)} manifests are still loose (hot sessions, and sessionless turns forever)")


def mpack_count(path: str) -> int:
    """nman out of a .gxm header. Mirrors internal/store/mpack.go."""
    with open(path, "rb") as f:
        hdr = f.read(24)
    if len(hdr) < 24 or hdr[:4] != b"GXM1":
        raise ValueError(f"bad magic {hdr[:4]!r}")
    return int.from_bytes(hdr[8:12], "little")


def manifest_finding(manifests: list[str]) -> None:
    print("\n=== loose manifest tier (uncompressed by design; the sweep folds it) ===")
    if not manifests:
        print("  none loose: every manifest is packed")
        return
    blobs = [Path(p).read_bytes() for p in manifests]
    raw = b"".join(blobs)
    phys = physical(manifests)
    shards = len({Path(p).parent for p in manifests})
    solid = comp(raw, 19, None)
    per_file = sum(comp(b, 10, None) for b in blobs)
    print(f"  {len(manifests)} files in {shards} shard dirs, mean {len(raw)//len(blobs):,} B")
    print(f"  logical            {len(raw):>10,}")
    print(f"  physical @{CLUSTER}B    {phys:>10,}   ({100*(phys-len(raw))/phys:.0f}% slack)")
    print(f"  per-file zstd L10  {per_file:>10,}   {len(raw)/per_file:5.1f}x")
    print(
        f"  solid zstd L19     {solid:>10,}   {len(raw)/solid:5.1f}x logical, "
        f"{phys/solid:.0f}x physical"
    )
    cost: collections.Counter[str] = collections.Counter()
    for b in blobs:
        for k, v in json.loads(b).items():
            cost[k] += len(json.dumps({k: v}))
    total = sum(cost.values())
    print("  bytes by top-level key:")
    for k, v in cost.most_common(6):
        print(f"    {k:14} {100*v/total:5.1f}%")


def live_finding(blobs: list[str]) -> None:
    print("\n=== finding 2: the live tier compresses per-blob ===")
    if not blobs:
        print("  live tier empty (everything archived)")
        return
    dec = _dctx().decompressobj
    raws, on_disk = [], 0
    for p in blobs:
        on_disk += os.path.getsize(p)
        try:
            raws.append(dec().decompress(Path(p).read_bytes()))
        except Exception as exc:  # a blob mid-write, or a genuinely bad one
            print(f"  ! {Path(p).name}: {exc}")
    raw = sum(len(r) for r in raws)
    solid = comp(b"".join(raws), 10, None)
    print(f"  {len(blobs)} blobs")
    print(f"  raw                {raw:>10,}")
    print(f"  per-blob, as shipped {on_disk:>8,}   {raw/on_disk:5.2f}x")
    print(f"  one solid stream   {solid:>10,}   {raw/solid:5.2f}x   <- {on_disk/solid:.1f}x gap")


def deep(archives: list[str], avgs: list[int]) -> None:
    print("\n=== deep: re-chunking every archived body ===")
    work = [(p, a) for a in avgs for p in archives]
    with Pool(min(os.cpu_count() or 4, max(len(work), 1))) as pool:
        results = pool.map(_chunk_job, work)
    by_avg: dict[int, list[dict]] = collections.defaultdict(list)
    for r in results:
        by_avg[r["avg"]].append(r)

    raw = sum(r["raw"] for r in by_avg[CHUNK_AVG])
    print(f"  {len(archives)} archives, {human(raw)} of bodies\n")

    print(f"  chunk size distribution at chunkAvg={CHUNK_AVG >> 10} KiB:")
    hist: collections.Counter[int] = collections.Counter()
    for r in by_avg[CHUNK_AVG]:
        hist.update(r["hist"])
    inst = sum(hist.values())
    tot_b = sum(k * v for k, v in hist.items())
    hi = CHUNK_AVG * 4
    buckets = [(0, CHUNK_AVG // 2), (CHUNK_AVG // 2, CHUNK_AVG), (CHUNK_AVG, hi // 2), (hi // 2, hi), (hi, hi + 1)]
    print(f"    mean chunk {tot_b//inst:,} B over {inst:,} instances")
    for lo, high in buckets:
        c = sum(v for k, v in hist.items() if lo <= k < high)
        b = sum(k * v for k, v in hist.items() if lo <= k < high)
        label = f"= {hi>>10}K (chunkMax)" if lo == hi else f"[{lo>>10:>3}K,{high>>10:>3}K)"
        print(f"    {label:18} {c:6,} inst {100*c/inst:5.1f}%   {b:12,} B {100*b/tot_b:5.1f}%")

    print("\n  finding 3: window sweep on the largest session pool")
    big = max((r["pool"] for r in by_avg[CHUNK_AVG]), key=len)
    print(f"    pool = {human(len(big))}, archiveWindow = 16 MiB")
    ex = ThreadPoolExecutor(os.cpu_count() or 4)
    futs = {w: ex.submit(comp, big, 19, w, False) for w in range(20, 26)}
    base = futs[24].result()
    for w in range(20, 26):
        c = futs[w].result()
        print(f"    window 2^{w} ({(1<<w)>>20:>3} MiB): {c:>9,}   {100*(c-base)/base:+6.1f}%")

    print("\n  per-session pool sizes, by chunkAvg (16 MiB is the window):")
    for a in sorted(by_avg):
        sizes = sorted((len(r["pool"]) for r in by_avg[a]), reverse=True)
        print(f"    avg{a>>10:>2}K: " + ", ".join(f"{s/2**20:.1f}M" for s in sizes))

    print("\n  chunkAvg variants (per-session, L19, w24 = as shipped):")
    rows = []
    for a in sorted(by_avg):
        pools = [r["pool"] for r in by_avg[a]]
        pool_b = sum(len(p) for p in pools)
        nch = sum(r["nchunks"] for r in by_avg[a])
        futs = [ex.submit(comp, p, 19, 24, False) for p in pools]
        z = sum(f.result() for f in futs)
        tbl = nch * 24  # measured compressed cost per chunk table entry
        rows.append((a, pool_b, z, tbl, z + tbl))
    ref = next((t for a, _, _, _, t in rows if a == CHUNK_AVG), rows[0][4])
    print(
        f"    {'chunkAvg':>9} {'poolRaw':>12} {'dedup':>6} {'zstd':>10} "
        f"{'table':>8} {'TOTAL':>10} {'ratio':>8} {'vs 8K':>7}"
    )
    for a, pool_b, z, tbl, total in rows:
        print(
            f"    {a>>10:>8}K {pool_b:>12,} {raw/pool_b:6.2f} {z:>10,} {tbl:>8,} "
            f"{total:>10,} {raw/total:7.1f}x {100*(total-ref)/ref:+6.1f}%"
        )

    print("\n  finding 4: what cross-session sharing would be worth")
    pools = {r["session"]: r["pool"] for r in by_avg[CHUNK_AVG]}
    sep = sum(comp(p, 19, 24, False) for p in pools.values())
    print(f"    per-session, self-contained (today)  {sep:>10,}")
    glob_pool = comp(b"".join(pools.values()), 19, 27, True)
    print(f"    one global pool                      {glob_pool:>10,}   {100*(sep-glob_pool)/sep:+6.1f}%")
    ordered = sorted(pools.values(), key=len, reverse=True)
    others = b"".join(ordered[1:])
    for kb in (256, 1024, 4096):
        pref = others[-kb * 1024 :]
        with_pref = 0
        dd = zstd.ZstdCompressionDict(pref, dict_type=zstd.DICT_TYPE_RAWCONTENT)
        params = zstd.ZstdCompressionParameters.from_level(19, window_log=27)
        for p in pools.values():
            with_pref += len(
                zstd.ZstdCompressor(compression_params=params, dict_data=dd).compress(p)
            )
        net = with_pref + comp(pref, 19, 27)
        print(
            f"    shared {kb:>4} KiB raw prefix, net    {net:>10,}   "
            f"{100*(sep-net)/sep:+6.1f}%   (one cross-archive edge)"
        )
    try:
        samples = [p[i : i + 65536] for p in pools.values() for i in range(0, len(p), 65536)]
        trained = zstd.train_dictionary(110 * 1024, samples[:4000])
        wd = sum(
            len(zstd.ZstdCompressor(level=19, dict_data=trained).compress(p))
            for p in pools.values()
        )
        net = wd + len(trained.as_bytes())
        print(f"    trained 110 KiB dictionary, net      {net:>10,}   {100*(sep-net)/sep:+6.1f}%")
    except Exception as exc:
        print(f"    trained dictionary: failed ({exc})")


def side_turns(home: Path) -> None:
    print("\n=== side-turns: do they fold into their session? ===")
    paths = sorted(glob.glob(str(home / "manifests" / "*" / "*.json")))
    no_tools = sessionless = 0
    urls: collections.Counter[str] = collections.Counter()
    for p in paths:
        m = json.loads(Path(p).read_bytes())
        req = m.get("req") or {}
        sid = req.get("session_id") or ""
        if req.get("model") and not req.get("tool_count"):
            no_tools += 1
        if not sid:
            sessionless += 1
            urls[m.get("url", "?")] += 1
    print(f"  {len(paths)} turns; {no_tools} are no-tool side calls ({100*no_tools/max(len(paths),1):.0f}%)")
    print(f"  sessionless (unarchivable by construction): {sessionless}")
    for u, c in urls.most_common():
        print(f"    {c:4}  {u}")


def main() -> int:
    ap = argparse.ArgumentParser(
        description="Audit where a goxray store's bytes are. Read-only.",
    )
    ap.add_argument("--home", type=Path, default=Path.home() / ".goxray")
    ap.add_argument(
        "--deep",
        action="store_true",
        help="re-chunk every archived body: histogram, window sweep, sharing variants",
    )
    ap.add_argument(
        "--avg",
        type=int,
        nargs="+",
        default=[2 << 10, 4 << 10, 8 << 10],
        help="chunkAvg values to compare under --deep (bytes)",
    )
    args = ap.parse_args()
    if not args.home.is_dir():
        print(f"no such store: {args.home}", file=sys.stderr)
        return 1

    archives, packs, blobs, manifests = tier_table(args.home)
    pack_finding(packs, manifests)
    manifest_finding(manifests)
    live_finding(blobs)
    side_turns(args.home)
    if args.deep:
        if not archives:
            print("\n--deep needs at least one archive; run `goxray archive --apply` first")
        else:
            deep(archives, sorted(set(args.avg)))
    else:
        print("\n(pass --deep for the chunk histogram, window sweep and sharing variants)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
