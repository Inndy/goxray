package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/wire"
)

// splitSSE carries the cache-creation TTL breakdown that older captures dropped:
// 1000 write tokens, only 200 of them at the cheap 5m rate.
const splitSSE = `event: message_start
data: {"type":"message_start","message":{"model":"claude-opus-5-20260115","usage":{"input_tokens":10,"cache_creation_input_tokens":1000,"cache_read_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":200,"ephemeral_1h_input_tokens":800},"service_tier":"standard"}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7,"output_tokens_details":{"thinking_tokens":3}}}

event: message_stop
data: {"type":"message_stop"}

`

func TestMigrateUsageBackfillsTTLSplit(t *testing.T) {
	d := t.TempDir()
	blobsDir := filepath.Join(d, "blobs")
	manDir := filepath.Join(d, "manifests")
	arcDir := filepath.Join(d, "archives")

	blobs, err := newLocalBlobs(blobsDir, arcDir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := blobs.Put([]byte(splitSSE))
	if err != nil {
		t.Fatal(err)
	}

	man := newLocalManifests(manDir, arcDir)
	id := "00000000-0000-7000-8000-00000000dd02"
	// A manifest as an older goxray would have written it: flat cache total only.
	ex := &model.Exchange{
		ID: id, State: model.StateComplete, ResRef: ref,
		ResHeaders: map[string]string{"content-type": "text/event-stream"},
		Req:        model.ReqMeta{Model: "claude-opus-5"},
		Res: model.ResMeta{Usage: model.Usage{
			InputTokens: 10, OutputTokens: 7, CacheReadTokens: 5, CacheCreateTokens: 1000,
		}},
	}
	if err := man.Write(ex); err != nil {
		t.Fatal(err)
	}

	// Freshly written manifests are inside the quiesce window, where the recorder
	// may still be about to supersede them.
	st, err := MigrateUsage(blobsDir, manDir, arcDir, wire.Anthropic{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Updated != 0 || st.Skipped != 1 {
		t.Errorf("recent manifest should be skipped, got updated=%d skipped=%d", st.Updated, st.Skipped)
	}

	backdate(t, man.path(id))

	st, err = MigrateUsage(blobsDir, manDir, arcDir, wire.Anthropic{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Updated != 1 {
		t.Fatalf("dry run found %d to update, want 1", st.Updated)
	}
	if st.CostTo <= st.CostFrom {
		t.Errorf("split pricing $%v should exceed flat-rate $%v", st.CostTo, st.CostFrom)
	}
	if after, _ := man.Read(id); after.Res.Usage.CacheCreate1h != 0 {
		t.Error("dry run wrote to disk")
	}

	if _, err := MigrateUsage(blobsDir, manDir, arcDir, wire.Anthropic{}, true); err != nil {
		t.Fatal(err)
	}
	got, err := man.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Res.Usage.CacheCreate5m != 200 || got.Res.Usage.CacheCreate1h != 800 {
		t.Errorf("split = %d/%d, want 200/800", got.Res.Usage.CacheCreate5m, got.Res.Usage.CacheCreate1h)
	}
	if got.Res.Usage.ThinkingTokens != 3 {
		t.Errorf("thinking tokens = %d, want 3", got.Res.Usage.ThinkingTokens)
	}
	if got.Res.Model != "claude-opus-5-20260115" {
		t.Errorf("resolved model = %q", got.Res.Model)
	}
	if got.Res.ServiceTier != "standard" {
		t.Errorf("service tier = %q", got.Res.ServiceTier)
	}

	// Re-running must find nothing left to do.
	backdate(t, man.path(id))
	again, err := MigrateUsage(blobsDir, manDir, arcDir, wire.Anthropic{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.Updated != 0 {
		t.Errorf("second pass updated %d records, want 0 (not idempotent)", again.Updated)
	}
}

// The window is recorded per turn now, but no older capture has it, and it is
// only in the request body — so the backfill has to read the request side, which
// the usage pass never touched. A turn whose response was never recorded still
// gets one: it is the case where a listing row is most in need of a denominator.
func TestMigrateUsageBackfillsCtxWindow(t *testing.T) {
	d := t.TempDir()
	blobsDir := filepath.Join(d, "blobs")
	manDir := filepath.Join(d, "manifests")
	arcDir := filepath.Join(d, "archives")

	blobs, err := newLocalBlobs(blobsDir, arcDir)
	if err != nil {
		t.Fatal(err)
	}
	// A bare model id, with the exact one named only inside the system prompt.
	reqRef, err := blobs.Put([]byte(`{"model":"claude-opus-5","system":[{"type":"text","text":"The exact model ID is claude-opus-5[1m]."}],"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}

	man := newLocalManifests(manDir, arcDir)
	id := "00000000-0000-7000-8000-00000000dd03"
	ex := &model.Exchange{
		ID: id, State: model.StatePersistFailed, ReqRef: reqRef,
		Req: model.ReqMeta{Model: "claude-opus-5"},
	}
	if err := man.Write(ex); err != nil {
		t.Fatal(err)
	}
	backdate(t, man.path(id))

	st, err := MigrateUsage(blobsDir, manDir, arcDir, wire.Anthropic{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Windows != 1 || st.Updated != 1 {
		t.Fatalf("windows=%d updated=%d, want 1/1", st.Windows, st.Updated)
	}
	got, err := man.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Req.CtxWindow != 1_000_000 {
		t.Errorf("ctx window = %d, want 1000000", got.Req.CtxWindow)
	}

	backdate(t, man.path(id))
	again, err := MigrateUsage(blobsDir, manDir, arcDir, wire.Anthropic{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.Updated != 0 || again.Windows != 0 {
		t.Errorf("second pass updated=%d windows=%d, want 0/0", again.Updated, again.Windows)
	}
}

// backdate pushes a file's mtime outside the quiesce window.
func backdate(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-2 * migrateQuiesce)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}
