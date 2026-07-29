package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestFsckCleanStore(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	recordSession(t, st, "sess-fsck-0000-0000-000000000001", 6)
	recordSession(t, st, "sess-fsck-0000-0000-000000000002", 4)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Before archiving: nothing to verify, but every body must still resolve.
	got, err := Fsck(FsckOptions{BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives})
	if err != nil {
		t.Fatal(err)
	}
	if got.Manifests != 10 || len(got.Missing) != 0 || len(got.Problems) != 0 {
		t.Fatalf("pre-archive fsck = %+v", got)
	}

	archiveAt(t, p, nil)

	got, err = Fsck(FsckOptions{BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives})
	if err != nil {
		t.Fatal(err)
	}
	if got.Archives != 2 {
		t.Errorf("checked %d archives, want 2", got.Archives)
	}
	if got.Bodies != 20 {
		t.Errorf("resolved %d bodies, want 20", got.Bodies)
	}
	if got.Chunks == 0 {
		t.Error("verified no chunks")
	}
	if len(got.Missing) != 0 || len(got.Problems) != 0 {
		t.Errorf("clean store reported problems: %+v %+v", got.Problems, got.Missing)
	}
}

// The point of fsck: damage must be reported, and reported against the
// exchanges it actually affects.
func TestFsckReportsDamage(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const session = "sess-damage-000-0000-000000000003"
	recordSession(t, st, session, 5)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	archiveAt(t, p, nil)

	path := shardPath(p.archives, session, archiveExt)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-32] ^= 0xff // corrupt the pool
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Fsck(FsckOptions{BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Problems) == 0 {
		t.Fatal("fsck passed a corrupted archive")
	}
	// The bodies it could no longer prove must be named, not silently dropped.
	if len(got.Missing) == 0 {
		t.Error("fsck reported the archive as broken but no affected exchanges")
	}
	for _, m := range got.Missing {
		if m.Session != session || m.Exchange == "" {
			t.Errorf("unhelpful missing entry: %+v", m)
		}
	}
}

// An archive is reached through its own session's path, so one filed under the
// wrong session holds bodies nothing can read. fsck used to remember verified
// refs globally and pass such a store as clean while every read from it failed —
// the one outcome an integrity checker must not have.
func TestFsckCatchesAnArchiveFiledUnderTheWrongSession(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const session = "sess-moved-0000-0000-00000000000a"
	_, _, ids := recordSession(t, st, session, 4)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	archiveAt(t, p, nil)

	to := shardPath(p.archives, "sess-elsewhere-00-00000000000b", archiveExt)
	if err := ensureDir(filepath.Dir(to)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(shardPath(p.archives, session, archiveExt), to); err != nil {
		t.Fatal(err)
	}

	// The premise: reads really are broken now.
	rd := readerAt(t, p)
	ex, err := rd.Manifest(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Blob(ex.Req.SessionID, ex.ReqRef); err == nil {
		t.Fatal("a body read back from a moved archive; the test proves nothing")
	}

	got, err := Fsck(FsckOptions{BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Missing) != 8 {
		t.Errorf("fsck found %d unreadable bodies, want all 8 of the moved session: %+v", len(got.Missing), got)
	}
	for _, m := range got.Missing {
		if m.Session != session {
			t.Errorf("missing body attributed to %q, want %q", m.Session, session)
		}
	}
}

// An archive whose name is not a session id the store resolves to a path is
// unreachable: every read of it fails. That is the shape a capture from before
// ids were normalized leaves behind, and the one case where "fsck says clean"
// would be a lie about data the user still has. It must be named, and it must be
// recoverable.
func TestFsckReportsAnUnreachableArchiveAndUnarchiveRecoversIt(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const session = "af5f1ea9-f08a-4d0b-8709-384a1ee6f0ec"
	reqs, ress, ids := recordSession(t, st, session, 4)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	archiveAt(t, p, nil)

	// A space is not a plain token, so no session id can produce this name.
	legacy := filepath.Join(p.archives, "xx", "sess with space"+archiveExt)
	if err := ensureDir(filepath.Dir(legacy)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(shardPath(p.archives, session, archiveExt), legacy); err != nil {
		t.Fatal(err)
	}

	got, err := Fsck(FsckOptions{BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Problems) != 1 {
		t.Errorf("problems = %v, want the unreachable archive named", got.Problems)
	}
	if got.Chunks == 0 {
		t.Error("an unreachable archive was not verified; whether its bytes survive is the useful part")
	}
	if len(got.Missing) != 8 {
		t.Errorf("fsck found %d unreadable bodies, want all 8: %+v", len(got.Missing), got)
	}

	// The way out: restoring works from the files on disk, so it reaches an
	// archive no session id points at.
	stats := archiveAt(t, p, func(o *ArchiveOptions) { o.Unarchive = true })
	if stats.Archived != 1 || stats.Refs != 8 {
		t.Fatalf("unarchive restored %+v, want 1 archive of 8 bodies", stats)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the unreachable archive survived being restored: %v", err)
	}
	assertBodies(t, p, ids, reqs, ress)

	if got, err = Fsck(FsckOptions{BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives}); err != nil {
		t.Fatal(err)
	}
	if len(got.Problems) != 0 || len(got.Missing) != 0 {
		t.Errorf("store still not clean after recovery: %+v", got)
	}
}

func TestFsckSessionFilter(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const a = "sess-one-0000-0000-000000000004"
	recordSession(t, st, a, 3)
	recordSession(t, st, "sess-two-0000-0000-000000000005", 3)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	archiveAt(t, p, nil)

	got, err := Fsck(FsckOptions{
		BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives, Session: a,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Archives != 1 {
		t.Errorf("checked %d archives, want just the named one", got.Archives)
	}
	if got.Manifests != 3 {
		t.Errorf("cross-checked %d manifests, want 3", got.Manifests)
	}
}
