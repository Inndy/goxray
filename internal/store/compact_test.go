package store

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.inndy.tw/goxray/internal/model"
)

// testPaths is a store's layout, kept separate from the *Store so a test can
// close the store (draining the async index) and then reopen it or read it
// through a Reader, which is what `goxray archive` does.
type testPaths struct{ blobs, manifests, archives, index string }

func tmpPaths(t *testing.T) testPaths {
	t.Helper()
	d := t.TempDir()
	return testPaths{
		blobs:     filepath.Join(d, "blobs"),
		manifests: filepath.Join(d, "manifests"),
		archives:  filepath.Join(d, "archives"),
		index:     filepath.Join(d, "index.db"),
	}
}

func openStoreAt(t *testing.T, p testPaths) *Store {
	t.Helper()
	st, err := Open(p.blobs, p.manifests, p.archives, p.index, model.ProviderAnthropic, 2)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

// recordSession drives real exchanges through the Recorder, so the archiver
// sees exactly what the proxy writes: manifests, blobs, and index rows.
func recordSession(t *testing.T, st *Store, session string, turns int) (reqBodies, resBodies [][]byte, ids []string) {
	t.Helper()
	convo := conversation(turns)
	for i := 0; i < turns; i++ {
		// conversation() already returns a whole request object; splice the
		// session metadata in after its opening brace so the wire parser sees a
		// well-formed body and tags the turn with a session.
		reqBody := append([]byte(fmt.Sprintf(`{"metadata":{"session_id":%q},`, session)), convo[i][1:]...)
		resBody := []byte(smallSSE + fmt.Sprintf("\n: turn %d of %s\n", i, session))

		ex := st.Recorder.NewExchange("POST", "/v1/messages", http.Header{})
		if err := st.Recorder.PersistRequest(ex, reqBody); err != nil {
			t.Fatalf("persist req: %v", err)
		}
		ex.Status = 200
		ex.ResHeaders = map[string]string{"content-type": "text/event-stream"}
		if err := st.Recorder.PersistResponse(ex, resBody); err != nil {
			t.Fatalf("persist res: %v", err)
		}
		reqBodies = append(reqBodies, reqBody)
		resBodies = append(resBodies, resBody)
		ids = append(ids, ex.ID)
	}
	return reqBodies, resBodies, ids
}

// archiveAt runs a pass through a read-only Reader, the way the CLI does. The
// caller must have closed the store first so the async index has drained.
func archiveAt(t *testing.T, p testPaths, mutate func(*ArchiveOptions)) ArchiveStats {
	t.Helper()
	rd, err := OpenReader(p.blobs, p.manifests, p.archives, p.index)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer rd.Close()

	o := ArchiveOptions{
		BlobsDir:     p.blobs,
		ManifestsDir: p.manifests,
		ArchivesDir:  p.archives,
		Index:        rd,
		Quiet:        0,
		Apply:        true,
	}
	if mutate != nil {
		mutate(&o)
	}
	st, err := ArchiveSessions(o)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if st.Failed != 0 {
		t.Fatalf("archive reported %d failures: %+v", st.Failed, st)
	}
	return st
}

func readerAt(t *testing.T, p testPaths) *Reader {
	t.Helper()
	rd, err := OpenReader(p.blobs, p.manifests, p.archives, p.index)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rd.Close() })
	return rd
}

func countLiveBlobs(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ".zst" {
			n++
		}
		return nil
	})
	return n
}

// assertBodies checks every recorded body still reads back byte-identically
// through the public reader — the only thing archiving is not allowed to break.
func assertBodies(t *testing.T, p testPaths, ids []string, reqs, ress [][]byte) {
	t.Helper()
	rd := readerAt(t, p)
	for i, id := range ids {
		ex, err := rd.Manifest(id)
		if err != nil {
			t.Fatalf("manifest %s: %v", id, err)
		}
		got, err := rd.Blob(ex.Req.SessionID, ex.ReqRef)
		if err != nil || !bytes.Equal(got, reqs[i]) {
			t.Fatalf("turn %d request: err=%v equal=%v", i, err, bytes.Equal(got, reqs[i]))
		}
		if ress == nil {
			continue
		}
		got, err = rd.Blob(ex.Req.SessionID, ex.ResRef)
		if err != nil || !bytes.Equal(got, ress[i]) {
			t.Fatalf("turn %d response: err=%v equal=%v", i, err, bytes.Equal(got, ress[i]))
		}
	}
}

func TestArchiveSessionEndToEnd(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	reqs, ress, ids := recordSession(t, st, "af5f1ea9-f08a-4d0b-8709-384a1ee6f0ef", 12)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	if n := countLiveBlobs(t, p.blobs); n != 24 {
		t.Fatalf("expected 24 live blobs, got %d", n)
	}

	stats := archiveAt(t, p, nil)
	if stats.Archived != 1 || stats.Refs != 24 {
		t.Fatalf("stats = %+v, want 1 session / 24 refs", stats)
	}
	if n := countLiveBlobs(t, p.blobs); n != 0 {
		t.Errorf("%d live blobs survived archiving", n)
	}
	if stats.BytesTo >= stats.BytesFrom {
		t.Errorf("archive (%d B) is no smaller than the blobs it replaced (%d B)", stats.BytesTo, stats.BytesFrom)
	}
	assertBodies(t, p, ids, reqs, ress)
}

func TestArchiveIsIdempotent(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	recordSession(t, st, "sess-idem-0000-0000-000000000001", 6)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	archiveAt(t, p, nil)
	again := archiveAt(t, p, nil)
	if again.Archived != 0 {
		t.Errorf("second pass archived %d sessions, want 0: %+v", again.Archived, again)
	}
}

func TestArchiveDryRunChangesNothing(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	recordSession(t, st, "sess-dry-00000-0000-000000000002", 5)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	before := countLiveBlobs(t, p.blobs)

	stats := archiveAt(t, p, func(o *ArchiveOptions) { o.Apply = false })
	if stats.Archived != 1 {
		t.Errorf("dry run reported %d sessions, want 1", stats.Archived)
	}
	if n := countLiveBlobs(t, p.blobs); n != before {
		t.Errorf("dry run removed blobs: %d -> %d", before, n)
	}
	if entries, err := os.ReadDir(p.archives); err == nil && len(entries) > 0 {
		t.Errorf("dry run wrote %d entries into the archives dir", len(entries))
	}
}

// A session still being written must be left alone: its newest manifest may be
// the inflight record, which would archive a turn as if it were finished.
func TestArchiveSkipsHotSessions(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	recordSession(t, st, "sess-hot-00000-0000-000000000003", 4)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	stats := archiveAt(t, p, func(o *ArchiveOptions) { o.Quiet = time.Hour })
	if stats.Archived != 0 {
		t.Errorf("archived %d hot sessions, want 0", stats.Archived)
	}
	if n := countLiveBlobs(t, p.blobs); n != 8 {
		t.Errorf("live blobs = %d, want 8 untouched", n)
	}
}

func TestArchiveSkipsInflight(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const session = "sess-inflight-0000-000000000004"
	recordSession(t, st, session, 1)

	ex := st.Recorder.NewExchange("POST", "/v1/messages", http.Header{})
	body := []byte(fmt.Sprintf(`{"metadata":{"session_id":%q},"messages":[]}`, session))
	if err := st.Recorder.PersistRequest(ex, body); err != nil {
		t.Fatal(err)
	}
	inflightRef := ex.ReqRef
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	archiveAt(t, p, nil)
	if _, err := os.Stat(shardPath(p.blobs, inflightRef, ".zst")); err != nil {
		t.Errorf("inflight blob was archived away: %v", err)
	}
}

// The resume path: turns that arrive after a session was archived are folded in
// by a later pass, and the rebuild must not drop what the old archive held.
func TestArchiveRebuildKeepsEarlierTurns(t *testing.T) {
	p := tmpPaths(t)
	const session = "sess-resume-00000-000000000005"

	st := openStoreAt(t, p)
	reqs1, ress1, ids1 := recordSession(t, st, session, 5)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	archiveAt(t, p, nil)

	st = openStoreAt(t, p)
	reqs2, ress2, ids2 := recordSession(t, st, session, 4)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	stats := archiveAt(t, p, nil)
	if stats.Archived != 1 {
		t.Fatalf("resume pass archived %d sessions, want 1", stats.Archived)
	}

	assertBodies(t, p,
		append(append([]string{}, ids1...), ids2...),
		append(append([][]byte{}, reqs1...), reqs2...),
		append(append([][]byte{}, ress1...), ress2...))
}

func TestUnarchiveRestoresExactly(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const session = "sess-undo-0000-0000-000000000006"
	reqs, ress, ids := recordSession(t, st, session, 7)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Snapshot the exact on-disk bytes so the restore can be compared to them.
	original := map[string][]byte{}
	_ = filepath.WalkDir(p.blobs, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(path) == ".zst" {
			b, _ := os.ReadFile(path)
			rel, _ := filepath.Rel(p.blobs, path)
			original[rel] = b
		}
		return nil
	})

	archiveAt(t, p, nil)
	archiveAt(t, p, func(o *ArchiveOptions) { o.Unarchive = true })

	if n := countLiveBlobs(t, p.blobs); n != len(original) {
		t.Errorf("restored %d blobs, want %d", n, len(original))
	}
	for rel, want := range original {
		got, err := os.ReadFile(filepath.Join(p.blobs, rel))
		if err != nil {
			t.Fatalf("restored %s: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("restored %s differs from the original file", rel)
		}
	}
	if _, err := os.Stat(shardPath(p.archives, session, archiveExt)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("archive survived unarchiving: %v", err)
	}
	assertBodies(t, p, ids, reqs, ress)
}

// Sessionless turns cannot be grouped, so they must stay live rather than be
// silently dropped or crash the pass.
func TestArchiveLeavesSessionlessTurnsAlone(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)

	ex := st.Recorder.NewExchange("POST", "/v1/messages?beta=true", http.Header{})
	if err := st.Recorder.PersistRequest(ex, []byte(`{"messages":[]}`)); err != nil {
		t.Fatal(err)
	}
	ex.Status = 200
	ex.ResHeaders = map[string]string{"content-type": "text/event-stream"}
	if err := st.Recorder.PersistResponse(ex, []byte(smallSSE)); err != nil {
		t.Fatal(err)
	}
	if ex.Req.SessionID != "" {
		t.Fatalf("expected a sessionless turn, got %q", ex.Req.SessionID)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	stats := archiveAt(t, p, nil)
	if stats.Archived != 0 {
		t.Errorf("archived %d sessionless sessions, want 0", stats.Archived)
	}
	if _, err := os.Stat(shardPath(p.blobs, ex.ReqRef, ".zst")); err != nil {
		t.Errorf("sessionless blob went missing: %v", err)
	}
}

func TestArchiveOnlyRestrictsToNamedSessions(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const keep = "sess-keep-0000-0000-000000000007"
	const gone = "sess-gone-0000-0000-000000000008"
	recordSession(t, st, keep, 3)
	recordSession(t, st, gone, 3)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	archiveAt(t, p, func(o *ArchiveOptions) { o.Only = []string{gone} })
	if _, err := os.Stat(shardPath(p.archives, gone, archiveExt)); err != nil {
		t.Errorf("named session was not archived: %v", err)
	}
	if _, err := os.Stat(shardPath(p.archives, keep, archiveExt)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unnamed session was archived anyway: %v", err)
	}
}

func TestArchiveLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	l1, err := acquireArchiveLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireArchiveLock(path); !errors.Is(err, errArchiveLocked) {
		t.Errorf("second acquire = %v, want errArchiveLocked", err)
	}
	l1.release()
	l2, err := acquireArchiveLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	l2.release()

	// A lock left behind by a crashed process must not wedge archiving forever.
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * lockTTL)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	l3, err := acquireArchiveLock(path)
	if err != nil {
		t.Fatalf("stale lock was not broken: %v", err)
	}
	l3.release()
}
