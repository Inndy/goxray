#!/usr/bin/env python3
"""Move goxray session archives from session-id filenames to digest filenames.

Archives used to be named after the session whose bodies they hold, which meant
a recorded session id had to be checked before it could become a path, and any
id that failed the check could not be archived at all. They are now named
`s-<sha256(session id)[:16]>.gxa` — a name that exists for every id — and each
archive records its own session id in a trailer so the file still says whose it
is.

This script performs that one-time move on an existing store:

    python3 tools/migrate-archive-names.py            # dry run (default)
    python3 tools/migrate-archive-names.py --apply

For each `.gxa` file it finds:

  * v1 file (no trailer) — the session id is the filename, which is what the
    old naming guaranteed. Append it as a trailer, mark the header v2, and move
    the file to its digest path.
  * v2 file — trust the trailer over the filename and move it to the digest path
    if it is not already there. This also puts back a file moved by hand.

Nothing is deleted: every file is rewritten to a temporary file in its
destination directory, fsynced, verified by reparsing it, and only then renamed
into place, after which the original is unlinked. A file that cannot be parsed
is reported and left exactly as it is.

Run `goxray fsck` afterwards. It verifies chunk hashes, which this script
deliberately does not: it never touches a byte of the compressed pool.
"""

from __future__ import annotations

import argparse
import hashlib
import os
import struct
import sys
import time
from dataclasses import dataclass
from pathlib import Path

MAGIC = b"GXA1"
HEADER_LEN = 56
EXT = ".gxa"
NAME_PREFIX = "s-"
MAX_SESSION = 4096  # archiveMaxSession in internal/store/archive.go

# The archive lock is a lease: an O_EXCL file whose holder rewrites it every 30s
# and which another pass may steal after 5 minutes of silence. Mirrored here so
# this script does not rename files under a running sweep.
LOCK_TTL = 300


class Bad(Exception):
    """The file is not an archive this script will touch."""


@dataclass
class Archive:
    path: Path
    version: int
    session: str | None  # None for v1, which recorded none
    session_len: int


def read_header(path: Path) -> Archive:
    size = path.stat().st_size
    if size < HEADER_LEN:
        raise Bad(f"{size} bytes, shorter than a header")
    with path.open("rb") as f:
        head = f.read(HEADER_LEN)
        if head[:4] != MAGIC:
            raise Bad(f"bad magic {head[:4]!r}")
        version, session_len = struct.unpack_from("<HH", head, 4)
        if version == 1:
            # The two bytes the length lives in were reserved and written as
            # zero. Anything else means this is not the format it claims.
            if session_len != 0:
                raise Bad(f"v1 header carries {session_len} in its reserved field")
            return Archive(path, 1, None, 0)
        if version != 2:
            raise Bad(f"unsupported version {version}")
        if session_len == 0:
            raise Bad("v2 header declares no session")
        if session_len > MAX_SESSION or HEADER_LEN + session_len > size:
            raise Bad(f"{session_len}-byte trailer does not fit in {size} bytes")
        f.seek(size - session_len)
        trailer = f.read(session_len)
    if len(trailer) != session_len:
        raise Bad("trailer is short")
    try:
        return Archive(path, 2, trailer.decode("utf-8"), session_len)
    except UnicodeDecodeError as e:
        raise Bad(f"trailer is not utf-8: {e}") from e


def digest_name(session: str) -> str:
    return NAME_PREFIX + hashlib.sha256(session.encode("utf-8")).hexdigest()[:32]


def target_path(archives: Path, session: str) -> Path:
    """Where a read looks for this session's archive.

    Mirrors archiveFile/shardPath: sharded on the LAST two characters of the
    name, because a uuid's entropy is in its tail and the same rule is used for
    blob refs.
    """
    name = digest_name(session)
    return archives / name[-2:] / (name + EXT)


def upgrade_bytes(raw: bytes, session: str) -> bytes:
    """Return raw as a v2 archive carrying session.

    Every section offset in the header is absolute, and the trailer goes after
    all of them, so this is an append plus a four-byte patch — no section moves,
    and the compressed pool is copied through untouched.
    """
    enc = session.encode("utf-8")
    if not enc:
        raise Bad("empty session id")
    if len(enc) > MAX_SESSION:
        raise Bad(f"session id is {len(enc)} bytes, over the {MAX_SESSION}-byte limit")
    out = bytearray(raw)
    struct.pack_into("<HH", out, 4, 2, len(enc))
    out += enc
    return bytes(out)


def fsync_dir(d: Path) -> None:
    fd = os.open(d, os.O_RDONLY)
    try:
        os.fsync(fd)
    except OSError:
        pass  # not all platforms allow fsync on a directory
    finally:
        os.close(fd)


def install(data: bytes, dest: Path, expect_session: str) -> None:
    """Write data to dest durably, having proved it parses as expect_session."""
    dest.parent.mkdir(parents=True, exist_ok=True)
    tmp = dest.parent / f".gxa-migrate-{os.getpid()}"
    try:
        with tmp.open("wb") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp, 0o600)
        # Read it back from disk rather than trusting the bytes we just built:
        # the whole point of the trailer is that the file says whose it is.
        check = read_header(tmp)
        if check.version != 2 or check.session != expect_session:
            raise Bad(f"rewritten file reports {check.session!r}, want {expect_session!r}")
        os.replace(tmp, dest)
    except BaseException:
        tmp.unlink(missing_ok=True)
        raise
    fsync_dir(dest.parent)


def lock_is_live(archives: Path) -> bool:
    lock = archives / ".lock"
    try:
        age = time.time() - lock.stat().st_mtime
    except FileNotFoundError:
        return False
    return age < LOCK_TTL


def prune_empty_dirs(archives: Path) -> list[Path]:
    gone = []
    for d in sorted((p for p in archives.iterdir() if p.is_dir()), reverse=True):
        try:
            next(d.iterdir())
        except StopIteration:
            d.rmdir()
            gone.append(d)
    return gone


def default_home() -> Path:
    """goxray's own default for --home; it reads no environment variable."""
    return Path.home() / ".goxray"


def main() -> int:
    ap = argparse.ArgumentParser(
        description="Move goxray archives to digest filenames.",
        epilog="Run `goxray fsck` afterwards to verify the store.",
    )
    ap.add_argument(
        "--store",
        type=Path,
        default=None,
        metavar="DIR",
        help="goxray home, i.e. its --home (default ~/.goxray)",
    )
    ap.add_argument(
        "--archives",
        type=Path,
        default=None,
        metavar="DIR",
        help="the archives directory itself, if it is not <store>/archives",
    )
    ap.add_argument("--apply", action="store_true", help="make the changes (default is a dry run)")
    ap.add_argument(
        "--force",
        action="store_true",
        help="proceed even though an archive pass looks to be running",
    )
    args = ap.parse_args()

    archives = args.archives or (args.store or default_home()) / "archives"
    if not archives.is_dir():
        print(f"no archives directory at {archives}", file=sys.stderr)
        return 1

    # Only --apply is blocked: a dry run reads and reports, so there is nothing
    # for a concurrent pass to collide with, and being able to look while the
    # proxy is up is the more useful behaviour.
    if args.apply and lock_is_live(archives) and not args.force:
        print(
            f"{archives / '.lock'} was touched in the last {LOCK_TTL}s, so an archive pass is\n"
            "probably running. Stop `goxray serve` (or wait for the pass to finish) and retry;\n"
            "--force overrides this.",
            file=sys.stderr,
        )
        return 1

    files = sorted(p for p in archives.rglob("*" + EXT) if p.is_file())
    moved = upgraded = already = 0
    problems: list[str] = []

    for path in files:
        try:
            a = read_header(path)
        except (Bad, OSError) as e:
            problems.append(f"{path.relative_to(archives)}: {e}")
            continue

        # A v1 file was named by the old rule, so its filename *is* the session
        # id. That is the only place the id can still be recovered from.
        session = a.session if a.session is not None else path.name[: -len(EXT)]
        dest = target_path(archives, session)
        needs_upgrade = a.version == 1

        if dest == path and not needs_upgrade:
            already += 1
            continue
        if dest != path and dest.exists():
            problems.append(
                f"{path.relative_to(archives)}: {dest.relative_to(archives)} already exists; "
                "left alone (unarchive one of them with `goxray archive --unarchive --apply`)"
            )
            continue

        what = []
        if needs_upgrade:
            what.append(f"record session {session!r}")
        if dest != path:
            what.append(f"move to {dest.relative_to(archives)}")
        print(f"{path.relative_to(archives)}: {', and '.join(what)}")
        if not args.apply:
            if needs_upgrade:
                upgraded += 1
            if dest != path:
                moved += 1
            continue

        try:
            raw = path.read_bytes()
            install(upgrade_bytes(raw, session) if needs_upgrade else raw, dest, session)
        except (Bad, OSError) as e:
            problems.append(f"{path.relative_to(archives)}: {e}")
            continue
        if needs_upgrade:
            upgraded += 1
        if dest != path:
            moved += 1
            # The new copy is durable and has been reparsed, so the original is
            # now the redundant one.
            try:
                path.unlink()
                fsync_dir(path.parent)
            except OSError as e:
                problems.append(f"{path.relative_to(archives)}: rewritten but not removed: {e}")

    if args.apply:
        prune_empty_dirs(archives)

    verb = "would be" if not args.apply else "were"
    print(
        f"\n{len(files)} archive(s) in {archives}: "
        f"{moved} {verb} moved, {upgraded} {verb} given a session trailer, {already} already current"
    )
    for p in problems:
        print(f"  problem: {p}", file=sys.stderr)
    if not args.apply and (moved or upgraded):
        print("\nThis was a dry run. Re-run with --apply.")
    elif args.apply:
        print("Now run `goxray fsck` to verify the store.")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
