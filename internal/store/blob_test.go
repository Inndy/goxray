package store

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func tmpBlobs(t *testing.T) (*localBlobs, string) {
	t.Helper()
	d := t.TempDir()
	b, err := newLocalBlobs(filepath.Join(d, "blobs"), filepath.Join(d, "archives"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b, d
}

// archiveInto folds refs out of the live blob dir into a session archive and
// removes the .zst files, the way the compaction pass does.
func archiveInto(t *testing.T, b *localBlobs, session string, refs []string, bodies [][]byte) {
	t.Helper()
	ab, err := newArchiveBuilder(session)
	if err != nil {
		t.Fatal(err)
	}
	defer ab.Close()
	for i, ref := range refs {
		if err := ab.Add(ref, bodies[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeFileSync(archiveFile(b.archives, session), ab.Bytes()); err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if err := os.Remove(shardPath(b.dir, ref, ".zst")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

func TestGetResolvesThroughArchive(t *testing.T) {
	b, _ := tmpBlobs(t)
	const session = "af5f1ea9-f08a-4d0b-8709-384a1ee6f0ef"

	bodies := conversation(12)
	refs := make([]string, len(bodies))
	for i, body := range bodies {
		ref, err := b.Put(body)
		if err != nil {
			t.Fatal(err)
		}
		refs[i] = ref
	}

	// Before archiving: served from the live files.
	for i, ref := range refs {
		got, err := b.Get(session, ref)
		if err != nil || !bytes.Equal(got, bodies[i]) {
			t.Fatalf("live read of %s: err=%v", ref, err)
		}
	}

	archiveInto(t, b, session, refs, bodies)

	for i, ref := range refs {
		got, err := b.Get(session, ref)
		if err != nil {
			t.Fatalf("archived read of %s: %v", ref, err)
		}
		if !bytes.Equal(got, bodies[i]) {
			t.Fatalf("archived read of %s returned %d bytes, want %d", ref, len(got), len(bodies[i]))
		}
	}
}

// A body that is in neither place must report "not recorded", not "no archive".
func TestGetMissingBlobReportsNotExist(t *testing.T) {
	b, _ := tmpBlobs(t)
	for _, session := range []string{"", "no-such-session"} {
		_, err := b.Get(session, "no-such-ref")
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("session %q: err = %v, want fs.ErrNotExist", session, err)
		}
	}
}

// Sessionless turns are never archived, so an empty session must not send the
// reader looking for an archive named after nothing.
func TestGetEmptySessionStaysLive(t *testing.T) {
	b, _ := tmpBlobs(t)
	want := []byte(`{"noise":true}`)
	ref, err := b.Put(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Get("", ref)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Get with empty session: %v", err)
	}
}

// A resumed session is re-archived by renaming a new file over the old path. A
// cached reader must notice rather than keep serving the previous inode, which
// would make the newly added turns look unrecorded.
func TestGetSeesRebuiltArchive(t *testing.T) {
	b, _ := tmpBlobs(t)
	const session = "7e618a48-2038-4369-aae3-af10a6e65742"

	bodies := conversation(10)
	refs := make([]string, len(bodies))
	for i, body := range bodies {
		ref, err := b.Put(body)
		if err != nil {
			t.Fatal(err)
		}
		refs[i] = ref
	}

	// Archive only the first half, then read one back so the reader is cached.
	archiveInto(t, b, session, refs[:5], bodies[:5])
	if _, err := b.Get(session, refs[0]); err != nil {
		t.Fatalf("priming read: %v", err)
	}
	if len(b.cache) != 1 {
		t.Fatalf("expected the archive to be cached, got %d entries", len(b.cache))
	}

	// Resume: rebuild the archive over the same path with every turn.
	archiveInto(t, b, session, refs, bodies)

	for i, ref := range refs {
		got, err := b.Get(session, ref)
		if err != nil {
			t.Fatalf("read %s after rebuild: %v", ref, err)
		}
		if !bytes.Equal(got, bodies[i]) {
			t.Fatalf("ref %s mismatch after rebuild", ref)
		}
	}
}

// Reading more sessions than the cache holds must not leak file handles or
// start returning the wrong archive's contents.
func TestGetCacheEvicts(t *testing.T) {
	b, _ := tmpBlobs(t)
	type sess struct {
		id     string
		refs   []string
		bodies [][]byte
	}
	var sessions []sess
	for s := 0; s < archiveCacheMax+2; s++ {
		bodies := conversation(4)
		refs := make([]string, len(bodies))
		for i, body := range bodies {
			ref, err := b.Put(body)
			if err != nil {
				t.Fatal(err)
			}
			refs[i] = ref
		}
		id := string(rune('a'+s)) + "0000000-0000-0000-0000-00000000000" + string(rune('0'+s))
		archiveInto(t, b, id, refs, bodies)
		sessions = append(sessions, sess{id, refs, bodies})
	}
	// Two passes, so the second one runs entirely against an evicted cache.
	for pass := 0; pass < 2; pass++ {
		for _, s := range sessions {
			for i, ref := range s.refs {
				got, err := b.Get(s.id, ref)
				if err != nil {
					t.Fatalf("pass %d: %s/%s: %v", pass, s.id, ref, err)
				}
				if !bytes.Equal(got, s.bodies[i]) {
					t.Fatalf("pass %d: %s/%s returned the wrong body", pass, s.id, ref)
				}
			}
		}
	}
	if len(b.cache) > archiveCacheMax {
		t.Errorf("cache holds %d archives, max is %d", len(b.cache), archiveCacheMax)
	}
}

// A damaged archive must surface as an error, never as "body was not recorded" —
// the dashboard renders that as a benign warning, which would hide real loss.
func TestGetCorruptArchiveIsNotMistakenForMissing(t *testing.T) {
	b, _ := tmpBlobs(t)
	const session = "55e1282c-b3d8-4be3-a5e7-f606b704f921"
	bodies := conversation(6)
	refs := make([]string, len(bodies))
	for i, body := range bodies {
		ref, _ := b.Put(body)
		refs[i] = ref
	}
	archiveInto(t, b, session, refs, bodies)

	p := archiveFile(b.archives, session)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	copy(raw[:4], "XXXX") // wreck the magic
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = b.Get(session, refs[0])
	if err == nil {
		t.Fatal("read a corrupt archive without error")
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("corrupt archive reported as missing blob: %v", err)
	}
}
