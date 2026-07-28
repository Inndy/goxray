package store

import (
	"bytes"
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

// ArchiveStats reports what a pass found or changed.
type ArchiveStats struct {
	Sessions  int // sessions considered
	Archived  int // sessions written (or that would be, on a dry run)
	Refs      int // bodies folded in
	Skipped   int // hot, inflight, unarchivable, or already done
	Failed    int
	BytesFrom int64 // live blob bytes the pass removes
	BytesTo   int64 // archive bytes it writes
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

	Apply     bool
	Unarchive bool
}

// lockTTL is how long an archive lock is honoured before it is treated as
// abandoned. Stealing one early is safe rather than merely unlikely: the loser
// reads its sources through the same verify pass, so it either produces a
// complete archive or aborts without unlinking.
const lockTTL = 15 * time.Minute

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
	}

	blobs, err := newLocalBlobs(o.BlobsDir, o.ArchivesDir)
	if err != nil {
		return st, err
	}
	defer blobs.Close()
	manifests := newLocalManifests(o.ManifestsDir)

	targets, err := o.candidates()
	if err != nil {
		return st, err
	}
	for _, sid := range targets {
		// Max bounds the work done, not the sessions looked at. Capping the
		// candidate list instead would starve older sessions forever: the list
		// is ordered most-recent-first, so already-archived sessions would fill
		// the budget on every pass and the tail would never be reached.
		if o.Max > 0 && st.Archived >= o.Max {
			break
		}
		st.Sessions++
		var err error
		if o.Unarchive {
			err = o.unarchiveSession(&st, blobs, sid)
		} else {
			err = o.archiveSession(&st, blobs, manifests, sid)
		}
		if err != nil {
			st.Failed++
			logf("archive %s: %v", sid, err)
		}
	}
	return st, nil
}

// candidates picks the sessions worth looking at. It reads the index, which is
// a projection: a session missing from it is simply archived on a later pass,
// never lost. The authoritative ref list still comes from manifests.
func (o ArchiveOptions) candidates() ([]string, error) {
	if len(o.Only) > 0 {
		return o.Only, nil
	}
	if o.Unarchive {
		return o.archivedSessions()
	}
	sessions, err := o.Index.Sessions(Filter{IncludeNoise: true})
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-o.Quiet).UnixMilli()
	var out []string
	for _, s := range sessions {
		// A sessionless turn cannot be grouped into a per-session archive.
		// They are the untagged side-turns, a few KB each, and stay live.
		if s.SessionID == "" {
			continue
		}
		cold := s.LastTS < cutoff
		crowded := o.MaxLiveTurns > 0 && s.Turns >= o.MaxLiveTurns
		if !cold && !crowded {
			continue
		}
		// An archive written after the session's last turn already covers it.
		// One stat is far cheaper than the query-and-read-manifests it saves,
		// and it keeps a store full of archived sessions from re-examining all
		// of them on every sweep.
		if fi, err := os.Stat(o.archivePath(s.SessionID)); err == nil && fi.ModTime().UnixMilli() > s.LastTS {
			continue
		}
		out = append(out, s.SessionID)
	}
	return out, nil
}

func (o ArchiveOptions) archivedSessions() ([]string, error) {
	var out []string
	err := filepath.WalkDir(o.ArchivesDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != archiveExt {
			return nil //nolint:nilerr // an unreadable shard is skipped, not fatal
		}
		out = append(out, d.Name()[:len(d.Name())-len(archiveExt)])
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return out, err
}

func (o ArchiveOptions) archivePath(sid string) string {
	return shardPath(o.ArchivesDir, sid, archiveExt)
}

// sessionRef is one body eligible for archiving.
type sessionRef struct {
	ref  string
	live bool // a .zst file exists and may be unlinked once verified
}

// archiveSession rebuilds one session's archive from every body it can reach —
// live files and the existing archive both — then verifies and installs it.
//
// Rebuilding rather than appending a second generation is deliberate: measured
// against a real store, a full rebuild is both smaller than chaining a new
// archive onto the old one and leaves no cross-archive reference to track.
func (o ArchiveOptions) archiveSession(st *ArchiveStats, blobs *localBlobs, manifests *localManifests, sid string) error {
	if sid == "" {
		st.Skipped++
		return nil
	}
	rows, err := o.Index.Query(Filter{Session: sid, IncludeNoise: true})
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-o.Quiet)

	var refs []sessionRef
	var bytesFrom int64
	seen := map[string]bool{}
	for _, row := range rows {
		// Manifests are the source of truth; the index row only said where to
		// look. A manifest still inflight, or written inside the quiet window,
		// is left alone — the exchange stays live and a later pass folds it in.
		p := manifests.path(row.ID)
		fi, err := os.Stat(p)
		if err != nil || fi.ModTime().After(cutoff) {
			continue
		}
		ex, err := manifests.Read(row.ID)
		if err != nil || ex.State == model.StateInflight {
			continue
		}
		for _, ref := range []string{ex.ReqRef, ex.ResRef} {
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
	// Nothing live means the session is already archived — the pass is a no-op
	// rather than a pointless rewrite, which is what makes it idempotent.
	if nLive == 0 {
		st.Skipped++
		return nil
	}

	st.Archived++
	st.Refs += len(refs)
	st.BytesFrom += bytesFrom
	if !o.Apply {
		return nil
	}

	final := o.archivePath(sid)
	tmp, size, err := o.buildArchive(blobs, sid, refs, final)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // no-op once renamed

	if err := o.verifyArchive(blobs, sid, refs, tmp, final); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	if err := fsyncDir(filepath.Dir(final)); err != nil {
		return err
	}
	st.BytesTo += size

	// Only now, with the archive durable and proven, do the originals go.
	for _, r := range refs {
		if !r.live {
			continue
		}
		if err := os.Remove(shardPath(o.BlobsDir, r.ref, ".zst")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logf("archive %s: unlink %s: %v", sid, r.ref, err)
		}
	}
	return nil
}

// buildArchive writes the new archive to a temp file beside its final path.
// Bodies are streamed through the builder one at a time, so peak memory is the
// session's unique content rather than its total traffic.
func (o ArchiveOptions) buildArchive(blobs *localBlobs, sid string, refs []sessionRef, final string) (string, int64, error) {
	b, err := newArchiveBuilder()
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
	data := b.Bytes()

	dir := filepath.Dir(final)
	if err := ensureDir(dir); err != nil {
		return "", 0, err
	}
	f, err := os.CreateTemp(dir, ".gxa-*")
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

// unarchiveSession rematerialises a session's bodies as individual blobs and
// removes the archive. Every recipe resolves to exact bytes, so this is a true
// inverse — which is what makes archiving something a user can undo.
func (o ArchiveOptions) unarchiveSession(st *ArchiveStats, blobs *localBlobs, sid string) error {
	path := o.archivePath(sid)
	a, err := openArchive(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			st.Skipped++
			return nil
		}
		return err
	}
	refs := a.Refs()
	st.Archived++
	st.Refs += len(refs)
	if fi, err := os.Stat(path); err == nil {
		st.BytesFrom += fi.Size()
	}
	if !o.Apply {
		a.Close()
		return nil
	}

	for _, ref := range refs {
		raw, err := a.Get(ref)
		if err != nil {
			a.Close()
			return fmt.Errorf("unarchive %s: %w", ref, err)
		}
		n, err := blobs.putRef(ref, raw)
		if err != nil {
			a.Close()
			return fmt.Errorf("unarchive %s: %w", ref, err)
		}
		st.BytesTo += n
	}
	// Close before unlinking: the reader holds the file open, and on Windows an
	// open handle blocks removal outright.
	a.Close()
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return fsyncDir(filepath.Dir(path))
}

// putRef writes a blob under a ref that already exists, which Put cannot do
// because it mints one. Only unarchiving needs this.
func (s *localBlobs) putRef(ref string, b []byte) (int64, error) {
	comp := s.enc.EncodeAll(b, nil)
	if err := writeFileSync(shardPath(s.dir, ref, ".zst"), comp); err != nil {
		return 0, err
	}
	return int64(len(comp)), nil
}

// ---------- cross-process lock ----------

type archiveLock struct{ path string }

// acquireArchiveLock keeps `serve`'s sweep and a hand-run `goxray archive`
// from rebuilding the same session at once. An O_EXCL file rather than flock:
// this repo ships a Windows binary, and the consequence of the weaker
// primitive — a stale lock stolen early — is bounded by the verify pass.
func acquireArchiveLock(path string) (*archiveLock, error) {
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "pid %d at %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			f.Close()
			return &archiveLock{path: path}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		fi, serr := os.Stat(path)
		if serr != nil {
			continue // it vanished between the two calls; try again
		}
		if time.Since(fi.ModTime()) < lockTTL {
			return nil, errArchiveLocked
		}
		logf("archive: breaking a lock left at %s", fi.ModTime().UTC().Format(time.RFC3339))
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return nil, errArchiveLocked
}

func (l *archiveLock) release() {
	if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logf("archive: release lock: %v", err)
	}
}
