package store

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// resumedInstall leaves a store one step from installing over an existing
// archive: a session archived, then resumed with more turns. It hands back the
// pass and the blob cache the pass will read through, so a test can inspect what
// the install left open.
func resumedInstall(t *testing.T, p testPaths, session string) (ArchiveOptions, *localBlobs) {
	t.Helper()
	st := openStoreAt(t, p)
	recordSession(t, st, session, 3)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	archiveAt(t, p, nil)

	st = openStoreAt(t, p)
	recordSession(t, st, session, 2)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	lock, err := acquireArchiveLock(filepath.Join(p.archives, ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.release() })
	blobs, err := newLocalBlobs(p.blobs, p.archives)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	return ArchiveOptions{
		BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives,
		Index: readerAt(t, p), Apply: true, lock: lock,
	}, blobs
}

// The install renames the new archive over the old one, and the verify pass
// immediately before it reads this session's already-archived bodies through
// blobs — which caches the archive it opened, i.e. exactly the file about to be
// replaced. Go opens files on Windows without FILE_SHARE_DELETE, so that rename
// fails there with "Access is denied": the sweep rebuilt the session, refused to
// install it, and logged the same failure every tick forever. Nothing can fail on
// unix, so what is asserted is the handle rather than the rename.
func TestArchiveInstallHoldsNoHandleOnItsTarget(t *testing.T) {
	p := tmpPaths(t)
	const session = "sess-reinstall-000-00000000012"
	o, blobs := resumedInstall(t, p, session)

	var stats ArchiveStats
	if _, err := o.archiveSession(&stats, blobs, newLocalManifests(p.manifests), session); err != nil {
		t.Fatalf("resume install: %v", err)
	}
	if stats.Archived != 1 {
		t.Fatalf("resume pass archived %d sessions, want 1: %+v", stats.Archived, stats)
	}
	final := archiveFile(p.archives, session)
	for _, a := range blobs.cache {
		if a.path == final {
			t.Errorf("the pass still holds %s open after installing over it", filepath.Base(final))
		}
	}
}

// A pass can only close its own cache. In serve the dashboard reads through the
// store's, which keeps the last two archives it served open, so a session anyone
// viewed after it was archived could never be re-archived on Windows — the same
// blocked rename, from a handle the pass has no reach into. Release is the seam
// that lets the install drop that one too.
func TestArchiveInstallReleasesAnotherReadersHandle(t *testing.T) {
	p := tmpPaths(t)
	const session = "sess-viewed-0000-000000000013"
	o, blobs := resumedInstall(t, p, session)

	rows, err := readerAt(t, p).Query(Filter{Session: session, IncludeNoise: true})
	if err != nil {
		t.Fatal(err)
	}
	var archived string
	for _, ex := range rows {
		if _, err := os.Stat(shardPath(p.blobs, ex.ReqRef, ".zst")); errors.Is(err, fs.ErrNotExist) {
			archived = ex.ReqRef
			break
		}
	}
	if archived == "" {
		t.Fatal("no archived body to read: the first pass left every blob live")
	}

	// What a dashboard detail view does, and what it leaves behind.
	viewer, err := newLocalBlobs(p.blobs, p.archives)
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	if _, err := viewer.Get(session, archived); err != nil {
		t.Fatalf("read archived body: %v", err)
	}
	if len(viewer.cache) != 1 {
		t.Fatalf("reading an archived body cached %d archives, want 1", len(viewer.cache))
	}
	o.Release = viewer.forget

	var stats ArchiveStats
	if _, err := o.archiveSession(&stats, blobs, newLocalManifests(p.manifests), session); err != nil {
		t.Fatalf("resume install: %v", err)
	}
	if len(viewer.cache) != 0 {
		t.Errorf("the install left %d handle(s) open from the other reader", len(viewer.cache))
	}
	// The reader must still serve the session after having its handle dropped.
	if _, err := viewer.Get(session, archived); err != nil {
		t.Errorf("read after release: %v", err)
	}
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

// A session id arrives in the body of a recorded request, and the proxy port has
// no Host guard, so it is attacker data in the same sense a filename inside a zip
// is. Archives are found by name, so an id used verbatim would let one request
// point an applied sweep at a .gxa path anywhere the process can write. The
// recorder normalizes it on the way in, which is what keeps every read path
// downstream free of the question.
func TestHostileSessionIdIsNormalizedOnTheWayIn(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const hostile = "../../../outsideAA"
	reqs, ress, ids := recordSession(t, st, hostile, 3)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Nothing downstream of the recorder ever sees the raw id.
	rd := readerAt(t, p)
	ex, err := rd.Manifest(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	sid := ex.Req.SessionID
	if sid == hostile {
		t.Fatalf("manifest recorded the raw session id %q", sid)
	}
	if sid != normalizeSessionID(hostile) || !isPlainToken(sid) {
		t.Fatalf("recorded session id %q is not a usable name", sid)
	}

	stats := archiveAt(t, p, nil)
	if stats.Archived != 1 {
		t.Fatalf("archived %d sessions, want 1: %+v", stats.Archived, stats)
	}
	// shardPath is what the archiver used to be handed the raw id through, so it
	// still reproduces exactly where the escape landed.
	if _, err := os.Stat(shardPath(p.archives, hostile, archiveExt)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an archive landed outside the store: %v", err)
	}
	inside := shardPath(p.archives, sid, archiveExt)
	if _, err := os.Stat(inside); err != nil {
		t.Errorf("no archive under the recorded session id: %v", err)
	}
	if rel, err := filepath.Rel(p.archives, inside); err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("archive path %q is not under %q", inside, p.archives)
	}
	// Still one session, still readable: normalizing keeps the grouping that
	// blanking the id would have thrown away.
	assertBodies(t, p, ids, reqs, ress)
}

// Manifests written before normalization existed still hold raw ids, and
// migrate-usage preserves them, so the archiver must refuse to build a path from
// one rather than assume the boundary caught everything.
func TestArchiveSkipsASessionIdItCannotFile(t *testing.T) {
	p := tmpPaths(t)
	const hostile = "../../../legacyAA"

	blobs, err := newLocalBlobs(p.blobs, p.archives)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := blobs.Put([]byte(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	blobs.Close()
	ex := &model.Exchange{
		ID: "019fa301-9d78-77fe-9d3f-1ff62ba44500", CaptureVer: model.CaptureVersion,
		State: model.StateComplete, StartedAt: time.Now().Add(-time.Hour),
		Req: model.ReqMeta{SessionID: hostile}, ReqRef: ref,
	}
	if err := newLocalManifests(p.manifests).Write(ex); err != nil {
		t.Fatal(err)
	}
	// Opening the store rebuilds the index from the manifests, so the archiver
	// sees this session exactly as it would see one recorded before the boundary
	// existed — with a live blob to fold in.
	if err := openStoreAt(t, p).Close(); err != nil {
		t.Fatal(err)
	}

	stats := archiveAt(t, p, nil)
	if stats.Archived != 0 || stats.Skipped != 0 || stats.Sessions != 0 {
		t.Errorf("stats = %+v, want the session filed nowhere and not attempted", stats)
	}
	// And by id, the way `goxray archive --session` reaches it directly.
	stats = archiveAt(t, p, func(o *ArchiveOptions) { o.Only = []string{hostile} })
	if stats.Archived != 0 || stats.Skipped != 1 {
		t.Errorf("stats = %+v, want the session skipped", stats)
	}
	if _, err := os.Stat(shardPath(p.archives, hostile, archiveExt)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an archive landed outside the store: %v", err)
	}
	if _, err := os.Stat(shardPath(p.blobs, ref, ".zst")); err != nil {
		t.Errorf("the blob was unlinked for a session that was never archived: %v", err)
	}
}

func TestNormalizeSessionID(t *testing.T) {
	const uuid = "af5f1ea9-f08a-4d0b-8709-384a1ee6f0ef"
	if got := normalizeSessionID(uuid); got != uuid {
		t.Errorf("normalizeSessionID(uuid) = %q, want it unchanged", got)
	}
	if got := normalizeSessionID(""); got != "" {
		t.Errorf("normalizeSessionID(sessionless) = %q, want empty", got)
	}
	seen := map[string]string{}
	for _, bad := range []string{
		"../../../outsideAA", "a/b", `a\b`, ".hidden", ".", "..", "trailing.",
		"nul", "NUL.gxa", "com1", "sess id", strings.Repeat("x", 65), "sess\x00",
	} {
		got := normalizeSessionID(bad)
		if got == bad {
			t.Errorf("normalizeSessionID(%q) passed it through", bad)
			continue
		}
		if !isPlainToken(got) {
			t.Errorf("normalizeSessionID(%q) = %q, still not a usable name", bad, got)
		}
		// Distinct sessions must stay distinct: sharing a name would put two
		// sessions in one archive.
		if prev, dup := seen[got]; dup {
			t.Errorf("%q and %q both normalize to %q", prev, bad, got)
		}
		seen[got] = bad
		// Re-parsing a body through migrate-usage normalizes it again.
		if again := normalizeSessionID(got); again != got {
			t.Errorf("normalizeSessionID is not idempotent: %q -> %q -> %q", bad, got, again)
		}
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

// Taking over an abandoned lock has to be survivable by its previous holder:
// if that pass is in fact still running, it must neither delete its successor's
// lock nor go on installing archives and unlinking blobs beside it.
func TestArchiveLockSurvivesBeingTakenOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	first, err := acquireArchiveLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.stillHeld() {
		t.Fatal("holder does not recognise its own lock")
	}

	// Age the lock as if the holder had been killed, and let another pass in.
	old := time.Now().Add(-2 * lockTTL)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	second, err := acquireArchiveLock(path)
	if err != nil {
		t.Fatalf("takeover failed: %v", err)
	}
	if first.stillHeld() {
		t.Error("displaced holder still believes it holds the lock")
	}
	if !second.stillHeld() {
		t.Error("new holder does not recognise its own lock")
	}

	first.release()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the displaced holder deleted its successor's lock: %v", err)
	}
	second.release()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the owner left its lock behind: %v", err)
	}
}

// The other half of the takeover: a pass that has lost its lock must not
// install, because its successor may already have archived the same session
// with turns this one never saw.
func TestArchiveWithoutTheLockChangesNothing(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const session = "sess-nolock-0000-000000000009"
	recordSession(t, st, session, 3)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ensureDir(p.archives); err != nil {
		t.Fatal(err)
	}

	lock, err := acquireArchiveLock(filepath.Join(p.archives, ".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.release()
	// Another pass declared this one abandoned and took the lock.
	if err := os.WriteFile(lock.path, []byte("someone else\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	blobs, err := newLocalBlobs(p.blobs, p.archives)
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	o := ArchiveOptions{
		BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives,
		Index: readerAt(t, p), Apply: true, lock: lock,
	}
	var stats ArchiveStats
	attempted, err := o.archiveSession(&stats, blobs, newLocalManifests(p.manifests), session)
	if !errors.Is(err, errArchiveLocked) {
		t.Fatalf("install without the lock returned %v, want errArchiveLocked", err)
	}
	if !attempted {
		t.Error("a failed install should still count as an attempt")
	}
	if stats.Archived != 0 {
		t.Errorf("reported %d sessions archived after refusing to install", stats.Archived)
	}
	if n := countLiveBlobs(t, p.blobs); n != 6 {
		t.Errorf("%d live blobs left, want all 6: nothing may be deleted without the lock", n)
	}
	if _, err := os.Stat(shardPath(p.archives, session, archiveExt)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an archive was installed without the lock: %v", err)
	}
}

// A session that fails to install must not be counted as archived: the CLI's
// summary and the sweep log are the only places anyone sees what happened, and
// they were reporting bodies as folded in while the blobs were still live files.
func TestArchiveDoesNotCountFailuresAsArchived(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const broken = "sess-broken-0000-000000000010"
	const fine = "sess-fine-00000-000000000011"
	recordSession(t, st, broken, 3)
	recordSession(t, st, fine, 3)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Occupy one session's archive path with a non-empty directory: the pass
	// builds and verifies, then cannot rename over it. Backdate it so the
	// candidate scan does not read it as an archive that already covers the
	// session.
	blocked := shardPath(p.archives, broken, archiveExt)
	if err := os.MkdirAll(filepath.Join(blocked, "in-the-way"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(blocked, old, old); err != nil {
		t.Fatal(err)
	}

	stats, err := ArchiveSessions(ArchiveOptions{
		BlobsDir: p.blobs, ManifestsDir: p.manifests, ArchivesDir: p.archives,
		Index: readerAt(t, p), Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Failed != 1 {
		t.Errorf("reported %d failures, want 1: %+v", stats.Failed, stats)
	}
	if stats.Archived != 1 {
		t.Errorf("reported %d sessions archived, want only the one that finished: %+v", stats.Archived, stats)
	}
	if stats.Refs != 6 {
		t.Errorf("reported %d bodies folded in, want the 6 of the session that finished", stats.Refs)
	}
	if n := countLiveBlobs(t, p.blobs); n != 6 {
		t.Errorf("%d live blobs left, want the failed session's 6 untouched", n)
	}
}
