package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"time"

	"github.com/klauspost/compress/zstd"
)

// ---------- corpus ----------

type blob struct {
	sess string
	seq  int
	id   string
	raw  []byte
	kind string
}

type corpus struct {
	blobs    []blob
	sessions []string
	bySess   map[string][]int
	totalRaw int64
}

func loadCorpus(root string, kinds string) *corpus {
	ms := loadManifests(root)
	c := &corpus{bySess: map[string][]int{}}
	for _, m := range ms {
		add := func(ref, kind string) {
			if ref == "" {
				return
			}
			raw, _, err := readBlob(root, ref)
			if err != nil {
				return
			}
			sess := m.Req.SessionID
			if sess == "" {
				sess = "(none)"
			}
			i := len(c.blobs)
			if _, ok := c.bySess[sess]; !ok {
				c.sessions = append(c.sessions, sess)
			}
			c.blobs = append(c.blobs, blob{sess: sess, seq: len(c.bySess[sess]), id: ref, raw: raw, kind: kind})
			c.bySess[sess] = append(c.bySess[sess], i)
			c.totalRaw += int64(len(raw))
		}
		if kinds == "req" || kinds == "both" {
			add(m.ReqRef, "req")
		}
		if kinds == "res" || kinds == "both" {
			add(m.ResRef, "res")
		}
	}
	return c
}

// ---------- result ----------

type result struct {
	name     string
	stored   int64
	encNs    int64
	encMaxNs int64
	decNs    int64
	decMaxNs int64
	readAmp  float64
	depthMax int
	ok       bool
	note     string
}

func (r result) line(base int64, rawTotal int64) string {
	dec := "-"
	decw := "-"
	if r.decNs > 1 {
		dec = fmt.Sprintf("%.0f MB/s", float64(rawTotal)/1e6/(float64(r.decNs)/1e9))
		decw = fmt.Sprintf("%.1f ms", float64(r.decMaxNs)/1e6)
	}
	return fmt.Sprintf("%-32s %8.1fM %6.2fx %7.2fx %8.0f MB/s %8.1f ms %11s %9s %6.2f %5d  %s",
		r.name, mib(r.stored), float64(rawTotal)/float64(r.stored), float64(base)/float64(r.stored),
		float64(rawTotal)/1e6/(float64(r.encNs)/1e9), float64(r.encMaxNs)/1e6,
		dec, decw,
		r.readAmp, r.depthMax, map[bool]string{true: "ok", false: "MISMATCH"}[r.ok])
}

func header() string {
	return fmt.Sprintf("%-32s %9s %6s %7s %12s %11s %11s %9s %6s %5s  %s",
		"strategy", "stored", "ratio", "vs-now", "enc-thru", "enc-worst", "dec-thru", "dec-worst", "amp", "depth", "verify")
}

func level(n int) zstd.EncoderLevel {
	switch n {
	case 1:
		return zstd.SpeedFastest
	case 2:
		return zstd.SpeedDefault
	case 3:
		return zstd.SpeedBetterCompression
	default:
		return zstd.SpeedBestCompression
	}
}

func pow2ceil(n int) int {
	w := 1 << 10
	for w < n {
		w <<= 1
	}
	if w > 1<<29 {
		w = 1 << 29
	}
	return w
}

// ---------- 1. per-file zstd ----------

func benchPerFile(c *corpus, lvl int, name string) result {
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(level(lvl)), zstd.WithEncoderConcurrency(1))
	dec, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	r := result{name: name, ok: true, depthMax: 1}
	store := make([][]byte, len(c.blobs))
	for i, b := range c.blobs {
		t0 := time.Now()
		comp := enc.EncodeAll(b.raw, nil)
		d := time.Since(t0).Nanoseconds()
		r.encNs += d
		if d > r.encMaxNs {
			r.encMaxNs = d
		}
		store[i] = comp
		r.stored += int64(len(comp))
	}
	var ampBytes int64
	for i, b := range c.blobs {
		t0 := time.Now()
		out, err := dec.DecodeAll(store[i], nil)
		d := time.Since(t0).Nanoseconds()
		r.decNs += d
		if d > r.decMaxNs {
			r.decMaxNs = d
		}
		if err != nil || !bytes.Equal(out, b.raw) {
			r.ok = false
		}
		ampBytes += int64(len(out))
	}
	r.readAmp = float64(ampBytes) / float64(c.totalRaw)
	return r
}

// ---------- 2. trained shared dictionary ----------

func benchTrainedDict(c *corpus, dictSize, lvl int, name string) result {
	// klauspost's BuildDict has no cover trainer: History is the raw match
	// content and Contents only seeds the entropy tables. Build History from the
	// head of each session's first request (system + tools + opening messages —
	// the part every later request in that session repeats verbatim), round
	// robin so no single session monopolises the budget.
	var samples [][]byte
	rng := rand.New(rand.NewSource(7))
	for _, s := range c.sessions {
		for j, i := range c.bySess[s] {
			if j == 0 || rng.Intn(6) == 0 {
				b := c.blobs[i].raw
				// Small samples: BuildDict feeds Contents through the block
				// encoder and panics on entries much larger than History.
				if len(b) > 16<<10 {
					b = b[:16<<10]
				}
				samples = append(samples, b)
			}
		}
	}
	var firsts [][]byte
	for _, s := range c.sessions {
		if idxs := c.bySess[s]; len(idxs) > 0 {
			firsts = append(firsts, c.blobs[idxs[0]].raw)
		}
	}
	var hist []byte
	for round := 0; len(hist) < dictSize; round++ {
		grew := false
		for _, f := range firsts {
			lo, hi := round*16<<10, (round+1)*16<<10
			if lo >= len(f) {
				continue
			}
			if hi > len(f) {
				hi = len(f)
			}
			hist = append(hist, f[lo:hi]...)
			grew = true
			if len(hist) >= dictSize {
				break
			}
		}
		if !grew {
			break
		}
	}
	if len(hist) > dictSize {
		hist = hist[:dictSize]
	}
	dict, err := zstd.BuildDict(zstd.BuildDictOptions{ID: 1, Contents: samples, History: hist, Level: level(lvl)})
	if err != nil {
		return result{name: name, note: "train failed: " + err.Error(), ok: false, encNs: 1, decNs: 1, stored: 1}
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level(lvl)), zstd.WithEncoderConcurrency(1), zstd.WithEncoderDict(dict))
	if err != nil {
		return result{name: name, note: "enc: " + err.Error(), ok: false, encNs: 1, decNs: 1, stored: 1}
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderDicts(dict))
	if err != nil {
		return result{name: name, note: "dec: " + err.Error(), ok: false, encNs: 1, decNs: 1, stored: 1}
	}
	r := result{name: name, ok: true, depthMax: 1, note: fmt.Sprintf("dict %dKB", dictSize>>10)}
	store := make([][]byte, len(c.blobs))
	for i, b := range c.blobs {
		t0 := time.Now()
		comp := enc.EncodeAll(b.raw, nil)
		d := time.Since(t0).Nanoseconds()
		r.encNs += d
		if d > r.encMaxNs {
			r.encMaxNs = d
		}
		store[i] = comp
		r.stored += int64(len(comp))
	}
	r.stored += int64(len(dict))
	var ampBytes int64
	for i, b := range c.blobs {
		t0 := time.Now()
		out, err := dec.DecodeAll(store[i], nil)
		d := time.Since(t0).Nanoseconds()
		r.decNs += d
		if d > r.decMaxNs {
			r.decMaxNs = d
		}
		if err != nil || !bytes.Equal(out, b.raw) {
			r.ok = false
		}
		ampBytes += int64(len(out))
	}
	r.readAmp = float64(ampBytes) / float64(c.totalRaw)
	return r
}

// ---------- 3. delta ----------

type deltaMode int

const (
	modeChain deltaMode = iota
	modeAnchor
	// modeAdaptive keeps one anchor per session (so read depth is never more
	// than 2) and re-anchors only when the delta stops paying off, instead of
	// on a fixed turn count.
	modeAdaptive
)

func benchDelta(c *corpus, mode deltaMode, K, lvl int, name string, useLRU bool) result {
	r := result{name: name, ok: true}
	dec, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(1<<31))
	plainEnc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(level(lvl)), zstd.WithEncoderConcurrency(1))

	type entry struct {
		data   []byte
		parent int
		depth  int
	}
	store := make([]entry, len(c.blobs))

	encDelta := func(parentRaw, raw []byte) ([]byte, error) {
		e, err := zstd.NewWriter(nil,
			zstd.WithEncoderLevel(level(lvl)),
			zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(pow2ceil(len(parentRaw)+len(raw))),
			zstd.WithEncoderDictRaw(1, parentRaw))
		if err != nil {
			return nil, err
		}
		out := e.EncodeAll(raw, nil)
		e.Close()
		return out, nil
	}

	nAnchors := 0
	for _, s := range c.sessions {
		idxs := c.bySess[s]
		anchor := -1
		for j, i := range idxs {
			raw := c.blobs[i].raw
			parent := -1
			isCP := j%K == 0
			if mode == modeAdaptive {
				isCP = anchor < 0
				parent = anchor
			} else if !isCP {
				if mode == modeChain {
					parent = idxs[j-1]
				} else {
					parent = anchor
				}
			}
			t0 := time.Now()
			var comp []byte
			if isCP || parent < 0 {
				comp = plainEnc.EncodeAll(raw, nil)
				isCP = true
			} else {
				var err error
				comp, err = encDelta(c.blobs[parent].raw, raw)
				if err != nil {
					r.note, r.ok = "enc: "+err.Error(), false
					r.encNs, r.decNs, r.stored = 1, 1, 1
					return r
				}
				// Adaptive: if the delta is no longer much smaller than a
				// standalone frame, the anchor has gone stale — store this blob
				// as the new anchor instead. K is the percent threshold.
				if mode == modeAdaptive {
					plain := plainEnc.EncodeAll(raw, nil)
					if len(comp)*100 > len(plain)*K {
						comp, isCP, parent = plain, true, -1
					}
				}
			}
			d := time.Since(t0).Nanoseconds()
			r.encNs += d
			if d > r.encMaxNs {
				r.encMaxNs = d
			}
			depth := 0
			if parent >= 0 {
				depth = store[parent].depth + 1
			}
			store[i] = entry{data: comp, parent: parent, depth: depth}
			r.stored += int64(len(comp)) + 24
			if depth > r.depthMax {
				r.depthMax = depth
			}
			if isCP {
				anchor = i
				nAnchors++
			}
		}
	}
	if mode == modeAdaptive {
		r.note = fmt.Sprintf("%d anchors / %d blobs", nAnchors, len(c.blobs))
	}

	lru := map[int][]byte{}
	var lruOrder []int
	const lruCap = 4
	var ampBytes int64
	var resolve func(i int) ([]byte, error)
	resolve = func(i int) ([]byte, error) {
		if useLRU {
			if v, ok := lru[i]; ok {
				return v, nil
			}
		}
		e := store[i]
		var out []byte
		var err error
		if e.parent < 0 {
			out, err = dec.DecodeAll(e.data, nil)
		} else {
			var pd []byte
			pd, err = resolve(e.parent)
			if err != nil {
				return nil, err
			}
			d2, e2 := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1),
				zstd.WithDecoderMaxMemory(1<<31), zstd.WithDecoderDictRaw(1, pd))
			if e2 != nil {
				return nil, e2
			}
			out, err = d2.DecodeAll(e.data, nil)
			d2.Close()
		}
		if err != nil {
			return nil, err
		}
		ampBytes += int64(len(out))
		if useLRU {
			lru[i] = out
			lruOrder = append(lruOrder, i)
			if len(lruOrder) > lruCap {
				delete(lru, lruOrder[0])
				lruOrder = lruOrder[1:]
			}
		}
		return out, nil
	}
	for i, b := range c.blobs {
		t0 := time.Now()
		out, err := resolve(i)
		d := time.Since(t0).Nanoseconds()
		r.decNs += d
		if d > r.decMaxNs {
			r.decMaxNs = d
		}
		if err != nil || !bytes.Equal(out, b.raw) {
			r.ok = false
			if r.note == "" && err != nil {
				r.note = err.Error()
			}
		}
	}
	r.readAmp = float64(ampBytes) / float64(c.totalRaw)
	return r
}

// ---------- 4. CDC dedup ----------

var gearTable = func() [256]uint64 {
	var t [256]uint64
	rng := rand.New(rand.NewSource(0xC0FFEE))
	for i := range t {
		t[i] = rng.Uint64()
	}
	return t
}()

func fastCDC(data []byte, minSz, avgSz, maxSz int) [][]byte {
	bits := 0
	for (1 << bits) < avgSz {
		bits++
	}
	mask := uint64(1<<bits) - 1
	var out [][]byte
	for len(data) > 0 {
		if len(data) <= minSz {
			out = append(out, data)
			break
		}
		var h uint64
		cut := len(data)
		if cut > maxSz {
			cut = maxSz
		}
		i := minSz
		for ; i < cut; i++ {
			h = (h << 1) + gearTable[data[i]]
			if h&mask == 0 {
				i++
				break
			}
		}
		out = append(out, data[:i])
		data = data[i:]
	}
	return out
}

func benchCDC(c *corpus, avgSz, lvl int, name string, packed bool) result {
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(level(lvl)), zstd.WithEncoderConcurrency(1))
	dec, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	r := result{name: name, ok: true, depthMax: 1}
	minSz, maxSz := avgSz/4, avgSz*4

	type chunkRec struct {
		comp []byte
		size int
	}
	chunks := map[[32]byte]*chunkRec{}
	recipes := make([][][32]byte, len(c.blobs))
	var chunkBytes, recipeBytes int64
	var nchunks, ndup int

	for i, b := range c.blobs {
		t0 := time.Now()
		parts := fastCDC(b.raw, minSz, avgSz, maxSz)
		rec := make([][32]byte, 0, len(parts))
		for _, p := range parts {
			h := sha256.Sum256(p)
			rec = append(rec, h)
			nchunks++
			if _, ok := chunks[h]; ok {
				ndup++
				continue
			}
			cb := p
			if !packed {
				cb = enc.EncodeAll(p, nil)
				chunkBytes += int64(len(cb))
			}
			chunks[h] = &chunkRec{comp: cb, size: len(p)}
		}
		d := time.Since(t0).Nanoseconds()
		r.encNs += d
		if d > r.encMaxNs {
			r.encMaxNs = d
		}
		recipes[i] = rec
		recipeBytes += int64(len(rec) * 8)
	}
	if packed {
		var buf []byte
		flush := func() {
			if len(buf) > 0 {
				chunkBytes += int64(len(enc.EncodeAll(buf, nil)))
				buf = buf[:0]
			}
		}
		keys := make([][32]byte, 0, len(chunks))
		for k := range chunks {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, b int) bool { return bytes.Compare(keys[a][:], keys[b][:]) < 0 })
		for _, k := range keys {
			buf = append(buf, chunks[k].comp...)
			if len(buf) >= 4<<20 {
				flush()
			}
		}
		flush()
	}
	r.stored = chunkBytes + recipeBytes + int64(len(chunks)*32)
	r.note = fmt.Sprintf("%d chunk refs, %.0f%% dup, %d unique, %.1fKB avg", nchunks, 100*float64(ndup)/float64(nchunks), len(chunks), float64(c.totalRaw)/float64(nchunks)/1024)

	if packed {
		r.note += " (read path not modelled)"
		r.decNs = 1
		return r
	}
	var ampBytes int64
	for i, b := range c.blobs {
		t0 := time.Now()
		var out []byte
		for _, h := range recipes[i] {
			p, err := dec.DecodeAll(chunks[h].comp, nil)
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

// ---------- driver ----------

func cmdBench(root string, args []string) {
	kinds := "req"
	if len(args) > 0 {
		kinds = args[0]
	}
	which := "all"
	if len(args) > 1 {
		which = args[1]
	}
	fmt.Fprintf(os.Stderr, "loading corpus (%s)...\n", kinds)
	c := loadCorpus(root, kinds)
	fmt.Printf("corpus: %d blobs, %.1f MiB raw, %d sessions\n\n", len(c.blobs), mib(c.totalRaw), len(c.sessions))

	var results []result
	run := func(tag string, f func() result) {
		if which != "all" && which != tag {
			return
		}
		fmt.Fprintf(os.Stderr, "  running %s...\n", tag)
		t0 := time.Now()
		res := result{name: tag, encNs: 1, decNs: 1, stored: 1}
		func() {
			defer func() {
				if v := recover(); v != nil {
					res.note = fmt.Sprintf("PANIC: %v", v)
				}
			}()
			res = f()
		}()
		fmt.Fprintf(os.Stderr, "    done in %s\n", time.Since(t0).Round(time.Millisecond))
		results = append(results, res)
	}

	run("now", func() result { return benchPerFile(c, 2, "per-file zstd default (today)") })
	run("fast", func() result { return benchPerFile(c, 1, "per-file zstd fastest") })
	run("better", func() result { return benchPerFile(c, 3, "per-file zstd better") })
	run("best", func() result { return benchPerFile(c, 4, "per-file zstd best") })
	run("dict64", func() result { return benchTrainedDict(c, 64<<10, 2, "trained dict 64K, per-file") })
	run("dict1m", func() result { return benchTrainedDict(c, 1<<20, 2, "trained dict 1M, per-file") })
	run("chain8", func() result { return benchDelta(c, modeChain, 8, 2, "delta-chain K=8 default", false) })
	run("chain16", func() result { return benchDelta(c, modeChain, 16, 2, "delta-chain K=16 default", false) })
	run("chain16b", func() result { return benchDelta(c, modeChain, 16, 4, "delta-chain K=16 best", false) })
	run("chain32b", func() result { return benchDelta(c, modeChain, 32, 4, "delta-chain K=32 best", false) })
	run("chaininf", func() result { return benchDelta(c, modeChain, 1<<30, 4, "delta-chain K=inf best", false) })
	run("anchor8", func() result { return benchDelta(c, modeAnchor, 8, 2, "delta-anchor K=8 default", false) })
	run("anchor16", func() result { return benchDelta(c, modeAnchor, 16, 2, "delta-anchor K=16 default", false) })
	run("anchor16b", func() result { return benchDelta(c, modeAnchor, 16, 4, "delta-anchor K=16 best", false) })
	run("anchor32b", func() result { return benchDelta(c, modeAnchor, 32, 4, "delta-anchor K=32 best", false) })
	run("adapt50", func() result { return benchDelta(c, modeAdaptive, 50, 2, "delta-adaptive 50% default", false) })
	run("adapt50b", func() result { return benchDelta(c, modeAdaptive, 50, 4, "delta-adaptive 50% best", false) })
	run("adapt25b", func() result { return benchDelta(c, modeAdaptive, 25, 4, "delta-adaptive 25% best", false) })
	run("cdc8", func() result { return benchCDC(c, 8<<10, 2, "CDC 8K + per-chunk zstd", false) })
	run("cdc16", func() result { return benchCDC(c, 16<<10, 2, "CDC 16K + per-chunk zstd", false) })
	run("cdc64", func() result { return benchCDC(c, 64<<10, 2, "CDC 64K + per-chunk zstd", false) })
	run("cdc16p", func() result { return benchCDC(c, 16<<10, 4, "CDC 16K packed 4M best", true) })
	run("cdc4", func() result { return benchCDC(c, 4<<10, 2, "CDC 4K + per-chunk zstd", false) })
	run("cdc2", func() result { return benchCDC(c, 2<<10, 2, "CDC 2K + per-chunk zstd", false) })
	run("pk8n", func() result { return benchCDCPacked(c, 8<<10, 0, 2, "CDC 8K packed, no dict") })
	run("pk4n", func() result { return benchCDCPacked(c, 4<<10, 0, 2, "CDC 4K packed, no dict") })
	run("pk2n", func() result { return benchCDCPacked(c, 2<<10, 0, 2, "CDC 2K packed, no dict") })
	run("pk4nb", func() result { return benchCDCPacked(c, 4<<10, 0, 4, "CDC 4K packed, no dict, best") })
	run("pk16n", func() result { return benchCDCPacked(c, 16<<10, 0, 2, "CDC 16K packed, no dict") })
	run("pk8", func() result { return benchCDCPacked(c, 8<<10, 512<<10, 2, "CDC 8K packed + 512K dict") })
	run("pk4", func() result { return benchCDCPacked(c, 4<<10, 512<<10, 2, "CDC 4K packed + 512K dict") })
	run("pk4m", func() result { return benchCDCPacked(c, 4<<10, 1<<20, 2, "CDC 4K packed + 1M dict") })
	run("pk2m", func() result { return benchCDCPacked(c, 2<<10, 1<<20, 2, "CDC 2K packed + 1M dict") })
	run("pk4mb", func() result { return benchCDCPacked(c, 4<<10, 1<<20, 4, "CDC 4K packed + 1M dict, best") })
	run("bun4", func() result { return benchCDCBundled(c, 4<<10, 1<<20, 2, "CDC 4K bundled 1M, no cache", 0) })
	run("bun8", func() result { return benchCDCBundled(c, 8<<10, 1<<20, 2, "CDC 8K bundled 1M, no cache", 0) })
	run("bun8c", func() result { return benchCDCBundled(c, 8<<10, 1<<20, 2, "CDC 8K bundled 1M, 8-bundle LRU", 8) })
	run("bun8b", func() result { return benchCDCBundled(c, 8<<10, 1<<20, 4, "CDC 8K bundled 1M best", 8) })
	run("bun8big", func() result { return benchCDCBundled(c, 8<<10, 8<<20, 4, "CDC 8K bundled 8M best", 8) })
	run("bun16", func() result { return benchCDCBundled(c, 16<<10, 1<<20, 2, "CDC 16K bundled 1M, 8-LRU", 8) })

	var base int64
	for _, r := range results {
		if r.name == "per-file zstd default (today)" {
			base = r.stored
		}
	}
	if base == 0 {
		base = c.totalRaw
	}
	fmt.Println(header())
	for _, r := range results {
		fmt.Println(r.line(base, c.totalRaw))
		if r.note != "" {
			fmt.Printf("%34s %s\n", "", r.note)
		}
	}
}
