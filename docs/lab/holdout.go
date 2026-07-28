package main

import (
	"crypto/sha256"
	"fmt"
	"sort"

	"github.com/klauspost/compress/zstd"
)

// cmdHoldout removes the "trained on the test set" flattery from the dictionary
// numbers: the dictionary is built only from the earliest sessions, then applied
// to the later ones, which is what a deployed store would actually experience.
// It also reports what a *stale* dictionary costs, since a shipped default
// dictionary ages.
func cmdHoldout(root string) {
	c := loadCorpus(root, "req")
	avgSz := 4 << 10
	minSz, maxSz := avgSz/4, avgSz*4

	// order sessions by first turn so "early" means early in time
	type ss struct {
		id string
		at int
	}
	var order []ss
	for _, s := range c.sessions {
		order = append(order, ss{s, c.bySess[s][0]})
	}
	sort.Slice(order, func(i, j int) bool { return order[i].at < order[j].at })

	split := len(order) / 2
	train := map[string]bool{}
	for i := 0; i < split; i++ {
		train[order[i].id] = true
	}
	var trainBlobs, testBlobs []int
	for i, b := range c.blobs {
		if train[b.sess] {
			trainBlobs = append(trainBlobs, i)
		} else {
			testBlobs = append(testBlobs, i)
		}
	}
	var testRaw int64
	for _, i := range testBlobs {
		testRaw += int64(len(c.blobs[i].raw))
	}
	fmt.Printf("train: %d sessions / %d blobs   test: %d sessions / %d blobs (%.1f MiB raw)\n\n",
		split, len(trainBlobs), len(order)-split, len(testBlobs), mib(testRaw))

	buildDict := func(from []int, size int) []byte {
		uniq := map[[32]byte]bool{}
		var ordered [][]byte
		for _, i := range from {
			for _, p := range fastCDC(c.blobs[i].raw, minSz, avgSz, maxSz) {
				h := sha256.Sum256(p)
				if !uniq[h] {
					uniq[h] = true
					ordered = append(ordered, p)
				}
			}
		}
		step := 1
		if len(ordered) > 512 {
			step = len(ordered) / 512
		}
		var hist []byte
		var samples [][]byte
		for i := 0; i < len(ordered) && len(hist) < size; i += step {
			hist = append(hist, ordered[i]...)
		}
		if len(hist) > size {
			hist = hist[len(hist)-size:]
		}
		for i := 0; i < len(ordered) && len(samples) < 256; i += step {
			s := ordered[i]
			if len(s) > 8<<10 {
				s = s[:8<<10]
			}
			samples = append(samples, s)
		}
		d, err := zstd.BuildDict(zstd.BuildDictOptions{ID: 1, Contents: samples, History: hist, Level: zstd.SpeedDefault})
		if err != nil {
			fmt.Println("dict build:", err)
			return nil
		}
		return d
	}

	// measure: dedup the test set only (cold store), compress chunks with dict
	measure := func(label string, dict []byte, cold bool) {
		var enc *zstd.Encoder
		var err error
		if dict != nil {
			enc, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1), zstd.WithEncoderDict(dict))
		} else {
			enc, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
		}
		if err != nil || enc == nil {
			fmt.Printf("  %-46s encoder unavailable: %v\n", label, err)
			return
		}
		seen := map[[32]byte]bool{}
		if !cold {
			for _, i := range trainBlobs {
				for _, p := range fastCDC(c.blobs[i].raw, minSz, avgSz, maxSz) {
					seen[sha256.Sum256(p)] = true
				}
			}
		}
		var packs, recipes int64
		nchunks := 0
		plain, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
		for _, i := range testBlobs {
			parts := fastCDC(c.blobs[i].raw, minSz, avgSz, maxSz)
			for _, p := range parts {
				h := sha256.Sum256(p)
				if seen[h] {
					continue
				}
				seen[h] = true
				packs += int64(len(enc.EncodeAll(p, nil)))
				nchunks++
			}
			rb := make([]byte, 0, len(parts)*8)
			for _, p := range parts {
				h := sha256.Sum256(p)
				rb = append(rb, h[:8]...)
			}
			recipes += int64(len(plain.EncodeAll(rb, nil)))
		}
		total := packs + recipes + int64(nchunks*44)
		if dict != nil {
			total += int64(len(dict))
		}
		fmt.Printf("  %-46s %6.2f MiB stored  (%5.1fx vs raw)  packs %.2fM recipes %.2fM new-chunks %d\n",
			label, mib(total), float64(testRaw)/float64(total), mib(packs), mib(recipes), nchunks)
	}

	// baseline for the test half
	base, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	var baseSz int64
	for _, i := range testBlobs {
		baseSz += int64(len(base.EncodeAll(c.blobs[i].raw, nil)))
	}
	fmt.Printf("  %-46s %6.2f MiB stored  (%5.1fx vs raw)\n", "per-file zstd default (today)", mib(baseSz), float64(testRaw)/float64(baseSz))

	fmt.Println("\ncold store (test sessions only, nothing shared from train):")
	measure("CDC 4K, no dict", nil, true)
	measure("CDC 4K + 512K dict trained on TEST (optimistic)", buildDict(testBlobs, 512<<10), true)
	measure("CDC 4K + 512K dict trained on TRAIN (honest)", buildDict(trainBlobs, 512<<10), true)

	fmt.Println("\nwarm store (train sessions already stored, chunks shared):")
	measure("CDC 4K, no dict", nil, false)
	measure("CDC 4K + 512K dict trained on TRAIN (honest)", buildDict(trainBlobs, 512<<10), false)
}
