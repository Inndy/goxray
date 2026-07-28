package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/klauspost/compress/zstd"
)

// benchCDCPacked models the design that keeps read amplification at 1.0 while
// still compressing small chunks well: content-defined chunks, deduplicated,
// each compressed as its own zstd frame *against a shared static dictionary*,
// and stored densely inside pack files addressed by (pack, offset, len).
//
// The dictionary is what makes per-chunk framing affordable — a 4 KB chunk
// compressed in isolation barely compresses, but primed with a dictionary of
// representative traffic it does. Because the dictionary is static, one encoder
// and one decoder are shared process-wide: no per-blob codec construction, which
// is the cost that makes delta chains slow to resolve.
func benchCDCPacked(c *corpus, avgSz, dictSize, lvl int, name string) result {
	r := result{name: name, ok: true, depthMax: 1}
	minSz, maxSz := avgSz/4, avgSz*4
	plain, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(level(lvl)), zstd.WithEncoderConcurrency(1))

	// pass 1: chunk, dedup, remember content
	type uchunk struct {
		data  []byte
		order int
	}
	uniq := map[[32]byte]*uchunk{}
	recipes := make([][][32]byte, len(c.blobs))
	var nrefs int
	for i, b := range c.blobs {
		parts := fastCDC(b.raw, minSz, avgSz, maxSz)
		rec := make([][32]byte, 0, len(parts))
		for _, p := range parts {
			h := sha256.Sum256(p)
			rec = append(rec, h)
			nrefs++
			if _, ok := uniq[h]; !ok {
				uniq[h] = &uchunk{data: p, order: len(uniq)}
			}
		}
		recipes[i] = rec
	}

	// dictionary: built from chunk content in insertion order, so it captures
	// what a *new* session's chunks will look like.
	var dict []byte
	var hist []byte
	var samples [][]byte
	if dictSize > 0 {
		ordered := make([][]byte, len(uniq))
		for _, u := range uniq {
			ordered[u.order] = u.data
		}
		step := 1
		if len(ordered) > 512 {
			step = len(ordered) / 512
		}
		for i := 0; i < len(ordered) && len(hist) < dictSize; i += step {
			hist = append(hist, ordered[i]...)
		}
		if len(hist) > dictSize {
			hist = hist[len(hist)-dictSize:]
		}
		for i := 0; i < len(ordered) && len(samples) < 256; i += step {
			s := ordered[i]
			if len(s) > 8<<10 {
				s = s[:8<<10]
			}
			samples = append(samples, s)
		}
		var err error
		dict, err = zstd.BuildDict(zstd.BuildDictOptions{ID: 1, Contents: samples, History: hist, Level: level(lvl)})
		if err != nil {
			return result{name: name, note: "dict: " + err.Error(), encNs: 1, decNs: 1, stored: 1}
		}
	}

	var enc *zstd.Encoder
	var dec *zstd.Decoder
	if dict != nil {
		enc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(level(lvl)), zstd.WithEncoderConcurrency(1), zstd.WithEncoderDict(dict))
		dec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderDicts(dict))
	} else {
		enc = plain
		dec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	}

	// pass 2: encode, timed the way the write path would see it — chunk + hash +
	// dedup lookup, compressing only chunks that are new.
	type loc struct{ off, size int }
	index := map[[32]byte]loc{}
	var packs []byte // all pack files concatenated; offsets address into it
	var recipeBytes int64
	seen := map[[32]byte]bool{}
	for i, b := range c.blobs {
		t0 := time.Now()
		parts := fastCDC(b.raw, minSz, avgSz, maxSz)
		for _, p := range parts {
			h := sha256.Sum256(p)
			if seen[h] {
				continue
			}
			seen[h] = true
			cb := enc.EncodeAll(p, nil)
			index[h] = loc{off: len(packs), size: len(cb)}
			packs = append(packs, cb...)
		}
		buf := make([]byte, 0, len(parts)*8)
		for _, h := range recipes[i] {
			buf = binary.LittleEndian.AppendUint64(buf, binary.LittleEndian.Uint64(h[:8]))
		}
		rc := plain.EncodeAll(buf, nil)
		recipeBytes += int64(len(rc))
		d := time.Since(t0).Nanoseconds()
		r.encNs += d
		if d > r.encMaxNs {
			r.encMaxNs = d
		}
	}

	// chunk index on disk: hash(32) + pack(4) + off(4) + len(4)
	indexBytes := int64(len(index) * 44)
	r.stored = int64(len(packs)) + recipeBytes + indexBytes + int64(len(dict))
	r.note = fmt.Sprintf("%d refs, %d unique (%.1f%% dup); packs %.2fM recipes %.2fM index %.2fM dict %.2fM",
		nrefs, len(index), 100*(1-float64(len(index))/float64(nrefs)),
		mib(int64(len(packs))), mib(recipeBytes), mib(indexBytes), mib(int64(len(dict))))

	// read: one pread + one frame decode per chunk, shared decoder, no cache.
	var ampBytes int64
	for i, b := range c.blobs {
		t0 := time.Now()
		out := make([]byte, 0, len(b.raw))
		for _, h := range recipes[i] {
			l := index[h]
			p, err := dec.DecodeAll(packs[l.off:l.off+l.size], nil)
			if err != nil {
				r.ok = false
			}
			out = append(out, p...)
		}
		d := time.Since(t0).Nanoseconds()
		r.decNs += d
		if d > r.decMaxNs {
			r.decMaxNs = d
		}
		if !bytes.Equal(out, b.raw) {
			r.ok = false
		}
		ampBytes += int64(len(out))
	}
	r.readAmp = float64(ampBytes) / float64(c.totalRaw)
	return r
}
