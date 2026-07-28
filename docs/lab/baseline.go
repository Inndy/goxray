package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/zstd"
)

// cmdBaseline reproduces today's write path exactly — zstd EncodeAll then
// writeFileSync (temp file, fsync, rename, fsync dir) — against real files, so
// the proposed design's write cost can be compared against the real thing
// rather than against a pure-CPU number.
func cmdBaseline(root string, args []string) {
	doSync := true
	if len(args) > 0 && args[0] == "nofsync" {
		doSync = false
	}
	out := filepath.Join(workDir(), "baselinestore")
	os.RemoveAll(out)
	os.MkdirAll(out, 0o700)

	c := loadCorpus(root, "req")
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	dec, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))

	var writeNs, writeMax, compNs, ioNs int64
	for _, b := range c.blobs {
		t0 := time.Now()
		tc := time.Now()
		comp := enc.EncodeAll(b.raw, nil)
		compNs += time.Since(tc).Nanoseconds()

		tio := time.Now()
		p := shardPath(out, b.id, ".zst")
		os.MkdirAll(filepath.Dir(p), 0o700)
		if err := writeAtomic(p, comp, doSync); err != nil {
			fmt.Println("write:", err)
			return
		}
		ioNs += time.Since(tio).Nanoseconds()

		d := time.Since(t0).Nanoseconds()
		writeNs += d
		if d > writeMax {
			writeMax = d
		}
	}

	var total int64
	filepath.Walk(out, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})

	fmt.Printf("TODAY'S PATH (fsync=%v): %.2f MiB on disk\n", doSync, mib(total))
	fmt.Printf("  total %.2fs = compress %.2fs + io/fsync %.2fs\n",
		float64(writeNs)/1e9, float64(compNs)/1e9, float64(ioNs)/1e9)
	fmt.Printf("  throughput %.0f MB/s, worst single request %.1f ms\n",
		float64(c.totalRaw)/1e6/(float64(writeNs)/1e9), float64(writeMax)/1e6)

	var readNs, readMax int64
	for _, b := range c.blobs {
		t0 := time.Now()
		raw, err := os.ReadFile(shardPath(out, b.id, ".zst"))
		if err == nil {
			dec.DecodeAll(raw, nil)
		}
		d := time.Since(t0).Nanoseconds()
		readNs += d
		if d > readMax {
			readMax = d
		}
	}
	fmt.Printf("READ (real file read): %.0f MB/s, worst request %.1f ms\n",
		float64(c.totalRaw)/1e6/(float64(readNs)/1e9), float64(readMax)/1e6)
}
