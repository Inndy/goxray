package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/wire"
)

// FsckOptions configures an integrity check.
type FsckOptions struct {
	BlobsDir     string
	ManifestsDir string
	ArchivesDir  string
	Session      string // restrict to one session; empty checks everything
}

// archivedRef is a body proven intact inside the archive at one particular
// path. Keyed by path rather than by session id because that is what a read
// resolves: a body is only reachable if it is in the file archiveFile() names
// for its session, and an archive that is intact somewhere else is not.
type archivedRef struct {
	path string
	ref  string
}

// FsckMissing is a manifest that points at a body nothing can produce.
type FsckMissing struct {
	Exchange string
	Session  string
	Side     string // "request" or "response"
	Ref      string
	Err      string
}

// FsckStats reports what the check found.
type FsckStats struct {
	Archives  int
	Chunks    int
	Bodies    int
	Packs     int // manifest packs checked
	Packed    int // manifests resolved out of them
	Manifests int
	Problems  []string // archive- and pack-level failures
	Missing   []FsckMissing
}

// Fsck verifies every archive against the content hashes it carries, then
// cross-checks the manifests: every recorded body must be produced by either a
// live blob or an archive.
//
// Content addressing is what makes this possible at all — an archive can prove
// its own chunks are the bytes that went in, so damage surfaces as a reported
// failure rather than as a body that silently reads back wrong. For a tool
// whose entire purpose is a faithful record, detectability is the property that
// matters most.
func Fsck(o FsckOptions) (FsckStats, error) {
	var st FsckStats

	blobs, err := newLocalBlobs(o.BlobsDir, o.ArchivesDir)
	if err != nil {
		return st, err
	}
	defer blobs.Close()

	// Pass 1: verify archives, and remember which refs they hold so the
	// manifest cross-check does not have to re-decode a pool per manifest.
	archived := map[archivedRef]bool{}
	only := ""
	if o.Session != "" {
		only = archiveFile(o.ArchivesDir, o.Session)
	}
	err = filepath.WalkDir(o.ArchivesDir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || filepath.Ext(p) != archiveExt {
			return nil //nolint:nilerr // an unreadable shard is reported below, not fatal
		}
		p = filepath.Clean(p)
		if only != "" && p != only {
			return nil
		}
		st.Archives++
		stem := d.Name()[:len(d.Name())-len(archiveExt)]
		label := stem
		a, err := openArchive(p)
		if err != nil {
			st.Problems = append(st.Problems, fmt.Sprintf("%s: %v", label, err))
			return nil
		}
		defer a.Close()
		// An archive says whose it is, so it can be checked against where it
		// is filed — and a file in the wrong place names the session that would
		// put it right. A v1 archive records no session and predates digest
		// naming, so the only thing to say about it is that it needs migrating.
		switch sid := a.Session(); {
		case sid != "":
			label = sid
			if want := archiveFile(o.ArchivesDir, sid); want != p {
				st.Problems = append(st.Problems, fmt.Sprintf(
					"%s: filed at %s but belongs at %s, so nothing can read it; `python3 tools/migrate-archive-names.py --apply` moves it",
					label, relPath(o.ArchivesDir, p), relPath(o.ArchivesDir, want)))
			}
		case !isArchiveName(stem):
			st.Problems = append(st.Problems, fmt.Sprintf(
				"%s: named after its session, so nothing can read it; `python3 tools/migrate-archive-names.py --apply` migrates it", label))
		}
		chunks, err := a.Verify()
		st.Chunks += chunks
		if err != nil {
			st.Problems = append(st.Problems, fmt.Sprintf("%s: %v", label, err))
			return nil
		}
		// Credited by path, whatever the file thinks of itself. Pass 2 asks the
		// same question a read asks — is the body in the file this session
		// resolves to — so a misfiled archive is not credited to anyone, and its
		// manifests report as unreadable rather than passing clean.
		for _, ref := range a.Refs() {
			archived[archivedRef{path: p, ref: ref}] = true
			st.Bodies++
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return st, err
	}

	// Pass 1b: the same for manifest packs. They are checked separately from the
	// archives, and are a separate file, because a manifest is the record of what
	// a body was: sharing a file would mean a damaged pool cost the ability to say
	// which exchanges it cost.
	onlyPack := ""
	if o.Session != "" {
		onlyPack = mpackFile(o.ArchivesDir, o.Session)
	}
	err = filepath.WalkDir(o.ArchivesDir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || filepath.Ext(p) != mpackExt {
			return nil //nolint:nilerr // an unreadable shard is reported below, not fatal
		}
		p = filepath.Clean(p)
		if onlyPack != "" && p != onlyPack {
			return nil
		}
		st.Packs++
		label := d.Name()[:len(d.Name())-len(mpackExt)]
		pk, err := openMpack(p)
		if err != nil {
			st.Problems = append(st.Problems, fmt.Sprintf("%s: %v", label, err))
			return nil
		}
		defer pk.Close()
		// A pack records whose it is, so being in the wrong place is detectable and
		// nameable, exactly as for an archive.
		if sid := pk.Session(); sid != "" {
			label = sid
			if want := mpackFile(o.ArchivesDir, sid); want != p {
				st.Problems = append(st.Problems, fmt.Sprintf(
					"%s: manifest pack filed at %s but belongs at %s, so nothing can read it",
					label, relPath(o.ArchivesDir, p), relPath(o.ArchivesDir, want)))
			}
		}
		n, err := pk.Verify()
		st.Packed += n
		if err != nil {
			st.Problems = append(st.Problems, fmt.Sprintf("%s: %v", label, err))
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return st, err
	}

	// Pass 2: every manifest's bodies must be reachable. A ref already verified
	// in pass 1 is known good; anything else has to be read to prove it.
	manifests := newLocalManifests(o.ManifestsDir, o.ArchivesDir)
	wsc := newWSChecker(blobs, manifests)
	err = manifests.EachID(func(id string) error {
		ex, err := manifests.Read(id)
		if err != nil {
			st.Problems = append(st.Problems, fmt.Sprintf("%s: unreadable manifest: %v", id, err))
			return nil
		}
		if o.Session != "" && ex.Req.SessionID != o.Session {
			return nil
		}
		st.Manifests++
		// An inflight turn legitimately has no response body yet.
		check := []struct {
			side string
			ref  string
		}{{"request", ex.ReqRef}, {"response", ex.ResRef}}
		if ex.WS != nil {
			check = append(check, struct {
				side string
				ref  string
			}{"log", ex.WS.Log})
			if p := wsc.check(ex); p != "" {
				st.Problems = append(st.Problems, fmt.Sprintf("%s: %s", id, p))
			}
		}
		home := archiveFile(o.ArchivesDir, ex.Req.SessionID)
		for _, c := range check {
			if c.ref == "" || archived[archivedRef{path: home, ref: c.ref}] {
				continue
			}
			if _, err := blobs.Get(ex.Req.SessionID, c.ref); err != nil {
				if errors.Is(err, fs.ErrNotExist) && ex.State == model.StateInflight && c.side != "log" {
					continue
				}
				st.Missing = append(st.Missing, FsckMissing{
					Exchange: id, Session: ex.Req.SessionID, Side: c.side, Ref: c.ref, Err: err.Error(),
				})
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return st, err
	}
	return st, nil
}

// relPath shortens a path for a message, falling back to the full path when it is
// not under dir at all.
func relPath(dir, path string) string {
	if r, err := filepath.Rel(dir, path); err == nil {
		return r
	}
	return path
}

// wsChecker verifies that a WebSocket turn's record is coherent: that its
// connection exists and owns the log, and that the range it names is really its
// own. Archive content hashes prove the log's bytes are the bytes that went in;
// they cannot prove a manifest points at the right ones, and a wrong range
// would otherwise render another turn's conversation without complaint.
type wsChecker struct {
	blobs     *localBlobs
	manifests *localManifests
	// turns caches each log's turn spans by record number of their create, so
	// a connection with many turns is decoded once. Spans, not turns: a turn
	// holds every event it saw.
	turns map[string]map[int64]turnSpan
	errs  map[string]string
}

func newWSChecker(b *localBlobs, m *localManifests) *wsChecker {
	return &wsChecker{blobs: b, manifests: m, turns: map[string]map[int64]turnSpan{}, errs: map[string]string{}}
}

type turnSpan struct {
	ID          string
	First, Last int64
}

func (c *wsChecker) check(ex *model.Exchange) string {
	ws := ex.WS
	if ws.Conn == "" || ws.Log == "" {
		return "websocket turn without a connection or log"
	}
	conn, err := c.manifests.Read(ws.Conn)
	if err != nil {
		return fmt.Sprintf("connection %s unreadable: %v", ws.Conn, err)
	}
	if conn.ResRef != ws.Log {
		return fmt.Sprintf("log %s is not connection %s's (%s)", ws.Log, ws.Conn, conn.ResRef)
	}
	if conn.Req.SessionID != ex.Req.SessionID {
		return fmt.Sprintf("session %q differs from its connection's %q", ex.Req.SessionID, conn.Req.SessionID)
	}
	turns, msg := c.load(ex.Req.SessionID, ws.Log)
	if msg != "" {
		if ex.State == model.StateInflight {
			return "" // its log may legitimately still be open elsewhere
		}
		return msg
	}
	t, ok := turns[ws.First]
	// While the log is live a turn may still be growing, and a settled one is
	// rewritten with its full range only when it ends; once the log is sealed
	// nothing can extend it, so the range must be exactly the turn's.
	sealed := !liveExists(c.blobs.dir, ws.Log)
	switch {
	case !ok:
		return fmt.Sprintf("record %d of log %s is not a response.create", ws.First, ws.Log)
	case t.ID == "":
		return fmt.Sprintf("record %d of log %s has no turn record naming it", ws.First, ws.Log)
	case t.ID != ex.ID:
		return fmt.Sprintf("record %d of log %s belongs to turn %s", ws.First, ws.Log, t.ID)
	case ws.Last < ws.First || ws.Last > t.Last:
		return fmt.Sprintf("range %d..%d is outside the turn's %d..%d", ws.First, ws.Last, t.First, t.Last)
	case sealed && ex.State == model.StateInflight:
		return fmt.Sprintf("still inflight though log %s is sealed", ws.Log)
	case sealed && ws.Last != t.Last:
		return fmt.Sprintf("range %d..%d stops short of the turn's %d..%d", ws.First, ws.Last, t.First, t.Last)
	}
	return ""
}

func (c *wsChecker) load(session, ref string) (map[int64]turnSpan, string) {
	if m, ok := c.turns[ref]; ok {
		return m, c.errs[ref]
	}
	m := map[int64]turnSpan{}
	c.turns[ref] = m
	raw, err := c.blobs.Get(session, ref)
	if err != nil {
		c.errs[ref] = fmt.Sprintf("log %s unreadable: %v", ref, err)
		return m, c.errs[ref]
	}
	if err := checkRecords(raw); err != nil {
		c.errs[ref] = fmt.Sprintf("log %s: %v", ref, err)
		return m, c.errs[ref]
	}
	rp := wire.NewReplayer()
	wire.EachRecord(raw, func(rec wire.Record) bool {
		if st := rp.Feed(rec); st.Ended != nil {
			m[st.Ended.First] = turnSpan{st.Ended.ID, st.Ended.First, st.Ended.Last}
		}
		return true
	})
	if cur := rp.Current(); cur != nil {
		m[cur.First] = turnSpan{cur.ID, cur.First, cur.Last}
	}
	return m, ""
}
