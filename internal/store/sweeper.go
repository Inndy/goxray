package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Sweeper folds idle sessions into archives in the background while the proxy
// runs, so disk stays flat without anyone remembering to run `goxray archive`.
//
// It decides what is cold by asking the *index*, never the recorder. The
// observer slot belongs to the dashboard Hub, and invariant 5 exists to keep
// extra work off the request goroutine — a sweeper that hooked the hot path to
// track last-seen times would be trading the fail-closed guarantee for a
// scheduling convenience. The index already knows the last turn of every
// session, it is queried from this goroutine, and a stale answer only delays an
// archive by one tick.
type Sweeper struct {
	opts   ArchiveOptions
	every  time.Duration
	cancel context.CancelFunc
	done   chan struct{}

	// deferred holds sessions that failed a recent pass. Only the sweep goroutine
	// touches it.
	deferred map[string]sweepDefer
}

// sweepDefer is how long a session that keeps failing is held back, and how many
// times in a row it has failed.
//
// A per-tick budget alone is not fairness. Candidates come newest-first, so a
// handful of sessions that fail every pass — an archive path occupied by
// something else, a permission problem, a body over the format limit — are the
// same handful every tick, and everything older sits behind them forever. Holding
// a failure back for twice as long each time means a broken session costs one
// attempt and then gets out of the way, while a transient failure (a busy disk)
// is retried within a tick or two.
type sweepDefer struct {
	fails int
	until time.Time
}

const (
	// sweepMaxSessions bounds work per tick so a shutdown never waits on a long
	// run of rebuilds.
	sweepMaxSessions = 4
	// sweepMaxInterval caps how long the sweeper sleeps regardless of the idle
	// window, so a large --archive-after does not mean a session sits live for
	// hours after it goes quiet.
	sweepMaxInterval = 5 * time.Minute
	// sweepLiveTurns archives a session that never goes quiet once it has this
	// many bodies, so a days-long session cannot hold the store open.
	sweepLiveTurns = 400
	// sweepMaxDefer caps the doubling, so a session that was broken and then
	// fixed is retried within the hour without anyone restarting the proxy.
	sweepMaxDefer = time.Hour
	// sweepForget drops a cooled-down session from the deferral table, so its
	// next failure starts the doubling over rather than inheriting a count from
	// hours ago. It also bounds the table.
	sweepForget = 2 * time.Hour
)

// NewSweeper prepares a background archive sweep. quiet is the idle window; a
// non-positive value means archiving is disabled and Start does nothing.
func NewSweeper(blobsDir, manifestsDir, archivesDir string, idx exchangeLister, quiet time.Duration) *Sweeper {
	return &Sweeper{
		opts: ArchiveOptions{
			BlobsDir:     blobsDir,
			ManifestsDir: manifestsDir,
			ArchivesDir:  archivesDir,
			Index:        idx,
			Quiet:        quiet,
			MaxLiveTurns: sweepLiveTurns,
			Max:          sweepMaxSessions,
			Apply:        true,
		},
		every:    min(quiet/4, sweepMaxInterval),
		deferred: map[string]sweepDefer{},
	}
}

// Start runs the sweep until Stop. It is a no-op when archiving is disabled.
func (s *Sweeper) Start() {
	if s == nil || s.opts.Quiet <= 0 {
		return
	}
	if s.every < time.Second {
		s.every = time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.run(ctx)
}

func (s *Sweeper) run(ctx context.Context) {
	defer close(s.done)
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.once()
		}
	}
}

func (s *Sweeper) once() {
	now := time.Now()
	opts := s.opts
	opts.Skip = s.holdBack(now)

	st, err := ArchiveSessions(opts)
	switch {
	case errors.Is(err, errArchiveLocked):
		// Someone ran `goxray archive` by hand. Theirs wins; try again next tick.
		return
	case err != nil:
		logf("archive sweep: %v", err)
		return
	}
	if st.Archived > 0 {
		logf("archived %d session(s), %d bodies, %s -> %s",
			st.Archived, st.Refs, byteSize(st.BytesFrom), byteSize(st.BytesTo))
	}
	for _, sid := range st.FailedSessions {
		d := s.deferred[sid]
		d.fails++
		wait := s.every << min(d.fails-1, 8)
		if wait > sweepMaxDefer || wait <= 0 {
			wait = sweepMaxDefer
		}
		d.until = now.Add(wait)
		s.deferred[sid] = d
		logf("archive: %s failed %d time(s); holding it back for %v", sid, d.fails, wait)
	}
}

// holdBack is the set of sessions still cooling down after a failure, and prunes
// the ones that have been quiet long enough to deserve a clean slate.
func (s *Sweeper) holdBack(now time.Time) map[string]bool {
	if len(s.deferred) == 0 {
		return nil
	}
	skip := make(map[string]bool, len(s.deferred))
	for sid, d := range s.deferred {
		switch {
		case now.Before(d.until):
			skip[sid] = true
		case now.Sub(d.until) > sweepForget:
			delete(s.deferred, sid)
		}
	}
	return skip
}

// Stop ends the sweep and waits for an in-flight pass to finish. Safe to call
// on a sweeper that was never started.
func (s *Sweeper) Stop() {
	if s == nil || s.cancel == nil {
		return
	}
	s.cancel()
	<-s.done
	s.cancel = nil
}

func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
