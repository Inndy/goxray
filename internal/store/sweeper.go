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
		every: min(quiet/4, sweepMaxInterval),
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
	st, err := ArchiveSessions(s.opts)
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
