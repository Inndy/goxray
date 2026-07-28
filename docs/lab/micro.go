package main

import (
	"bytes"
	"fmt"
	"sort"
	"time"

	"github.com/klauspost/compress/zstd"
)

// cmdMicro isolates the costs that land on the fail-closed write path: building
// a dictionary-primed encoder (which cannot be pooled, since the dictionary is
// baked in at construction) versus the compression itself.
func cmdMicro(root string) {
	c := loadCorpus(root, "req")

	// pick a representative large turn and its predecessor
	// Maximise min(child, parent): the largest turn whose predecessor is also
	// large. Maximising the child alone lands on a context reset, where the
	// parent is tiny and the delta measures nothing.
	var child, parent []byte
	best := 0
	for _, s := range c.sessions {
		idxs := c.bySess[s]
		for j := 1; j < len(idxs); j++ {
			cc, pp := c.blobs[idxs[j]].raw, c.blobs[idxs[j-1]].raw
			m := len(cc)
			if len(pp) < m {
				m = len(pp)
			}
			if m > best {
				best, child, parent = m, cc, pp
			}
		}
	}
	fmt.Printf("worst-case turn: child %.2f MiB, parent %.2f MiB, window %d MiB\n\n",
		mib(int64(len(child))), mib(int64(len(parent))), pow2ceil(len(parent)+len(child))>>20)

	timeIt := func(n int, f func()) (med, p95 time.Duration) {
		ds := make([]time.Duration, n)
		for i := range ds {
			t0 := time.Now()
			f()
			ds[i] = time.Since(t0)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[n/2], ds[int(float64(n)*0.95)]
	}

	row := func(label string, med, p95 time.Duration, extra string) {
		fmt.Printf("  %-46s med %7.2f ms   p95 %7.2f ms  %s\n",
			label, float64(med.Nanoseconds())/1e6, float64(p95.Nanoseconds())/1e6, extra)
	}

	fmt.Println("WRITE PATH (per request, single-threaded):")

	shared, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	var plainSz int
	med, p95 := timeIt(30, func() { plainSz = len(shared.EncodeAll(child, nil)) })
	row("plain EncodeAll, shared encoder (today)", med, p95, fmt.Sprintf("-> %d KB", plainSz/1024))

	win := pow2ceil(len(parent) + len(child))
	med, p95 = timeIt(30, func() {
		e, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(win), zstd.WithEncoderDictRaw(1, parent))
		e.Close()
	})
	row("NewWriter with raw dict (construction only)", med, p95, "")

	var deltaSz int
	med, p95 = timeIt(30, func() {
		e, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault),
			zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(win), zstd.WithEncoderDictRaw(1, parent))
		deltaSz = len(e.EncodeAll(child, nil))
		e.Close()
	})
	row("construction + delta EncodeAll", med, p95, fmt.Sprintf("-> %d KB (%.1fx smaller)", deltaSz/1024, float64(plainSz)/float64(deltaSz)))

	// Concurrency 0 lets the encoder skip some per-instance buffers.
	med, p95 = timeIt(30, func() {
		e, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest),
			zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(win), zstd.WithEncoderDictRaw(1, parent))
		deltaSz = len(e.EncodeAll(child, nil))
		e.Close()
	})
	row("same at SpeedFastest", med, p95, fmt.Sprintf("-> %d KB", deltaSz/1024))

	fmt.Println("\nREAD PATH (resolve one turn):")
	plain := shared.EncodeAll(child, nil)
	pcomp := shared.EncodeAll(parent, nil)
	e, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(win), zstd.WithEncoderDictRaw(1, parent))
	delta := e.EncodeAll(child, nil)
	e.Close()

	sharedDec, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(1<<31))
	med, p95 = timeIt(30, func() { sharedDec.DecodeAll(plain, nil) })
	row("plain DecodeAll, shared decoder (today)", med, p95, "")

	med, p95 = timeIt(30, func() {
		p, _ := sharedDec.DecodeAll(pcomp, nil)
		d, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(1<<31), zstd.WithDecoderDictRaw(1, p))
		out, err := d.DecodeAll(delta, nil)
		d.Close()
		if err != nil || !bytes.Equal(out, child) {
			panic("roundtrip")
		}
	})
	row("resolve depth-2 (decode parent + delta)", med, p95, "")

	pRaw, _ := sharedDec.DecodeAll(pcomp, nil)
	med, p95 = timeIt(30, func() {
		d, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(1<<31), zstd.WithDecoderDictRaw(1, pRaw))
		d.DecodeAll(delta, nil)
		d.Close()
	})
	row("resolve with parent already cached", med, p95, "")

	// Wrong-dictionary safety: does the frame checksum catch a parent that is
	// not the one the delta was written against?
	bad := append([]byte{}, pRaw...)
	if len(bad) > 1000 {
		bad[len(bad)/2] ^= 0xff
	}
	d, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(1<<31), zstd.WithDecoderDictRaw(1, bad))
	out, err := d.DecodeAll(delta, nil)
	d.Close()
	fmt.Printf("\nwrong-parent decode: err=%v, bytes_equal=%v  (detected=%v)\n",
		err, bytes.Equal(out, child), err != nil || !bytes.Equal(out, child))

	// Same, but with a parent of a different length (dict id matches, content
	// does not) — the case a stale or swapped file would produce.
	d2, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(1<<31), zstd.WithDecoderDictRaw(1, pRaw[:len(pRaw)/2]))
	out2, err2 := d2.DecodeAll(delta, nil)
	d2.Close()
	fmt.Printf("truncated-parent decode: err=%v, bytes_equal=%v\n", err2, bytes.Equal(out2, child))
}
