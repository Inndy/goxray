package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/pricing"
	"go.inndy.tw/goxray/internal/wire"
)

// MigrateStats reports what a MigrateUsage pass found or changed. A manifest
// that was examined and had nothing to recover lands in none of the counters
// but Scanned.
type MigrateStats struct {
	Scanned  int
	Updated  int
	Skipped  int // inflight or still inside the quiet window: not examined
	Failed   int // a blob was unreadable or unparseable
	Windows  int // context windows recovered from request blobs
	CostFrom float64
	CostTo   float64
}

// migrateQuiesce is how long a manifest must have been untouched before it is
// eligible for rewrite. The recorder writes each manifest twice (inflight, then
// final); rewriting inside that window could clobber the final write with an
// enriched copy of the inflight one. A minute is far longer than any turn's
// commit gap and costs only a second pass for anything still settling.
const migrateQuiesce = time.Minute

// MigrateUsage re-parses recorded blobs and backfills the metadata that older
// captures did not keep: the cache-creation TTL split above all, without which
// 1h writes are billed at the 5m rate, and the context window, without which a
// listing row cannot show a context percentage until the turn is opened.
//
// This deliberately breaks the "blobs are never re-parsed" rule that holds for
// index rebuilds. A rebuild is a hot path that must stay cheap and must not
// depend on blob availability; this is a one-time repair, run by hand, that
// recovers information the manifest never captured. It reuses the live
// WireParser rather than reimplementing the scan, because a stream reports
// usage in several events and anything that sums instead of assigning
// double-counts.
//
// Safe to run against a live store: inflight and recently-written manifests are
// skipped, and each rewrite is an atomic temp-file rename. Idempotent — a second
// pass over already-migrated records reports zero updates.
func MigrateUsage(blobsDir, manifestsDir, archivesDir string, parser wire.WireParser, apply bool) (MigrateStats, error) {
	var st MigrateStats

	blobs, err := newLocalBlobs(blobsDir, archivesDir)
	if err != nil {
		return st, err
	}
	defer blobs.Close()
	manifests := newLocalManifests(manifestsDir, archivesDir)
	cutoff := time.Now().Add(-migrateQuiesce)

	err = manifests.EachID(func(id string) error {
		st.Scanned++

		// The quiet window only applies to a loose manifest, because it exists to
		// avoid catching the recorder's inflight write before the final one lands.
		// A packed manifest already cleared that window before the sweep folded it
		// in, so a missing loose file is not a failure here.
		if fi, err := os.Stat(manifests.path(id)); err == nil {
			if fi.ModTime().After(cutoff) {
				st.Skipped++
				return nil
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			st.Failed++
			return nil
		}

		// Read the stored bytes, not Read's repriced copy: the idempotence check
		// below compares this record against the one it would write, and a derived
		// cost on one side of that comparison would make every manifest look
		// changed.
		data, err := manifests.readRaw(id)
		if err != nil {
			st.Failed++
			return nil
		}
		var ex model.Exchange
		if err := json.Unmarshal(data, &ex); err != nil {
			st.Failed++
			return nil
		}
		// An inflight manifest is the one the recorder is still going to supersede;
		// the quiet window above cannot protect it, since a stream may run for
		// minutes before the final write.
		if ex.State == model.StateInflight {
			st.Skipped++
			return nil
		}

		changed := false

		// The context window comes from the request body, and the marker that
		// reveals it usually sits in the system prompt. Reusing the live parser
		// keeps this in step with what capture now records; only the window is
		// taken from the re-parse, so a parser that has since changed its mind
		// about cwd or session id cannot rewrite history through this path.
		if ex.Req.CtxWindow == 0 && ex.ReqRef != "" {
			raw, err := blobs.Get(ex.Req.SessionID, ex.ReqRef)
			if err != nil {
				st.Failed++
			} else if w := parser.ParseRequest(nil, raw).CtxWindow; w > 0 {
				ex.Req.CtxWindow = w
				st.Windows++
				changed = true
			}
		}

		if ex.ResRef == "" {
			return finishMigrate(&st, manifests, &ex, changed, apply)
		}
		raw, err := blobs.Get(ex.Req.SessionID, ex.ResRef)
		if err != nil {
			st.Failed++
			return finishMigrate(&st, manifests, &ex, changed, apply)
		}
		// A recorded-but-empty body is normal (204s, 404s, aborted streams) and
		// has no usage to recover. Counting it as a failure would bury a real
		// parse regression in expected noise.
		if len(raw) == 0 {
			return finishMigrate(&st, manifests, &ex, changed, apply)
		}
		meta, ok := parser.ParseResponse(nil, raw, ex.ResHeaders["content-type"])
		if !ok {
			st.Failed++
			return finishMigrate(&st, manifests, &ex, changed, apply)
		}

		// Only the fields the old capture could not hold are taken from the
		// re-parse. Everything else in the manifest stays as recorded, so a
		// parser that has since changed its mind about, say, titles cannot
		// rewrite history through this path.
		before := ex.Res
		ex.Res.Usage.CacheCreate5m = meta.Usage.CacheCreate5m
		ex.Res.Usage.CacheCreate1h = meta.Usage.CacheCreate1h
		ex.Res.Usage.ThinkingTokens = meta.Usage.ThinkingTokens
		ex.Res.ServiceTier = meta.ServiceTier
		ex.Res.Model = meta.Model

		st.CostFrom += pricing.Price(before, ex.Req.Model)
		st.CostTo += pricing.Price(ex.Res, ex.Req.Model)

		return finishMigrate(&st, manifests, &ex, changed || ex.Res != before, apply)
	})
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	return st, err
}

// finishMigrate records the outcome for one manifest and writes it when there is
// something to write. It exists because the two backfills fail independently —
// a missing response body must not cost a recovered context window its rewrite.
func finishMigrate(st *MigrateStats, manifests *localManifests, ex *model.Exchange, changed, apply bool) error {
	if !changed {
		return nil
	}
	st.Updated++
	if !apply {
		return nil
	}
	return manifests.Write(ex)
}
