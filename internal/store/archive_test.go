package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
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
	b, err := newArchiveBuilder()
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
	b, err := newArchiveBuilder()
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
