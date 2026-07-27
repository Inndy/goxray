package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/pricing"
)

// ManifestStore persists the immutable per-exchange audit record. A manifest is
// written twice in the happy path — once inflight (before forwarding), once
// final (at commit) — each via temp-file + rename + dir fsync, so a crash never
// leaves torn JSON. The final write atomically supersedes the inflight one.
type ManifestStore interface {
	Write(ex *model.Exchange) error
	Read(id string) (*model.Exchange, error)
	// EachID invokes fn for every manifest id (filename scan, no parse). Used by
	// index rebuild and startup reconcile.
	EachID(fn func(id string) error) error
}

type localManifests struct {
	dir string
}

func newLocalManifests(dir string) *localManifests {
	return &localManifests{dir: dir}
}

func (m *localManifests) path(id string) string {
	return shardPath(m.dir, id, ".json")
}

// Write records the exchange with its cost stripped: the rate table moves, so a
// figure frozen here would be a stale claim in a record whose whole point is
// that it stays true. Read derives it instead. Zeroed on a copy because callers
// go on to project the live value into the index and push it to the dashboard.
func (m *localManifests) Write(ex *model.Exchange) error {
	rec := *ex
	rec.Res.Cost = 0
	data, err := json.Marshal(&rec)
	if err != nil {
		return err
	}
	return writeFileSync(m.path(ex.ID), data)
}

// Read returns the manifest with its cost computed from the stored token counts.
// Manifests written before the field was dropped still carry one; it loses to
// the derived figure. Repricing here rather than at each call site means the
// index rebuild, the dashboard and the CLI cannot disagree, and a rate change
// needs no rewrite of the audit records: delete index.db and restart.
func (m *localManifests) Read(id string) (*model.Exchange, error) {
	data, err := readFile(m.path(id))
	if err != nil {
		return nil, err
	}
	var ex model.Exchange
	if err := json.Unmarshal(data, &ex); err != nil {
		return nil, err
	}
	ex.Res.Cost = pricing.Price(ex.Res, ex.Req.Model)
	return &ex, nil
}

// EachID walks the sharded manifest tree yielding ids. Unreadable shard dirs are
// skipped (best-effort); a callback error stops the walk.
func (m *localManifests) EachID(fn func(id string) error) error {
	err := filepath.WalkDir(m.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			// Skip unreadable entries rather than abort the whole scan, but say
			// so — a silently skipped shard is an invisible hole in the index.
			logf("manifest scan: skipping %s: %v", p, err)
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		id := strings.TrimSuffix(d.Name(), ".json")
		return fn(id)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
