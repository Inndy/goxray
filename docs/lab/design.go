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

// cmdDesign implements the recommended layout for real, against real files, and
// verifies every request round-trips byte-identically. Everything before this
// measured components in memory; this measures the thing itself, including
// fsync on the write path and pread on the read path.
//
// Layout under <root>/chunks:
//
//	packs/NNNN.pack   append-only; [hash32|rawLen u32|compLen u32|zstd frame]*
//	dict              shared static zstd dictionary (optional)
//	index             derived: hash -> (pack, offset, compLen); rebuildable by scanning packs
//	recipes/<shard>/<ref>.rcp   zstd(delta-varint chunk ids) + trailing plain refs
type designCfg struct {
	avgSz    int
	dictSize int
	lvl      int
	packMax  int
	fsync    bool
}

type chunkLoc struct {
	pack  uint32
	off   uint32
	clen  uint32
	rawSz uint32
	id    uint64
}

func cmdDesign(root string, args []string) {
	cfg := designCfg{avgSz: 4 << 10, dictSize: 512 << 10, lvl: 2, packMax: 16 << 20, fsync: true}
	for _, a := range args {
		switch a {
		case "nodict":
			cfg.dictSize = 0
		case "nofsync":
			cfg.fsync = false
		case "2k":
			cfg.avgSz = 2 << 10
		case "4k":
			cfg.avgSz = 4 << 10
		case "8k":
			cfg.avgSz = 8 << 10
		case "16k":
			cfg.avgSz = 16 << 10
		case "dict1m":
			cfg.dictSize = 1 << 20
		case "dict256k":
			cfg.dictSize = 256 << 10
		}
	}
	out := filepath.Join(workDir(), "designstore")
	os.RemoveAll(out)
	os.MkdirAll(filepath.Join(out, "packs"), 0o700)

	c := loadCorpus(root, "req")
	fmt.Printf("corpus: %d requests, %.1f MiB raw\n", len(c.blobs), mib(c.totalRaw))
	fmt.Printf("config: %dK chunks, dict %dKB, level %d, pack max %dMB, fsync=%v\n\n",
		cfg.avgSz>>10, cfg.dictSize>>10, cfg.lvl, cfg.packMax>>20, cfg.fsync)

	minSz, maxSz := cfg.avgSz/4, cfg.avgSz*4
	plain, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(level(cfg.lvl)), zstd.WithEncoderConcurrency(1))

	// --- build the shared dictionary from a warmup prefix of traffic. A real
	// deployment ships a dictionary built this way from an earlier store; using
	// the first 20% here keeps it honest (the other 80% is unseen).
	var dict []byte
	if cfg.dictSize > 0 {
		warm := len(c.blobs) / 5
		uniq := map[[32]byte]bool{}
		var ordered [][]byte
		for i := 0; i < warm; i++ {
			for _, p := range fastCDC(c.blobs[i].raw, minSz, cfg.avgSz, maxSz) {
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
		for i := 0; i < len(ordered) && len(hist) < cfg.dictSize; i += step {
			hist = append(hist, ordered[i]...)
		}
		if len(hist) > cfg.dictSize {
			hist = hist[len(hist)-cfg.dictSize:]
		}
		for i := 0; i < len(ordered) && len(samples) < 256; i += step {
			s := ordered[i]
			if len(s) > 8<<10 {
				s = s[:8<<10]
			}
			samples = append(samples, s)
		}
		d, err := zstd.BuildDict(zstd.BuildDictOptions{ID: 1, Contents: samples, History: hist, Level: level(cfg.lvl)})
		if err != nil {
			fmt.Printf("dictionary build failed (%v) — continuing without one\n", err)
		} else {
			dict = d
			os.WriteFile(filepath.Join(out, "dict"), dict, 0o600)
			fmt.Printf("dictionary: %d KB built from the first %d requests (%.0f%% of traffic)\n\n",
				len(dict)>>10, warm, 100*float64(warm)/float64(len(c.blobs)))
		}
	}

	var enc *zstd.Encoder
	var dec *zstd.Decoder
	var err error
	if dict != nil {
		enc, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(level(cfg.lvl)), zstd.WithEncoderConcurrency(1), zstd.WithEncoderDict(dict))
		if err != nil {
			fmt.Println("encoder with dict:", err)
			return
		}
		dec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderDicts(dict))
	} else {
		enc = plain
		dec, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	}

	// --- WRITE
	index := map[[32]byte]chunkLoc{}
	var packNo uint32
	packPath := func(n uint32) string { return filepath.Join(out, "packs", fmt.Sprintf("%04d.pack", n)) }
	pack, _ := os.OpenFile(packPath(0), os.O_CREATE|os.O_WRONLY, 0o600)
	var packOff uint32

	var writeNs, writeMax int64
	var nrefs, nnew int
	var chunkNs, hashNs, compNs, ioNs int64

	for _, b := range c.blobs {
		t0 := time.Now()

		tc := time.Now()
		parts := fastCDC(b.raw, minSz, cfg.avgSz, maxSz)
		chunkNs += time.Since(tc).Nanoseconds()

		recipe := make([]byte, 0, len(parts)*3)
		var prevID int64
		var pending []byte // new chunk records to append this turn
		for _, p := range parts {
			th := time.Now()
			h := sha256.Sum256(p)
			hashNs += time.Since(th).Nanoseconds()
			nrefs++
			loc, ok := index[h]
			if !ok {
				tcp := time.Now()
				cb := enc.EncodeAll(p, nil)
				compNs += time.Since(tcp).Nanoseconds()
				rec := make([]byte, 0, 40+len(cb))
				rec = append(rec, h[:]...)
				rec = binary.LittleEndian.AppendUint32(rec, uint32(len(p)))
				rec = binary.LittleEndian.AppendUint32(rec, uint32(len(cb)))
				rec = append(rec, cb...)
				loc = chunkLoc{pack: packNo, off: packOff + uint32(len(pending)) + 40, clen: uint32(len(cb)), rawSz: uint32(len(p)), id: uint64(len(index))}
				index[h] = loc
				pending = append(pending, rec...)
				nnew++
			}
			d := int64(loc.id) - prevID
			prevID = int64(loc.id)
			recipe = binary.AppendUvarint(recipe, uint64(d<<1)^uint64(d>>63))
		}

		tio := time.Now()
		if len(pending) > 0 {
			if _, err := pack.Write(pending); err != nil {
				fmt.Println("pack write:", err)
				return
			}
			if cfg.fsync {
				pack.Sync()
			}
			packOff += uint32(len(pending))
		}
		// recipe: atomic temp+rename, exactly as blobs are written today
		rp := shardPath(filepath.Join(out, "recipes"), b.id, ".rcp")
		os.MkdirAll(filepath.Dir(rp), 0o700)
		if err := writeAtomic(rp, plain.EncodeAll(recipe, nil), cfg.fsync); err != nil {
			fmt.Println("recipe write:", err)
			return
		}
		ioNs += time.Since(tio).Nanoseconds()

		if packOff >= uint32(cfg.packMax) {
			pack.Close()
			packNo++
			pack, _ = os.OpenFile(packPath(packNo), os.O_CREATE|os.O_WRONLY, 0o600)
			packOff = 0
		}

		d := time.Since(t0).Nanoseconds()
		writeNs += d
		if d > writeMax {
			writeMax = d
		}
	}
	pack.Close()

	// --- persist the index (derived, but it has to live somewhere)
	idxBuf := make([]byte, 0, len(index)*48)
	for h, l := range index {
		idxBuf = append(idxBuf, h[:16]...)
		idxBuf = binary.LittleEndian.AppendUint32(idxBuf, l.pack)
		idxBuf = binary.LittleEndian.AppendUint32(idxBuf, l.off)
		idxBuf = binary.LittleEndian.AppendUint32(idxBuf, l.clen)
		idxBuf = binary.LittleEndian.AppendUint32(idxBuf, l.rawSz)
		idxBuf = binary.LittleEndian.AppendUint64(idxBuf, l.id)
	}
	os.WriteFile(filepath.Join(out, "index"), plain.EncodeAll(idxBuf, nil), 0o600)

	fmt.Printf("WRITE: %d refs, %d new chunks (%.1f%% dup)\n", nrefs, nnew, 100*(1-float64(nnew)/float64(nrefs)))
	fmt.Printf("  total %.2fs = chunk %.2fs + hash %.2fs + compress %.2fs + io/fsync %.2fs\n",
		float64(writeNs)/1e9, float64(chunkNs)/1e9, float64(hashNs)/1e9, float64(compNs)/1e9, float64(ioNs)/1e9)
	fmt.Printf("  throughput %.0f MB/s, worst single request %.1f ms\n\n",
		float64(c.totalRaw)/1e6/(float64(writeNs)/1e9), float64(writeMax)/1e6)

	// --- disk accounting
	sizes := map[string]int64{}
	filepath.Walk(out, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(out, p)
		top := rel
		if i := len(filepath.Dir(rel)); i > 0 && filepath.Dir(rel) != "." {
			top = filepath.Dir(rel)
			if j := filepath.Dir(top); j != "." && j != top {
				top = j
			}
		}
		sizes[top] += fi.Size()
		return nil
	})
	var total int64
	var keys []string
	for k, v := range sizes {
		total += v
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Println("ON DISK:")
	for _, k := range keys {
		fmt.Printf("  %-12s %8.2f MiB\n", k, mib(sizes[k]))
	}
	fmt.Printf("  %-12s %8.2f MiB   (today: 129.5 MiB -> %.1fx smaller)\n\n", "TOTAL", mib(total), 129.5/mib(total))

	// --- READ: real pread against the pack files, shared decoder, no cache
	packs := map[uint32]*os.File{}
	for n := uint32(0); n <= packNo; n++ {
		f, err := os.Open(packPath(n))
		if err == nil {
			packs[n] = f
		}
	}
	byID := make([]chunkLoc, len(index))
	for _, l := range index {
		byID[l.id] = l
	}

	var readNs, readMax int64
	var bad int
	var syscalls int
	for _, b := range c.blobs {
		t0 := time.Now()
		rp := shardPath(filepath.Join(out, "recipes"), b.id, ".rcp")
		rc, err := os.ReadFile(rp)
		if err != nil {
			bad++
			continue
		}
		recipe, err := dec.DecodeAll(rc, nil)
		if err != nil {
			bad++
			continue
		}
		got := make([]byte, 0, len(b.raw))
		var id int64
		buf := make([]byte, 0, maxSz*2)
		for len(recipe) > 0 {
			u, n := binary.Uvarint(recipe)
			if n <= 0 {
				break
			}
			recipe = recipe[n:]
			id += int64(u>>1) ^ -int64(u&1)
			l := byID[id]
			if cap(buf) < int(l.clen) {
				buf = make([]byte, l.clen)
			}
			buf = buf[:l.clen]
			if _, err := packs[l.pack].ReadAt(buf, int64(l.off)); err != nil {
				bad++
				break
			}
			syscalls++
			p, err := dec.DecodeAll(buf, nil)
			if err != nil {
				bad++
				break
			}
			got = append(got, p...)
		}
		d := time.Since(t0).Nanoseconds()
		readNs += d
		if d > readMax {
			readMax = d
		}
		if !bytes.Equal(got, b.raw) {
			bad++
		}
	}
	fmt.Printf("READ (real pread, shared decoder, no cache): %.0f MB/s, worst request %.1f ms, %.0f preads/request\n",
		float64(c.totalRaw)/1e6/(float64(readNs)/1e9), float64(readMax)/1e6, float64(syscalls)/float64(len(c.blobs)))
	if bad == 0 {
		fmt.Printf("VERIFY: all %d requests round-tripped byte-identically\n", len(c.blobs))
	} else {
		fmt.Printf("VERIFY: %d FAILURES out of %d\n", bad, len(c.blobs))
	}
}

func writeAtomic(path string, data []byte, doSync bool) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if doSync {
		if err := tmp.Sync(); err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if doSync {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	}
	return nil
}
