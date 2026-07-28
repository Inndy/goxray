package main

import (
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/klauspost/compress/zstd"
)

// cmdGenerations prices the one case where a session archive cannot stay
// self-contained: the session is archived, then resumed, so a second archive
// arrives holding turns that share most of their content with the first.
//
// Three ways out, measured against each other:
//
//	independent  every generation self-contained; shared content stored again
//	dependent    gen N's recipes may reference gen N-1's chunk pool
//	rebuild      throw gen N-1 away and re-archive the whole session
//
// "dependent" is the only one that creates a cross-archive edge, so it is the
// only one that needs dependency tracking to stay correct under deletion. The
// question is whether it buys enough to be worth that.
func cmdGenerations(root string, args []string) {
	every := 50
	if len(args) > 0 {
		fmt.Sscanf(args[0], "%d", &every)
	}
	c := loadCorpus(root, "req")
	avgSz := 8 << 10
	minSz, maxSz := avgSz/4, avgSz*4
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<24))

	fmt.Printf("simulating a resume every %d turns (8K chunks, solid pools)\n\n", every)

	// pool cost for an arbitrary set of turns, optionally excluding chunks
	// already present in a set of prior generations.
	poolCost := func(turns []int, prior []map[[32]byte]bool) (int64, int, int64) {
		local := map[[32]byte]bool{}
		var raw []byte
		var recipes int64
		nchunks := 0
		for _, i := range turns {
			parts := fastCDC(c.blobs[i].raw, minSz, avgSz, maxSz)
			recipes += int64(len(parts) * 2) // varint ids, pre-zstd
			for _, p := range parts {
				h := sha256.Sum256(p)
				if local[h] {
					continue
				}
				inPrior := false
				for _, pr := range prior {
					if pr[h] {
						inPrior = true
						break
					}
				}
				if inPrior {
					continue
				}
				local[h] = true
				raw = append(raw, p...)
				nchunks++
			}
		}
		return int64(len(enc.EncodeAll(raw, nil))), nchunks, recipes
	}

	var indepTotal, depTotal, rebuildTotal int64
	var indepGens, depEdges int
	var rebuildNs int64

	fmt.Printf("%-10s %5s %5s %11s %11s %11s\n", "session", "turns", "gens", "independent", "dependent", "rebuild")
	for _, sid := range c.sessions {
		idxs := c.bySess[sid]
		if len(idxs) < every {
			// one generation: all three strategies coincide
			p, _, r := poolCost(idxs, nil)
			indepTotal += p + r
			depTotal += p + r
			rebuildTotal += p + r
			indepGens++
			continue
		}
		var gens [][]int
		for i := 0; i < len(idxs); i += every {
			e := i + every
			if e > len(idxs) {
				e = len(idxs)
			}
			gens = append(gens, idxs[i:e])
		}

		var indep, dep int64
		var prior []map[[32]byte]bool
		for _, g := range gens {
			p, _, r := poolCost(g, nil)
			indep += p + r
			pd, _, rd := poolCost(g, prior)
			dep += pd + rd
			seen := map[[32]byte]bool{}
			for _, i := range g {
				for _, pp := range fastCDC(c.blobs[i].raw, minSz, avgSz, maxSz) {
					seen[sha256.Sum256(pp)] = true
				}
			}
			prior = append(prior, seen)
		}
		indepGens += len(gens)
		depEdges += len(gens) - 1

		t0 := time.Now()
		reb, _, rr := poolCost(idxs, nil)
		rebuildNs += time.Since(t0).Nanoseconds()
		reb += rr

		indepTotal += indep
		depTotal += dep
		rebuildTotal += reb
		fmt.Printf("%-10s %5d %5d %10.2fM %10.2fM %10.2fM\n", sid[:8], len(idxs), len(gens),
			mib(indep), mib(dep), mib(reb))
	}

	fmt.Printf("\n%-28s %8.2f MiB  (%d archives, no cross-archive edges)\n", "independent generations", mib(indepTotal), indepGens)
	fmt.Printf("%-28s %8.2f MiB  (%d cross-archive edges to track)\n", "dependent generations", mib(depTotal), depEdges)
	fmt.Printf("%-28s %8.2f MiB  (%d archives, no edges)\n", "rebuild whole session", mib(rebuildTotal), len(c.sessions))
	fmt.Printf("\ndependency tracking buys %.2f MiB over independent (%.0f%%),\n",
		mib(indepTotal-depTotal), 100*float64(indepTotal-depTotal)/float64(indepTotal))
	fmt.Printf("but rebuilding beats it by %.2f MiB and costs %.0f ms per resumed session.\n",
		mib(depTotal-rebuildTotal), float64(rebuildNs)/1e6/float64(max(1, depEdges)))
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
