package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/klauspost/compress/zstd"
)

// cmdArchive models compaction as a post-session archival step rather than a
// write-time format: live turns keep today's per-file zstd, and once a session
// goes quiet its requests are rewritten into one self-contained archive.
//
// Self-contained is the whole point. Every dependency an archive has is inside
// it, so the dependency graph of the store is exactly "turn -> its session
// archive" — no global chunk pool, no shared dictionary, nothing that couples
// one session's bytes to another's. A session is also the unit a person exports
// or deletes, so it is the right blast radius.
//
// Layout:
//
//	sessions/<sid>/<gen>.gxa   header | chunk table | solid chunk pool | recipes
//	archive-index              derived: ref -> (sid, gen, recipe slot)
//
// The chunk pool is compressed solid, which is what replaces the trained
// dictionary: chunks compress against each other instead of against a global
// corpus, and a session's whole pool is small enough to materialise on first
// touch (measured below).
type archiveCfg struct {
	avgSz   int
	lvl     int
	poolMax int  // 0 = one solid frame for the whole pool
	twoPool bool // separate solid pools for request and response bodies
}

func cmdArchive(root string, args []string) {
	cfg := archiveCfg{avgSz: 4 << 10, lvl: 4, poolMax: 0}
	for _, a := range args {
		switch a {
		case "2k":
			cfg.avgSz = 2 << 10
		case "4k":
			cfg.avgSz = 4 << 10
		case "8k":
			cfg.avgSz = 8 << 10
		case "16k":
			cfg.avgSz = 16 << 10
		case "fast":
			cfg.lvl = 2
		case "best":
			cfg.lvl = 4
		case "split256k":
			cfg.poolMax = 256 << 10
		case "split1m":
			cfg.poolMax = 1 << 20
		case "twopool":
			cfg.twoPool = true
		}
	}
	kind := "req"
	for _, a := range args {
		if a == "both" || a == "res" {
			kind = a
		}
	}
	out := filepath.Join(workDir(), "archivestore")
	os.RemoveAll(out)
	os.MkdirAll(out, 0o700)

	c := loadCorpus(root, kind)
	minSz, maxSz := cfg.avgSz/4, cfg.avgSz*4
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(level(cfg.lvl)), zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(1<<24))
	dec, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(1<<31))

	fmt.Printf("corpus: %d requests, %.1f MiB raw, %d sessions\n", len(c.blobs), mib(c.totalRaw), len(c.sessions))
	fmt.Printf("config: %s bodies, %dK chunks, level %d, pool %s\n\n", kind, cfg.avgSz>>10, cfg.lvl,
		map[bool]string{true: "one solid frame", false: fmt.Sprintf("%dK frames", cfg.poolMax>>10)}[cfg.poolMax == 0])

	type archive struct {
		sid        string
		path       string
		poolFrames [][]byte // compressed
		frameRaw   []int
		// chunk id -> (frame, offset, len) into the *uncompressed* pool
		locFrame []uint32
		locOff   []uint32
		locLen   []uint32
		recipes  map[string][]byte
		rawTotal int64
	}
	var archives []*archive
	byRef := map[string]*archive{}

	var buildNs int64
	var totalPool, totalRecipe, totalTable int64

	for _, sid := range c.sessions {
		idxs := c.bySess[sid]
		t0 := time.Now()

		ids := map[[32]byte]uint32{}
		var poolRaw []byte
		var frames [][]byte
		var frameRaw []int
		var locFrame, locOff, locLen []uint32
		var hashes [][16]byte
		flush := func() {
			if len(poolRaw) == 0 {
				return
			}
			frames = append(frames, enc.EncodeAll(poolRaw, nil))
			frameRaw = append(frameRaw, len(poolRaw))
			poolRaw = nil
		}

		a := &archive{sid: sid, recipes: map[string][]byte{}}
		// Chunks are pooled per body kind when twoPool is set, so opening a turn
		// materialises only the pool it needs rather than the session's whole
		// content. Chunk ids stay archive-global, so a chunk shared between a
		// request and a response still dedups to one copy.
		build := func(kind string) {
			for _, i := range idxs {
				b := c.blobs[i]
				if kind != "" && b.kind != kind {
					continue
				}
				a.rawTotal += int64(len(b.raw))
				parts := fastCDC(b.raw, minSz, cfg.avgSz, maxSz)
				recipe := make([]byte, 0, len(parts)*3)
				var prev int64
				for _, p := range parts {
					h := sha256.Sum256(p)
					id, ok := ids[h]
					if !ok {
						id = uint32(len(ids))
						ids[h] = id
						locFrame = append(locFrame, uint32(len(frames)))
						locOff = append(locOff, uint32(len(poolRaw)))
						locLen = append(locLen, uint32(len(p)))
						var h16 [16]byte
						copy(h16[:], h[:16])
						hashes = append(hashes, h16)
						poolRaw = append(poolRaw, p...)
						if cfg.poolMax > 0 && len(poolRaw) >= cfg.poolMax {
							flush()
						}
					}
					d := int64(id) - prev
					prev = int64(id)
					recipe = binary.AppendUvarint(recipe, uint64(d<<1)^uint64(d>>63))
				}
				a.recipes[b.id] = enc.EncodeAll(recipe, nil)
				byRef[b.id] = a
			}
			flush()
		}
		if cfg.twoPool {
			build("req")
			build("res")
		} else {
			build("")
		}
		a.poolFrames, a.frameRaw = frames, frameRaw
		a.locFrame, a.locOff, a.locLen = locFrame, locOff, locLen
		buildNs += time.Since(t0).Nanoseconds()

		// write the archive as one file
		dir := filepath.Join(out, "sessions", sid)
		os.MkdirAll(dir, 0o700)
		a.path = filepath.Join(dir, "0000.gxa")
		var buf []byte
		buf = append(buf, "GXA1"...)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(ids)))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(frames)))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(a.recipes)))
		// chunk table: hash16 | frame | off | len
		table := make([]byte, 0, len(ids)*28)
		for id := 0; id < len(locFrame); id++ {
			table = append(table, hashes[id][:]...)
			table = binary.LittleEndian.AppendUint32(table, locFrame[id])
			table = binary.LittleEndian.AppendUint32(table, locOff[id])
			table = binary.LittleEndian.AppendUint32(table, locLen[id])
		}
		ctab := enc.EncodeAll(table, nil)
		totalTable += int64(len(ctab))
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(ctab)))
		buf = append(buf, ctab...)
		for i, f := range frames {
			buf = binary.LittleEndian.AppendUint32(buf, uint32(len(f)))
			buf = binary.LittleEndian.AppendUint32(buf, uint32(frameRaw[i]))
			buf = append(buf, f...)
			totalPool += int64(len(f))
		}
		refs := make([]string, 0, len(a.recipes))
		for r := range a.recipes {
			refs = append(refs, r)
		}
		sort.Strings(refs)
		for _, r := range refs {
			buf = append(buf, r...)
			buf = binary.LittleEndian.AppendUint32(buf, uint32(len(a.recipes[r])))
			buf = append(buf, a.recipes[r]...)
			totalRecipe += int64(len(a.recipes[r]))
		}
		if err := writeAtomic(a.path, buf, true); err != nil {
			fmt.Println("archive write:", err)
			return
		}
		archives = append(archives, a)
	}

	// derived ref -> archive index
	var idx []byte
	for ref, a := range byRef {
		idx = append(idx, ref...)
		idx = append(idx, a.sid...)
	}
	cidx := enc.EncodeAll(idx, nil)
	os.WriteFile(filepath.Join(out, "archive-index"), cidx, 0o600)

	var onDisk int64
	filepath.Walk(out, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			onDisk += fi.Size()
		}
		return nil
	})

	fmt.Printf("BUILD: %.2fs (%.0f MB/s), %d archives\n", float64(buildNs)/1e9,
		float64(c.totalRaw)/1e6/(float64(buildNs)/1e9), len(archives))
	fmt.Printf("  pool %.2f MiB  chunk tables %.2f MiB  recipes %.2f MiB  ref index %.2f MiB\n",
		mib(totalPool), mib(totalTable), mib(totalRecipe), mib(int64(len(cidx))))
	fmt.Printf("  ON DISK %.2f MiB   (today 129.5 MiB -> %.1fx smaller)\n\n", mib(onDisk), 129.5/mib(onDisk))

	// ---- read paths
	// (a) cold: every turn read independently, archive pool materialised each time
	// (b) session-resident: pool decoded once per session, then every turn resolved
	resolve := func(a *archive, pool [][]byte, ref string) []byte {
		rec, err := dec.DecodeAll(a.recipes[ref], nil)
		if err != nil {
			return nil
		}
		out := make([]byte, 0, 1<<20)
		var id int64
		for len(rec) > 0 {
			u, n := binary.Uvarint(rec)
			if n <= 0 {
				break
			}
			rec = rec[n:]
			id += int64(u>>1) ^ -int64(u&1)
			f, off, l := a.locFrame[id], a.locOff[id], a.locLen[id]
			out = append(out, pool[f][off:off+l]...)
		}
		return out
	}

	var coldNs, coldMax, coldMaterialised int64
	var bad int
	for _, b := range c.blobs {
		a := byRef[b.id]
		t0 := time.Now()
		// Materialise only the frames this turn actually references — with
		// separate request/response pools that is usually one of them.
		pool := make([][]byte, len(a.poolFrames))
		need := map[uint32]bool{}
		rec, _ := dec.DecodeAll(a.recipes[b.id], nil)
		var id int64
		for len(rec) > 0 {
			u, n := binary.Uvarint(rec)
			if n <= 0 {
				break
			}
			rec = rec[n:]
			id += int64(u>>1) ^ -int64(u&1)
			need[a.locFrame[id]] = true
		}
		for i := range need {
			pool[i], _ = dec.DecodeAll(a.poolFrames[i], nil)
			coldMaterialised += int64(len(pool[i]))
		}
		got := resolve(a, pool, b.id)
		d := time.Since(t0).Nanoseconds()
		coldNs += d
		if d > coldMax {
			coldMax = d
		}
		if !bytes.Equal(got, b.raw) {
			bad++
		}
	}

	var warmNs, warmMax, poolNs int64
	for _, a := range archives {
		t0 := time.Now()
		pool := make([][]byte, len(a.poolFrames))
		for i, f := range a.poolFrames {
			pool[i], _ = dec.DecodeAll(f, nil)
		}
		poolNs += time.Since(t0).Nanoseconds()
		for _, i := range c.bySess[a.sid] {
			b := c.blobs[i]
			t1 := time.Now()
			got := resolve(a, pool, b.id)
			d := time.Since(t1).Nanoseconds()
			warmNs += d
			if d > warmMax {
				warmMax = d
			}
			if !bytes.Equal(got, b.raw) {
				bad++
			}
		}
	}

	fmt.Printf("READ cold (frames rebuilt per turn):  %5.0f MB/s, worst %6.2f ms, %.1f MiB materialised per read\n",
		float64(c.totalRaw)/1e6/(float64(coldNs)/1e9), float64(coldMax)/1e6,
		mib(coldMaterialised)/float64(len(c.blobs)))
	fmt.Printf("READ session-resident:                %5.0f MB/s, worst %6.2f ms\n",
		float64(c.totalRaw)/1e6/(float64(warmNs)/1e9), float64(warmMax)/1e6)
	fmt.Printf("  pool materialisation: %.2f ms per session avg, %.2f MiB resident for the largest\n",
		float64(poolNs)/1e6/float64(len(archives)), func() float64 {
			var m int64
			for _, a := range archives {
				var t int64
				for _, n := range a.frameRaw {
					t += int64(n)
				}
				if t > m {
					m = t
				}
			}
			return mib(m)
		}())

	fmt.Printf("\nper-session detail:\n%-10s %6s %10s %10s %9s %8s\n", "session", "turns", "raw", "archive", "ratio", "pool MiB")
	sort.Slice(archives, func(i, j int) bool { return archives[i].rawTotal > archives[j].rawTotal })
	for _, a := range archives {
		if a.rawTotal < 1<<20 {
			continue
		}
		fi, _ := os.Stat(a.path)
		var poolRaw int64
		for _, n := range a.frameRaw {
			poolRaw += int64(n)
		}
		fmt.Printf("%-10s %6d %9.1fM %9.2fM %8.1fx %8.2f\n", a.sid[:8], len(a.recipes),
			mib(a.rawTotal), mib(fi.Size()), float64(a.rawTotal)/float64(fi.Size()), mib(poolRaw))
	}

	if bad == 0 {
		fmt.Printf("\nVERIFY: all %d requests round-tripped byte-identically (both read paths)\n", len(c.blobs))
	} else {
		fmt.Printf("\nVERIFY: %d FAILURES\n", bad)
	}
}
