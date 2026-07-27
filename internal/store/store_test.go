package store

import (
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"go.inndy.tw/goxray/internal/model"
)

const smallSSE = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":5}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

func tmpStore(t *testing.T) *Store {
	t.Helper()
	d := t.TempDir()
	st, err := Open(filepath.Join(d, "blobs"), filepath.Join(d, "manifests"),
		filepath.Join(d, "index.db"), model.ProviderAnthropic, 2)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

func TestBlobRoundTrip(t *testing.T) {
	d := t.TempDir()
	b, err := newLocalBlobs(filepath.Join(d, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("verbatim \x00 bytes with 中文 and \n newlines")
	ref, err := b.Put(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("round trip mismatch: %q != %q", got, want)
	}
}

func TestRecorderFlow(t *testing.T) {
	st := tmpStore(t)
	rec := st.Recorder

	reqBody := []byte(`{"model":"claude-opus-4-8","metadata":{"session_id":"sess-1"},` +
		`"system":[{"type":"text","text":"Primary working directory: /w\n"}],"messages":[{"role":"user","content":"hi"}]}`)

	ex := rec.NewExchange("POST", "/v1/messages", http.Header{"X-Api-Key": {"sk-secret"}})
	if err := rec.PersistRequest(ex, reqBody); err != nil {
		t.Fatalf("persist req: %v", err)
	}
	if ex.State != model.StateInflight {
		t.Errorf("state after req = %q, want inflight", ex.State)
	}

	ex.Status = 200
	ex.ResHeaders = map[string]string{
		"content-type":              "text/event-stream",
		"request-id":                "req_9",
		"anthropic-organization-id": "org-abc-123",
	}
	if err := rec.PersistResponse(ex, []byte(smallSSE)); err != nil {
		t.Fatalf("persist res: %v", err)
	}
	// Org identity is preserved verbatim in the response headers (no separate
	// account field/layer).
	if m := ex.ResHeaders["anthropic-organization-id"]; m != "org-abc-123" {
		t.Errorf("org id not retained in res headers: %q", m)
	}
	if err := st.Close(); err != nil { // drains async index
		t.Fatal(err)
	}

	rd, err := OpenReader(st.Blobs.(*localBlobs).dir, st.Manifests.(*localManifests).dir, indexOf(t, st))
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()

	// Manifest round-trips full metadata.
	m, err := rd.Manifest(ex.ID)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if m.State != model.StateComplete {
		t.Errorf("manifest state = %q", m.State)
	}
	if m.Req.Model != "claude-opus-4-8" || m.Req.Cwd != "/w" || m.Req.SessionID != "sess-1" {
		t.Errorf("req meta = %+v", m.Req)
	}
	if m.Res.Usage.OutputTokens != 7 || m.Res.StopReason != "end_turn" {
		t.Errorf("res meta = %+v", m.Res)
	}
	if m.RequestID() != "req_9" {
		t.Errorf("request-id = %q", m.RequestID())
	}
	if m.Res.Cost <= 0 {
		t.Errorf("expected nonzero cost, got %v", m.Res.Cost)
	}

	// Raw response blob is byte-exact.
	raw, err := rd.Blob(m.ResRef)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != smallSSE {
		t.Errorf("raw res not byte-exact")
	}

	// Index projection has the row.
	rows, err := rd.List(Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != ex.ID {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
}

// A cost stamped at record time goes stale as soon as the rate table moves, so
// reads must recompute it rather than trust what is on disk.
// A failed recovery (or any lost upsert) must not be sealed by a clean-shutdown
// mark, or the projection gap becomes permanent: the next start would skip
// reconcile and the missing rows would never reappear.
func TestDegradedIndexStaysUncleanUntilRecoverySucceeds(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "index.db")

	idx, _, _, err := OpenIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	idx.degraded.Store(true)
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	idx, _, wasClean, err := OpenIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if wasClean {
		t.Error("degraded close reported clean shutdown; next start would skip reconcile")
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	idx, _, wasClean, err = OpenIndex(path)
	if err != nil || !wasClean {
		t.Errorf("healthy close should mark clean (wasClean=%v, err=%v)", wasClean, err)
	}
	idx.Close()
}

type flakyBlobs struct {
	BlobStore
	failAfter int
	n         int
}

func (f *flakyBlobs) Put(b []byte) (string, error) {
	f.n++
	if f.n > f.failAfter {
		return "", errors.New("disk full")
	}
	return f.BlobStore.Put(b)
}

// When the response blob write fails, the manifests may still be writable: the
// record must land as terminal persist_failed, not stay inflight forever and
// masquerade as a crash-interrupted turn.
func TestBlobFailureRecordsPersistFailed(t *testing.T) {
	d := t.TempDir()
	blobs, err := newLocalBlobs(filepath.Join(d, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	manifests := newLocalManifests(filepath.Join(d, "manifests"))
	idx, _, _, err := OpenIndex(filepath.Join(d, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	rec := NewRecorder(&flakyBlobs{BlobStore: blobs, failAfter: 1}, manifests, idx, model.ProviderAnthropic, 1)

	ex := rec.NewExchange("POST", "/v1/messages", nil)
	if err := rec.PersistRequest(ex, []byte(`{"model":"x"}`)); err != nil {
		t.Fatal(err)
	}
	ex.State = model.StateComplete
	if err := rec.PersistResponse(ex, []byte(smallSSE)); err == nil {
		t.Fatal("PersistResponse should fail when the blob store fails")
	}
	rec.Close()
	idx.Close()

	got, err := manifests.Read(ex.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.StatePersistFailed {
		t.Errorf("manifest state = %q, want persist_failed", got.State)
	}
	if got.ResRef != "" {
		t.Errorf("res_ref = %q, want empty (blob was never stored)", got.ResRef)
	}
}

// A handler that outlives the shutdown deadline can reach a closed store; the
// late writes must degrade to errors/no-ops, never a send-on-closed-channel
// panic (an overflow-goroutine panic would take the whole process down).
func TestStoreWritesAfterCloseNoPanic(t *testing.T) {
	st := tmpStore(t)
	rec := st.Recorder
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	if err := st.Index.Upsert(model.Exchange{ID: "late"}); err == nil {
		t.Error("Upsert after Close: want error, got nil")
	}
	rec.enqueueIndex(model.Exchange{ID: "late"}) // must not panic
	rec.Close()                                  // second Close must be a no-op
}

func TestManifestReadRepricesStoredCost(t *testing.T) {
	man := newLocalManifests(filepath.Join(t.TempDir(), "manifests"))

	ex := &model.Exchange{
		ID: "00000000-0000-7000-8000-00000000dd01", State: model.StateComplete,
		Req: model.ReqMeta{Model: "claude-opus-5"},
		Res: model.ResMeta{
			Usage: model.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
			Cost:  0, // as if the model had no price on file when it was recorded
		},
	}
	if err := man.Write(ex); err != nil {
		t.Fatal(err)
	}

	got, err := man.Read(ex.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := 30.0; got.Res.Cost != want {
		t.Errorf("cost = %v, want %v recomputed from tokens", got.Res.Cost, want)
	}

	// A wrong figure already on disk must lose to the derived one, not win.
	ex.Res.Cost = 999
	if err := man.Write(ex); err != nil {
		t.Fatal(err)
	}
	got, err = man.Read(ex.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Res.Cost != 30.0 {
		t.Errorf("stale stored cost survived read: %v", got.Res.Cost)
	}
}

func TestRebuildFromManifests(t *testing.T) {
	d := t.TempDir()
	blobsDir := filepath.Join(d, "blobs")
	manDir := filepath.Join(d, "manifests")
	idxPath := filepath.Join(d, "index.db")

	man := newLocalManifests(manDir)
	// Two manifests written directly (as if by a prior run).
	for _, id := range []string{"00000000-0000-7000-8000-0000000000aa", "00000000-0000-7000-8000-0000000000bb"} {
		ex := &model.Exchange{ID: id, Provider: model.ProviderAnthropic, State: model.StateComplete,
			Req: model.ReqMeta{Model: "claude-opus-4-8", SessionID: "s"}}
		if err := man.Write(ex); err != nil {
			t.Fatal(err)
		}
	}

	idx, needRebuild, _, err := OpenIndex(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if !needRebuild {
		t.Errorf("fresh index should need rebuild")
	}
	if err := idx.RebuildFrom(man); err != nil {
		t.Fatal(err)
	}
	rows, err := idx.Query(Filter{Limit: 10, IncludeNoise: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Errorf("rebuild produced %d rows, want 2", len(rows))
	}
	_ = idx.Close()
	_ = blobsDir
}

func TestReconcileStragglers(t *testing.T) {
	d := t.TempDir()
	man := newLocalManifests(filepath.Join(d, "manifests"))
	idx, _, _, err := OpenIndex(filepath.Join(d, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// A committed manifest whose async index upsert was "lost" (never Upserted).
	ex := &model.Exchange{ID: "00000000-0000-7000-8000-0000000000cc",
		Provider: model.ProviderAnthropic, State: model.StateComplete}
	if err := man.Write(ex); err != nil {
		t.Fatal(err)
	}

	if rows, _ := idx.Query(Filter{Limit: 10, IncludeNoise: true}); len(rows) != 0 {
		t.Fatalf("expected empty index, got %d", len(rows))
	}
	if err := idx.Reconcile(man); err != nil {
		t.Fatal(err)
	}
	rows, _ := idx.Query(Filter{Limit: 10, IncludeNoise: true})
	if len(rows) != 1 {
		t.Errorf("reconcile recovered %d rows, want 1", len(rows))
	}
}

// indexOf returns the index db path for a Store built by tmpStore.
func indexOf(t *testing.T, st *Store) string {
	t.Helper()
	// blobs dir is <home>/blobs; index is a sibling.
	return filepath.Join(filepath.Dir(st.Blobs.(*localBlobs).dir), "index.db")
}
