package store

import (
	"os"
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
