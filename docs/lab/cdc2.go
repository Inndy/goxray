package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/klauspost/compress/zstd"
)

// benchCDCBundled models the design an implementation would actually ship:
// content-defined chunks, deduplicated globally, with *new* chunks appended in
// insertion order into solid zstd "bundles" of ~bundleSz uncompressed bytes.
// Insertion order is session order, so a session's chunks land together and a
// read touches few bundles. Reading a blob decodes each needed bundle once,
// through a small LRU that models the page cache / a resident bundle cache.
func benchCDCBundled(c *corpus, avgSz, bundleSz, lvl int, name string, lruCap int) result {
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(level(lvl)), zstd.WithEncoderConcurrency(1))
	dec, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(1<<31))
	r := result{name: name, ok: true, depthMax: 1}
	minSz, maxSz := avgSz/4, avgSz*4

	type loc struct{ bundle, off, size int }
	index := map[[32]byte]loc{}
	recipes := make([][][32]byte, len(c.blobs))

	var bundleRaw []byte
	var bundles [][]byte // compressed
	var bundleRawLen []int
	flush := func() {
		if len(bundleRaw) == 0 {
			return
		}
		bundles = append(bundles, enc.EncodeAll(bundleRaw, nil))
		bundleRawLen = append(bundleRawLen, len(bundleRaw))
		bundleRaw = nil
	}

	var recipeBytes int64
	var nchunks, ndup int
	for i, b := range c.blobs {
		t0 := time.Now()
		parts := fastCDC(b.raw, minSz, avgSz, maxSz)
		rec := make([][32]byte, 0, len(parts))
		for _, p := range parts {
			h := sha256.Sum256(p)
			rec = append(rec, h)
			nchunks++
			if _, ok := index[h]; ok {
				ndup++
				continue
			}
			index[h] = loc{bundle: len(bundles), off: len(bundleRaw), size: len(p)}
			bundleRaw = append(bundleRaw, p...)
			if len(bundleRaw) >= bundleSz {
				flush()
			}
		}
		d := time.Since(t0).Nanoseconds()
		r.encNs += d
		if d > r.encMaxNs {
			r.encMaxNs = d
		}
		recipes[i] = rec
		// recipe on disk: 8-byte chunk id per reference, zstd'd
		buf := make([]byte, 0, len(rec)*8)
		for _, h := range rec {
			buf = binary.LittleEndian.AppendUint64(buf, binary.LittleEndian.Uint64(h[:8]))
		}
		recipeBytes += int64(len(enc.EncodeAll(buf, nil)))
	}
	flush()

	var bundleBytes int64
	for _, b := range bundles {
		bundleBytes += int64(len(b))
	}
	// chunk index: hash(32) + bundle(4) + off(4) + size(4). Derived state, but
	// count it — it has to live somewhere.
	indexBytes := int64(len(index) * 44)
	r.stored = bundleBytes + recipeBytes + indexBytes
	r.note = fmt.Sprintf("%d refs, %.1f%% dup, %d unique chunks, %d bundles; bundles %.1fM recipes %.2fM index %.2fM",
		nchunks, 100*float64(ndup)/float64(nchunks), len(index), len(bundles),
		mib(bundleBytes), mib(recipeBytes), mib(indexBytes))

	// read path
	lru := map[int][]byte{}
	var order []int
	getBundle := func(id int) []byte {
		if v, ok := lru[id]; ok {
			return v
		}
		out, err := dec.DecodeAll(bundles[id], nil)
		if err != nil {
			r.ok = false
		}
		if lruCap > 0 {
			lru[id] = out
			order = append(order, id)
			if len(order) > lruCap {
				delete(lru, order[0])
				order = order[1:]
			}
		}
		return out
	}
	var ampBytes int64
	var bundleTouches int
	for i, b := range c.blobs {
		t0 := time.Now()
		out := make([]byte, 0, len(b.raw))
		seen := map[int]bool{}
		for _, h := range recipes[i] {
			l := index[h]
			if !seen[l.bundle] {
				seen[l.bundle] = true
				bundleTouches++
				if _, cached := lru[l.bundle]; !cached {
					ampBytes += int64(bundleRawLen[l.bundle])
				}
			}
			bd := getBundle(l.bundle)
			out = append(out, bd[l.off:l.off+l.size]...)
		}
		d := time.Since(t0).Nanoseconds()
		r.decNs += d
		if d > r.decMaxNs {
			r.decMaxNs = d
		}
		if !bytes.Equal(out, b.raw) {
			r.ok = false
		}
	}
	r.readAmp = float64(ampBytes) / float64(c.totalRaw)
	r.note += fmt.Sprintf("; %.1f bundles/read", float64(bundleTouches)/float64(len(c.blobs)))
	return r
}
