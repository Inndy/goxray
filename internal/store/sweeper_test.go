package store

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"
	"time"
)

func countArchives(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == archiveExt {
			n++
		}
		return nil
	})
	return n
}

// Each pass must do bounded work *and* make progress. Capping the candidate
// list rather than the work would let already-archived sessions — which sort
// first, being the most recently active — fill the budget forever.
func TestSweeperMakesProgressInBoundedBatches(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const sessions = sweepMaxSessions + 2
	for i := 0; i < sessions; i++ {
		recordSession(t, st, fmt.Sprintf("sess-sweep-0000-0000-00000000%04d", i), 2)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	rd := readerAt(t, p)
	s := NewSweeper(p.blobs, p.manifests, p.archives, rd, time.Minute)
	s.opts.Quiet = 0 // every session is eligible immediately

	s.once()
	if n := countArchives(t, p.archives); n != sweepMaxSessions {
		t.Fatalf("first pass wrote %d archives, want the %d-session cap", n, sweepMaxSessions)
	}
	s.once()
	if n := countArchives(t, p.archives); n != sessions {
		t.Fatalf("second pass left %d archives, want all %d", n, sessions)
	}
	// A third pass has nothing left to do and must not churn.
	s.once()
	if n := countLiveBlobs(t, p.blobs); n != 0 {
		t.Errorf("%d live blobs remain after three passes", n)
	}
}

// A session whose archive is newer than its last turn must be skipped without
// reading a single manifest — that filter is what keeps a store full of
// archived sessions from re-examining all of them every tick.
func TestSweeperSkipsAlreadyArchivedSessions(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	recordSession(t, st, "sess-done-0000-0000-000000000001", 3)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	rd := readerAt(t, p)
	s := NewSweeper(p.blobs, p.manifests, p.archives, rd, time.Minute)
	s.opts.Quiet = 0
	s.once()

	targets, err := s.opts.candidates()
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Errorf("candidates() still offers %v after archiving", targets)
	}
}

func TestSweeperDisabledDoesNothing(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	recordSession(t, st, "sess-off-0000-0000-000000000002", 2)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	before := countLiveBlobs(t, p.blobs)

	rd := readerAt(t, p)
	s := NewSweeper(p.blobs, p.manifests, p.archives, rd, 0)
	s.Start() // no-op
	s.Stop()  // must not block or panic
	if n := countLiveBlobs(t, p.blobs); n != before {
		t.Errorf("a disabled sweeper archived something: %d -> %d", before, n)
	}
	if n := countArchives(t, p.archives); n != 0 {
		t.Errorf("a disabled sweeper wrote %d archives", n)
	}
}

func TestSweeperStartStopTerminates(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	recordSession(t, st, "sess-run-0000-0000-000000000003", 2)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	rd := readerAt(t, p)
	s := NewSweeper(p.blobs, p.manifests, p.archives, rd, time.Hour)
	s.Start()
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	s.Stop() // idempotent
}

// A hand-run `goxray archive` holding the lock must make the sweep back off
// rather than fail loudly or fight it.
func TestSweeperYieldsToAnotherPass(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	recordSession(t, st, "sess-lock-0000-0000-000000000004", 2)
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

	rd := readerAt(t, p)
	s := NewSweeper(p.blobs, p.manifests, p.archives, rd, time.Minute)
	s.opts.Quiet = 0
	s.once() // must return quietly

	if n := countArchives(t, p.archives); n != 0 {
		t.Errorf("sweep archived %d sessions while another pass held the lock", n)
	}
	lock.release()

	s.once()
	if n := countArchives(t, p.archives); n != 1 {
		t.Errorf("sweep did not resume after the lock was released: %d archives", n)
	}
}
