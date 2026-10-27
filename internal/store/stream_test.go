package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/wire"
)

func wsItem(text string) string {
	return `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}`
}

func wsCreate(prev string, items ...string) []byte {
	p := ""
	if prev != "" {
		p = `"previous_response_id":"` + prev + `",`
	}
	return []byte(`{"type":"response.create","model":"gpt-6.1-sol",` + p +
		`"client_metadata":{"session_id":"sess-1","thread_id":"thr-1"},"input":[` + strings.Join(items, ",") + `]}`)
}

func wsEvents(id, text string) [][]byte {
	return [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"` + id + `","status":"in_progress"}}`),
		[]byte(`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}}`),
		[]byte(`{"type":"response.completed","response":{"id":"` + id + `","model":"gpt-6.1-sol","status":"completed",` +
			`"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":60},"output_tokens":5}}}`),
	}
}

func openConn(t *testing.T, st *Store) (*model.Exchange, *StreamLog) {
	t.Helper()
	conn := st.Recorder.NewExchange("GET", "/backend-api/codex/responses", nil)
	conn.Provider = model.ProviderOpenAI
	conn.Transport = model.TransportWS
	conn.Kind = model.KindWSConn
	conn.ReqHeaders = map[string]string{"session-id": "sess-1"}
	sl, err := st.Recorder.OpenStream(conn)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if err := sl.Upgraded(101, nil); err != nil {
		t.Fatalf("upgraded: %v", err)
	}
	return conn, sl
}

// turn drives one exchange through the log the way the proxy does: the create
// is made durable, the events are recorded, and the held one is committed.
func turn(t *testing.T, sl *StreamLog, create []byte, events [][]byte) {
	t.Helper()
	if err := sl.Client(create, false); err != nil {
		t.Fatalf("client: %v", err)
	}
	for _, e := range events {
		if sl.Server(e, false) {
			if err := sl.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}
	}
}

func wsTurns(t *testing.T, st *Store, conn string) []*model.Exchange {
	t.Helper()
	var out []*model.Exchange
	err := st.Manifests.EachID(func(id string) error {
		ex, err := st.Manifests.Read(id)
		if err == nil && ex.WS != nil && ex.WS.Conn == conn {
			out = append(out, ex)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStreamRecordsTurnsAndSeals(t *testing.T) {
	st := tmpStore(t)
	defer st.Close()
	conn, sl := openConn(t, st)
	turn(t, sl, wsCreate("", wsItem("hi")), wsEvents("r1", "hello"))
	turn(t, sl, wsCreate("r1", wsItem("again")), wsEvents("r2", "ok"))
	if err := sl.End("clean", model.StateComplete, model.StateClientAborted); err != nil {
		t.Fatalf("end: %v", err)
	}

	lb := st.Blobs.(*localBlobs)
	if _, err := os.Stat(livePath(lb.dir, conn.ID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("live log still present after a clean end: %v", err)
	}
	c, err := st.Manifests.Read(conn.ID)
	if err != nil || c.State != model.StateComplete || c.ResRef != conn.ID || c.Status != 101 || !c.IsNoise {
		t.Fatalf("connection manifest = %+v, %v", c, err)
	}
	turns := wsTurns(t, st, conn.ID)
	if len(turns) != 2 {
		t.Fatalf("got %d turn manifests, want 2", len(turns))
	}
	log, err := st.Blobs.Get("sess-1", conn.ID)
	if err != nil {
		t.Fatalf("sealed log unreadable: %v", err)
	}
	for _, ex := range turns {
		if ex.State != model.StateComplete || ex.Req.SessionID != "sess-1" || ex.Res.Usage.CacheReadTokens != 60 ||
			ex.Res.Usage.InputTokens != 40 || ex.Req.Model != "gpt-6.1-sol" || ex.ReqRef != "" || ex.ResRef != "" {
			t.Errorf("turn %s = %+v", ex.ID, ex)
		}
		rt := wire.ReplayTurn(log, ex.WS.First, 0)
		if rt == nil || rt.ID != ex.ID {
			t.Fatalf("turn %s does not start at its own record", ex.ID)
		}
	}
	second := turns[0]
	if second.Res.ResponseID != "r2" {
		second = turns[1]
	}
	if second.Req.MsgCount != 3 || second.Req.PrevResponseID != "r1" {
		t.Errorf("the continuation should be parsed from its full input: %+v", second.Req)
	}

	fs, err := Fsck(FsckOptions{BlobsDir: lb.dir, ManifestsDir: filepath.Dir(lb.dir) + "/manifests", ArchivesDir: lb.archives})
	if err != nil || len(fs.Problems) > 0 || len(fs.Missing) > 0 {
		t.Fatalf("fsck: %v %v %v", err, fs.Problems, fs.Missing)
	}
}

func TestStreamRecoveryFinishesAnInterruptedClose(t *testing.T) {
	st := tmpStore(t)
	conn, sl := openConn(t, st)
	turn(t, sl, wsCreate("", wsItem("hi")), wsEvents("r1", "hello"))
	// The second turn's create is durable and has started streaming when the
	// process dies.
	if err := sl.Client(wsCreate("r1", wsItem("again")), false); err != nil {
		t.Fatal(err)
	}
	sl.Server([]byte(`{"type":"response.created","response":{"id":"r2"}}`), false)
	sl.w.close() // the process is gone: no end record, no seal

	// Its manifest never made it either.
	var lost string
	for _, ex := range wsTurns(t, st, conn.ID) {
		if ex.State == model.StateInflight {
			lost = ex.ID
			_ = os.Remove(st.Manifests.(*localManifests).path(ex.ID))
		}
	}
	if lost == "" {
		t.Fatal("expected an inflight turn")
	}

	lb := st.Blobs.(*localBlobs)
	if p := RecoverStreams(lb.dir, st.Manifests, st.Index); len(p) > 0 {
		t.Fatalf("recovery problems: %v", p)
	}
	if _, err := os.Stat(livePath(lb.dir, conn.ID)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("recovery left the live log")
	}
	c, _ := st.Manifests.Read(conn.ID)
	if c.State != model.StateUpstreamError {
		t.Errorf("connection state = %s", c.State)
	}
	ex, err := st.Manifests.Read(lost)
	if err != nil {
		t.Fatalf("the turn whose manifest was lost was not rebuilt: %v", err)
	}
	if ex.State != model.StateUpstreamError || ex.WS == nil || ex.Req.Model != "gpt-6.1-sol" {
		t.Errorf("rebuilt turn = %+v", ex)
	}
	// Idempotent: a second run has nothing to do.
	if p := RecoverStreams(lb.dir, st.Manifests, st.Index); len(p) > 0 {
		t.Fatalf("second recovery: %v", p)
	}
	st.Close()
}

func TestRecoveryLeavesAnOrphanLogAlone(t *testing.T) {
	st := tmpStore(t)
	defer st.Close()
	lb := st.Blobs.(*localBlobs)
	w, err := createLive(lb.dir, "0190aaaa-0000-7000-8000-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	_ = w.append(wire.AppendRecord(nil, wire.Record{N: 1, T: 1, D: "c"}, []byte(`{}`), false))
	w.close()
	p := RecoverStreams(lb.dir, st.Manifests, st.Index)
	if len(p) != 1 || !strings.Contains(p[0], "no connection manifest") {
		t.Fatalf("problems = %v", p)
	}
	if _, err := os.Stat(livePath(lb.dir, "0190aaaa-0000-7000-8000-000000000001")); err != nil {
		t.Fatal("an orphan log must be left in place")
	}
}

func TestLiveLogTornTailVersusCorruption(t *testing.T) {
	dir := t.TempDir()
	w, err := createLive(dir, "ref")
	if err != nil {
		t.Fatal(err)
	}
	_ = w.append(wire.AppendRecord(nil, wire.Record{N: 1, T: 1, D: "c"}, []byte(`{"a":1}`), false))
	_ = w.append(wire.AppendRecord(nil, wire.Record{N: 2, T: 1, D: "s"}, []byte(`{"b":2}`), false))
	w.close()
	path := livePath(dir, "ref")
	good, _ := os.ReadFile(path)

	// A final segment cut short is a crash mid-write: dropped, not an error.
	torn := append(append([]byte{}, good...), 50, 0, 0, 0, 1, 2, 3)
	_ = os.WriteFile(path, torn, 0o600)
	raw, err := readLive(path)
	if err != nil || strings.Count(string(raw), "\n") != 2 {
		t.Fatalf("torn tail: %q, %v", raw, err)
	}

	// A complete segment that does not decode is damage.
	bad := append([]byte{}, good...)
	bad[len(bad)-3] ^= 0xff
	_ = os.WriteFile(path, bad, 0o600)
	if _, err := readLive(path); !errors.Is(err, errLiveCorrupt) {
		t.Fatalf("interior damage read as %v", err)
	}

	// So is an absurd length.
	_ = os.WriteFile(path, append([]byte{0xff, 0xff, 0xff, 0xff}, good...), 0o600)
	if _, err := readLive(path); !errors.Is(err, errLiveCorrupt) {
		t.Fatalf("oversized length read as %v", err)
	}
}

func TestCheckRecordsCatchesGaps(t *testing.T) {
	var log []byte
	log = wire.AppendRecord(log, wire.Record{N: 1, T: 1, D: "c"}, []byte(`{}`), false)
	log = wire.AppendRecord(log, wire.Record{N: 3, T: 1, D: "s"}, []byte(`{}`), false)
	if err := checkRecords(log); !errors.Is(err, errLiveCorrupt) {
		t.Fatalf("gap read as %v", err)
	}
}

func TestStreamHardLimitRefusesAndRotates(t *testing.T) {
	soft, hard := logSoftLimit, logHardLimit
	defer func() { logSoftLimit, logHardLimit = soft, hard }()
	logSoftLimit, logHardLimit = 600, 2000

	st := tmpStore(t)
	defer st.Close()
	_, sl := openConn(t, st)
	turn(t, sl, wsCreate("", wsItem("hi")), wsEvents("r1", "hello"))
	if !sl.ShouldRotate() {
		t.Fatal("past the soft limit the connection should rotate")
	}
	if err := sl.Client(wsCreate("r1", wsItem("again")), false); !errors.Is(err, errRotating{}) {
		t.Fatalf("a new turn after rotation started: %v", err)
	}
	// A frame that would cross the hard limit is refused, and the next barrier
	// fails, so the proxy drops the connection.
	big := []byte(`{"type":"response.output_text.delta","delta":"` + strings.Repeat("x", 3000) + `"}`)
	if !sl.Server(big, false) {
		t.Fatal("an over-limit frame must be held")
	}
	if err := sl.Commit(); !errors.Is(err, errLogFull) {
		t.Fatalf("commit past the hard limit: %v", err)
	}
	// A full log can still be finished: the end record is exempt.
	if err := sl.End("persist_failed", model.StatePersistFailed, model.StatePersistFailed); err != nil {
		t.Fatalf("end of a full log: %v", err)
	}
	log, err := st.Blobs.Get("sess-1", sl.conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	var last wire.Record
	wire.EachRecord(log, func(r wire.Record) bool { last = r; return true })
	if last.Op != wire.OpEnd {
		t.Fatalf("last record = %+v, want the end record", last)
	}
}

// After a failed append the file's tail is unknown, so nothing is appended or
// sealed after it: the .gxl stays for recovery, which reads it strictly.
func TestStreamAppendFailureLeavesTheLiveLog(t *testing.T) {
	st := tmpStore(t)
	conn, sl := openConn(t, st)
	turn(t, sl, wsCreate("", wsItem("hi")), wsEvents("r1", "hello"))
	_ = sl.w.f.Close() // the next write fails
	if err := sl.Client(wsCreate("r1", wsItem("again")), false); err == nil {
		t.Fatal("a create was acknowledged though its segment was not written")
	}
	if err := sl.End("persist_failed", model.StatePersistFailed, model.StatePersistFailed); err == nil {
		t.Fatal("End sealed a log whose last append failed")
	}
	lb := st.Blobs.(*localBlobs)
	if _, err := os.Stat(livePath(lb.dir, conn.ID)); err != nil {
		t.Fatalf("the live log must stay for recovery: %v", err)
	}
	if p := RecoverStreams(lb.dir, st.Manifests, st.Index); len(p) > 0 {
		t.Fatalf("recovery: %v", p)
	}
	for _, ex := range wsTurns(t, st, conn.ID) {
		if ex.State != model.StateComplete {
			t.Errorf("turn %s = %s, want the one durable turn, complete", ex.ID, ex.State)
		}
	}
	st.Close()
}

// A repeated record — a retried segment — is corruption, and a log carrying
// one is never installed as a blob.
func TestSealRefusesARepeatedRecord(t *testing.T) {
	dir := t.TempDir()
	w, err := createLive(dir, "ref")
	if err != nil {
		t.Fatal(err)
	}
	seg := wire.AppendRecord(nil, wire.Record{N: 1, T: 1, D: "c"}, []byte(`{"a":1}`), false)
	_ = w.append(seg)
	_ = w.append(seg)
	w.close()
	if _, err := sealLive(dir, "ref"); !errors.Is(err, errLiveCorrupt) {
		t.Fatalf("seal of a repeated record: %v", err)
	}
	if _, err := os.Stat(shardPath(dir, "ref", ".zst")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a corrupt log was installed as a blob")
	}
}

// An interrupted turn is not over until its acknowledgement: the connection does
// not rotate before it, and the final manifest's range runs past the terminal
// event to the turn's real end.
func TestStreamInterruptAckExtendsTheTurn(t *testing.T) {
	soft := logSoftLimit
	defer func() { logSoftLimit = soft }()
	logSoftLimit = 1

	st := tmpStore(t)
	conn, sl := openConn(t, st)
	if err := sl.Client(wsCreate("", wsItem("hi")), false); err != nil {
		t.Fatal(err)
	}
	sl.Server([]byte(`{"type":"response.created","response":{"id":"r1"}}`), false)
	if err := sl.Client([]byte(`{"type":"response.interrupt","response_id":"r1"}`), false); err != nil {
		t.Fatal(err)
	}
	if !sl.Server([]byte(`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete"}}`), false) {
		t.Fatal("the terminal event must be held")
	}
	if err := sl.Commit(); err != nil {
		t.Fatal(err)
	}
	if sl.ShouldRotate() {
		t.Fatal("rotated while Codex still waits for the interrupt acknowledgement")
	}
	if !sl.Server([]byte(`{"type":"response.interrupt.accepted","response_id":"r1"}`), false) {
		t.Fatal("the acknowledgement must be held")
	}
	if err := sl.Commit(); err != nil {
		t.Fatal(err)
	}
	if !sl.ShouldRotate() {
		t.Fatal("acknowledged: the connection is between turns and should rotate")
	}
	if err := sl.End("rotated", model.StateComplete, model.StateUpstreamError); err != nil {
		t.Fatal(err)
	}
	turns := wsTurns(t, st, conn.ID)
	if len(turns) != 1 {
		t.Fatalf("got %d turns", len(turns))
	}
	log, _ := st.Blobs.Get("sess-1", conn.ID)
	rt := wire.ReplayTurn(log, turns[0].WS.First, 0)
	if turns[0].WS.Last != rt.Last || rt.Last <= rt.TerminalN {
		t.Fatalf("range ends at %d; the turn runs %d..%d, terminal at %d", turns[0].WS.Last, rt.First, rt.Last, rt.TerminalN)
	}
	lb := st.Blobs.(*localBlobs)
	st.Close()
	fs, _ := Fsck(FsckOptions{BlobsDir: lb.dir, ManifestsDir: filepath.Join(filepath.Dir(lb.dir), "manifests"), ArchivesDir: lb.archives})
	if len(fs.Problems) > 0 {
		t.Fatalf("fsck: %v", fs.Problems)
	}
}

// A turn's range runs to the connection's close, but the turn ended with its
// response: a socket left open while the user reads must not count as time the
// model spent, nor as the agent being active.
func TestStreamTurnEndsAtItsTerminalEvent(t *testing.T) {
	st := tmpStore(t)
	defer st.Close()
	conn, sl := openConn(t, st)
	turn(t, sl, wsCreate("", wsItem("hi")), wsEvents("r1", "hello"))
	answered := time.Now()
	time.Sleep(300 * time.Millisecond)
	if err := sl.End("client_gone", model.StateComplete, model.StateClientAborted); err != nil {
		t.Fatal(err)
	}
	turns := wsTurns(t, st, conn.ID)
	if len(turns) != 1 {
		t.Fatalf("got %d turns", len(turns))
	}
	if end := turns[0].EndedAt; end.After(answered.Add(100 * time.Millisecond)) {
		t.Errorf("turn ended %v after its response, at the connection's close", end.Sub(answered))
	}
}

// A connection that closes cleanly around a response still streaming did not
// complete that response.
func TestStreamCleanCloseDoesNotCompleteAnOpenTurn(t *testing.T) {
	st := tmpStore(t)
	defer st.Close()
	conn, sl := openConn(t, st)
	if err := sl.Client(wsCreate("", wsItem("hi")), false); err != nil {
		t.Fatal(err)
	}
	sl.Server([]byte(`{"type":"response.created","response":{"id":"r1"}}`), false)
	sl.CloseFrame(wire.DirClient, 1000, "")
	if err := sl.End("clean", model.StateComplete, model.StateClientAborted); err != nil {
		t.Fatal(err)
	}
	turns := wsTurns(t, st, conn.ID)
	if len(turns) != 1 || turns[0].State != model.StateClientAborted {
		t.Fatalf("turns = %+v", turns)
	}
	if c, _ := st.Manifests.Read(conn.ID); c.State != model.StateComplete {
		t.Errorf("connection state = %s", c.State)
	}
}

// A settled manifest whose range stopped at the terminal event is extended by
// recovery once the log shows where the turn really ended.
func TestRecoveryExtendsAStaleRange(t *testing.T) {
	st := tmpStore(t)
	conn, sl := openConn(t, st)
	turn(t, sl, wsCreate("", wsItem("hi")), wsEvents("r1", "hello"))
	sl.CloseFrame(wire.DirServer, 1000, "")
	if err := sl.Commit(); err != nil {
		t.Fatal(err)
	}
	sl.w.close() // crash before End

	lb := st.Blobs.(*localBlobs)
	if p := RecoverStreams(lb.dir, st.Manifests, st.Index); len(p) > 0 {
		t.Fatalf("recovery: %v", p)
	}
	// 1 turn record, 2 create, 3–5 events with the terminal last, 6 the close.
	turns := wsTurns(t, st, conn.ID)
	if len(turns) != 1 || turns[0].State != model.StateComplete || turns[0].WS.Last != 6 {
		t.Fatalf("turn after recovery = %+v", turns)
	}
	st.Close()
}

// Only a manifest that is missing is rebuilt. One that cannot be read is
// evidence, and recovery leaves the connection exactly as found.
func TestRecoveryLeavesAnUnreadableManifestAlone(t *testing.T) {
	st := tmpStore(t)
	defer st.Close()
	conn, sl := openConn(t, st)
	if err := sl.Client(wsCreate("", wsItem("hi")), false); err != nil {
		t.Fatal(err)
	}
	sl.w.close()
	turns := wsTurns(t, st, conn.ID)
	if len(turns) != 1 {
		t.Fatalf("got %d turns", len(turns))
	}
	mp := st.Manifests.(*localManifests).path(turns[0].ID)
	if err := os.WriteFile(mp, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	lb := st.Blobs.(*localBlobs)
	if p := RecoverStreams(lb.dir, st.Manifests, st.Index); len(p) != 1 {
		t.Fatalf("problems = %v", p)
	}
	if got, _ := os.ReadFile(mp); string(got) != "{not json" {
		t.Fatal("recovery overwrote an unreadable manifest")
	}
	if _, err := os.Stat(livePath(lb.dir, conn.ID)); err != nil {
		t.Fatal("recovery removed the live log of a connection it could not account for")
	}
}

func TestFsckCatchesAMisplacedRange(t *testing.T) {
	st := tmpStore(t)
	conn, sl := openConn(t, st)
	turn(t, sl, wsCreate("", wsItem("hi")), wsEvents("r1", "hello"))
	turn(t, sl, wsCreate("r1", wsItem("again")), wsEvents("r2", "ok"))
	_ = sl.End("clean", model.StateComplete, model.StateClientAborted)
	turns := wsTurns(t, st, conn.ID)
	// Point the first turn at the second one's records.
	a, b := turns[0], turns[1]
	a.WS.First = b.WS.First
	data, _ := json.Marshal(a)
	_ = writeFileSync(st.Manifests.(*localManifests).path(a.ID), data)
	lb := st.Blobs.(*localBlobs)
	st.Close()
	fs, _ := Fsck(FsckOptions{BlobsDir: lb.dir, ManifestsDir: filepath.Join(filepath.Dir(lb.dir), "manifests"), ArchivesDir: lb.archives})
	if len(fs.Problems) != 1 || !strings.Contains(fs.Problems[0], "belongs to turn") {
		t.Fatalf("problems = %v", fs.Problems)
	}
}

// An acknowledgement can arrive before the terminal event. The turn is still
// running then, and must not be settled with a guessed outcome.
func TestStreamAckBeforeTerminalKeepsTheTurnInflight(t *testing.T) {
	st := tmpStore(t)
	defer st.Close()
	conn, sl := openConn(t, st)
	if err := sl.Client(wsCreate("", wsItem("hi")), false); err != nil {
		t.Fatal(err)
	}
	sl.Server([]byte(`{"type":"response.created","response":{"id":"r1"}}`), false)
	if err := sl.Client([]byte(`{"type":"response.interrupt","response_id":"r1"}`), false); err != nil {
		t.Fatal(err)
	}
	if !sl.Server([]byte(`{"type":"response.interrupt.accepted","response_id":"r1"}`), false) {
		t.Fatal("the acknowledgement must be held")
	}
	if err := sl.Commit(); err != nil {
		t.Fatal(err)
	}
	if turns := wsTurns(t, st, conn.ID); len(turns) != 1 || turns[0].State != model.StateInflight {
		t.Fatalf("after an early acknowledgement: %+v", turns)
	}
	sl.Server([]byte(`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete"}}`), false)
	if err := sl.Commit(); err != nil {
		t.Fatal(err)
	}
	if turns := wsTurns(t, st, conn.ID); turns[0].State != model.StateComplete {
		t.Fatalf("after the terminal event: %s", turns[0].State)
	}
	_ = sl.End("clean", model.StateComplete, model.StateClientAborted)
}

// A readable manifest that describes other records is not this turn's, and
// recovery must not rewrite it into one.
func TestRecoveryRefusesAManifestThatIsNotTheTurns(t *testing.T) {
	st := tmpStore(t)
	defer st.Close()
	conn, sl := openConn(t, st)
	if err := sl.Client(wsCreate("", wsItem("hi")), false); err != nil {
		t.Fatal(err)
	}
	sl.w.close()
	turns := wsTurns(t, st, conn.ID)
	ex := turns[0]
	ex.WS.First = 40
	data, _ := json.Marshal(ex)
	mp := st.Manifests.(*localManifests).path(ex.ID)
	_ = writeFileSync(mp, data)

	lb := st.Blobs.(*localBlobs)
	if p := RecoverStreams(lb.dir, st.Manifests, st.Index); len(p) != 1 || !strings.Contains(p[0], "does not describe") {
		t.Fatalf("problems = %v", p)
	}
	if _, err := os.Stat(livePath(lb.dir, conn.ID)); err != nil {
		t.Fatal("the live log of an unaccountable connection was removed")
	}
	if _, err := os.Stat(shardPath(lb.dir, conn.ID, ".zst")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("recovery sealed before it had checked every manifest")
	}
}

// Recovery writes under the ids the connection manifest supplies, so one that
// is not this log's connection stops it before anything is sealed.
func TestRecoveryRefusesAConnectionManifestThatIsNotTheLogs(t *testing.T) {
	st := tmpStore(t)
	defer st.Close()
	conn, sl := openConn(t, st)
	if err := sl.Client(wsCreate("", wsItem("hi")), false); err != nil {
		t.Fatal(err)
	}
	sl.w.close()
	c, _ := st.Manifests.Read(conn.ID)
	c.Kind = ""
	data, _ := json.Marshal(c)
	_ = writeFileSync(st.Manifests.(*localManifests).path(conn.ID), data)

	lb := st.Blobs.(*localBlobs)
	if p := RecoverStreams(lb.dir, st.Manifests, st.Index); len(p) != 1 || !strings.Contains(p[0], "not the connection") {
		t.Fatalf("problems = %v", p)
	}
	if _, err := os.Stat(shardPath(lb.dir, conn.ID, ".zst")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("sealed a log whose connection manifest does not own it")
	}
}
