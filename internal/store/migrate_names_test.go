package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacyStore records two sessions, archives them, then rewrites both archives
// into the shape the store had before digest naming: a v1 file named after its
// session. That is what tools/migrate-archive-names.py has to migrate.
func legacyStore(t *testing.T, p testPaths, sessions ...string) (ids []string, reqs, ress [][]byte) {
	t.Helper()
	st := openStoreAt(t, p)
	for _, sid := range sessions {
		q, s, i := recordSession(t, st, sid, 3)
		reqs = append(reqs, q...)
		ress = append(ress, s...)
		ids = append(ids, i...)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	archiveAt(t, p, nil)

	for _, sid := range sessions {
		legacy := shardPath(p.archives, sid, archiveExt)
		if err := ensureDir(filepath.Dir(legacy)); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(archiveFile(p.archives, sid), legacy); err != nil {
			t.Fatal(err)
		}
		downgradeToV1(t, legacy)
	}
	return ids, reqs, ress
}

func runMigrate(t *testing.T, p testPaths, args ...string) string {
	t.Helper()
	script := filepath.Join("..", "..", "tools", "migrate-archive-names.py")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("migration script not found: %v", err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	cmd := exec.Command(python, append([]string{script, "--archives", p.archives}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("migrate %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// The migration is a script rather than a code path in this package, so its one
// job — that a store written under the old naming reads correctly under the new
// one — is checked here rather than taken on trust.
func TestMigrateArchiveNamesScript(t *testing.T) {
	p := tmpPaths(t)
	sessions := []string{
		"af5f1ea9-f08a-4d0b-8709-384a1ee6f0ec",
		"sess-legacy-0000-0000-00000000000e",
	}
	ids, reqs, ress := legacyStore(t, p, sessions...)

	// The premise: every body is unreachable, and fsck says which script fixes it.
	before, err := Fsck(FsckOptions{BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Missing) != 12 || len(before.Problems) != 2 {
		t.Fatalf("pre-migration fsck = %+v, want 12 unreadable bodies and both archives flagged", before)
	}

	// A dry run must change nothing.
	out := runMigrate(t, p)
	if !strings.Contains(out, "dry run") {
		t.Errorf("dry run did not say so:\n%s", out)
	}
	for _, sid := range sessions {
		if _, err := os.Stat(shardPath(p.archives, sid, archiveExt)); err != nil {
			t.Fatalf("a dry run moved %s: %v", sid, err)
		}
	}

	out = runMigrate(t, p, "--apply")
	for _, sid := range sessions {
		if _, err := os.Stat(shardPath(p.archives, sid, archiveExt)); err == nil {
			t.Errorf("the legacy file for %s survived --apply", sid)
		}
		path := archiveFile(p.archives, sid)
		a, err := openArchive(path)
		if err != nil {
			t.Fatalf("%s: %v", sid, err)
		}
		// Upgraded, not merely moved: the file now says whose it is.
		if a.Session() != sid {
			t.Errorf("%s reports session %q", filepath.Base(path), a.Session())
		}
		a.Close()
	}

	after, err := Fsck(FsckOptions{BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Problems) != 0 || len(after.Missing) != 0 {
		t.Fatalf("store not clean after migration: %+v\n%s", after, out)
	}
	if after.Bodies != 12 {
		t.Errorf("fsck proved %d bodies, want 12", after.Bodies)
	}
	// The bytes, which is the only thing that actually matters.
	assertBodies(t, p, ids, reqs, ress)

	// Idempotent: a second pass has nothing left to do.
	out = runMigrate(t, p, "--apply")
	if !strings.Contains(out, "0 were moved") || !strings.Contains(out, "2 already current") {
		t.Errorf("second --apply was not a no-op:\n%s", out)
	}
}

// A file the archiver never wrote must be reported and left alone, not moved
// somewhere a read would then trust it.
func TestMigrateArchiveNamesLeavesUnparseableFilesAlone(t *testing.T) {
	p := tmpPaths(t)
	if err := ensureDir(filepath.Join(p.archives, "xx")); err != nil {
		t.Fatal(err)
	}
	junk := filepath.Join(p.archives, "xx", "not-an-archive"+archiveExt)
	if err := os.WriteFile(junk, []byte("NOPE and then some padding to get past the header length"), 0o600); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join("..", "..", "tools", "migrate-archive-names.py")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("migration script not found: %v", err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	out, err := exec.Command(python, script, "--archives", p.archives, "--apply").CombinedOutput()
	if err == nil {
		t.Errorf("migrating a store with a bad file reported success:\n%s", out)
	}
	if !strings.Contains(string(out), "bad magic") {
		t.Errorf("output does not explain the problem:\n%s", out)
	}
	if _, err := os.Stat(junk); err != nil {
		t.Errorf("the unparseable file was not left where it was: %v", err)
	}
}

// The sweep and a hand-run pass exclude each other through archives/.lock, and
// so must this: renaming files under a running pass would move an archive it is
// about to install over.
func TestMigrateArchiveNamesRefusesUnderALiveLock(t *testing.T) {
	p := tmpPaths(t)
	sessions := []string{"af5f1ea9-f08a-4d0b-8709-384a1ee6f0eb"}
	legacyStore(t, p, sessions...)

	if err := os.WriteFile(filepath.Join(p.archives, ".lock"), []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join("..", "..", "tools", "migrate-archive-names.py")
	if _, err := os.Stat(script); err != nil {
		t.Skipf("migration script not found: %v", err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	out, err := exec.Command(python, script, "--archives", p.archives, "--apply").CombinedOutput()
	if err == nil {
		t.Errorf("migration ran under a live lock:\n%s", out)
	}
	if _, err := os.Stat(shardPath(p.archives, sessions[0], archiveExt)); err != nil {
		t.Errorf("a locked-out run moved a file anyway: %v", err)
	}

	// --force is the way through, for a lock left behind by a killed process.
	runMigrate(t, p, "--apply", "--force")
	if _, err := os.Stat(archiveFile(p.archives, sessions[0])); err != nil {
		t.Errorf("--force did not migrate: %v", err)
	}
}

// legacyize rewrites every archive in a store the way it would have been written
// before digest naming: a v1 file named after its session, which the trailer is
// the only remaining record of. Returns how many it converted.
func legacyize(t *testing.T, archives string) int {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(archives, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == archiveExt {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range paths {
		a, err := openArchive(p)
		if err != nil {
			t.Fatal(err)
		}
		sid := a.Session()
		a.Close()
		// The old naming could only file an id that was already a name, so an id
		// it could not file has no legacy shape to reproduce.
		if sid == "" || !isPlainToken(sid) {
			continue
		}
		legacy := shardPath(archives, sid, archiveExt)
		if err := ensureDir(filepath.Dir(legacy)); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(p, legacy); err != nil {
			t.Fatal(err)
		}
		downgradeToV1(t, legacy)
		n++
	}
	return n
}

// The migration, against a real store rather than a fabricated one. Opt-in on
// the same fixture as TestArchiveFixtureRoundTrip, and equally non-destructive:
// it works on a copy under t.TempDir().
//
//	GOXRAY_STORE_FIXTURE=~/.goxray go test ./internal/store/ -run MigrateArchiveNamesFixture -v
func TestMigrateArchiveNamesFixture(t *testing.T) {
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
	st := openStoreAt(t, p)
	if err := st.Index.RebuildFrom(newLocalManifests(p.manifests)); err != nil {
		t.Fatalf("rebuild index: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	want := hashAllBodies(t, p)
	if len(want) == 0 {
		t.Fatal("fixture store has no readable bodies")
	}
	archiveAt(t, p, func(o *ArchiveOptions) { o.Quiet = time.Minute })

	n := legacyize(t, p.archives)
	if n == 0 {
		t.Fatal("no archives to migrate")
	}
	t.Logf("legacyized %d archives", n)

	// The premise: a store in that shape is broken until it is migrated.
	before, err := Fsck(FsckOptions{BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives})
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Problems) != n || len(before.Missing) == 0 {
		t.Fatalf("pre-migration fsck = %d problems, %d unreadable bodies; want %d and some",
			len(before.Problems), len(before.Missing), n)
	}

	t.Log(strings.TrimSpace(runMigrate(t, p, "--apply")))

	after, err := Fsck(FsckOptions{BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives})
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Problems) != 0 || len(after.Missing) != 0 {
		t.Fatalf("store not clean after migration: %+v", after)
	}
	t.Logf("fsck: %d archives, %d chunks, %d bodies, %d manifests",
		after.Archives, after.Chunks, after.Bodies, after.Manifests)
	compareHashes(t, "after migration", want, hashAllBodies(t, p))
}
