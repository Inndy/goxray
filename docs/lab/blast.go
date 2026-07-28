package main

import (
	"crypto/sha256"
	"fmt"
	"sort"

	"github.com/klauspost/compress/zstd"
)

// cmdBlast measures what each layout costs when a single stored file is lost:
// how many exchanges become unreadable. Storage wins from sharing are paid for
// in exactly this currency, so it is the number that decides how much sharing
// is acceptable.
func cmdBlast(root string) {
	c := loadCorpus(root, "req")
	n := len(c.blobs)
	fmt.Printf("%d request blobs, %d sessions\n\n", n, len(c.sessions))

	report := func(name string, lossSets [][]int, nfiles int) {
		// lossSets[f] = blobs lost if file f is lost
		var counts []int
		total := 0
		worst := 0
		for _, s := range lossSets {
			counts = append(counts, len(s))
			total += len(s)
			if len(s) > worst {
				worst = len(s)
			}
		}
		sort.Ints(counts)
		p50 := counts[len(counts)/2]
		p99 := counts[int(float64(len(counts))*0.99)]
		fmt.Printf("%-34s files=%-6d  mean=%6.1f  p50=%-5d p99=%-6d worst=%-5d (%.0f%% of store)\n",
			name, nfiles, float64(total)/float64(len(counts)), p50, p99, worst, 100*float64(worst)/float64(n))
	}

	// --- today: one file per blob
	{
		sets := make([][]int, n)
		for i := range sets {
			sets[i] = []int{i}
		}
		report("per-file zstd (today)", sets, n)
	}

	// --- shared trained dictionary: every blob depends on the one dict file
	{
		sets := make([][]int, n+1)
		for i := 0; i < n; i++ {
			sets[i] = []int{i}
		}
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		sets[n] = all
		report("trained dict (1 dict file)", sets, n+1)
	}

	// --- delta chain: losing a blob kills its descendants up to the next checkpoint
	for _, K := range []int{8, 16, 32} {
		parent := make([]int, n)
		for i := range parent {
			parent[i] = -1
		}
		for _, s := range c.sessions {
			idxs := c.bySess[s]
			for j := 1; j < len(idxs); j++ {
				if j%K != 0 {
					parent[idxs[j]] = idxs[j-1]
				}
			}
		}
		kids := make([][]int, n)
		for i, p := range parent {
			if p >= 0 {
				kids[p] = append(kids[p], i)
			}
		}
		sets := make([][]int, n)
		for i := range sets {
			var stack, out []int
			stack = append(stack, i)
			for len(stack) > 0 {
				v := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				out = append(out, v)
				stack = append(stack, kids[v]...)
			}
			sets[i] = out
		}
		report(fmt.Sprintf("delta-chain K=%d", K), sets, n)
	}

	// --- delta anchor: losing an anchor kills the whole group
	for _, K := range []int{8, 16} {
		parent := make([]int, n)
		for i := range parent {
			parent[i] = -1
		}
		for _, s := range c.sessions {
			idxs := c.bySess[s]
			anchor := -1
			for j, i := range idxs {
				if j%K == 0 {
					anchor = i
				} else {
					parent[i] = anchor
				}
			}
		}
		kids := make([][]int, n)
		for i, p := range parent {
			if p >= 0 {
				kids[p] = append(kids[p], i)
			}
		}
		sets := make([][]int, n)
		for i := range sets {
			sets[i] = append([]int{i}, kids[i]...)
		}
		report(fmt.Sprintf("delta-anchor K=%d", K), sets, n)
	}

	// --- CDC, one file per turn holding that turn's new chunks + its recipe
	{
		avgSz := 8 << 10
		owner := map[[32]byte]int{}
		refs := make([][][32]byte, n)
		for i, b := range c.blobs {
			parts := fastCDC(b.raw, avgSz/4, avgSz, avgSz*4)
			for _, p := range parts {
				h := sha256.Sum256(p)
				if _, ok := owner[h]; !ok {
					owner[h] = i
				}
				refs[i] = append(refs[i], h)
			}
		}
		sets := make([][]int, n)
		for i := range sets {
			sets[i] = []int{i}
		}
		for i, rs := range refs {
			seen := map[int]bool{i: true}
			for _, h := range rs {
				o := owner[h]
				if !seen[o] {
					seen[o] = true
					sets[o] = append(sets[o], i)
				}
			}
		}
		report("CDC 8K, per-turn chunk files", sets, n)

		// fan-out distribution of individual chunks
		fan := map[[32]byte]int{}
		for _, rs := range refs {
			seen := map[[32]byte]bool{}
			for _, h := range rs {
				if !seen[h] {
					seen[h] = true
					fan[h]++
				}
			}
		}
		var fs []int
		for _, v := range fan {
			fs = append(fs, v)
		}
		sort.Ints(fs)
		hot := 0
		for _, v := range fs {
			if v > 32 {
				hot++
			}
		}
		fmt.Printf("%-34s chunks=%d  fanout p50=%d p99=%d max=%d; %d chunks (%.1f%%) referenced by >32 blobs\n",
			"  chunk fan-out", len(fs), fs[len(fs)/2], fs[int(float64(len(fs))*0.99)], fs[len(fs)-1], hot, 100*float64(hot)/float64(len(fs)))

		// cost of replicating hot chunks (a second copy of anything with fanout > T)
		enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
		content := map[[32]byte][]byte{}
		for _, b := range c.blobs {
			for _, p := range fastCDC(b.raw, avgSz/4, avgSz, avgSz*4) {
				h := sha256.Sum256(p)
				if _, ok := content[h]; !ok {
					content[h] = p
				}
			}
		}
		for _, T := range []int{8, 32, 128} {
			var extra, base int64
			for h, p := range content {
				cz := int64(len(enc.EncodeAll(p, nil)))
				base += cz
				if fan[h] > T {
					extra += cz
				}
			}
			fmt.Printf("%-34s +%.2f MiB (%.0f%% overhead) to keep a 2nd copy of every chunk with fanout > %d\n",
				"  hot-chunk mirroring", mib(extra), 100*float64(extra)/float64(base), T)
		}
	}
}
