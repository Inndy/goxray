package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// A session archive holds every recorded body of one session in a single
// self-contained file: content-defined chunks, deduplicated within the session,
// with the unique chunks concatenated into a solid zstd pool.
//
// Self-contained is the design, not an implementation detail. Nothing in an
// archive references anything outside it — no shared dictionary, no global
// chunk store, no other archive. That keeps the store's dependency graph a
// forest of depth one (a turn depends on its session's archive and nothing
// else), which is what makes integrity checking, deletion, and blast radius all
// per-session, with no refcounts to get wrong.
//
// Solid pooling is also what replaces a trained dictionary: measured against a
// real store, chunks compressing against their own session beat chunks
// compressing against a dictionary trained on a global corpus, and cost nothing
// in coupling.
//
// On-disk layout (all integers little-endian):
//
//	header   56 bytes, see archiveHeaderLen
//	table    zstd( nchunks × {hash16, frame u32, off u32, len u32} )
//	refs     zstd( nrefs   × {refLen u16, ref, recipeLen u32, recipe} )
//	dir      nframes × {off u64, compLen u32, rawLen u32}     (uncompressed)
//	frames   the pool, one or more zstd frames — the bulk, read lazily
//
// A recipe is the chunk id sequence of one body, stored as zigzag varint
// deltas. Consecutive turns reference the same chunks in the same order, so the
// deltas are mostly small and repetitive; measured, this is 11.4× smaller than
// storing chunk hashes, which are random bytes and do not compress at all.
const (
	archiveMagic     = "GXA1"
	archiveVersion   = 1
	archiveHeaderLen = 56
	archiveExt       = ".gxa"

	chunkEntryLen = 16 + 4 + 4 + 4
	frameDirLen   = 8 + 4 + 4

	// poolFlushAt bounds builder memory. One solid frame compresses best and
	// every real session is far under this (the largest measured pool was
	// 10.7 MiB), so this only exists so a pathological session cannot exhaust
	// memory rather than as a tuning knob.
	poolFlushAt = 64 << 20

	// archiveWindow must be at least as large as a pool frame for the
	// compressor to match across the whole of it.
	archiveWindow = 1 << 24
)

// Format limits. Every count, offset and length in an archive header is
// untrusted input: fsck exists because a stored archive can be damaged, so a few
// corrupted bytes must produce a reported integrity error rather than a
// multi-gigabyte allocation in whatever process happened to read the file — the
// dashboard included. Each limit is also enforced by the builder, so it can only
// ever reject a file goxray did not write.
const (
	// archiveMaxFrameRaw is one decoded pool frame: the builder flushes at
	// poolFlushAt and can overshoot by at most one chunk.
	archiveMaxFrameRaw = poolFlushAt + chunkMax
	// archiveMaxPool is every frame of one archive decoded, i.e. the session's
	// unique content. The largest measured on a real store was 10.7 MiB.
	archiveMaxPool = 2 << 30
	// archiveMaxMeta bounds one decoded metadata section, and doubles as the
	// decoder's memory ceiling — it must stay above archiveMaxFrameRaw.
	archiveMaxMeta = 256 << 20
	// archiveMaxBody is one reassembled body. Bodies are HTTP request and
	// response bodies; the largest in this store is under 10 MiB.
	archiveMaxBody = 256 << 20
)

// errNotInArchive means the archive is intact and simply does not hold this
// ref. Callers distinguish it from corruption: a partially archived session is
// normal (turns that arrived mid-build stay live as .zst files).
var errNotInArchive = fs.ErrNotExist

type chunkLoc struct {
	frame uint32
	off   uint32
	n     uint32
}

type frameDir struct {
	off     uint64
	compLen uint32
	rawLen  uint32
}

// ---------- writing ----------

type refEntry struct {
	ref    string
	recipe []byte
}

type archiveBuilder struct {
	enc *zstd.Encoder

	ids    map[[32]byte]uint32
	hashes [][16]byte
	locs   []chunkLoc

	pool     []byte // current uncompressed frame
	frames   [][]byte
	frameRaw []uint32
	poolRaw  int64 // every frame, flushed or not: bounded by archiveMaxPool

	refs []refEntry
	seen map[string]bool
}

func newArchiveBuilder() (*archiveBuilder, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(archiveWindow))
	if err != nil {
		return nil, err
	}
	return &archiveBuilder{enc: enc, ids: map[[32]byte]uint32{}, seen: map[string]bool{}}, nil
}

func (b *archiveBuilder) Close() {
	if b.enc != nil {
		b.enc.Close()
		b.enc = nil
	}
}

// Add records one body. raw is chunked and any new chunk content is copied into
// the pool; raw itself is not retained, so peak memory is the session's *unique*
// content plus recipes, not its total traffic.
func (b *archiveBuilder) Add(ref string, raw []byte) error {
	if ref == "" {
		return errors.New("archive: empty ref")
	}
	if b.seen[ref] {
		return fmt.Errorf("archive: duplicate ref %s", ref)
	}
	// The reader refuses to reassemble a body or a pool past these limits, so
	// refusing to write one is what keeps a limit from ever locking a body it
	// accepted out of reach. A session this large is not archived at all: its
	// blobs stay live and every pass reports the failure.
	if len(raw) > archiveMaxBody {
		return fmt.Errorf("archive: body %s is %d bytes, over the %d-byte limit", ref, len(raw), archiveMaxBody)
	}
	b.seen[ref] = true

	recipe := make([]byte, 0, 64)
	var prev int64
	eachChunk(raw, func(c []byte) {
		h := sha256.Sum256(c)
		id, ok := b.ids[h]
		if !ok {
			id = uint32(len(b.ids))
			b.ids[h] = id
			var h16 [16]byte
			copy(h16[:], h[:16])
			b.hashes = append(b.hashes, h16)
			b.locs = append(b.locs, chunkLoc{frame: uint32(len(b.frames)), off: uint32(len(b.pool)), n: uint32(len(c))})
			b.pool = append(b.pool, c...)
			b.poolRaw += int64(len(c))
			if len(b.pool) >= poolFlushAt {
				b.flush()
			}
		}
		d := int64(id) - prev
		prev = int64(id)
		recipe = binary.AppendUvarint(recipe, uint64(d<<1)^uint64(d>>63))
	})
	// Checked after the walk rather than inside it: eachChunk cannot fail, and
	// the overshoot is bounded by this one body.
	if b.poolRaw > archiveMaxPool {
		return fmt.Errorf("archive: pool reached %d bytes, over the %d-byte limit", b.poolRaw, archiveMaxPool)
	}
	b.refs = append(b.refs, refEntry{ref: ref, recipe: recipe})
	return nil
}

func (b *archiveBuilder) flush() {
	if len(b.pool) == 0 {
		return
	}
	b.frames = append(b.frames, b.enc.EncodeAll(b.pool, nil))
	b.frameRaw = append(b.frameRaw, uint32(len(b.pool)))
	b.pool = nil
}

// Bytes serialises the archive. The caller writes it with writeFileSync so the
// install is a single atomic rename.
func (b *archiveBuilder) Bytes() []byte {
	b.flush()

	table := make([]byte, 0, len(b.locs)*chunkEntryLen)
	for i, l := range b.locs {
		table = append(table, b.hashes[i][:]...)
		table = binary.LittleEndian.AppendUint32(table, l.frame)
		table = binary.LittleEndian.AppendUint32(table, l.off)
		table = binary.LittleEndian.AppendUint32(table, l.n)
	}
	ctab := b.enc.EncodeAll(table, nil)

	var refbuf []byte
	for _, r := range b.refs {
		refbuf = binary.LittleEndian.AppendUint16(refbuf, uint16(len(r.ref)))
		refbuf = append(refbuf, r.ref...)
		refbuf = binary.LittleEndian.AppendUint32(refbuf, uint32(len(r.recipe)))
		refbuf = append(refbuf, r.recipe...)
	}
	crefs := b.enc.EncodeAll(refbuf, nil)

	tableOff := uint64(archiveHeaderLen)
	refsOff := tableOff + uint64(len(ctab))
	dirOff := refsOff + uint64(len(crefs))
	framesOff := dirOff + uint64(len(b.frames)*frameDirLen)

	out := make([]byte, 0, int(framesOff))
	out = append(out, archiveMagic...)
	out = binary.LittleEndian.AppendUint16(out, archiveVersion)
	out = binary.LittleEndian.AppendUint16(out, 0) // flags, reserved
	out = binary.LittleEndian.AppendUint32(out, uint32(len(b.locs)))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(b.frames)))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(b.refs)))
	out = binary.LittleEndian.AppendUint64(out, tableOff)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(ctab)))
	out = binary.LittleEndian.AppendUint64(out, refsOff)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(crefs)))
	out = binary.LittleEndian.AppendUint64(out, dirOff)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(b.frames)*frameDirLen))
	if len(out) != archiveHeaderLen {
		panic(fmt.Sprintf("archive header is %d bytes, want %d", len(out), archiveHeaderLen))
	}

	out = append(out, ctab...)
	out = append(out, crefs...)
	off := framesOff
	for i, f := range b.frames {
		out = binary.LittleEndian.AppendUint64(out, off)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(f)))
		out = binary.LittleEndian.AppendUint32(out, b.frameRaw[i])
		off += uint64(len(f))
	}
	for _, f := range b.frames {
		out = append(out, f...)
	}
	return out
}

// ---------- reading ----------

type archiveReader struct {
	path    string
	f       *os.File
	dec     *zstd.Decoder
	modTime int64
	size    int64

	locs   []chunkLoc
	hashes [][16]byte
	dir    []frameDir
	refs   map[string][]byte
	order  []string

	mu   sync.Mutex
	pool [][]byte // materialised frames, nil until first touched
}

func openArchive(path string) (*archiveReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	hdr := make([]byte, archiveHeaderLen)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("archive %s: read header: %w", path, err)
	}
	if string(hdr[:4]) != archiveMagic {
		f.Close()
		return nil, fmt.Errorf("archive %s: bad magic %q", path, hdr[:4])
	}
	if v := binary.LittleEndian.Uint16(hdr[4:]); v != archiveVersion {
		f.Close()
		return nil, fmt.Errorf("archive %s: unsupported version %d", path, v)
	}
	nchunks := binary.LittleEndian.Uint32(hdr[8:])
	nframes := binary.LittleEndian.Uint32(hdr[12:])
	nrefs := binary.LittleEndian.Uint32(hdr[16:])
	tableOff := binary.LittleEndian.Uint64(hdr[20:])
	tableLen := binary.LittleEndian.Uint32(hdr[28:])
	refsOff := binary.LittleEndian.Uint64(hdr[32:])
	refsLen := binary.LittleEndian.Uint32(hdr[40:])
	dirOff := binary.LittleEndian.Uint64(hdr[44:])
	dirLen := binary.LittleEndian.Uint32(hdr[52:])

	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(archiveMaxMeta))
	if err != nil {
		f.Close()
		return nil, err
	}
	a := &archiveReader{
		path: path, f: f, dec: dec,
		modTime: fi.ModTime().UnixNano(), size: fi.Size(),
	}
	fail := func(err error) (*archiveReader, error) {
		a.Close()
		return nil, err
	}

	// Nothing below allocates on a length the file has not been shown to hold.
	// The arithmetic is deliberately overflow-free: off is 64-bit and off+n would
	// wrap for a corrupt header, which is exactly the input this guards against.
	sect := func(off uint64, n uint32, what string) ([]byte, error) {
		if n == 0 {
			return nil, nil
		}
		if off < archiveHeaderLen || off > uint64(a.size) || uint64(n) > uint64(a.size)-off {
			return nil, fmt.Errorf("archive %s: %s section past EOF", path, what)
		}
		buf := make([]byte, n)
		if _, err := f.ReadAt(buf, int64(off)); err != nil {
			return nil, fmt.Errorf("archive %s: read %s: %w", path, what, err)
		}
		return buf, nil
	}

	// The frame directory comes first even though it sits third in the file: it
	// is uncompressed, it bounds the whole pool, and the chunk table is only
	// meaningful checked against it.
	if int64(nframes)*frameDirLen != int64(dirLen) {
		return fail(fmt.Errorf("archive %s: frame dir is %d bytes, want %d for %d frames",
			path, dirLen, int64(nframes)*frameDirLen, nframes))
	}
	dirbuf, err := sect(dirOff, dirLen, "dir")
	if err != nil {
		return fail(err)
	}
	a.dir = make([]frameDir, nframes)
	var poolRaw int64
	for i := range a.dir {
		e := dirbuf[i*frameDirLen:]
		d := frameDir{
			off:     binary.LittleEndian.Uint64(e),
			compLen: binary.LittleEndian.Uint32(e[8:]),
			rawLen:  binary.LittleEndian.Uint32(e[12:]),
		}
		if d.off < archiveHeaderLen || d.off > uint64(a.size) || uint64(d.compLen) > uint64(a.size)-d.off {
			return fail(fmt.Errorf("archive %s: frame %d runs past EOF", path, i))
		}
		if d.compLen == 0 || d.rawLen == 0 || d.rawLen > archiveMaxFrameRaw {
			return fail(fmt.Errorf("archive %s: frame %d claims %d raw bytes from %d compressed",
				path, i, d.rawLen, d.compLen))
		}
		poolRaw += int64(d.rawLen)
		if poolRaw > archiveMaxPool {
			return fail(fmt.Errorf("archive %s: pool claims over %d bytes", path, archiveMaxPool))
		}
		a.dir[i] = d
	}
	a.pool = make([][]byte, nframes)

	if int64(nchunks)*chunkEntryLen > archiveMaxMeta {
		return fail(fmt.Errorf("archive %s: header claims %d chunks", path, nchunks))
	}
	ctab, err := sect(tableOff, tableLen, "table")
	if err != nil {
		return fail(err)
	}
	var table []byte
	if len(ctab) > 0 {
		if table, err = dec.DecodeAll(ctab, nil); err != nil {
			return fail(fmt.Errorf("archive %s: decode table: %w", path, err))
		}
	}
	if int64(len(table)) != int64(nchunks)*chunkEntryLen {
		return fail(fmt.Errorf("archive %s: table is %d bytes, want %d for %d chunks",
			path, len(table), int64(nchunks)*chunkEntryLen, nchunks))
	}
	a.locs = make([]chunkLoc, nchunks)
	a.hashes = make([][16]byte, nchunks)
	for i := range a.locs {
		e := table[i*chunkEntryLen:]
		copy(a.hashes[i][:], e[:16])
		l := chunkLoc{
			frame: binary.LittleEndian.Uint32(e[16:]),
			off:   binary.LittleEndian.Uint32(e[20:]),
			n:     binary.LittleEndian.Uint32(e[24:]),
		}
		// Resolving a chunk against its frame here means Get's length pass
		// cannot be talked into sizing a buffer for chunks that do not fit.
		if int64(l.frame) >= int64(len(a.dir)) {
			return fail(fmt.Errorf("archive %s: chunk %d names frame %d of %d", path, i, l.frame, len(a.dir)))
		}
		if l.n == 0 || l.n > chunkMax || uint64(l.off)+uint64(l.n) > uint64(a.dir[l.frame].rawLen) {
			return fail(fmt.Errorf("archive %s: chunk %d is %d bytes at offset %d, outside frame %d",
				path, i, l.n, l.off, l.frame))
		}
		a.locs[i] = l
	}

	crefs, err := sect(refsOff, refsLen, "refs")
	if err != nil {
		return fail(err)
	}
	var refbuf []byte
	if len(crefs) > 0 {
		if refbuf, err = dec.DecodeAll(crefs, nil); err != nil {
			return fail(fmt.Errorf("archive %s: decode refs: %w", path, err))
		}
	}
	// nrefs is only a hint until the section proves it: every entry costs at
	// least a length pair and one byte of ref, so the decoded size is the real
	// bound on how many there can be.
	const minRefEntry = 2 + 1 + 4
	hint := int64(nrefs)
	if room := int64(len(refbuf)/minRefEntry) + 1; hint > room {
		hint = room
	}
	a.refs = make(map[string][]byte, int(hint))
	for i := uint32(0); i < nrefs; i++ {
		if len(refbuf) < 2 {
			return fail(fmt.Errorf("archive %s: refs truncated", path))
		}
		rl := int(binary.LittleEndian.Uint16(refbuf))
		refbuf = refbuf[2:]
		if len(refbuf) < rl+4 {
			return fail(fmt.Errorf("archive %s: refs truncated", path))
		}
		ref := string(refbuf[:rl])
		refbuf = refbuf[rl:]
		pl := int(binary.LittleEndian.Uint32(refbuf))
		refbuf = refbuf[4:]
		if len(refbuf) < pl {
			return fail(fmt.Errorf("archive %s: refs truncated", path))
		}
		a.refs[ref] = refbuf[:pl]
		a.order = append(a.order, ref)
		refbuf = refbuf[pl:]
	}
	return a, nil
}

func (a *archiveReader) Close() error {
	if a.dec != nil {
		a.dec.Close()
		a.dec = nil
	}
	if a.f != nil {
		err := a.f.Close()
		a.f = nil
		return err
	}
	return nil
}

// Refs returns the refs this archive holds, in the order they were added.
func (a *archiveReader) Refs() []string { return a.order }

// Has reports whether the archive holds a ref, without materialising anything.
func (a *archiveReader) Has(ref string) bool {
	_, ok := a.refs[ref]
	return ok
}

// stale reports whether the file behind this reader has been replaced — an
// archive rebuild renames a new file over the old path, and a cached reader
// would otherwise keep serving the previous inode.
func (a *archiveReader) stale() bool {
	fi, err := os.Stat(a.path)
	if err != nil {
		return true
	}
	return fi.ModTime().UnixNano() != a.modTime || fi.Size() != a.size
}

// frame materialises one pool frame, decoding it at most once.
func (a *archiveReader) frame(i uint32) ([]byte, error) {
	if int(i) >= len(a.dir) {
		return nil, fmt.Errorf("archive %s: frame %d out of range", a.path, i)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if p := a.pool[i]; p != nil {
		return p, nil
	}
	d := a.dir[i]
	comp := make([]byte, d.compLen)
	if _, err := a.f.ReadAt(comp, int64(d.off)); err != nil {
		return nil, fmt.Errorf("archive %s: read frame %d: %w", a.path, i, err)
	}
	raw, err := a.dec.DecodeAll(comp, nil)
	if err != nil {
		return nil, fmt.Errorf("archive %s: decode frame %d: %w", a.path, i, err)
	}
	if uint32(len(raw)) != d.rawLen {
		return nil, fmt.Errorf("archive %s: frame %d is %d bytes, want %d", a.path, i, len(raw), d.rawLen)
	}
	a.pool[i] = raw
	return raw, nil
}

// Get reassembles one body. Returns errNotInArchive if this archive simply does
// not hold the ref, which is a normal state for a partially archived session.
func (a *archiveReader) Get(ref string) ([]byte, error) {
	recipe, ok := a.refs[ref]
	if !ok {
		return nil, errNotInArchive
	}
	// Two passes: sum the chunk lengths, then fill an exactly sized buffer.
	// Bodies run to megabytes, so growing by append would repeatedly copy them.
	total, err := a.recipeLen(ref, recipe)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, total)
	err = a.eachRecipeChunk(ref, recipe, func(l chunkLoc, f []byte) {
		out = append(out, f[l.off:l.off+l.n]...)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (a *archiveReader) recipeLen(ref string, recipe []byte) (int, error) {
	var total int64
	var id int64
	for len(recipe) > 0 {
		u, n := binary.Uvarint(recipe)
		if n <= 0 {
			return 0, fmt.Errorf("archive %s: %s: malformed recipe", a.path, ref)
		}
		recipe = recipe[n:]
		id += int64(u>>1) ^ -int64(u&1)
		if id < 0 || id >= int64(len(a.locs)) {
			return 0, fmt.Errorf("archive %s: %s: chunk id %d out of range", a.path, ref, id)
		}
		total += int64(a.locs[id].n)
		if total > archiveMaxBody {
			return 0, fmt.Errorf("archive %s: %s: body claims over %d bytes", a.path, ref, archiveMaxBody)
		}
	}
	return int(total), nil
}

// eachRecipeChunk walks a recipe, materialising frames as needed. Bounds are
// checked against the decoded frame so a corrupt table produces an error rather
// than a panic in a dashboard handler.
func (a *archiveReader) eachRecipeChunk(ref string, recipe []byte, fn func(chunkLoc, []byte)) error {
	var id int64
	for len(recipe) > 0 {
		u, n := binary.Uvarint(recipe)
		if n <= 0 {
			return fmt.Errorf("archive %s: %s: malformed recipe", a.path, ref)
		}
		recipe = recipe[n:]
		id += int64(u>>1) ^ -int64(u&1)
		if id < 0 || id >= int64(len(a.locs)) {
			return fmt.Errorf("archive %s: %s: chunk id %d out of range", a.path, ref, id)
		}
		l := a.locs[id]
		f, err := a.frame(l.frame)
		if err != nil {
			return err
		}
		if uint64(l.off)+uint64(l.n) > uint64(len(f)) {
			return fmt.Errorf("archive %s: %s: chunk %d out of frame bounds", a.path, ref, id)
		}
		fn(l, f)
	}
	return nil
}

// Verify decodes every frame, checks each chunk against the hash recorded in
// this archive's own table, and resolves every ref. Content addresses are what
// make damage detectable rather than silent, which matters more here than
// anywhere else in the store: the point of the record is that it is faithful.
func (a *archiveReader) Verify() (chunks int, err error) {
	for i := range a.dir {
		if _, err := a.frame(uint32(i)); err != nil {
			return chunks, err
		}
	}
	for id, l := range a.locs {
		f, err := a.frame(l.frame)
		if err != nil {
			return chunks, err
		}
		if uint64(l.off)+uint64(l.n) > uint64(len(f)) {
			return chunks, fmt.Errorf("archive %s: chunk %d out of frame bounds", a.path, id)
		}
		sum := sha256.Sum256(f[l.off : l.off+l.n])
		if !bytes.Equal(sum[:16], a.hashes[id][:]) {
			return chunks, fmt.Errorf("archive %s: chunk %d fails its hash", a.path, id)
		}
		chunks++
	}
	for _, ref := range a.order {
		if _, err := a.Get(ref); err != nil {
			return chunks, err
		}
	}
	return chunks, nil
}
