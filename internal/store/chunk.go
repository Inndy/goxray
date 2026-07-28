package store

import (
	"crypto/sha256"
	"encoding/binary"
)

// Content-defined chunking splits a body at boundaries chosen by its own bytes
// rather than at fixed offsets, so an edit shifts only the chunks around it
// instead of every chunk after it. That is the whole reason a session archive
// works: Claude Code re-sends the entire conversation every turn *and rewrites
// the echo in place* (stripping `caller`, moving `cache_control`), so neither a
// shared prefix nor fixed blocks would line up between consecutive turns.
// Measured on a real store, 95% of chunks in a session are already present from
// an earlier turn.
const (
	chunkAvg = 8 << 10 // must stay a power of two: chunkMask depends on it
	chunkMin = chunkAvg / 4
	chunkMax = chunkAvg * 4

	chunkMask = chunkAvg - 1
)

// gearTable is derived from SHA-256 of a fixed seed rather than from math/rand.
// The sequence rand produces for a given seed is not guaranteed stable across
// Go releases, and a table that shifted between builds would silently stop new
// chunks from matching those in archives written by an older binary — a
// compression regression with no error to notice it by.
var gearTable = buildGearTable()

func buildGearTable() [256]uint64 {
	var t [256]uint64
	for i := range t {
		h := sha256.Sum256([]byte{'g', 'o', 'x', 'r', 'a', 'y', '/', 'g', 'e', 'a', 'r', '/', '1', byte(i)})
		t[i] = binary.LittleEndian.Uint64(h[:8])
	}
	return t
}

// eachChunk calls fn with successive content-defined slices of data. The slices
// alias data and must not be retained by fn.
func eachChunk(data []byte, fn func(chunk []byte)) {
	for len(data) > 0 {
		n := nextChunk(data)
		fn(data[:n])
		data = data[n:]
	}
}

// nextChunk returns the length of the leading chunk of data. Boundaries are
// clamped to [chunkMin, chunkMax] so that neither a long incompressible run
// (the encrypted `thinking.signature` blocks, which never trigger the mask) nor
// a pathologically self-similar one can produce a single huge chunk or a flood
// of tiny ones. The clamp is why chunk lengths are recorded explicitly in the
// archive: a reader must never re-derive boundaries to read data back.
func nextChunk(data []byte) int {
	if len(data) <= chunkMin {
		return len(data)
	}
	limit := min(len(data), chunkMax)
	var h uint64
	// The first chunkMin bytes are not hashed: they can never hold a boundary,
	// so rolling over them would only cost time.
	for i := chunkMin; i < limit; i++ {
		h = h<<1 + gearTable[data[i]]
		if h&chunkMask == 0 {
			return i + 1
		}
	}
	return limit
}
