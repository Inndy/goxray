package store

import (
	"fmt"
	"io/fs"
	"os"
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

// A session that cannot be archived at all must not hold the store open. The
// per-tick budget is spent newest-first, so without a cooldown the same failures
// would consume it on every tick and nothing older would ever be archived.
func TestSweeperHoldsBackFailingSessions(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	// Enough failures to exhaust the per-tick attempt budget on their own, plus
	// one archivable session older than all of them. Recorded first, so it sorts
	// last: candidates come newest-first.
	var broken []string
	recordSession(t, st, "sess-fair-0000-0000-00000000ffff", 2)
	for i := 0; i < 2*sweepMaxSessions; i++ {
		sid := fmt.Sprintf("sess-brokn-000-0000-00000000%04d", i)
		broken = append(broken, sid)
		recordSession(t, st, sid, 2)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Occupy each broken session's archive path with a directory: the pass builds
	// and verifies, then cannot install.
	for _, sid := range broken {
		blocked := shardPath(p.archives, sid, archiveExt)
		if err := os.MkdirAll(filepath.Join(blocked, "in-the-way"), 0o700); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(blocked, old, old); err != nil {
			t.Fatal(err)
		}
	}

	rd := readerAt(t, p)
	s := NewSweeper(p.blobs, p.manifests, p.archives, rd, time.Minute)
	s.opts.Quiet = 0

	s.once()
	if n := countArchives(t, p.archives); n != 0 {
		t.Fatalf("first pass wrote %d archives; the failures were supposed to fill its budget", n)
	}
	if len(s.deferred) != len(broken) {
		t.Fatalf("deferred %d sessions after a pass, want the %d that failed", len(s.deferred), len(broken))
	}
	for _, sid := range broken {
		if d := s.deferred[sid]; d.fails != 1 || d.until.Before(time.Now()) {
			t.Errorf("%s: deferral = %+v, want one failure and a future cooldown", sid, d)
		}
	}

	// The next tick must reach the session behind them rather than spend the
	// budget on the same failures again.
	s.once()
	if n := countArchives(t, p.archives); n != 1 {
		t.Errorf("%d archives after the second pass; the archivable session is still starving", n)
	}
	for _, sid := range broken {
		if d := s.deferred[sid]; d.fails != 1 {
			t.Errorf("%s was retried while cooling down: %+v", sid, d)
		}
	}

	// Once the cooldown expires it is tried again, and fails again — with a
	// longer cooldown, not a permanent exclusion.
	for sid, d := range s.deferred {
		d.until = time.Now().Add(-time.Second)
		s.deferred[sid] = d
	}
	s.once()
	for _, sid := range broken[:1] {
		if d := s.deferred[sid]; d.fails != 2 {
			t.Errorf("%s was not retried after its cooldown: %+v", sid, d)
		}
	}
}

func TestSweeperForgetsAnOldDeferral(t *testing.T) {
	p := tmpPaths(t)
	s := NewSweeper(p.blobs, p.manifests, p.archives, nil, time.Minute)
	now := time.Now()
	s.deferred["cooling"] = sweepDefer{fails: 1, until: now.Add(time.Minute)}
	s.deferred["expired"] = sweepDefer{fails: 1, until: now.Add(-time.Minute)}
	s.deferred["ancient"] = sweepDefer{fails: 9, until: now.Add(-sweepForget - time.Minute)}

	skip := s.holdBack(now)
	if !skip["cooling"] || skip["expired"] || skip["ancient"] {
		t.Errorf("holdBack = %v, want only the one still cooling down", skip)
	}
	if _, ok := s.deferred["ancient"]; ok {
		t.Error("a long-cooled session kept its failure count, so its next failure inherits the backoff")
	}
	if _, ok := s.deferred["expired"]; !ok {
		t.Error("a just-expired session was forgotten too early")
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
