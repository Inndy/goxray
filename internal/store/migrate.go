package store

import (
	"encoding/json"
	"errors"
	"os"
	"time"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/pricing"
	"go.inndy.tw/goxray/internal/wire"
)

// MigrateStats reports what a MigrateUsage pass found or changed.
type MigrateStats struct {
	Scanned  int
	Updated  int
	Skipped  int // inflight, too recent, or no response blob
	Failed   int // blob unreadable or unparseable
	CostFrom float64
	CostTo   float64
}

// migrateQuiesce is how long a manifest must have been untouched before it is
// eligible for rewrite. The recorder writes each manifest twice (inflight, then
// final); rewriting inside that window could clobber the final write with an
// enriched copy of the inflight one. A minute is far longer than any turn's
// commit gap and costs only a second pass for anything still settling.
const migrateQuiesce = time.Minute

// MigrateUsage re-parses recorded response blobs and backfills the usage detail
// that older captures did not keep — the cache-creation TTL split above all,
// without which 1h writes are billed at the 5m rate.
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
	manifests := newLocalManifests(manifestsDir)
	cutoff := time.Now().Add(-migrateQuiesce)

	err = manifests.EachID(func(id string) error {
		st.Scanned++

		path := manifests.path(id)
		fi, err := os.Stat(path)
		if err != nil {
			st.Failed++
			return nil
		}
		if fi.ModTime().After(cutoff) {
			st.Skipped++
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			st.Failed++
			return nil
		}
		var ex model.Exchange
		if err := json.Unmarshal(data, &ex); err != nil {
			st.Failed++
			return nil
		}
		if ex.State == model.StateInflight || ex.ResRef == "" {
			st.Skipped++
			return nil
		}

		raw, err := blobs.Get(ex.Req.SessionID, ex.ResRef)
		if err != nil {
			st.Failed++
			return nil
		}
		// A recorded-but-empty body is normal (204s, 404s, aborted streams) and
		// has no usage to recover. Counting it as a failure would bury a real
		// parse regression in expected noise.
		if len(raw) == 0 {
			st.Skipped++
			return nil
		}
		meta, ok := parser.ParseResponse(nil, raw, ex.ResHeaders["content-type"])
		if !ok {
			st.Failed++
			return nil
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

		if ex.Res == before {
			return nil
		}
		st.Updated++
		if !apply {
			return nil
		}
		return manifests.Write(&ex)
	})
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	return st, err
}
