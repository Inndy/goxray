package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.inndy.tw/goxray/internal/model"
)

// countLooseManifests counts the .json files still sitting in the manifest tree.
func countLooseManifests(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".json") {
			n++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return n
}

// packOf reads the manifest pack a session's manifests belong in.
func packOf(t *testing.T, p testPaths, session string) *mpackReader {
	t.Helper()
	pk, err := openMpack(mpackFile(p.archives, session))
	if err != nil {
		t.Fatalf("open pack: %v", err)
	}
	t.Cleanup(func() { pk.Close() })
	return pk
}

// A pass folds a cold session's manifests into its pack and deletes the loose
// copies — the only place in the store that deletes a manifest — and every one of
// them still reads back through the public Reader afterwards.
func TestArchivePacksManifests(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const session = "6f2d1c4b-0000-4000-8000-00000000ma01"
	reqs, ress, ids := recordSession(t, st, session, 6)

	before := map[string]*model.Exchange{}
	for _, id := range ids {
		ex, err := st.Manifests.Read(id)
		if err != nil {
			t.Fatal(err)
		}
		before[id] = ex
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if got := countLooseManifests(t, p.manifests); got != len(ids) {
		t.Fatalf("before archiving: %d loose manifests, want %d", got, len(ids))
	}

	stats := archiveAt(t, p, nil)
	if stats.Manifests != len(ids) {
		t.Errorf("stats reported %d manifests folded in, want %d", stats.Manifests, len(ids))
	}
	if got := countLooseManifests(t, p.manifests); got != 0 {
		t.Errorf("%d loose manifests survived the pass, want 0", got)
	}
	if _, err := os.Stat(mpackFile(p.archives, session)); err != nil {
		t.Fatalf("no manifest pack was installed: %v", err)
	}

	rd := readerAt(t, p)
	for _, id := range ids {
		got, err := rd.Manifest(id)
		if err != nil {
			t.Fatalf("read packed manifest %s: %v", id, err)
		}
		if !reflect.DeepEqual(got, before[id]) {
			t.Errorf("manifest %s differs after packing:\n got %+v\nwant %+v", id, got, before[id])
		}
	}
	// The bodies must still be fine too: the two tiers install in one pass.
	assertBodies(t, p, ids, reqs, ress)
}

// index.db is a projection and must stay deletable-and-rebuildable (invariant 3).
// Packing the manifests it is rebuilt from must not change a single row.
func TestIndexRebuildAfterPackingIsIdentical(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const session = "6f2d1c4b-0000-4000-8000-00000000ma02"
	recordSession(t, st, session, 5)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	rd := readerAt(t, p)
	want, err := rd.List(Filter{IncludeNoise: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(want) == 0 {
		t.Fatal("nothing recorded")
	}
	rd.Close()

	archiveAt(t, p, nil)

	// Exactly the documented repricing procedure: delete the index and restart.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(p.index + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	st2 := openStoreAt(t, p)
	if err := st2.Index.RebuildFrom(newLocalManifests(p.manifests, p.archives)); err != nil {
		t.Fatalf("rebuild from packed manifests: %v", err)
	}
	if err := st2.Close(); err != nil {
		t.Fatal(err)
	}

	rd2 := readerAt(t, p)
	got, err := rd2.List(Filter{IncludeNoise: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("rebuilt index has %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("row %d differs after packing:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

// During the overlap window a manifest exists both loose and packed. EachID must
// yield it once, or every projection built from it would double.
func TestEachIDYieldsEachManifestOnce(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const session = "6f2d1c4b-0000-4000-8000-00000000ma03"
	_, _, ids := recordSession(t, st, session, 4)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	archiveAt(t, p, nil)

	m := newLocalManifests(p.manifests, p.archives)
	// Put one manifest back as a loose file so it exists in both tiers at once,
	// which is exactly the state the pass leaves behind between its two steps.
	overlap := ids[1]
	raw, err := m.readRaw(overlap)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileSync(m.path(overlap), raw); err != nil {
		t.Fatal(err)
	}

	seen := map[string]int{}
	if err := m.EachID(func(id string) error {
		seen[id]++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(ids) {
		t.Errorf("EachID yielded %d distinct ids, want %d", len(seen), len(ids))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("EachID yielded %s %d times, want once", id, n)
		}
	}
}

// Unarchiving is only a true inverse if it puts the audit record back too.
func TestUnarchiveRestoresManifests(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const session = "6f2d1c4b-0000-4000-8000-00000000ma04"
	reqs, ress, ids := recordSession(t, st, session, 5)

	want := map[string][]byte{}
	m := newLocalManifests(p.manifests, p.archives)
	for _, id := range ids {
		raw, err := m.readRaw(id)
		if err != nil {
			t.Fatal(err)
		}
		want[id] = raw
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	archiveAt(t, p, nil)
	archiveAt(t, p, func(o *ArchiveOptions) { o.Unarchive = true })

	if got := countLooseManifests(t, p.manifests); got != len(ids) {
		t.Errorf("%d loose manifests after unarchiving, want %d", got, len(ids))
	}
	if _, err := os.Stat(mpackFile(p.archives, session)); !os.IsNotExist(err) {
		t.Errorf("the manifest pack survived unarchiving: %v", err)
	}
	for _, id := range ids {
		got, err := os.ReadFile(newLocalManifests(p.manifests, "").path(id))
		if err != nil {
			t.Fatalf("restored manifest %s: %v", id, err)
		}
		if !bytes.Equal(got, want[id]) {
			t.Errorf("manifest %s came back different", id)
		}
	}
	assertBodies(t, p, ids, reqs, ress)
}

// A resumed session is rebuilt, not appended to. The rebuild must carry forward
// the manifests an earlier pass already packed rather than dropping them.
func TestArchiveRebuildKeepsPackedManifests(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const session = "6f2d1c4b-0000-4000-8000-00000000ma05"
	_, _, first := recordSession(t, st, session, 3)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	archiveAt(t, p, nil)

	// Resume: more turns arrive after the session was archived.
	st2 := openStoreAt(t, p)
	_, _, second := recordSession(t, st2, session, 2)
	if err := st2.Close(); err != nil {
		t.Fatal(err)
	}
	archiveAt(t, p, nil)

	pk := packOf(t, p, session)
	for _, id := range append(append([]string{}, first...), second...) {
		if !pk.Has(id) {
			t.Errorf("manifest %s was dropped by the rebuild", id)
		}
	}
	if got := countLooseManifests(t, p.manifests); got != 0 {
		t.Errorf("%d loose manifests left after the rebuild, want 0", got)
	}
}

// verifyMpack must refuse a replacement that would lose a manifest the installed
// pack holds, which is what makes an overwrite provably non-destructive.
func TestVerifyRefusesDroppingAPackedManifest(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const session = "6f2d1c4b-0000-4000-8000-00000000ma06"
	_, _, ids := recordSession(t, st, session, 4)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	archiveAt(t, p, nil)

	m := newLocalManifests(p.manifests, p.archives)
	// A candidate built from every manifest but one.
	var mans []sessionMan
	for _, id := range ids[1:] {
		raw, err := m.readRaw(id)
		if err != nil {
			t.Fatal(err)
		}
		mans = append(mans, sessionMan{id: id, raw: raw})
	}
	o := ArchiveOptions{ArchivesDir: p.archives, ManifestsDir: p.manifests}
	final := mpackFile(p.archives, session)
	tmp, _, err := o.buildMpack(session, mans, final)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp)

	err = o.verifyMpack(mans, tmp, final)
	if err == nil {
		t.Fatal("verify accepted a pack that drops a manifest")
	}
	if !strings.Contains(err.Error(), ids[0]) {
		t.Errorf("error should name the dropped manifest %s: %v", ids[0], err)
	}
}

// Read reprices on the way out; readRaw must hand back exactly what is stored, or
// migrate-usage's stored-vs-stored idempotence check would see every manifest as
// changed.
func TestReadRepricesPackedManifestButReadRawDoesNot(t *testing.T) {
	p := tmpPaths(t)
	st := openStoreAt(t, p)
	const session = "6f2d1c4b-0000-4000-8000-00000000ma07"
	_, _, ids := recordSession(t, st, session, 2)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	archiveAt(t, p, nil)

	m := newLocalManifests(p.manifests, p.archives)
	raw, err := m.readRaw(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	var stored model.Exchange
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Res.Cost != 0 {
		t.Errorf("stored manifest carries a cost of %v; it must be derived on read", stored.Res.Cost)
	}
	ex, err := m.Read(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if ex.Res.Cost == 0 {
		t.Error("Read did not reprice a packed manifest")
	}
}

// A missing manifest must report as missing rather than as corruption, the same
// way a missing blob does.
func TestReadMissingManifestReportsNotExist(t *testing.T) {
	p := tmpPaths(t)
	m := newLocalManifests(p.manifests, p.archives)
	if _, err := m.Read("00000000-0000-7000-8000-00000000ffff"); !os.IsNotExist(err) {
		t.Errorf("err = %v, want a not-exist error", err)
	}
}

// fsck exists because stored bytes can be wrong, so no header field may be
// believed far enough to allocate on it. Mirrors TestArchiveRejectsImpossibleHeader.
func TestMpackRejectsImpossibleHeader(t *testing.T) {
	b, err := newMpackBuilder("6f2d1c4b-0000-4000-8000-00000000ma08")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Add("00000000-0000-7000-8000-0000000000a1", []byte(`{"id":"a1"}`)); err != nil {
		t.Fatal(err)
	}
	good := b.Bytes()

	cases := []struct {
		name   string
		off    int
		val    uint32
		size16 bool
	}{
		{name: "manifest count", off: 8, val: 0xffffffff},
		{name: "directory length", off: 12, val: 0xffffffff},
		{name: "payload length", off: 16, val: 0xffffffff},
		{name: "raw length", off: 20, val: 0xffffffff},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := append([]byte{}, good...)
			binary.LittleEndian.PutUint32(raw[c.off:], c.val)
			path := filepath.Join(t.TempDir(), "bad"+mpackExt)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			p, err := openMpack(path)
			if err == nil {
				p.Close()
				t.Fatalf("openMpack accepted a %s of %d", c.name, c.val)
			}
		})
	}
}

// Damage inside a pack must be reported, not served.
func TestMpackDetectsCorruption(t *testing.T) {
	b, err := newMpackBuilder("6f2d1c4b-0000-4000-8000-00000000ma09")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	const id = "00000000-0000-7000-8000-0000000000b1"
	body := []byte(strings.Repeat(`{"id":"b1","state":"complete"}`, 40))
	if err := b.Add(id, body); err != nil {
		t.Fatal(err)
	}
	raw := b.Bytes()
	// The payload is the last section before the session trailer.
	raw[len(raw)-len("6f2d1c4b-0000-4000-8000-00000000ma09")-4] ^= 0xff

	path := filepath.Join(t.TempDir(), "damaged"+mpackExt)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := openMpack(path)
	if err != nil {
		return // rejected at open, which is also a correct answer
	}
	defer p.Close()
	if _, err := p.Verify(); err == nil {
		t.Error("Verify passed a corrupted pack")
	}
}

// A pack records whose session it holds, so a misfiled one is detectable and
// nameable rather than merely unreadable.
func TestMpackRecordsItsSession(t *testing.T) {
	const session = "6f2d1c4b-0000-4000-8000-00000000ma10"
	b, err := newMpackBuilder(session)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Add("00000000-0000-7000-8000-0000000000c1", []byte(`{"id":"c1"}`)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := mpackFile(dir, session)
	if err := writeFileSync(path, b.Bytes()); err != nil {
		t.Fatal(err)
	}
	p, err := openMpack(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.Session() != session {
		t.Errorf("Session() = %q, want %q", p.Session(), session)
	}
}

// An empty pack is never written: a session with no manifests has nothing to pack,
// and installing an empty file would leave one that reads resolve to but no
// manifest lives in.
func TestNoPackForNoManifests(t *testing.T) {
	p := tmpPaths(t)
	o := ArchiveOptions{ArchivesDir: p.archives, ManifestsDir: p.manifests}
	tmp, size, err := o.buildMpack("6f2d1c4b-0000-4000-8000-00000000ma11", nil, mpackFile(p.archives, "x"))
	if err != nil {
		t.Fatal(err)
	}
	if tmp != "" || size != 0 {
		t.Errorf("buildMpack wrote %q (%d bytes) for no manifests", tmp, size)
	}
}
