package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"strings"
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

// Filter narrows a Query. Since is a UnixMilli lower bound (exclusive), used by
// the dashboard to fetch only what appeared after its last poll.
type Filter struct {
	Session      string
	Source       string
	Limit        int
	Offset       int
	Since        int64
	IncludeNoise bool
}

// where builds the shared WHERE clause for entry and session queries.
func (f Filter) where() (string, []any) {
	var args []any
	var parts []string
	if f.Session != "" {
		parts = append(parts, "session = ?")
		args = append(args, f.Session)
	}
	if f.Source != "" {
		parts = append(parts, "source = ?")
		args = append(args, f.Source)
	}
	if f.Since > 0 {
		parts = append(parts, "ts > ?")
		args = append(args, f.Since)
	}
	if !f.IncludeNoise {
		parts = append(parts, "is_noise = 0")
	}
	if len(parts) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(parts, " AND "), args
}

// limitOffset appends LIMIT/OFFSET. SQLite requires a LIMIT before OFFSET, so a
// bare offset uses the "unlimited" sentinel -1.
func (f Filter) limitOffset() (string, []any) {
	switch {
	case f.Limit > 0 && f.Offset > 0:
		return " LIMIT ? OFFSET ?", []any{f.Limit, f.Offset}
	case f.Limit > 0:
		return " LIMIT ?", []any{f.Limit}
	case f.Offset > 0:
		return " LIMIT -1 OFFSET ?", []any{f.Offset}
	}
	return "", nil
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

// Sessions returns per-session rollups, most recently active first.
func (i *Index) Sessions(f Filter) ([]Session, error) {
	return querySessions(i.db, f)
}

// entryCols mirrors upsertSQL's column order so a row round-trips through the
// projection without losing any field the dashboard needs (the earlier list
// dropped cache_read/cache_create, which the context bar depends on).
const entryCols = `id,ts,capture_ver,state,provider,source,session,is_noise,method,url,request_id,
  model,msg_count,tool_count,input_tokens,output_tokens,cache_read,cache_create,
  cost,status,stop_reason,cwd,is_subagent,title,elapsed_ms,ratelimit,req_ref,res_ref,parse_ok`

func queryEntries(db *sql.DB, f Filter) ([]model.Exchange, error) {
	where, args := f.where()
	tail, tailArgs := f.limitOffset()
	q := `SELECT ` + entryCols + ` FROM entries` + where + ` ORDER BY ts DESC` + tail
	rows, err := db.Query(q, append(args, tailArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Exchange
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanEntry(rows *sql.Rows) (model.Exchange, error) {
	var e model.Exchange
	var ts, elapsed int64
	var isNoise, isSubagent, parseOK int
	var reqID, ratelimit string
	if err := rows.Scan(
		&e.ID, &ts, &e.CaptureVer, &e.State, &e.Provider, &e.Source, &e.Req.SessionID, &isNoise,
		&e.Method, &e.URL, &reqID,
		&e.Req.Model, &e.Req.MsgCount, &e.Req.ToolCount,
		&e.Res.Usage.InputTokens, &e.Res.Usage.OutputTokens,
		&e.Res.Usage.CacheReadTokens, &e.Res.Usage.CacheCreateTokens,
		&e.Res.Cost, &e.Status, &e.Res.StopReason, &e.Req.Cwd, &isSubagent,
		&e.Res.Title, &elapsed, &ratelimit, &e.ReqRef, &e.ResRef, &parseOK,
	); err != nil {
		return e, err
	}
	e.IsNoise = isNoise != 0
	e.Req.IsSubagent = isSubagent != 0
	e.ParseOK = parseOK != 0
	e.StartedAt = time.UnixMilli(ts).UTC()
	if elapsed > 0 {
		e.EndedAt = e.StartedAt.Add(time.Duration(elapsed) * time.Millisecond)
	}
	// Reassemble the curated response headers the projection flattened.
	h := map[string]string{}
	if reqID != "" {
		h["request-id"] = reqID
	}
	if ratelimit != "" {
		var rl map[string]string
		if json.Unmarshal([]byte(ratelimit), &rl) == nil {
			maps.Copy(h, rl)
		}
	}
	if len(h) > 0 {
		e.ResHeaders = h
	}
	return e, nil
}

// Session is a per-session rollup. Derived live from a GROUP BY rather than a
// maintained side file — the index is already the projection, so there is no
// second thing to keep in sync or reconcile.
type Session struct {
	SessionID    string   `json:"session_id"`
	Cwd          string   `json:"cwd"`
	Model        string   `json:"model"`
	Title        string   `json:"title"`
	Turns        int      `json:"turns"`
	Subagents    int      `json:"subagents"`
	Cost         float64  `json:"cost"`
	InputTokens  int      `json:"input_tokens"`
	OutputTokens int      `json:"output_tokens"`
	CacheRead    int      `json:"cache_read"`
	CacheCreate  int      `json:"cache_create"`
	FirstTS      int64    `json:"first_ts"`
	LastTS       int64    `json:"last_ts"`
	Sources      []string `json:"sources"`
}

// querySessions rolls entries up by session. Model/cwd/title come from
// correlated subqueries picking the most recent non-empty value, because a bare
// column alongside two aggregates (MIN and MAX) is not guaranteed by SQLite to
// come from any particular row.
func querySessions(db *sql.DB, f Filter) ([]Session, error) {
	where, args := f.where()
	tail, tailArgs := f.limitOffset()
	q := `SELECT session,
	    COUNT(*), COALESCE(SUM(is_subagent),0), COALESCE(SUM(cost),0),
	    COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
	    COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_create),0),
	    MIN(ts), MAX(ts),
	    COALESCE(GROUP_CONCAT(DISTINCT source),''),
	    COALESCE((SELECT t.model FROM entries t WHERE t.session=e.session AND t.model!='' ORDER BY t.ts DESC LIMIT 1),''),
	    COALESCE((SELECT t.cwd   FROM entries t WHERE t.session=e.session AND t.cwd  !='' ORDER BY t.ts DESC LIMIT 1),''),
	    COALESCE((SELECT t.title FROM entries t WHERE t.session=e.session AND t.title!='' ORDER BY t.ts DESC LIMIT 1),'')
	  FROM entries e` + where + ` GROUP BY session ORDER BY MAX(ts) DESC` + tail
	rows, err := db.Query(q, append(args, tailArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		var s Session
		var sources string
		if err := rows.Scan(&s.SessionID, &s.Turns, &s.Subagents, &s.Cost,
			&s.InputTokens, &s.OutputTokens, &s.CacheRead, &s.CacheCreate,
			&s.FirstTS, &s.LastTS, &sources, &s.Model, &s.Cwd, &s.Title); err != nil {
			return nil, err
		}
		// Always an array, never null: a nil slice would marshal to JSON null and
		// break every client that treats this as a list.
		s.Sources = []string{}
		for src := range strings.SplitSeq(sources, ",") {
			if src != "" {
				s.Sources = append(s.Sources, src)
			}
		}
		out = append(out, s)
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
