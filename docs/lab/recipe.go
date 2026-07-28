package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"github.com/klauspost/compress/zstd"
)

// cmdRecipe examines two things the headline numbers hide:
//
//  1. Recipe encoding. Storing 8 bytes of chunk *hash* per reference is storing
//     random bytes, which zstd cannot compress. Chunk ids assigned in insertion
//     order are near-monotonic within a turn (a turn re-references last turn's
//     chunks in the same order), so delta+varint then zstd should collapse them.
//  2. Read fan-out: how many distinct files a single blob read has to touch,
//     which decides whether chunks need packing or can live one file per turn.
func cmdRecipe(root string) {
	c := loadCorpus(root, "req")
	plain, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))

	for _, avgSz := range []int{4 << 10, 8 << 10} {
		minSz, maxSz := avgSz/4, avgSz*4
		ids := map[[32]byte]uint64{}
		owner := map[[32]byte]int{}
		var hashRecipes, seqRecipes int64
		var nrefs int
		var fanFiles []int
		var totalHashRaw, totalSeqRaw int64

		for i, b := range c.blobs {
			parts := fastCDC(b.raw, minSz, avgSz, maxSz)
			hbuf := make([]byte, 0, len(parts)*8)
			sbuf := make([]byte, 0, len(parts)*3)
			var prev int64
			files := map[int]bool{}
			for _, p := range parts {
				h := sha256.Sum256(p)
				id, ok := ids[h]
				if !ok {
					id = uint64(len(ids))
					ids[h] = id
					owner[h] = i
				}
				files[owner[h]] = true
				nrefs++
				hbuf = append(hbuf, h[:8]...)
				// zigzag delta of the sequential id
				d := int64(id) - prev
				prev = int64(id)
				u := uint64(d<<1) ^ uint64(d>>63)
				sbuf = binary.AppendUvarint(sbuf, u)
			}
			totalHashRaw += int64(len(hbuf))
			totalSeqRaw += int64(len(sbuf))
			hashRecipes += int64(len(plain.EncodeAll(hbuf, nil)))
			seqRecipes += int64(len(plain.EncodeAll(sbuf, nil)))
			delete(files, i)
			fanFiles = append(fanFiles, len(files))
		}
		sort.Ints(fanFiles)
		fmt.Printf("chunk size %dK — %d refs, %d unique\n", avgSz>>10, nrefs, len(ids))
		fmt.Printf("  recipes as 8-byte hashes:   %6.2f MiB raw -> %6.2f MiB zstd\n", mib(totalHashRaw), mib(hashRecipes))
		fmt.Printf("  recipes as delta-varint ids:%6.2f MiB raw -> %6.2f MiB zstd  (%.1fx smaller)\n",
			mib(totalSeqRaw), mib(seqRecipes), float64(hashRecipes)/float64(seqRecipes))
		fmt.Printf("  read fan-out if chunks live in per-turn files: p50=%d p90=%d p99=%d max=%d distinct files\n\n",
			fanFiles[len(fanFiles)/2], fanFiles[int(float64(len(fanFiles))*0.9)],
			fanFiles[int(float64(len(fanFiles))*0.99)], fanFiles[len(fanFiles)-1])
	}

	// sha256 throughput on this machine — it is on the fail-closed write path
	big := c.blobs[0].raw
	for _, b := range c.blobs {
		if len(b.raw) > len(big) {
			big = b.raw
		}
	}
	t0 := time.Now()
	n := 20
	for i := 0; i < n; i++ {
		for off := 0; off < len(big); off += 4 << 10 {
			end := off + 4<<10
			if end > len(big) {
				end = len(big)
			}
			sha256.Sum256(big[off:end])
		}
	}
	el := time.Since(t0)
	fmt.Printf("sha256 over 4K chunks: %.0f MB/s (%.2f ms for the largest %.2f MiB request)\n",
		float64(len(big))*float64(n)/1e6/el.Seconds(), float64(el.Nanoseconds())/float64(n)/1e6, mib(int64(len(big))))

	t0 = time.Now()
	for i := 0; i < n; i++ {
		fastCDC(big, 1<<10, 4<<10, 16<<10)
	}
	el = time.Since(t0)
	fmt.Printf("fastCDC chunking:      %.0f MB/s (%.2f ms for the same request)\n",
		float64(len(big))*float64(n)/1e6/el.Seconds(), float64(el.Nanoseconds())/float64(n)/1e6)
}
