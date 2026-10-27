package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"go.inndy.tw/goxray/internal/model"
)

// Archiving folds a session's recorded bodies into one self-contained file and
// removes the individual blobs. It runs after the fact rather than at record
// time, which is what keeps it clear of the fail-closed write path: nothing
// here can make PersistRequest slower, or fail, or depend on shared state.
//
// The safety property is that nothing is deleted that has not just been proven
// reproducible. Every pass builds the archive, re-reads it from disk, and
// byte-compares every body against the live file before unlinking anything.

// ArchiveStats reports what a pass found or changed. Everything but Sessions,
// Skipped and Failed counts completed work only: a session that failed part way
// through is in Failed and in none of the others.
type ArchiveStats struct {
	Sessions  int   // sessions considered
	Archived  int   // sessions written (or that would be, on a dry run)
	Refs      int   // bodies folded in
	Manifests int   // manifests folded in (or restored, when unarchiving)
	Skipped   int   // hot, inflight, unarchivable, deferred, or already done
	Failed    int   // attempted and did not finish; nothing was deleted for these
	BytesFrom int64 // live blob bytes the pass removed
	BytesTo   int64 // archive bytes it wrote

	// FailedSessions names what Failed counted, so a caller that runs the pass
	// again — the background sweep — can hold those back rather than spend every
	// tick's budget on them.
	FailedSessions []string
}

// exchangeLister is the read-only slice of the index the archiver needs. Both
// *Index and *Reader satisfy it, so the same pass runs from `goxray archive`
// and from the background sweep.
type exchangeLister interface {
	Sessions(f Filter) ([]Session, error)
	Query(f Filter) ([]model.Exchange, error)
}

// ArchiveOptions configures one pass.
type ArchiveOptions struct {
	BlobsDir     string
	ManifestsDir string
	ArchivesDir  string
	Index        exchangeLister

	// Quiet is how long a session (and each individual manifest) must have been
	// untouched to be eligible. It guards the same window migrate-usage does:
	// the recorder writes each manifest twice, and archiving inside that gap
	// could capture an inflight record as if it were final.
	Quiet time.Duration

	// MaxLiveTurns archives a session that has never gone quiet once it has
	// this many live bodies, so a days-long session cannot hold the store open
	// indefinitely. 0 disables it.
	MaxLiveTurns int

	// Only restricts the pass to these session ids. Empty means every eligible
	// session.
	Only []string

	// Max caps sessions per pass so the background sweep does bounded work per
	// tick. 0 is unlimited.
	Max int

	// Skip holds targets to leave for a later pass — keyed by session id, or by
	// archive path when unarchiving. The sweep uses it to defer sessions that
	// just failed; a hand-run pass leaves it nil, because someone who asked for
	// an archive now wants everything tried now.
	Skip map[string]bool

	Apply     bool
	Unarchive bool

	// Release drops any handle the rest of this process holds on an archive that
	// is about to be replaced — in serve, the store the dashboard reads through,
	// whose blob cache keeps the two most recently read archives open. Windows
	// will not rename over a file this process still has open, and a pass can only
	// close its own cache. nil for a hand-run pass: its Reader answers index
	// queries and never reads a blob.
	Release func(archivePath string)

	// lock is the pass's own archive lock, set by ArchiveSessions. Every step
	// that deletes something re-checks it: see archiveLock.stillHeld.
	lock *archiveLock
}

const (
	// lockHeartbeat is how often the holder proves it is still alive by
	// rewriting its lock file, which bumps the mtime another pass reads.
	lockHeartbeat = 30 * time.Second

	// lockTTL is how long a lock may go untouched before another pass treats it
	// as abandoned. Because a live holder refreshes every lockHeartbeat,
	// reaching the TTL means the owner died or is wedged — not merely slow,
	// which is the case that used to let two passes run against one session.
	lockTTL = 5 * time.Minute
)

var errArchiveLocked = errors.New("another archive pass is running")

// ArchiveSessions runs one pass. Safe against a live proxy and idempotent: a
// second pass over an already-archived store reports zero archived.
func ArchiveSessions(o ArchiveOptions) (ArchiveStats, error) {
	var st ArchiveStats
	if o.Quiet < 0 {
		o.Quiet = 0
	}
	if o.Apply {
		if err := ensureDir(o.ArchivesDir); err != nil {
			return st, err
		}
		lock, err := acquireArchiveLock(filepath.Join(o.ArchivesDir, ".lock"))
		if err != nil {
			return st, err
		}
		defer lock.release()
		o.lock = lock
	}

	blobs, err := newLocalBlobs(o.BlobsDir, o.ArchivesDir)
	if err != nil {
		return st, err
	}
	defer blobs.Close()
	manifests := newLocalManifests(o.ManifestsDir, o.ArchivesDir)

	targets, err := o.candidates()
	if err != nil {
		return st, err
	}
	// Max bounds the work done, not the sessions looked at. Capping the candidate
	// list instead would starve older sessions forever: the list is ordered
	// most-recent-first, so already-archived sessions would fill the budget on
	// every pass and the tail would never be reached.
	//
	// It counts sessions actually archived, and attempts are capped separately at
	// twice that: counting successes alone would let sessions that fail every time
	// make one pass unboundedly long. The cap alone does not make the sweep fair —
	// the same newest failures would fill the budget on every tick — so the caller
	// that repeats (Sweeper) puts a failed session in Skip for a doubling cooldown.
	// Together: bounded work per pass, and nothing older starves behind a session
	// that cannot be archived at all.
	attempts, maxAttempts := 0, 2*o.Max
	for _, sid := range targets {
		if o.Max > 0 && (st.Archived >= o.Max || attempts >= maxAttempts) {
			break
		}
		st.Sessions++
		if o.Skip[sid] {
			st.Skipped++
			continue
		}
		var attempted bool
		var err error
		if o.Unarchive {
			attempted, err = o.unarchiveArchive(&st, blobs, manifests, sid)
		} else {
			attempted, err = o.archiveSession(&st, blobs, manifests, sid)
		}
		if attempted {
			attempts++
		}
		if err != nil {
			st.Failed++
			st.FailedSessions = append(st.FailedSessions, sid)
			logf("archive %s: %v", sid, err)
		}
	}
	return st, nil
}

// candidates picks the sessions worth looking at. It reads the index, which is
// a projection: a session missing from it is simply archived on a later pass,
// never lost. The authoritative ref list still comes from manifests.
func (o ArchiveOptions) candidates() ([]string, error) {
	if o.Unarchive {
		// Restoring works from the files on disk, not from session ids. An
		// archive no session id resolves to — one still named after its session
		// because tools/migrate-archive-names.py has not been run, or a file moved
		// by hand — is unreachable through a read, and is precisely the one
		// someone needs to get back out; `fsck` names it and points here. Taking
		// the name from the directory keeps that safe: it is a filename that
		// exists in a shard, not a path built out of a recorded request.
		if len(o.Only) > 0 {
			var out []string
			for _, sid := range o.Only {
				if p := o.archivePath(sid); p != "" {
					out = append(out, p)
				}
			}
			return out, nil
		}
		return o.archivedPaths()
	}
	if len(o.Only) > 0 {
		return o.Only, nil
	}
	sessions, err := o.Index.Sessions(Filter{IncludeNoise: true})
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-o.Quiet).UnixMilli()
	var out []string
	for _, s := range sessions {
		// A turn with no session id has no group to be folded into: the untagged
		// side-turns, a few KB each. They stay live.
		archive := o.archivePath(s.SessionID)
		if archive == "" {
			continue
		}
		cold := s.LastTS < cutoff
		crowded := o.MaxLiveTurns > 0 && s.Turns >= o.MaxLiveTurns
		if !cold && !crowded {
			continue
		}
		// Both files written after the session's last turn already cover it. Two
		// stats are far cheaper than the query-and-read-manifests they save, and
		// they keep a store full of archived sessions from re-examining all of
		// them on every sweep.
		//
		// Both, not just the archive: a store archived before manifests were
		// packed has an archive newer than its last turn and no pack at all, and
		// testing only the archive would leave every one of those manifests loose
		// forever.
		if covers(archive, s.LastTS) && covers(mpackFile(o.ArchivesDir, s.SessionID), s.LastTS) {
			continue
		}
		out = append(out, s.SessionID)
	}
	return out, nil
}

// covers reports whether path exists and was written after the given turn
// timestamp, i.e. whether it already accounts for everything the session has.
func covers(path string, lastTS int64) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.ModTime().UnixMilli() > lastTS
}

// archivedPaths lists every archive file in the store, reachable by a session id
// or not.
func (o ArchiveOptions) archivedPaths() ([]string, error) {
	var out []string
	err := filepath.WalkDir(o.ArchivesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != archiveExt {
			return nil //nolint:nilerr // an unreadable shard is skipped, not fatal
		}
		out = append(out, p)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return out, err
}

// archivePath is the file this session's bodies belong in, or "" if the session
// cannot be archived at all.
func (o ArchiveOptions) archivePath(sid string) string {
	return archiveFile(o.ArchivesDir, sid)
}

// sessionRef is one body eligible for archiving.
type sessionRef struct {
	ref  string
	live bool // a .zst file exists and may be unlinked once verified
}

// sessionMan is one manifest eligible for folding in. raw is the stored bytes,
// wherever they came from: a manifest already packed by an earlier pass has to be
// carried into the rebuild, or resuming a session would drop it.
type sessionMan struct {
	id    string
	raw   []byte
	loose bool // a .json file exists and may be unlinked once verified
}

// archiveSession rebuilds one session's archive from every body it can reach —
// live files and the existing archive both — then verifies and installs it.
//
// Rebuilding rather than appending a second generation is deliberate: measured
// against a real store, a full rebuild is both smaller than chaining a new
// archive onto the old one and leaves no cross-archive reference to track.
//
// The bool reports whether the session cost this pass any real work, which is
// what Max budgets — a session that turned out to be already archived did not,
// and one that failed did.
func (o ArchiveOptions) archiveSession(st *ArchiveStats, blobs *localBlobs, manifests *localManifests, sid string) (bool, error) {
	// No file to put it in: a sessionless turn, which has nothing to group it
	// with. It stays live.
	final := o.archivePath(sid)
	if final == "" {
		st.Skipped++
		return false, nil
	}
	rows, err := o.Index.Query(Filter{Session: sid, IncludeNoise: true})
	if err != nil {
		return true, err // an attempt: it failed rather than being skipped
	}
	cutoff := time.Now().Add(-o.Quiet)

	var refs []sessionRef
	var mans []sessionMan
	var bytesFrom int64
	seen := map[string]bool{}
	for _, row := range rows {
		// Manifests are the source of truth; the index row only said where to
		// look. A manifest still inflight, or written inside the quiet window,
		// is left alone — the exchange stays live and a later pass folds it in.
		//
		// A missing loose file is not that case: it means an earlier pass already
		// packed this manifest, and it is already past every gate a loose one has
		// to clear. Skipping it here would drop it from the rebuilt archive.
		loose := false
		if fi, err := os.Stat(manifests.path(row.ID)); err == nil {
			if fi.ModTime().After(cutoff) {
				continue
			}
			loose = true
		}
		ex, err := manifests.Read(row.ID)
		if err != nil || ex.State == model.StateInflight {
			continue
		}
		// A WebSocket turn whose connection is still open: its bytes are in a log
		// that is still being written, and folding the turn now would pack a
		// manifest whose body cannot be archived yet. It waits, like an inflight
		// one.
		if ex.WS != nil && liveExists(o.BlobsDir, ex.WS.Log) {
			continue
		}
		if raw, err := manifests.readRaw(row.ID); err == nil {
			mans = append(mans, sessionMan{id: row.ID, raw: raw, loose: loose})
			if loose {
				bytesFrom += int64(len(raw))
			}
		}
		for _, ref := range bodyRefs(ex) {
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			live := false
			if lfi, err := os.Stat(shardPath(o.BlobsDir, ref, ".zst")); err == nil {
				live = true
				bytesFrom += lfi.Size()
			}
			refs = append(refs, sessionRef{ref: ref, live: live})
		}
	}

	nLive := 0
	for _, r := range refs {
		if r.live {
			nLive++
		}
	}
	nLooseMan := 0
	for _, m := range mans {
		if m.loose {
			nLooseMan++
		}
	}
	// Nothing loose in either tier means the session is already archived — the
	// pass is a no-op rather than a pointless rewrite, which is what makes it
	// idempotent.
	//
	// Counting loose manifests as work is also what upgrades a store archived
	// before manifests were packed: its bodies are already folded in, so the
	// bodies alone would say "done", but nothing ever deleted its manifests, so
	// they are all still loose and the session gets rebuilt once. No version
	// check needed.
	if nLive == 0 && nLooseMan == 0 {
		st.Skipped++
		return false, nil
	}

	// Counted once the work is done, never before. Reporting a session as
	// archived and then failing to install it told the CLI and the sweep log that
	// bodies had been folded in when they were still live files.
	commit := func(bytesTo int64) {
		st.Archived++
		st.Refs += len(refs)
		st.Manifests += nLooseMan
		st.BytesFrom += bytesFrom
		st.BytesTo += bytesTo
	}
	if !o.Apply {
		commit(0)
		return true, nil
	}

	// What the two files being replaced looked like before they were verified
	// against. Rechecked immediately before the renames, so one that changed while
	// this pass was building — a second pass that added turns this one has never
	// seen — is not overwritten by a stale snapshot.
	packPath := mpackFile(o.ArchivesDir, sid)
	was, err := fileGeneration(final)
	if err != nil {
		return true, err
	}
	wasPack, err := fileGeneration(packPath)
	if err != nil {
		return true, err
	}
	// The body archive is rebuilt only when there is something live to fold into
	// it. With nothing live it already holds every body this session has, so
	// rewriting it would be pure cost — and would make packing the manifests of a
	// session whose archive has been damaged or misfiled fail on a body it never
	// needed to touch.
	var tmp string
	var size int64
	if nLive > 0 {
		tmp, size, err = o.buildArchive(blobs, sid, refs, final)
		if err != nil {
			return true, err
		}
		defer os.Remove(tmp) // no-op once renamed
	}

	tmpPack, packSize, err := o.buildMpack(sid, mans, packPath)
	if err != nil {
		return true, err
	}
	if tmpPack != "" {
		defer os.Remove(tmpPack)
	}

	if tmp != "" {
		if err := o.verifyArchive(blobs, sid, refs, tmp, final); err != nil {
			return true, err
		}
	}
	if tmpPack != "" {
		if err := o.verifyMpack(mans, tmpPack, packPath); err != nil {
			return true, err
		}
	}
	if now, err := fileGeneration(final); err != nil {
		return true, err
	} else if now != was {
		return true, fmt.Errorf("%s changed while this pass was building; leaving it to the next", filepath.Base(final))
	}
	if now, err := fileGeneration(packPath); err != nil {
		return true, err
	} else if now != wasPack {
		return true, fmt.Errorf("%s changed while this pass was building; leaving it to the next", filepath.Base(packPath))
	}
	if !o.lock.stillHeld() {
		return true, errArchiveLocked
	}

	// The rename replaces a file this process may still have open, and Go opens
	// files on Windows without FILE_SHARE_DELETE: an open handle makes it fail
	// with "Access is denied" rather than replacing anything, so the session is
	// rebuilt, verified, refused, and back again every sweep. Two holders, both
	// this process's own: verifyArchive read the already-archived bodies through
	// blobs, which caches the archive it opened, and in serve the dashboard's
	// cache holds up to two more. Same rule as unarchiving's close-before-unlink,
	// one step earlier. The manifest pack needs no equivalent: nothing caches a
	// pack reader, which is why localManifests opens and closes one per read.
	if tmp != "" {
		blobs.forget(final)
		if o.Release != nil {
			o.Release(final)
		}
		if err := os.Rename(tmp, final); err != nil {
			return true, err
		}
	}
	// Both files land before anything is deleted. A failure between the two
	// renames leaves the bodies live and the manifests loose, so the next pass
	// simply rebuilds both.
	if tmpPack != "" {
		if err := os.Rename(tmpPack, packPath); err != nil {
			return true, err
		}
	}
	if err := fsyncDir(filepath.Dir(final)); err != nil {
		return true, err
	}

	// Only now, with both files durable and proven, do the originals go.
	for _, r := range refs {
		if !r.live {
			continue
		}
		if err := os.Remove(shardPath(o.BlobsDir, r.ref, ".zst")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logf("archive %s: unlink %s: %v", sid, r.ref, err)
		}
	}
	// Manifests are the audit record, so these are the most consequential
	// deletions in the store, and they happen only here: after the pack has been
	// reopened from disk, every manifest byte-compared against it, both renames
	// done, and the lock rechecked.
	for _, m := range mans {
		if !m.loose {
			continue
		}
		if err := os.Remove(manifests.path(m.id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logf("archive %s: unlink manifest %s: %v", sid, m.id, err)
		}
	}
	commit(size + packSize)
	return true, nil
}

// fileGeneration identifies a file well enough to notice that it has been
// replaced, which for an archive is always a rename of a freshly built file.
// Empty means the file is not there.
func fileGeneration(path string) (string, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d/%d", fi.ModTime().UnixNano(), fi.Size()), nil
}

// buildArchive writes the new archive to a temp file beside its final path.
// Bodies are streamed through the builder one at a time, so peak memory is the
// session's unique content rather than its total traffic.
func (o ArchiveOptions) buildArchive(blobs *localBlobs, sid string, refs []sessionRef, final string) (string, int64, error) {
	b, err := newArchiveBuilder(sid)
	if err != nil {
		return "", 0, err
	}
	defer b.Close()
	for _, r := range refs {
		raw, err := blobs.Get(sid, r.ref)
		if err != nil {
			return "", 0, fmt.Errorf("read %s: %w", r.ref, err)
		}
		if err := b.Add(r.ref, raw); err != nil {
			return "", 0, err
		}
	}
	return writeTemp(b.Bytes(), final, ".gxa-*")
}

// writeTemp lands data in a temp file in the directory it will be installed in,
// fsynced, so that installing it is one atomic rename within a filesystem. Unlike
// writeFileSync it hands the temp name back rather than renaming, because both
// callers have to verify the file from disk before it may replace anything.
func writeTemp(data []byte, final, pattern string) (string, int64, error) {
	dir := filepath.Dir(final)
	if err := ensureDir(dir); err != nil {
		return "", 0, err
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", 0, err
	}
	name := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", 0, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(name)
		return "", 0, err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", 0, err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return "", 0, err
	}
	return name, int64(len(data)), nil
}

// buildMpack writes the session's manifest pack to a temp file beside its
// destination. Returns "" when the session has no manifests to pack at all, which
// is the sessionless case and nothing else.
func (o ArchiveOptions) buildMpack(sid string, mans []sessionMan, final string) (string, int64, error) {
	if len(mans) == 0 {
		return "", 0, nil
	}
	b, err := newMpackBuilder(sid)
	if err != nil {
		return "", 0, err
	}
	defer b.Close()
	for _, m := range mans {
		if err := b.Add(m.id, m.raw); err != nil {
			return "", 0, err
		}
	}
	return writeTemp(b.Bytes(), final, ".gxm-*")
}

// verifyMpack reopens the candidate pack from disk and byte-compares every
// manifest against the bytes the build was given, then refuses a replacement that
// would drop one the current pack holds. Same discipline as verifyArchive, for the
// same reason: what follows it deletes the only other copy.
//
// The comparison is against the collected bytes rather than a re-read, because
// re-reading a manifest that an earlier pass already packed would resolve through
// this same tier and prove nothing.
func (o ArchiveOptions) verifyMpack(mans []sessionMan, tmp, final string) error {
	p, err := openMpack(tmp)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	defer p.Close()

	for _, m := range mans {
		got, err := p.Get(m.id)
		if err != nil {
			return fmt.Errorf("verify: manifest %s: %w", m.id, err)
		}
		if !bytes.Equal(got, m.raw) {
			return fmt.Errorf("verify: manifest %s: pack holds %d bytes, source has %d", m.id, len(got), len(m.raw))
		}
	}

	if old, err := openMpack(final); err == nil {
		defer old.Close()
		for _, id := range old.IDs() {
			if !p.Has(id) {
				return fmt.Errorf("verify: replacement would drop manifest %s", id)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		logf("archive: replacing unreadable manifest pack %s: %v", relPath(o.ArchivesDir, final), err)
	}
	return nil
}

// verifyArchive is the reason this is safe to run against real data: it reopens
// the candidate from disk and byte-compares every body against the source that
// is about to be deleted. It also refuses to install an archive that would drop
// a ref the current one holds, which makes an overwrite provably
// non-destructive even if two passes race for the same session.
func (o ArchiveOptions) verifyArchive(blobs *localBlobs, sid string, refs []sessionRef, tmp, final string) error {
	a, err := openArchive(tmp)
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	defer a.Close()

	for _, r := range refs {
		want, err := blobs.Get(sid, r.ref)
		if err != nil {
			return fmt.Errorf("verify: reread %s: %w", r.ref, err)
		}
		got, err := a.Get(r.ref)
		if err != nil {
			return fmt.Errorf("verify: %s: %w", r.ref, err)
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("verify: %s: archive holds %d bytes, source has %d", r.ref, len(got), len(want))
		}
	}

	if old, err := openArchive(final); err == nil {
		defer old.Close()
		for _, ref := range old.Refs() {
			if !a.Has(ref) {
				return fmt.Errorf("verify: replacement would drop %s", ref)
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		// An unreadable existing archive is exactly when a rewrite is wanted,
		// but say so rather than silently discarding whatever it held.
		logf("archive %s: replacing unreadable archive: %v", sid, err)
	}
	return nil
}

// unarchiveArchive rematerialises one archive's bodies as individual blobs and
// removes the file. Every recipe resolves to exact bytes, so this is a true
// inverse — which is what makes archiving something a user can undo.
//
// It takes the archive's path rather than a session id so that an archive no
// session id resolves to can still be restored. Refs are checked before they
// become filenames (see putRef), which is what keeps reading a name off disk from
// being a way to write anywhere.
func (o ArchiveOptions) unarchiveArchive(st *ArchiveStats, blobs *localBlobs, manifests *localManifests, path string) (bool, error) {
	a, err := openArchive(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			st.Skipped++
			return false, nil
		}
		return true, err
	}
	refs := a.Refs()
	// The pack is found from the session the archive records rather than from the
	// archive's own path, so a misfiled archive still restores its manifests.
	packPath := mpackFile(o.ArchivesDir, a.Session())
	var manIDs []string
	if packPath != "" {
		if p, err := openMpack(packPath); err == nil {
			manIDs = p.IDs()
			p.Close()
		} else if !errors.Is(err, fs.ErrNotExist) {
			a.Close()
			return true, fmt.Errorf("unarchive: %s: %w", relPath(o.ArchivesDir, packPath), err)
		}
	}
	var from int64
	if fi, err := os.Stat(path); err == nil {
		from = fi.Size()
	}
	if packPath != "" {
		if fi, err := os.Stat(packPath); err == nil {
			from += fi.Size()
		}
	}
	commit := func(bytesTo int64) {
		st.Archived++
		st.Refs += len(refs)
		st.Manifests += len(manIDs)
		st.BytesFrom += from
		st.BytesTo += bytesTo
	}
	if !o.Apply {
		a.Close()
		commit(0)
		return true, nil
	}

	var written int64
	for _, ref := range refs {
		raw, err := a.Get(ref)
		if err != nil {
			a.Close()
			return true, fmt.Errorf("unarchive %s: %w", ref, err)
		}
		n, err := blobs.putRef(ref, raw)
		if err != nil {
			a.Close()
			return true, fmt.Errorf("unarchive %s: %w", ref, err)
		}
		written += n
	}
	// Manifests go back as loose files before either file is removed, so a failure
	// anywhere in here leaves the record intact.
	if len(manIDs) > 0 {
		p, err := openMpack(packPath)
		if err != nil {
			a.Close()
			return true, fmt.Errorf("unarchive: %s: %w", relPath(o.ArchivesDir, packPath), err)
		}
		for _, id := range manIDs {
			raw, err := p.Get(id)
			if err != nil {
				p.Close()
				a.Close()
				return true, fmt.Errorf("unarchive manifest %s: %w", id, err)
			}
			// The id came off disk and is about to become a filename, so it gets
			// the same treatment as a ref (see putRef).
			if !isPlainToken(id) {
				p.Close()
				a.Close()
				return true, fmt.Errorf("unarchive manifest %q: not a plain token", id)
			}
			if err := writeFileSync(manifests.path(id), raw); err != nil {
				p.Close()
				a.Close()
				return true, fmt.Errorf("unarchive manifest %s: %w", id, err)
			}
			written += int64(len(raw))
		}
		p.Close()
	}
	// Close before unlinking: the reader holds the file open, and on Windows an
	// open handle blocks removal outright.
	a.Close()
	if !o.lock.stillHeld() {
		return true, errArchiveLocked
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return true, err
	}
	// The pack goes last: its manifests are already back as loose files, so losing
	// this removal costs a stale duplicate that the next pass folds back in, while
	// losing it before the loose writes would cost the record.
	if packPath != "" {
		if err := os.Remove(packPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return true, err
		}
	}
	if err := fsyncDir(filepath.Dir(path)); err != nil {
		return true, err
	}
	commit(written)
	return true, nil
}

// putRef writes a blob under a ref that already exists, which Put cannot do
// because it mints one. Only unarchiving needs this.
//
// Put's refs are minted uuids, but this one comes out of an archive's ref table
// — bytes on disk — and is about to become a path. Refuse anything that is not
// the shape Put produces rather than let a doctored archive pick the filename.
func (s *localBlobs) putRef(ref string, b []byte) (int64, error) {
	if !isPlainToken(ref) {
		return 0, fmt.Errorf("unsafe ref %q", ref)
	}
	comp := s.enc.EncodeAll(b, nil)
	if err := writeFileSync(shardPath(s.dir, ref, ".zst"), comp); err != nil {
		return 0, err
	}
	return int64(len(comp)), nil
}

// ---------- cross-process lock ----------

// archiveLock is an O_EXCL file carrying a random token that identifies its
// owner. An O_EXCL file rather than flock because this repo ships a Windows
// binary and Go's syscall package has no portable file lock; the token is what
// makes the weaker primitive safe to reason about, because every operation that
// depends on holding the lock can ask whether the file on disk is still ours.
type archiveLock struct {
	path  string
	token []byte
	f     *os.File // held open so the heartbeat writes to our inode, never a successor's file
	stop  chan struct{}
	done  chan struct{}
}

// acquireArchiveLock keeps `serve`'s sweep and a hand-run `goxray archive` from
// rebuilding the same session at once.
func acquireArchiveLock(path string) (*archiveLock, error) {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	// Two tries, so one abandoned lock can be broken and then taken.
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			l := &archiveLock{
				path:  path,
				token: []byte(hex.EncodeToString(token)),
				f:     f,
				stop:  make(chan struct{}),
				done:  make(chan struct{}),
			}
			if err := l.mark(); err != nil {
				f.Close()
				os.Remove(path)
				return nil, err
			}
			go l.beat()
			return l, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if err := breakAbandonedLock(path); err != nil {
			return nil, err
		}
	}
	return nil, errArchiveLocked
}

// breakAbandonedLock removes a lock whose holder has stopped refreshing it, so
// a killed process cannot wedge archiving forever. It returns errArchiveLocked
// while the lock is still being refreshed.
func breakAbandonedLock(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return nil //nolint:nilerr // it vanished between the two calls; the retry will take it
	}
	if time.Since(fi.ModTime()) < lockTTL {
		return errArchiveLocked
	}
	// Re-stat immediately before the unlink so a fresh lock that a third pass
	// installed in the meantime is not the one deleted. That still leaves a
	// window this cannot close, which is why the displaced holder also re-checks
	// ownership before it installs anything (see stillHeld).
	again, err := os.Stat(path)
	if err != nil || !again.ModTime().Equal(fi.ModTime()) || again.Size() != fi.Size() {
		return nil //nolint:nilerr // replaced under us; leave the new one alone and retry
	}
	logf("archive: breaking a lock last refreshed at %s", fi.ModTime().UTC().Format(time.RFC3339))
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// mark rewrites the lock file through the open handle, which both refreshes the
// mtime and keeps the write on our own inode even if the path has been replaced.
func (l *archiveLock) mark() error {
	body := fmt.Appendf(nil, "%s\npid %d, refreshed %s\n", l.token, os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if _, err := l.f.WriteAt(body, 0); err != nil {
		return err
	}
	if err := l.f.Truncate(int64(len(body))); err != nil {
		return err
	}
	// Windows can hold a handle's metadata updates back until close, which would
	// make a live holder look abandoned to another process.
	return l.f.Sync()
}

func (l *archiveLock) beat() {
	defer close(l.done)
	t := time.NewTicker(lockHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			if err := l.mark(); err != nil {
				logf("archive: refreshing the lock failed, another pass may take over: %v", err)
			}
		}
	}
}

// stillHeld reports whether the lock file on disk is still the one we created.
// Every destructive step re-asks, because the answer can change: a pass that was
// declared abandoned and taken over must not go on installing archives and
// unlinking blobs alongside its successor. A nil lock is a dry run, which
// destroys nothing.
func (l *archiveLock) stillHeld() bool {
	if l == nil {
		return true
	}
	b, err := os.ReadFile(l.path)
	return err == nil && bytes.HasPrefix(b, l.token)
}

// release stops the heartbeat, closes the handle, and only then unlinks — in
// that order, and the order is not cosmetic. Go opens files on Windows without
// FILE_SHARE_DELETE, so os.Remove of a file this process still holds open fails
// outright; removing before the close left every successful pass's lock on disk
// and blocked the next pass until the TTL expired. Unarchiving already knew this
// about archive files; the lock file is the same rule.
func (l *archiveLock) release() {
	close(l.stop)
	<-l.done
	if err := l.f.Close(); err != nil {
		logf("archive: close lock: %v", err)
	}
	// Ownership is re-read after the close because the answer decides whether the
	// file is ours to delete: a pass that was declared abandoned and taken over
	// must leave its successor's lock alone. Its own heartbeat has stopped, so
	// nothing it does from here can refresh a lock it no longer holds.
	if !l.stillHeld() {
		logf("archive: lock was taken over by another pass; leaving it alone")
		return
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logf("archive: release lock: %v", err)
	}
}
