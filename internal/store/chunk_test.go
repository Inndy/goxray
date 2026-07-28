package store

import (
	"bytes"
	"math/rand"
	"testing"
)

// jsonish builds a body with the shape that actually matters here: long
// repeated structure with high-entropy islands, like a conversation carrying
// encrypted thinking signatures.
func jsonish(seed int64, n int) []byte {
	rng := rand.New(rand.NewSource(seed))
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString(`{"type":"tool_result","tool_use_id":"toolu_`)
		for i := 0; i < 24; i++ {
			b.WriteByte(byte('a' + rng.Intn(26)))
		}
		b.WriteString(`","content":"`)
		for i := 0; i < 200+rng.Intn(2000); i++ {
			b.WriteByte(byte('A' + rng.Intn(64)))
		}
		b.WriteString(`"},`)
	}
	return b.Bytes()
}

func chunksOf(data []byte) [][]byte {
	var out [][]byte
	eachChunk(data, func(c []byte) { out = append(out, c) })
	return out
}

func TestChunkReassembles(t *testing.T) {
	for _, size := range []int{0, 1, chunkMin - 1, chunkMin, chunkMin + 1, chunkMax, chunkMax + 7, 300 << 10} {
		data := jsonish(1, size)[:size]
		var got []byte
		for _, c := range chunksOf(data) {
			got = append(got, c...)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("size %d: reassembly mismatch", size)
		}
	}
}

func TestChunkBounds(t *testing.T) {
	data := jsonish(2, 512<<10)
	cs := chunksOf(data)
	if len(cs) < 8 {
		t.Fatalf("expected many chunks, got %d", len(cs))
	}
	for i, c := range cs {
		if len(c) > chunkMax {
			t.Errorf("chunk %d is %d bytes, over chunkMax %d", i, len(c), chunkMax)
		}
		// Only the final chunk may be shorter than the minimum.
		if len(c) < chunkMin && i != len(cs)-1 {
			t.Errorf("chunk %d is %d bytes, under chunkMin %d", i, len(c), chunkMin)
		}
	}
}

func TestChunkDeterministic(t *testing.T) {
	data := jsonish(3, 256<<10)
	a, b := chunksOf(data), chunksOf(data)
	if len(a) != len(b) {
		t.Fatalf("chunk count differs between runs: %d != %d", len(a), len(b))
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			t.Fatalf("chunk %d length differs between runs", i)
		}
	}
}

// A boundary table that shifted between builds would stop new chunks matching
// archives written by an older binary — silent, and invisible except as a
// compression regression. Pin a value so a change to the derivation has to be
// deliberate.
func TestGearTableIsPinned(t *testing.T) {
	if got := gearTable[0]; got != 0x16ceb98bcfec5de7 {
		t.Errorf("gearTable[0] = %#016x; changing the gear table breaks dedup against existing archives", got)
	}
}

// The property the archive depends on: an edit perturbs only nearby boundaries
// instead of resyncing the whole remainder, which is what fixed-size blocks or
// prefix sharing would do.
func TestChunkEditLocality(t *testing.T) {
	orig := jsonish(4, 512<<10)
	edited := make([]byte, 0, len(orig)+16)
	cut := len(orig) / 2
	edited = append(edited, orig[:cut]...)
	edited = append(edited, []byte("INSERTED-BYTES--")...)
	edited = append(edited, orig[cut:]...)

	set := map[string]bool{}
	for _, c := range chunksOf(orig) {
		set[string(c)] = true
	}
	after := chunksOf(edited)
	shared := 0
	for _, c := range after {
		if set[string(c)] {
			shared++
		}
	}
	if ratio := float64(shared) / float64(len(after)); ratio < 0.9 {
		t.Errorf("only %.0f%% of chunks survived a 16-byte insertion (%d/%d); boundaries are not resyncing",
			ratio*100, shared, len(after))
	}
}
