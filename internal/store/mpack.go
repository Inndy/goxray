package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// A manifest pack holds every manifest of one session in a single self-contained
// file, as one solid zstd frame plus a directory of lengths.
//
// Manifests were the only uncompressed tier in the store, and had grown to
// outweigh every archive combined: 618 files, 2.53 MiB on disk for 1.10 MiB of
// JSON — half of it 4 KiB cluster slack, and two thirds of the rest the same
// `anthropic-ratelimit-*` headers written once per turn. Per-file compression
// only reaches 2.1× because the redundancy is *between* manifests, not inside
// one. Packed per session it reaches 16.2×, within 9% of pooling every manifest
// in the store into one file, which per-session scope beats on blast radius.
// See docs/store-efficiency.md.
//
// On-disk layout (all integers little-endian):
//
//	header   24 bytes, see mpackHeaderLen
//	dir      zstd( nman × {idLen u16, id, rawLen u32} )
//	pay      zstd( every manifest of the session, concatenated )
//	session  the session id, sessionLen bytes, at EOF
//
// Three things this is deliberately not:
//
// It is **not part of the session's .gxa**, though it names the same session and
// rides the same pass. Manifests are the audit record and are what fsck reads to
// say which exchanges a damaged archive cost; sharing one file would mean a
// corrupt pool frame, a misfiled archive, or a downgraded header took the record
// with it, leaving "this archive is broken" instead of "these six turns are
// unreadable". They are also the mutable half — `goxray migrate-usage` rewrites
// manifests — and folding them in would make changing one JSON field rebuild a
// 14.8 MiB body pool.
//
// It **does not chunk**. A manifest is ~2 KB, so it is a single chunk at any sane
// chunkAvg, dedup between two of them never fires, and a 16-byte incompressible
// hash each would be ~20% pure overhead. One solid frame is where the ratio comes
// from, and a directory of lengths is enough to slice one back out because the
// offsets are the running sum.
//
// It **holds no offsets into itself** beyond the two section lengths in the
// header, so there is nothing for a corrupt byte to turn into a bad seek.
const (
	mpackMagic     = "GXM1"
	mpackVersion   = 1
	mpackHeaderLen = 24
	mpackExt       = ".gxm"

	// Bounds on untrusted input, in the same spirit as the archive's: a flipped
	// count must produce a reported error rather than a huge allocation in
	// whatever process read the file. Both are also enforced by the builder, so
	// they can only ever reject a file goxray did not write. mpackMaxRaw stays
	// under archiveMaxMeta because that is the decoder's memory ceiling.
	mpackMaxCount = 1 << 20
	mpackMaxRaw   = 128 << 20
)

// errNotInPack means the pack is intact and simply does not hold this manifest —
// normal for a turn that is still live, or whose session predates packing.
var errNotInPack = errNotInArchive

type manEntry struct {
	id string
	n  uint32
}

type manLoc struct {
	off uint32
	n   uint32
}

// ---------- writing ----------

type mpackBuilder struct {
	enc     *zstd.Encoder
	session string

	dir  []manEntry
	pool []byte
	seen map[string]bool
}

func newMpackBuilder(session string) (*mpackBuilder, error) {
	if session == "" {
		return nil, errors.New("mpack: empty session id")
	}
	if len(session) > archiveMaxSession {
		return nil, fmt.Errorf("mpack: session id is %d bytes, over the %d-byte limit", len(session), archiveMaxSession)
	}
	// No window override: the payload is under a megabyte for any real session,
	// so the default window already spans it.
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	return &mpackBuilder{enc: enc, session: session, seen: map[string]bool{}}, nil
}

func (b *mpackBuilder) Close() {
	if b.enc != nil {
		b.enc.Close()
		b.enc = nil
	}
}

// Add records one manifest verbatim. raw is copied, so the caller may reuse it.
func (b *mpackBuilder) Add(id string, raw []byte) error {
	if id == "" {
		return errors.New("mpack: empty manifest id")
	}
	// idLen is a u16 and an id is a uuid, so anything near this is already not an
	// id; the section still has to be writable to be readable.
	if len(id) > math.MaxUint16 {
		return fmt.Errorf("mpack: manifest id is %d bytes, over the %d-byte limit", len(id), math.MaxUint16)
	}
	if b.seen[id] {
		return fmt.Errorf("mpack: duplicate manifest %s", id)
	}
	if len(b.dir)+1 > mpackMaxCount {
		return fmt.Errorf("mpack: over the %d-manifest limit", mpackMaxCount)
	}
	if int64(len(b.pool))+int64(len(raw)) > mpackMaxRaw {
		return fmt.Errorf("mpack: manifests reached %d bytes, over the %d-byte limit",
			int64(len(b.pool))+int64(len(raw)), mpackMaxRaw)
	}
	b.seen[id] = true
	b.dir = append(b.dir, manEntry{id: id, n: uint32(len(raw))})
	b.pool = append(b.pool, raw...)
	return nil
}

func (b *mpackBuilder) Len() int { return len(b.dir) }

// Bytes serialises the pack. The caller writes it with writeFileSync so the
// install is a single atomic rename.
func (b *mpackBuilder) Bytes() []byte {
	var dirbuf []byte
	for _, m := range b.dir {
		dirbuf = binary.LittleEndian.AppendUint16(dirbuf, uint16(len(m.id)))
		dirbuf = append(dirbuf, m.id...)
		dirbuf = binary.LittleEndian.AppendUint32(dirbuf, m.n)
	}
	cdir := b.enc.EncodeAll(dirbuf, nil)
	cpay := b.enc.EncodeAll(b.pool, nil)

	out := make([]byte, 0, mpackHeaderLen+len(cdir)+len(cpay)+len(b.session))
	out = append(out, mpackMagic...)
	out = binary.LittleEndian.AppendUint16(out, mpackVersion)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(b.session)))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(b.dir)))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(cdir)))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(cpay)))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(b.pool)))
	if len(out) != mpackHeaderLen {
		panic(fmt.Sprintf("mpack header is %d bytes, want %d", len(out), mpackHeaderLen))
	}
	out = append(out, cdir...)
	out = append(out, cpay...)
	return append(out, b.session...)
}

// ---------- reading ----------

type mpackReader struct {
	path    string
	f       *os.File
	dec     *zstd.Decoder
	modTime int64
	size    int64
	session string

	locs   map[string]manLoc
	order  []string
	payOff uint64
	payLen uint32
	rawLen uint32

	mu  sync.Mutex
	pay []byte // materialised payload, nil until first touched
}

func openMpack(path string) (*mpackReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	hdr := make([]byte, mpackHeaderLen)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("mpack %s: read header: %w", path, err)
	}
	if string(hdr[:4]) != mpackMagic {
		f.Close()
		return nil, fmt.Errorf("mpack %s: bad magic %q", path, hdr[:4])
	}
	version := binary.LittleEndian.Uint16(hdr[4:])
	if version != mpackVersion {
		f.Close()
		return nil, fmt.Errorf("mpack %s: unsupported version %d", path, version)
	}
	sessionLen := binary.LittleEndian.Uint16(hdr[6:])
	nman := binary.LittleEndian.Uint32(hdr[8:])
	dirLen := binary.LittleEndian.Uint32(hdr[12:])
	payLen := binary.LittleEndian.Uint32(hdr[16:])
	rawLen := binary.LittleEndian.Uint32(hdr[20:])

	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(archiveMaxMeta))
	if err != nil {
		f.Close()
		return nil, err
	}
	m := &mpackReader{
		path: path, f: f, dec: dec,
		modTime: fi.ModTime().UnixNano(), size: fi.Size(),
	}
	fail := func(err error) (*mpackReader, error) {
		m.Close()
		return nil, err
	}

	// The trailer is what every other section has to end before, so it is read
	// first: limit, not the file size, is EOF for the rest of the format.
	limit := m.size - int64(sessionLen)
	if limit < mpackHeaderLen {
		return fail(fmt.Errorf("mpack %s: %d-byte session trailer does not fit in %d bytes", path, sessionLen, m.size))
	}
	if sessionLen > 0 {
		buf := make([]byte, sessionLen)
		if _, err := f.ReadAt(buf, limit); err != nil {
			return fail(fmt.Errorf("mpack %s: read session: %w", path, err))
		}
		m.session = string(buf)
	}
	if int64(nman) > mpackMaxCount {
		return fail(fmt.Errorf("mpack %s: header claims %d manifests", path, nman))
	}
	if int64(rawLen) > mpackMaxRaw {
		return fail(fmt.Errorf("mpack %s: manifests claim %d bytes, over the %d-byte limit", path, rawLen, mpackMaxRaw))
	}

	// Overflow-free on purpose: off is 64-bit and off+n would wrap for a corrupt
	// header, which is exactly the input this guards against.
	inBounds := func(off uint64, n uint32) bool {
		return off >= mpackHeaderLen && off <= uint64(limit) && uint64(n) <= uint64(limit)-off
	}
	if !inBounds(mpackHeaderLen, dirLen) {
		return fail(fmt.Errorf("mpack %s: directory past EOF", path))
	}
	payOff := uint64(mpackHeaderLen) + uint64(dirLen)
	if !inBounds(payOff, payLen) {
		return fail(fmt.Errorf("mpack %s: payload past EOF", path))
	}
	m.payOff, m.payLen, m.rawLen = payOff, payLen, rawLen

	if nman == 0 {
		return m, nil
	}
	cdir := make([]byte, dirLen)
	if _, err := f.ReadAt(cdir, mpackHeaderLen); err != nil {
		return fail(fmt.Errorf("mpack %s: read directory: %w", path, err))
	}
	dirbuf, err := dec.DecodeAll(cdir, nil)
	if err != nil {
		return fail(fmt.Errorf("mpack %s: decode directory: %w", path, err))
	}
	// nman is only a hint until the decoded directory proves it: every entry costs
	// at least a length pair and one byte of id.
	const minEntry = 2 + 1 + 4
	hint := int64(nman)
	if room := int64(len(dirbuf)/minEntry) + 1; hint > room {
		hint = room
	}
	m.locs = make(map[string]manLoc, int(hint))
	var off uint64
	for i := uint32(0); i < nman; i++ {
		if len(dirbuf) < 2 {
			return fail(fmt.Errorf("mpack %s: directory truncated", path))
		}
		il := int(binary.LittleEndian.Uint16(dirbuf))
		dirbuf = dirbuf[2:]
		if len(dirbuf) < il+4 {
			return fail(fmt.Errorf("mpack %s: directory truncated", path))
		}
		id := string(dirbuf[:il])
		dirbuf = dirbuf[il:]
		n := binary.LittleEndian.Uint32(dirbuf)
		dirbuf = dirbuf[4:]
		if off+uint64(n) > uint64(rawLen) {
			return fail(fmt.Errorf("mpack %s: manifest %s runs past the %d-byte payload", path, id, rawLen))
		}
		m.locs[id] = manLoc{off: uint32(off), n: n}
		m.order = append(m.order, id)
		off += uint64(n)
	}
	// Every payload byte must be claimed by exactly one manifest. A short fall
	// means the directory and the payload disagree about what is in the file, and
	// slicing on the directory alone would then hand back the wrong record rather
	// than an error.
	if off != uint64(rawLen) {
		return fail(fmt.Errorf("mpack %s: directory covers %d of %d payload bytes", path, off, rawLen))
	}
	return m, nil
}

func (m *mpackReader) Close() error {
	if m.dec != nil {
		m.dec.Close()
		m.dec = nil
	}
	if m.f != nil {
		err := m.f.Close()
		m.f = nil
		return err
	}
	return nil
}

// IDs returns the exchange ids this pack holds, in the order they were added.
func (m *mpackReader) IDs() []string { return m.order }

// Session returns the session the pack was built for.
func (m *mpackReader) Session() string { return m.session }

// Has reports whether the pack holds a manifest, without decoding anything.
func (m *mpackReader) Has(id string) bool {
	_, ok := m.locs[id]
	return ok
}

// stale reports whether the file behind this reader has been replaced. A pack is
// rebuilt by renaming a new file over the old path, so a cached reader would
// otherwise keep serving the previous inode.
func (m *mpackReader) stale() bool {
	fi, err := os.Stat(m.path)
	if err != nil {
		return true
	}
	return fi.ModTime().UnixNano() != m.modTime || fi.Size() != m.size
}

// payload materialises the solid frame, decoding it at most once.
func (m *mpackReader) payload() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pay != nil {
		return m.pay, nil
	}
	comp := make([]byte, m.payLen)
	if _, err := m.f.ReadAt(comp, int64(m.payOff)); err != nil {
		return nil, fmt.Errorf("mpack %s: read payload: %w", m.path, err)
	}
	raw, err := m.dec.DecodeAll(comp, nil)
	if err != nil {
		return nil, fmt.Errorf("mpack %s: decode payload: %w", m.path, err)
	}
	if uint32(len(raw)) != m.rawLen {
		return nil, fmt.Errorf("mpack %s: payload is %d bytes, want %d", m.path, len(raw), m.rawLen)
	}
	m.pay = raw
	return raw, nil
}

// Get returns one manifest verbatim, or errNotInPack if this pack does not hold
// it.
func (m *mpackReader) Get(id string) ([]byte, error) {
	loc, ok := m.locs[id]
	if !ok {
		return nil, errNotInPack
	}
	pay, err := m.payload()
	if err != nil {
		return nil, err
	}
	// Copied out: the payload is cached and shared, and a manifest goes on to be
	// unmarshalled and repriced by its caller.
	out := make([]byte, loc.n)
	copy(out, pay[loc.off:loc.off+loc.n])
	return out, nil
}

// Verify resolves every manifest the pack claims. There is no per-manifest hash
// to check — they are not chunked, so there is no chunk table to check them
// against — so what stands in for one is the zstd frame checksum plus the
// directory/payload length agreement enforced at open.
func (m *mpackReader) Verify() (manifests int, err error) {
	for _, id := range m.order {
		if _, err := m.Get(id); err != nil {
			return manifests, err
		}
		manifests++
	}
	return manifests, nil
}
