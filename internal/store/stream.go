package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/pricing"
	"go.inndy.tw/goxray/internal/wire"
)

var (
	errStreamEnded = errors.New("stream already ended")
	errLogFull     = errors.New("connection log reached its size limit")
)

// errRotating refuses a create on a connection past its soft limit. It is not a
// recording failure: nothing was recorded or forwarded, and the proxy closes
// the connection cleanly so the client resends on a fresh one.
type errRotating struct{}

func (errRotating) Error() string  { return "connection is rotating; new turns go to a new connection" }
func (errRotating) Rotating() bool { return true }

// StreamLog records one WebSocket connection: the connection exchange, its
// frame log, and a turn exchange per response.create. Every method takes the
// one lock, so record numbering, the turn marker that must sit directly before
// its create, the barriers and the end all happen in one order — the log's.
type StreamLog struct {
	r     *Recorder
	blobs *localBlobs
	conn  *model.Exchange

	mu      sync.Mutex
	w       *liveWriter
	n       int64
	size    int64  // decoded bytes recorded so far
	pending []byte // records since the last barrier
	rp      *wire.Replayer
	turns   map[*wire.Turn]*model.Exchange
	dirty   map[*wire.Turn]bool  // reached a terminal event or ended since its last settle
	settled map[*wire.Turn]int64 // the Last its final manifest was written for
	ending  map[*wire.Turn]bool  // ended: forget it once settled
	full    bool                 // hit logHardLimit; the next barrier must fail
	rotate  bool                 // past logSoftLimit; no new turns
	ended   bool
}

// frame is one record to append, with its payload.
type frame struct {
	rec     wire.Record
	payload []byte
	binary  bool
}

// OpenStream starts recording a connection. The live log is created and the
// connection exchange written as inflight before it returns, so it runs before
// goxray dials upstream: nothing is sent anywhere the record does not know of.
func (r *Recorder) OpenStream(conn *model.Exchange) (*StreamLog, error) {
	lb, ok := r.blobs.(*localBlobs)
	if !ok {
		return nil, errors.New("blob store cannot hold connection logs")
	}
	if conn.Req.SessionID == "" {
		conn.Req.SessionID = normalizeSessionID(firstHeader(conn.ReqHeaders, "session-id", "session_id"))
	}
	// The log's ref is the connection's own id: a .gxl then names the manifest
	// that owns it, which is what lets recovery start from the file alone.
	conn.ResRef = conn.ID
	// Hidden from turn lists: it is the socket, not a model call.
	conn.IsNoise = true
	w, err := createLive(lb.dir, conn.ID)
	if err != nil {
		return nil, err
	}
	if err := r.manifests.Write(conn); err != nil {
		w.close()
		_ = os.Remove(livePath(lb.dir, conn.ID))
		return nil, err
	}
	r.notify(conn)
	return &StreamLog{
		r: r, blobs: lb, conn: conn, w: w, rp: wire.NewReplayer(),
		turns: map[*wire.Turn]*model.Exchange{}, dirty: map[*wire.Turn]bool{},
		settled: map[*wire.Turn]int64{}, ending: map[*wire.Turn]bool{},
	}, nil
}

func firstHeader(h map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := h[k]; v != "" {
			return v
		}
	}
	return ""
}

// record appends frames to the pending segment, all or none, and folds them
// into the replay, returning the last one's step. With limit set it reports
// false, recording nothing, if they would carry the log past its hard limit;
// close and end records pass limit false, since a connection must always be
// able to finish, and logSlack is what they may add.
func (s *StreamLog) record(limit bool, frames ...frame) (wire.Step, bool) {
	now := time.Now().UnixMilli()
	var lines []byte
	for i := range frames {
		frames[i].rec.N, frames[i].rec.T = s.n+int64(i)+1, now
		lines = wire.AppendRecord(lines, frames[i].rec, frames[i].payload, frames[i].binary)
	}
	if limit && s.size+int64(len(lines)) > logHardLimit {
		s.full = true
		return wire.Step{}, false
	}
	s.n += int64(len(frames))
	s.size += int64(len(lines))
	s.pending = append(s.pending, lines...)
	if s.size > logSoftLimit {
		s.rotate = true
	}
	var st wire.Step
	for _, f := range frames {
		// The replay reads the payload back off the record; which field the
		// line stored it in does not matter to it.
		if f.binary {
			f.rec.B = f.payload
		} else if f.payload != nil {
			f.rec.P = f.payload
		}
		st = s.rp.Feed(f.rec)
	}
	return st, true
}

// flush writes the pending segment durably.
func (s *StreamLog) flush() error {
	if err := s.w.append(s.pending); err != nil {
		return err
	}
	s.pending = s.pending[:0]
	return nil
}

// Upgraded records the upstream's handshake answer. It is a barrier: the
// connection manifest carries the 101 before the client is told of it.
func (s *StreamLog) Upgraded(status int, headers map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn.Status = status
	s.conn.ResHeaders = headers
	return s.r.manifests.Write(s.conn)
}

// Client records a client frame durably. Only after it returns nil may the
// frame be forwarded upstream.
func (s *StreamLog) Client(payload []byte, binary bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return errStreamEnded
	}
	isCreate := !binary && wire.EventType(payload) == "response.create"
	frames := []frame{{rec: wire.Record{D: wire.DirClient}, payload: payload, binary: binary}}
	var turn *model.Exchange
	if isCreate {
		if s.rotate {
			return errRotating{}
		}
		turn = s.newTurn()
		// The marker goes in directly before its create, as one unit, so no
		// durable state ever has one without the other.
		frames = append([]frame{{rec: wire.Record{D: wire.DirClient, Op: wire.OpTurn, ID: turn.ID}}}, frames...)
	}
	st, ok := s.record(true, frames...)
	if !ok {
		return errLogFull
	}
	if st.Ended != nil {
		s.dirty[st.Ended], s.ending[st.Ended] = true, true
	}
	if err := s.flush(); err != nil {
		return err
	}
	if err := s.settleDirty(""); err != nil {
		return err
	}
	if isCreate && st.Started {
		s.turns[st.Turn] = turn
		if err := s.persistTurnRequest(turn, st.Turn); err != nil {
			return err
		}
	}
	return nil
}

// Server records an upstream frame in memory and reports whether it must be
// held until Commit.
func (s *StreamLog) Server(payload []byte, binary bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return true // Commit will refuse
	}
	st, ok := s.record(true, frame{rec: wire.Record{D: wire.DirServer}, payload: payload, binary: binary})
	if !ok {
		return true // full: Commit fails and the connection is dropped
	}
	if st.Held && st.Turn != nil {
		s.dirty[st.Turn] = true
	}
	return st.Held
}

// Commit makes everything recorded so far durable and writes the final
// manifest of any turn that reached its end.
func (s *StreamLog) Commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return errStreamEnded
	}
	if s.full {
		return errLogFull
	}
	if err := s.flush(); err != nil {
		return err
	}
	return s.settleDirty("")
}

// ShouldRotate reports whether the log has passed its soft limit and the
// connection is at a point the client also sees as between turns, so the
// proxy may close it now. A terminal event alone is not that point: an
// interrupted turn is not over for Codex until its acknowledgement arrives.
func (s *StreamLog) ShouldRotate() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rotate && s.rp.Quiescent()
}

// CloseFrame records a close frame seen in direction dir.
func (s *StreamLog) CloseFrame(dir string, code int, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ended {
		s.record(false, frame{rec: wire.Record{D: dir, Op: wire.OpClose, Code: code, Reason: reason}})
	}
}

// Refused records an upgrade the upstream answered with something other than
// 101. It fails if the answer cannot be recorded whole, and then the answer
// must not be relayed: End still succeeds, so it cannot be the one to say so.
func (s *StreamLog) Refused(status int, headers map[string]string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return errStreamEnded
	}
	s.conn.Status = status
	s.conn.ResHeaders = headers
	if _, ok := s.record(true, frame{rec: wire.Record{D: wire.DirServer, Op: wire.OpRefused, Code: status}, payload: body}); !ok {
		return errLogFull
	}
	return nil
}

// End finishes the connection in the order docs/codex.md fixes: end record and
// last segment, seal into a blob, settle the turns, settle the connection,
// unlink the live file. If any step fails the .gxl stays, and the next start
// finishes the job. state is the connection's disposition; open is the
// disposition of a turn still waiting for its terminal event, which a cleanly
// closed connection did not complete.
func (s *StreamLog) End(how string, state, open model.ExchangeState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return nil
	}
	s.ended = true
	if s.w.err != nil {
		// What is on disk past the last good segment is unknown, so this
		// process does not seal it; recovery reads it strictly at next start.
		_ = s.w.close()
		return s.w.err
	}
	st, _ := s.record(false, frame{rec: wire.Record{Op: wire.OpEnd, Reason: how}})
	if st.Ended != nil {
		s.dirty[st.Ended], s.ending[st.Ended] = true, true
	}
	ferr := s.flush()
	if cerr := s.w.close(); ferr == nil {
		ferr = cerr
	}
	if ferr != nil {
		return ferr
	}
	if _, err := sealLive(s.blobs.dir, s.conn.ID); err != nil {
		return err
	}
	if err := s.settleDirty(open); err != nil {
		return err
	}
	if s.conn.State == model.StateInflight || s.conn.State == "" {
		s.conn.State = state
	}
	if err := s.r.PersistFinal(s.conn); err != nil {
		return err
	}
	if err := os.Remove(livePath(s.blobs.dir, s.conn.ID)); err != nil {
		return err
	}
	return fsyncDir(dirOf(livePath(s.blobs.dir, s.conn.ID)))
}

func (s *StreamLog) newTurn() *model.Exchange {
	ex := s.r.NewExchange("WS", s.conn.URL, nil)
	ex.Provider = s.conn.Provider
	ex.Transport = model.TransportWS
	ex.Source = s.conn.Source
	ex.ReqHeaders = s.conn.ReqHeaders
	ex.WS = &model.WSRef{Conn: s.conn.ID, Log: s.conn.ResRef}
	return ex
}

// persistTurnRequest writes a new turn's inflight manifest, parsed from the
// full request the replay rebuilt — the frame itself may carry only the new
// items.
func (s *StreamLog) persistTurnRequest(ex *model.Exchange, t *wire.Turn) error {
	ex.WS.First, ex.WS.Last = t.First, t.Last
	fillTurnRequest(ex, s.conn, t)
	if err := s.r.manifests.Write(ex); err != nil {
		return err
	}
	s.r.notify(ex)
	return nil
}

// settleDirty writes the final manifest of every turn that has ended or
// reached a terminal event since the last call. A turn is written again when it
// ends after its terminal event, so the manifest's range runs to the turn's
// real end — an interrupt acknowledgement can follow the terminal event. state
// is the disposition for a turn that ended without a terminal event; "" means
// one ended by a new create, which is the client abandoning it.
//
// "complete" means the API's terminal event is recorded, not that Codex has
// released the turn. A turn with no terminal event that has not ended either —
// an acknowledgement can also come first — stays inflight: its outcome is not
// known yet, and writing a guess would outlive a crash, since recovery keeps a
// final manifest's outcome.
func (s *StreamLog) settleDirty(state model.ExchangeState) error {
	for t := range s.dirty {
		ex := s.turns[t]
		if t.Terminal == "" && !s.ending[t] {
			delete(s.dirty, t)
			continue
		}
		if last, ok := s.settled[t]; ex != nil && !(ok && last == t.Last) {
			st := model.StateComplete
			if t.Terminal == "" {
				st = state
				if st == "" {
					st = model.StateClientAborted
				}
			}
			fillTurnResponse(ex, t, st)
			if err := s.r.PersistFinal(ex); err != nil {
				return err
			}
			s.settled[t] = t.Last
		}
		delete(s.dirty, t)
		if s.ending[t] {
			// Over: nothing will touch it again, and it holds every event.
			delete(s.turns, t)
			delete(s.settled, t)
			delete(s.ending, t)
		}
	}
	return nil
}

func fillTurnRequest(ex, conn *model.Exchange, t *wire.Turn) {
	parser := wire.For(ex.Provider)
	ex.IsNoise = parser.IsNoise(ex.Method, strings.SplitN(ex.URL, "?", 2)[0], nil, t.Create)
	body := t.Request
	if body == nil {
		body = t.Create
	}
	ex.Req = parser.ParseRequest(headerOf(conn.ReqHeaders), body)
	if ex.Req.PrevResponseID == "" {
		ex.Req.PrevResponseID = t.PrevResponseID
	}
	// A turn's session is its connection's: the log is archived under it, and a
	// turn filed under another would not find its own bytes.
	ex.Req.SessionID = conn.Req.SessionID
	if t.StartedMS > 0 {
		ex.StartedAt = time.UnixMilli(t.StartedMS).UTC()
	}
}

func fillTurnResponse(ex *model.Exchange, t *wire.Turn, state model.ExchangeState) {
	ex.State = state
	ex.WS.Last = t.Last
	meta, ok := wire.For(ex.Provider).ParseResponse(nil, t.EventsJSONL(), "application/jsonl")
	meta.Cost = pricing.Price(meta, ex.Req.Model)
	ex.Res, ex.ParseOK = meta, ok
	if t.ResponseID != "" {
		ex.Res.ResponseID = t.ResponseID
	}
	if ms := t.DoneMS(); ms > 0 {
		ex.EndedAt = time.UnixMilli(ms).UTC()
	} else {
		ex.EndedAt = time.Now().UTC()
	}
}

// PersistFinal writes an exchange's final manifest and projects it, for
// exchanges whose bodies were recorded elsewhere — a WebSocket turn or
// connection, whose bytes are in the connection log.
func (r *Recorder) PersistFinal(ex *model.Exchange) error {
	if ex.EndedAt.IsZero() {
		ex.EndedAt = time.Now().UTC()
	}
	if err := r.manifests.Write(ex); err != nil {
		return err
	}
	r.enqueueIndex(*ex)
	r.notify(ex)
	return nil
}

func dirOf(path string) string {
	i := strings.LastIndexAny(path, `/\`)
	if i < 0 {
		return "."
	}
	return path[:i]
}

// ── recovery ────────────────────────────────────────────────

// RecoverStreams finishes the close of every connection a previous process left
// open. A .gxl outliving its process means steps 2–5 of the close never
// completed; they are re-run here, idempotently, and every manifest settled is
// also upserted into idx. Turns are found from the log's own turn records, so a
// turn whose inflight manifest never got written is created rather than lost.
// Returns one line per connection it could not recover; their files are left
// exactly as they were.
func RecoverStreams(blobsDir string, manifests ManifestStore, idx *Index) []string {
	dir := livePath(blobsDir, "x")
	dir = dir[:len(dir)-len("x.gxl")]
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []string{fmt.Sprintf("%s: %v; no connection recovered", dir, err)}
	}
	var problems []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".gxl") {
			continue
		}
		ref := strings.TrimSuffix(name, ".gxl")
		if !isPlainToken(ref) {
			problems = append(problems, fmt.Sprintf("%s: not a name goxray writes; left alone", name))
			continue
		}
		if err := recoverStream(blobsDir, ref, manifests, idx); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
		}
	}
	return problems
}

func recoverStream(blobsDir, ref string, manifests ManifestStore, idx *Index) error {
	conn, err := manifests.Read(ref)
	if err != nil {
		return fmt.Errorf("no connection manifest (%v); left in place", err)
	}
	// Everything below is written under the ids this manifest supplies, so it
	// has to be the connection that owns this log.
	if conn.ID != ref || conn.ResRef != ref || conn.Kind != model.KindWSConn || conn.Transport != model.TransportWS {
		return fmt.Errorf("manifest %s is not the connection that owns this log; left in place", ref)
	}
	log, err := readLive(livePath(blobsDir, ref))
	if err != nil {
		return err
	}

	write := func(ex *model.Exchange) error {
		if err := manifests.Write(ex); err != nil {
			return err
		}
		if idx != nil {
			_ = idx.Upsert(*ex)
		}
		return nil
	}

	rp := wire.NewReplayer()
	var turns []*wire.Turn
	var lastMS int64
	wire.EachRecord(log, func(rec wire.Record) bool {
		lastMS = rec.T
		if st := rp.Feed(rec); st.Ended != nil {
			turns = append(turns, st.Ended)
		}
		return true
	})
	if cur := rp.Current(); cur != nil {
		turns = append(turns, cur)
	}
	// Everything is read and checked before anything is written: a log this
	// pass cannot account for is left exactly as found, sealed or not.
	type settle struct {
		t  *wire.Turn
		ex *model.Exchange // nil: the manifest is missing
	}
	var todo []settle
	seen := map[string]bool{}
	for _, t := range turns {
		if t.ID == "" || !isPlainToken(t.ID) {
			return fmt.Errorf("turn at record %d has no turn record naming it; left in place", t.First)
		}
		if seen[t.ID] {
			return fmt.Errorf("turn %s is named twice in the log; left in place", t.ID)
		}
		seen[t.ID] = true
		ex, err := manifests.Read(t.ID)
		switch {
		case err == nil:
			// A readable manifest must also be this turn's: one describing other
			// records would be rewritten into a lie, and the turn left with none.
			if ex.ID != t.ID || ex.WS == nil || ex.WS.Conn != conn.ID || ex.WS.Log != ref || ex.WS.First != t.First ||
				ex.Req.SessionID != conn.Req.SessionID {
				return fmt.Errorf("manifest %s does not describe the turn at record %d; left in place", t.ID, t.First)
			}
			todo = append(todo, settle{t, ex})
		case errors.Is(err, fs.ErrNotExist):
			todo = append(todo, settle{t, nil})
		default:
			return fmt.Errorf("turn %s: %v; left in place", t.ID, err)
		}
	}
	if _, err := sealLive(blobsDir, ref); err != nil {
		return err
	}

	for _, s := range todo {
		t, ex := s.t, s.ex
		switch {
		case ex != nil && ex.State != model.StateInflight:
			// Settled, perhaps at its terminal event and before the rest of the
			// turn arrived. Only the range can be stale; the outcome is not.
			if ex.WS.Last == t.Last {
				continue
			}
			ex.WS.Last = t.Last
			switch {
			case t.DoneMS() > 0:
				ex.EndedAt = time.UnixMilli(t.DoneMS()).UTC()
			case lastMS > 0:
				ex.EndedAt = time.UnixMilli(lastMS).UTC()
			}
			if err := write(ex); err != nil {
				return err
			}
			continue
		case ex == nil:
			// The create was durable, so it may have reached the API; its
			// manifest was not. Rebuild it from the log rather than lose it.
			ex = &model.Exchange{
				ID: t.ID, Provider: conn.Provider, CaptureVer: model.CaptureVersion,
				Transport: model.TransportWS, Method: "WS", URL: conn.URL, Source: conn.Source,
				ReqHeaders: conn.ReqHeaders, WS: &model.WSRef{Conn: conn.ID, Log: ref, First: t.First, Last: t.Last},
			}
			fillTurnRequest(ex, conn, t)
		}
		st := model.StateComplete
		if t.Terminal == "" {
			st = model.StateUpstreamError
		}
		fillTurnResponse(ex, t, st)
		if t.DoneMS() == 0 && lastMS > 0 {
			ex.EndedAt = time.UnixMilli(lastMS).UTC()
		}
		if err := write(ex); err != nil {
			return err
		}
	}
	if conn.State == model.StateInflight || conn.State == "" {
		conn.State = model.StateUpstreamError
		if lastMS > 0 {
			conn.EndedAt = time.UnixMilli(lastMS).UTC()
		} else {
			conn.EndedAt = time.Now().UTC()
		}
		if err := write(conn); err != nil {
			return err
		}
	}
	if err := os.Remove(livePath(blobsDir, ref)); err != nil {
		return err
	}
	logf("recovered connection %s: %d turn(s) settled from its log", ref, len(turns))
	return nil
}

// bodyRefs lists every blob an exchange's record depends on. A WebSocket turn
// names its connection's log directly, so the log is archived and checked on
// the turn's account rather than only through the connection exchange — a
// turn must never be left pointing at bytes nothing else kept.
func bodyRefs(ex *model.Exchange) []string {
	refs := []string{ex.ReqRef, ex.ResRef}
	if ex.WS != nil {
		refs = append(refs, ex.WS.Log)
	}
	return refs
}
