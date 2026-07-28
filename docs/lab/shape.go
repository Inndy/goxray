package main

import (
	"fmt"
	"time"
)

// cmdShape reports the per-session growth curve and the time span of the store,
// which is what a projection of future disk use has to be built on.
func cmdShape(root string) {
	ms := loadManifests(root)
	if len(ms) == 0 {
		return
	}
	span := ms[len(ms)-1].StartedAt.Sub(ms[0].StartedAt)
	fmt.Printf("store spans %s (%s .. %s), %d turns => %.1f turns/day\n\n",
		span.Round(time.Hour), ms[0].StartedAt.Format("2006-01-02"), ms[len(ms)-1].StartedAt.Format("2006-01-02"),
		len(ms), float64(len(ms))/(span.Hours()/24))

	c := loadCorpus(root, "req")
	fmt.Printf("%-38s %5s  %s\n", "session", "turns", "request size KB by turn (every 8th)")
	var shrink, grow int
	for _, s := range c.sessions {
		idxs := c.bySess[s]
		if len(idxs) < 8 {
			continue
		}
		line := ""
		for j, i := range idxs {
			if j%8 == 0 {
				line += fmt.Sprintf("%d ", len(c.blobs[i].raw)/1024)
			}
			if j > 0 {
				if len(c.blobs[i].raw) < len(c.blobs[idxs[j-1]].raw) {
					shrink++
				} else {
					grow++
				}
			}
		}
		short := s
		if len(short) > 8 {
			short = short[:8]
		}
		fmt.Printf("%-38s %5d  %s\n", short, len(idxs), line)
	}
	fmt.Printf("\nturn-to-turn: %d grew, %d shrank (%.0f%% shrink = context resets / branches)\n",
		grow, shrink, 100*float64(shrink)/float64(grow+shrink))

	// How similar is turn k to turn k-1 vs to the session's first turn? Measured
	// as the length of the shared literal prefix, which is what a naive
	// suffix-append scheme would capture and a delta coder gets for free.
	var prefPrev, prefFirst, tot int64
	for _, s := range c.sessions {
		idxs := c.bySess[s]
		for j := 1; j < len(idxs); j++ {
			cur := c.blobs[idxs[j]].raw
			prefPrev += int64(sharedPrefix(cur, c.blobs[idxs[j-1]].raw))
			prefFirst += int64(sharedPrefix(cur, c.blobs[idxs[0]].raw))
			tot += int64(len(cur))
		}
	}
	fmt.Printf("shared literal prefix with previous turn: %.1f%% of bytes; with first turn: %.1f%%\n",
		100*float64(prefPrev)/float64(tot), 100*float64(prefFirst)/float64(tot))
}

func sharedPrefix(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
