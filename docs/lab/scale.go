package main

import (
	"crypto/sha256"
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// cmdScale answers the question the headline ratio cannot: is the dedup win an
// artifact of this corpus being one person, one repo, one day — or is it
// structural? It re-runs the dedup with the chunk index reset at each session
// boundary, which is the pessimistic bound for a store where no two sessions
// share anything, and plots how unique content accumulates with traffic.
func cmdScale(root string) {
	c := loadCorpus(root, "req")
	avgSz := 4 << 10
	minSz, maxSz := avgSz/4, avgSz*4
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))

	// global dedup, tracking the accumulation curve
	fmt.Println("accumulation of unique content as traffic arrives (global dedup, 4K chunks):")
	seen := map[[32]byte]bool{}
	var uniqRaw, seenRaw int64
	marks := []float64{0.1, 0.25, 0.5, 0.75, 1.0}
	mi := 0
	for _, b := range c.blobs {
		for _, p := range fastCDC(b.raw, minSz, avgSz, maxSz) {
			h := sha256.Sum256(p)
			if !seen[h] {
				seen[h] = true
				uniqRaw += int64(len(p))
			}
		}
		seenRaw += int64(len(b.raw))
		if mi < len(marks) && float64(seenRaw) >= marks[mi]*float64(c.totalRaw) {
			fmt.Printf("  after %5.0f MiB of traffic: %6.1f MiB unique (%.1f%%), %d chunks\n",
				mib(seenRaw), mib(uniqRaw), 100*float64(uniqRaw)/float64(seenRaw), len(seen))
			mi++
		}
	}

	// per-session dedup: no cross-session sharing at all
	var isoUniqRaw, isoComp int64
	isoChunks := 0
	for _, s := range c.sessions {
		local := map[[32]byte]bool{}
		for _, i := range c.bySess[s] {
			for _, p := range fastCDC(c.blobs[i].raw, minSz, avgSz, maxSz) {
				h := sha256.Sum256(p)
				if !local[h] {
					local[h] = true
					isoUniqRaw += int64(len(p))
					isoComp += int64(len(enc.EncodeAll(p, nil)))
					isoChunks++
				}
			}
		}
	}
	var globComp int64
	globChunks := 0
	seen2 := map[[32]byte]bool{}
	for _, b := range c.blobs {
		for _, p := range fastCDC(b.raw, minSz, avgSz, maxSz) {
			h := sha256.Sum256(p)
			if !seen2[h] {
				seen2[h] = true
				globComp += int64(len(enc.EncodeAll(p, nil)))
				globChunks++
			}
		}
	}
	fmt.Printf("\nglobal dedup:      %6.2f MiB unique raw -> %6.2f MiB per-chunk zstd (%d chunks)\n",
		mib(uniqRaw), mib(globComp), globChunks)
	fmt.Printf("per-session dedup: %6.2f MiB unique raw -> %6.2f MiB per-chunk zstd (%d chunks)\n",
		mib(isoUniqRaw), mib(isoComp), isoChunks)
	fmt.Printf("cross-session sharing accounts for %.1f%% of the win; %.1f%% is intra-session (structural)\n",
		100*(1-float64(globComp)/float64(isoComp)), 100*float64(globComp)/float64(isoComp))

	// per-session unique content vs that session's final request: the claim is
	// that a session costs about its own final conversation, not the sum.
	fmt.Printf("\n%-12s %6s %10s %12s %12s %8s\n", "session", "turns", "raw sent", "final req", "unique raw", "vs final")
	for _, s := range c.sessions {
		idxs := c.bySess[s]
		if len(idxs) < 10 {
			continue
		}
		local := map[[32]byte]bool{}
		var u, tot int64
		for _, i := range idxs {
			tot += int64(len(c.blobs[i].raw))
			for _, p := range fastCDC(c.blobs[i].raw, minSz, avgSz, maxSz) {
				h := sha256.Sum256(p)
				if !local[h] {
					local[h] = true
					u += int64(len(p))
				}
			}
		}
		final := int64(len(c.blobs[idxs[len(idxs)-1]].raw))
		fmt.Printf("%-12s %6d %9.1fM %11.2fM %11.2fM %7.2fx\n",
			s[:min(8, len(s))], len(idxs), mib(tot), mib(final), mib(u), float64(u)/float64(final))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
