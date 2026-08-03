package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// conversation fabricates the growth shape the archive exists for: each turn
// resends everything before it, with the echo lightly rewritten, plus a fresh
// high-entropy block standing in for an encrypted thinking signature.
func conversation(turns int) [][]byte {
	rng := rand.New(rand.NewSource(11))
	fill := func(n int, base byte) string {
		var sb strings.Builder
		for i := 0; i < n; i++ {
			sb.WriteByte(base + byte(rng.Intn(26)))
		}
		return sb.String()
	}

	tools := make([]string, 40)
	for i := range tools {
		tools[i] = fmt.Sprintf(`{"name":"tool_%d","description":"does the %d thing"}`, i, i)
	}
	head := `{"model":"claude-opus-5","tools":[` + strings.Join(tools, ",") + `],"messages":[`

	var msgs []string
	var out [][]byte
	for t := 0; t < turns; t++ {
		msgs = append(msgs,
			fmt.Sprintf(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%d","content":%q}]}`, t, fill(3000, 'A')),
			fmt.Sprintf(`{"role":"assistant","content":[{"type":"thinking","signature":%q}]}`, fill(1200, 'a')))
		body := head + strings.Join(msgs, ",") + `]}`
		// Claude Code rewrites the echo between turns, so consecutive requests
		// are not prefixes of one another. Reproduce that from the second turn
		// on, or the fixture would flatter the chunker.
		if t > 0 {
			body = strings.Replace(body, `"tool_use_id":"toolu_0"`, `"tool_use_id":"toolu_x"`, 1)
		}
		if !json.Valid([]byte(body)) {
			panic("conversation fixture is not valid JSON")
		}
		out = append(out, []byte(body))
	}
	return out
}

func buildArchive(t *testing.T, path string, refs []string, bodies [][]byte) {
	t.Helper()
	b, err := newArchiveBuilder("s-" + filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for i, ref := range refs {
		if err := b.Add(ref, bodies[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeFileSync(path, b.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	bodies := conversation(24)
	// Bodies the store really contains alongside the big ones: empty, tiny,
	// and binary with NULs.
	bodies = append(bodies, []byte{}, []byte("x"), []byte("\x00\xff\xfe binary \x00 body"))
	refs := make([]string, len(bodies))
	for i := range refs {
		refs[i] = fmt.Sprintf("019fa301-9d78-77fe-9d3f-1ff62ba445%02d", i)
	}

	path := filepath.Join(t.TempDir(), "s"+archiveExt)
	buildArchive(t, path, refs, bodies)

	a, err := openArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if len(a.Refs()) != len(refs) {
		t.Errorf("Refs() = %d, want %d", len(a.Refs()), len(refs))
	}
	for i, ref := range refs {
		got, err := a.Get(ref)
		if err != nil {
			t.Fatalf("get %s: %v", ref, err)
		}
		if !bytes.Equal(got, bodies[i]) {
			t.Fatalf("ref %s: %d bytes back, want %d", ref, len(got), len(bodies[i]))
		}
	}

	if _, err := a.Get("no-such-ref"); err != errNotInArchive {
		t.Errorf("Get(missing) = %v, want errNotInArchive", err)
	}
	if a.Has("no-such-ref") {
		t.Error("Has(missing) = true")
	}

	n, err := a.Verify()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if n == 0 {
		t.Error("verify reported zero chunks")
	}

	// The point of the exercise: a session costs far less than its traffic.
	var raw int64
	for _, b := range bodies {
		raw += int64(len(b))
	}
	fi, _ := os.Stat(path)
	if ratio := float64(raw) / float64(fi.Size()); ratio < 8 {
		t.Errorf("archive is only %.1fx smaller than the raw bodies (%d -> %d)", ratio, raw, fi.Size())
	}
}

func TestArchiveDetectsCorruption(t *testing.T) {
	bodies := conversation(8)
	refs := make([]string, len(bodies))
	for i := range refs {
		refs[i] = fmt.Sprintf("ref-%02d", i)
	}
	path := filepath.Join(t.TempDir(), "s"+archiveExt)
	buildArchive(t, path, refs, bodies)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte deep in the pool — the frames are the tail of the file.
	raw[len(raw)-64] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := openArchive(path)
	if err != nil {
		// Refusing to open at all is a fine way to detect it.
		return
	}
	defer a.Close()

	if _, err := a.Verify(); err == nil {
		t.Error("Verify accepted a corrupted archive")
	}
	// Silently returning wrong bytes is the failure mode that actually matters.
	for i, ref := range refs {
		got, err := a.Get(ref)
		if err == nil && !bytes.Equal(got, bodies[i]) {
			t.Fatalf("ref %s returned wrong bytes with no error", ref)
		}
	}
}

func TestArchiveRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string][]byte{
		"empty":  {},
		"short":  []byte("GXA1"),
		"magic":  append([]byte("NOPE"), make([]byte, 200)...),
		"claims": append([]byte("GXA1\x01\x00\x00\x00\xff\xff\xff\xff"), make([]byte, 100)...),
	} {
		p := filepath.Join(dir, name+archiveExt)
		if err := os.WriteFile(p, content, 0o600); err != nil {
			t.Fatal(err)
		}
		a, err := openArchive(p)
		if err == nil {
			a.Close()
			t.Errorf("%s: opened without error", name)
		}
	}
}

// A damaged header must be reported, and reported *cheaply*. Every count in it
// is a length the reader would otherwise allocate on trust: a single flipped
// nrefs used to ask for a four-billion-entry map and take the process down with
// it, which turns `goxray fsck` — the tool for finding damage — into the thing
// the damage kills.
func TestArchiveRejectsImpossibleHeader(t *testing.T) {
	bodies := conversation(6)
	refs := []string{"r0", "r1", "r2", "r3", "r4", "r5"}
	dir := t.TempDir()
	good := filepath.Join(dir, "good"+archiveExt)
	buildArchive(t, good, refs, bodies)
	clean, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	dirOff := binary.LittleEndian.Uint64(clean[44:])

	for _, tc := range []struct {
		name string
		at   int
		size int // 4 or 8 bytes
		val  uint64
	}{
		{"nchunks", 8, 4, 0xfffffff0},
		{"nframes", 12, 4, 0xfffffff0},
		{"nrefs", 16, 4, 0xfffffff0},
		{"tableOff", 20, 8, 1 << 60},
		{"tableLen", 28, 4, 0xfffffff0},
		{"refsOff", 32, 8, 0xffffffffffffff00},
		{"refsLen", 40, 4, 0xfffffff0},
		{"dirOff", 44, 8, 0},
		{"dirLen", 52, 4, 0xfffffff0},
		// Not the header: the frame directory, whose compLen is the read size
		// for a pool frame.
		{"frameCompLen", int(dirOff) + 8, 4, 0xfffffff0},
		{"frameRawLen", int(dirOff) + 12, 4, 0xfffffff0},
	} {
		raw := append([]byte(nil), clean...)
		if tc.size == 4 {
			binary.LittleEndian.PutUint32(raw[tc.at:], uint32(tc.val))
		} else {
			binary.LittleEndian.PutUint64(raw[tc.at:], tc.val)
		}
		p := filepath.Join(dir, tc.name+archiveExt)
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}

		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		a, err := openArchive(p)
		runtime.ReadMemStats(&after)
		if err == nil {
			a.Close()
			t.Errorf("%s: opened an archive claiming %d", tc.name, tc.val)
			continue
		}
		// A rejection that first allocates gigabytes is not a rejection.
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 32<<20 {
			t.Errorf("%s: rejected the file but allocated %d MiB doing it", tc.name, grew>>20)
		}
	}
}

// A resumed session is re-archived from scratch rather than chained onto, so
// rebuilding with more turns must produce a complete archive.
func TestArchiveRebuildWithMoreTurns(t *testing.T) {
	bodies := conversation(20)
	refs := make([]string, len(bodies))
	for i := range refs {
		refs[i] = fmt.Sprintf("ref-%02d", i)
	}
	path := filepath.Join(t.TempDir(), "s"+archiveExt)

	buildArchive(t, path, refs[:10], bodies[:10])
	first, _ := os.Stat(path)

	buildArchive(t, path, refs, bodies)
	a, err := openArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i, ref := range refs {
		got, err := a.Get(ref)
		if err != nil {
			t.Fatalf("get %s: %v", ref, err)
		}
		if !bytes.Equal(got, bodies[i]) {
			t.Fatalf("ref %s mismatch after rebuild", ref)
		}
	}
	second, _ := os.Stat(path)
	// Doubling the turns must not double the archive: the added turns are
	// mostly chunks the first ten already contributed.
	if second.Size() > first.Size()*2 {
		t.Errorf("rebuild grew from %d to %d for 2x the turns; dedup is not working across the rebuild",
			first.Size(), second.Size())
	}
}

func TestArchiveBuilderRejectsDuplicateRef(t *testing.T) {
	b, err := newArchiveBuilder("sess")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Add("a", []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := b.Add("a", []byte("two")); err == nil {
		t.Error("Add accepted a duplicate ref")
	}
	if err := b.Add("", []byte("x")); err == nil {
		t.Error("Add accepted an empty ref")
	}
}

// downgradeToV1 rewrites an archive as the format shipped before the session
// trailer: drop the trailer, put the version back, zero the length field. It is
// tools/migrate-archive-names.py's upgrade run backwards, and it exists so the
// reader's v1 support is tested against a file this build did not write.
func downgradeToV1(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	n := int(binary.LittleEndian.Uint16(raw[6:]))
	if n == 0 {
		t.Fatalf("%s has no session trailer to remove", path)
	}
	raw = raw[:len(raw)-n]
	binary.LittleEndian.PutUint16(raw[4:], 1)
	binary.LittleEndian.PutUint16(raw[6:], 0)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The session id is in the file, so an archive can say whose it is without
// anyone reading its filename.
func TestArchiveRecordsItsSession(t *testing.T) {
	const session = "af5f1ea9-f08a-4d0b-8709-384a1ee6f0ef"
	path := filepath.Join(t.TempDir(), "a"+archiveExt)
	b, err := newArchiveBuilder(session)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	bodies := conversation(4)
	for i, body := range bodies {
		if err := b.Add(fmt.Sprintf("ref-%d", i), body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeFileSync(path, b.Bytes()); err != nil {
		t.Fatal(err)
	}

	a, err := openArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Session() != session {
		t.Errorf("archive reports session %q, want %q", a.Session(), session)
	}
	// The trailer must not have been counted as part of any section.
	if _, err := a.Verify(); err != nil {
		t.Fatal(err)
	}
	for i, body := range bodies {
		got, err := a.Get(fmt.Sprintf("ref-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("body %d came back wrong", i)
		}
	}
}

// A v1 archive is a v2 archive with no trailer: every section is addressed by
// offset, so nothing about reading one changes except that it cannot say whose
// session it holds. Upgrading a store must not be a precondition for reading it.
func TestArchiveReadsV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a"+archiveExt)
	bodies := conversation(6)
	refs := make([]string, len(bodies))
	for i := range bodies {
		refs[i] = fmt.Sprintf("ref-%d", i)
	}
	buildArchive(t, path, refs, bodies)
	downgradeToV1(t, path)

	a, err := openArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Session() != "" {
		t.Errorf("v1 archive reported session %q", a.Session())
	}
	if _, err := a.Verify(); err != nil {
		t.Fatal(err)
	}
	for i, body := range bodies {
		got, err := a.Get(refs[i])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("body %d came back wrong", i)
		}
	}
}

// The length field was reserved-and-zero in v1, so a v1 file carrying anything
// there is damage. Believing it would carve live bytes off the end of the pool.
func TestArchiveRejectsV1WithATrailerLength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a"+archiveExt)
	buildArchive(t, path, []string{"a"}, [][]byte{[]byte("hello")})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint16(raw[4:], 1) // v1 ...
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	} // ... but the trailer length is still set
	if _, err := openArchive(path); err == nil {
		t.Error("openArchive accepted a v1 header with a non-zero reserved field")
	}
}

// A trailer that claims more bytes than the file holds must be refused before
// it is subtracted from every section bound.
func TestArchiveRejectsAnOversizedTrailer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a"+archiveExt)
	buildArchive(t, path, []string{"a"}, [][]byte{[]byte("hello")})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint16(raw[6:], uint16(len(raw)))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openArchive(path); err == nil {
		t.Error("openArchive accepted a trailer longer than the file")
	}
}
