package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"

	"go.inndy.tw/goxray/internal/model"
)

// schemaVer is bumped whenever the SQLite projection shape changes. On a bump,
// the index is dropped and rebuilt from manifests — no ALTER/backfill.
const schemaVer = 1

const createSQL = `
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT);
CREATE TABLE IF NOT EXISTS entries(
  id TEXT PRIMARY KEY, ts INTEGER NOT NULL, capture_ver INTEGER, state TEXT,
  provider TEXT, source TEXT, session TEXT, is_noise INTEGER,
  method TEXT, url TEXT, request_id TEXT,
  model TEXT, msg_count INT, tool_count INT,
  input_tokens INT, output_tokens INT, cache_read INT, cache_create INT,
  cost REAL, status INT, stop_reason TEXT, cwd TEXT, is_subagent INT,
  title TEXT, elapsed_ms INT, ratelimit TEXT,
  req_ref TEXT, res_ref TEXT, parse_ok INT
);
CREATE INDEX IF NOT EXISTS idx_session ON entries(session, ts);
CREATE INDEX IF NOT EXISTS idx_source ON entries(source, ts);
CREATE INDEX IF NOT EXISTS idx_ts ON entries(ts);`

const upsertSQL = `
INSERT OR REPLACE INTO entries(
  id,ts,capture_ver,state,provider,source,session,is_noise,method,url,request_id,
  model,msg_count,tool_count,input_tokens,output_tokens,cache_read,cache_create,
  cost,status,stop_reason,cwd,is_subagent,title,elapsed_ms,ratelimit,req_ref,res_ref,parse_ok)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

// Filter narrows a Query.
type Filter struct {
	Session      string
	Source       string
	Limit        int
	IncludeNoise bool
}

type writeReq struct {
	ex   model.Exchange
	resp chan error
}

// Index is the queryable projection of the manifests. Writes funnel through a
// single goroutine (WAL = one writer); reads use the pool concurrently.
type Index struct {
	db      *sql.DB
	writeCh chan writeReq
	done    chan struct{}
}

// OpenIndex opens (or creates) the SQLite index. needFullRebuild is true when
// the DB was absent or its schema version differs — the caller should then run
// RebuildFrom. wasClean reports whether the previous run shut down cleanly; when
// false the caller should Reconcile against manifests.
func OpenIndex(path string) (idx *Index, needFullRebuild, wasClean bool, err error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, false, false, err
	}
	if _, err := db.Exec(createSQL); err != nil {
		db.Close()
		return nil, false, false, err
	}

	prevVer := getMeta(db, "schema_ver")
	wasClean = getMeta(db, "clean_shutdown") == "1"
	if prevVer != fmt.Sprint(schemaVer) {
		// Schema drift (or fresh DB): wipe and force a full rebuild.
		if _, err := db.Exec(`DELETE FROM entries`); err != nil {
			db.Close()
			return nil, false, false, err
		}
		setMeta(db, "schema_ver", fmt.Sprint(schemaVer))
		needFullRebuild = true
		wasClean = true // nothing to reconcile after a full rebuild
	}
	// Mark running (not-clean) until Close flips it back.
	setMeta(db, "clean_shutdown", "0")

	idx = &Index{db: db, writeCh: make(chan writeReq), done: make(chan struct{})}
	go idx.writer()
	return idx, needFullRebuild, wasClean, nil
}

func (i *Index) writer() {
	defer close(i.done)
	stmt, err := i.db.Prepare(upsertSQL)
	if err != nil {
		log.Printf("goxray: index: prepare upsert: %v", err)
		// Drain to avoid deadlocking senders.
		for req := range i.writeCh {
			req.resp <- err
		}
		return
	}
	defer stmt.Close()
	for req := range i.writeCh {
		req.resp <- doUpsert(stmt, &req.ex)
	}
}

// Upsert writes one exchange row. Serialized through the writer goroutine;
// blocks the caller until done (callers run it off the request hot path).
func (i *Index) Upsert(ex model.Exchange) error {
	resp := make(chan error, 1)
	i.writeCh <- writeReq{ex: ex, resp: resp}
	return <-resp
}

func doUpsert(stmt *sql.Stmt, ex *model.Exchange) error {
	_, err := stmt.Exec(
		ex.ID, ex.StartedAt.UnixMilli(), ex.CaptureVer, string(ex.State),
		string(ex.Provider), ex.Source, ex.Req.SessionID, b2i(ex.IsNoise),
		ex.Method, ex.URL, ex.RequestID(),
		ex.Req.Model, ex.Req.MsgCount, ex.Req.ToolCount,
		ex.Res.Usage.InputTokens, ex.Res.Usage.OutputTokens,
		ex.Res.Usage.CacheReadTokens, ex.Res.Usage.CacheCreateTokens,
		ex.Res.Cost, ex.Status, ex.Res.StopReason, ex.Req.Cwd, b2i(ex.Req.IsSubagent),
		ex.Res.Title, ex.ElapsedMS(), rateLimitJSON(ex), ex.ReqRef, ex.ResRef, b2i(ex.ParseOK),
	)
	return err
}

// Query returns matching exchanges, newest first.
func (i *Index) Query(f Filter) ([]model.Exchange, error) {
	return queryEntries(i.db, f)
}

func queryEntries(db *sql.DB, f Filter) ([]model.Exchange, error) {
	q := `SELECT id,ts,state,provider,source,session,is_noise,method,url,request_id,model,
	       msg_count,input_tokens,output_tokens,cost,status,stop_reason,title,elapsed_ms,req_ref,res_ref
	       FROM entries`
	var args []any
	var where []string
	if f.Session != "" {
		where = append(where, "session = ?")
		args = append(args, f.Session)
	}
	if f.Source != "" {
		where = append(where, "source = ?")
		args = append(args, f.Source)
	}
	if !f.IncludeNoise {
		where = append(where, "is_noise = 0")
	}
	for n, w := range where {
		if n == 0 {
			q += " WHERE "
		} else {
			q += " AND "
		}
		q += w
	}
	q += " ORDER BY ts DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Exchange
	for rows.Next() {
		var e model.Exchange
		var ts, elapsed int64
		var isNoise int
		var reqID string
		if err := rows.Scan(&e.ID, &ts, &e.State, &e.Provider, &e.Source, &e.Req.SessionID, &isNoise,
			&e.Method, &e.URL, &reqID, &e.Req.Model, &e.Req.MsgCount,
			&e.Res.Usage.InputTokens, &e.Res.Usage.OutputTokens, &e.Res.Cost, &e.Status,
			&e.Res.StopReason, &e.Res.Title, &elapsed, &e.ReqRef, &e.ResRef); err != nil {
			return nil, err
		}
		e.IsNoise = isNoise != 0
		e.StartedAt = time.UnixMilli(ts).UTC()
		if elapsed > 0 {
			e.EndedAt = e.StartedAt.Add(time.Duration(elapsed) * time.Millisecond)
		}
		if reqID != "" {
			e.ResHeaders = map[string]string{"request-id": reqID}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RebuildFrom wipes the projection and replays every manifest. Used on schema
// bump or a missing DB. Unreadable/torn manifests are skipped, not fatal.
func (i *Index) RebuildFrom(m ManifestStore) error {
	if _, err := i.db.Exec(`DELETE FROM entries`); err != nil {
		return err
	}
	n := 0
	err := m.EachID(func(id string) error {
		ex, err := m.Read(id)
		if err != nil {
			log.Printf("goxray: rebuild: skip %s: %v", id, err)
			return nil
		}
		if err := i.Upsert(*ex); err != nil {
			return err
		}
		n++
		return nil
	})
	log.Printf("goxray: index rebuilt from %d manifests", n)
	return err
}

// Reconcile upserts manifests missing from the index (crash recovery for lost
// async Upserts). It loads existing ids once, then walks manifest filenames and
// reads only the stragglers — cheap when the queue was nearly drained.
func (i *Index) Reconcile(m ManifestStore) error {
	have := map[string]struct{}{}
	rows, err := i.db.Query(`SELECT id FROM entries`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		have[id] = struct{}{}
	}
	rows.Close()

	n := 0
	err = m.EachID(func(id string) error {
		if _, ok := have[id]; ok {
			return nil
		}
		ex, err := m.Read(id)
		if err != nil {
			return nil
		}
		if err := i.Upsert(*ex); err != nil {
			return err
		}
		n++
		return nil
	})
	if n > 0 {
		log.Printf("goxray: reconciled %d manifests missing from index", n)
	}
	return err
}

// Close drains pending writes, marks a clean shutdown, and closes the DB.
func (i *Index) Close() error {
	close(i.writeCh)
	<-i.done // writer drained
	setMeta(i.db, "clean_shutdown", "1")
	return i.db.Close()
}

func getMeta(db *sql.DB, k string) string {
	var v string
	_ = db.QueryRow(`SELECT v FROM meta WHERE k=?`, k).Scan(&v)
	return v
}

func setMeta(db *sql.DB, k, v string) {
	if _, err := db.Exec(`INSERT OR REPLACE INTO meta(k,v) VALUES(?,?)`, k, v); err != nil {
		log.Printf("goxray: index: set meta %s: %v", k, err)
	}
}

func rateLimitJSON(ex *model.Exchange) string {
	if len(ex.ResHeaders) == 0 {
		return ""
	}
	rl := map[string]string{}
	for k, v := range ex.ResHeaders {
		if k == "retry-after" || len(k) > 20 && k[:20] == "anthropic-ratelimit-" {
			rl[k] = v
		}
	}
	if len(rl) == 0 {
		return ""
	}
	b, _ := json.Marshal(rl)
	return string(b)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
