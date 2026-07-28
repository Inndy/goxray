package store

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestArchiveFixtureRoundTrip runs the whole archive cycle against a real
// store and proves every recorded body survives it byte for byte. Opt-in, so
// no real conversation content is ever needed to run the suite:
//
//	GOXRAY_STORE_FIXTURE=~/.goxray go test ./internal/store/ -run Fixture -v
//
// It always works on a copy under t.TempDir(). Archiving deletes blob files,
// and a test is the last thing that should be doing that to someone's store.
func TestArchiveFixtureRoundTrip(t *testing.T) {
	src := os.Getenv("GOXRAY_STORE_FIXTURE")
	if src == "" {
		t.Skip("set GOXRAY_STORE_FIXTURE=<path to a goxray home> to run")
	}

	p := tmpPaths(t)
	root := filepath.Dir(p.blobs)
	for _, sub := range []string{"blobs", "manifests"} {
		if err := copyTree(filepath.Join(src, sub), filepath.Join(root, sub)); err != nil {
			t.Fatalf("copy %s: %v", sub, err)
		}
	}
	if err := copyFile(filepath.Join(src, "index.db"), p.index); err != nil {
		t.Logf("no index.db to copy (%v); rebuilding", err)
	}

	// Rebuild the projection from the copied manifests so the archiver sees a
	// consistent view regardless of what the source store's WAL held.
	st := openStoreAt(t, p)
	if err := st.Index.RebuildFrom(newLocalManifests(p.manifests)); err != nil {
		t.Fatalf("rebuild index: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	before := hashAllBodies(t, p)
	if len(before) == 0 {
		t.Fatal("fixture store has no readable bodies")
	}
	liveBefore := dirBytes(t, p.blobs)
	t.Logf("fixture: %d bodies, %s of blobs", len(before), byteSize(liveBefore))

	// --- archive
	start := time.Now()
	stats := archiveAt(t, p, func(o *ArchiveOptions) { o.Quiet = time.Minute })
	archived := dirBytes(t, p.archives)
	t.Logf("archived %d sessions / %d bodies in %s: %s -> %s (%.1fx)",
		stats.Archived, stats.Refs, time.Since(start).Round(time.Millisecond),
		byteSize(stats.BytesFrom), byteSize(archived),
		float64(stats.BytesFrom)/float64(max(archived, 1)))
	t.Logf("store blobs now %s, archives %s", byteSize(dirBytes(t, p.blobs)), byteSize(archived))

	after := hashAllBodies(t, p)
	compareHashes(t, "after archive", before, after)

	// --- fsck
	fk, err := Fsck(FsckOptions{BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives})
	if err != nil {
		t.Fatalf("fsck: %v", err)
	}
	t.Logf("fsck: %d archives, %d chunks, %d bodies, %d manifests", fk.Archives, fk.Chunks, fk.Bodies, fk.Manifests)
	for _, pr := range fk.Problems {
		t.Errorf("fsck problem: %s", pr)
	}
	for _, m := range fk.Missing {
		t.Errorf("fsck missing: %s %s %s: %s", m.Exchange, m.Side, m.Ref, m.Err)
	}

	// --- idempotence
	if again := archiveAt(t, p, func(o *ArchiveOptions) { o.Quiet = time.Minute }); again.Archived != 0 {
		t.Errorf("second pass archived %d sessions, want 0", again.Archived)
	}

	// --- unarchive
	archiveAt(t, p, func(o *ArchiveOptions) { o.Unarchive = true })
	restored := hashAllBodies(t, p)
	compareHashes(t, "after unarchive", before, restored)
	if n := countArchives(t, p.archives); n != 0 {
		t.Errorf("%d archives survived unarchiving", n)
	}
}

// hashAllBodies reads every body of every manifest through the public reader —
// the same path the CLI and dashboard use — and returns id/side -> sha256.
func hashAllBodies(t *testing.T, p testPaths) map[string]string {
	t.Helper()
	rd, err := OpenReader(p.blobs, p.manifests, p.archives, p.index)
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()

	out := map[string]string{}
	m := newLocalManifests(p.manifests)
	if err := m.EachID(func(id string) error {
		ex, err := m.Read(id)
		if err != nil {
			return nil //nolint:nilerr // an unreadable manifest is the fixture's problem, not the archiver's
		}
		for _, c := range []struct{ side, ref string }{{"req", ex.ReqRef}, {"res", ex.ResRef}} {
			if c.ref == "" {
				continue
			}
			body, err := rd.Blob(ex.Req.SessionID, c.ref)
			if err != nil {
				t.Errorf("read %s %s: %v", id, c.side, err)
				continue
			}
			sum := sha256.Sum256(body)
			out[id+"/"+c.side] = hex.EncodeToString(sum[:])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func compareHashes(t *testing.T, stage string, want, got map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d bodies readable, want %d", stage, len(got), len(want))
	}
	bad := 0
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("%s: %s went missing", stage, k)
			bad++
		} else if g != w {
			t.Errorf("%s: %s changed: %s -> %s", stage, k, w[:12], g[:12])
			bad++
		}
		if bad > 10 {
			t.Fatalf("%s: too many mismatches", stage)
		}
	}
	if bad == 0 {
		t.Logf("%s: all %d bodies byte-identical", stage, len(want))
	}
}

func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var n int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		return copyFile(p, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	// Preserve mtime: the archiver's quiet window is measured from it, and a
	// freshly copied manifest would look like a turn that just landed.
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}
