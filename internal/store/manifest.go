package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

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

// localManifests keeps manifests in two tiers, exactly as localBlobs keeps
// bodies: a loose .json per exchange while its session is live, and the session's
// manifest pack once the sweep has folded them in. Reads try loose first, so the
// window where both exist is correct by construction and so is --unarchive.
//
// A packed read opens the pack and closes it again rather than caching the
// reader. That is deliberate: a held handle is what stops an install renaming
// over the file on Windows (see invariant 7), and paying an open per read is
// cheaper than owning a second cache that the install would have to know how to
// drop. Opening a pack does not touch its payload, so the cost is a stat and one
// small directory decode.
type localManifests struct {
	dir      string
	archives string // where packs live; "" leaves the packed tier out entirely

	mu   sync.Mutex
	locs map[string]string // exchange id -> pack path; nil until first built
	gen  string            // fingerprint of the pack set locs was built from
}

func newLocalManifests(dir, archives string) *localManifests {
	return &localManifests{dir: dir, archives: archives}
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
	data, err := m.readRaw(id)
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

// readRaw returns a manifest's stored bytes, loose copy first. Callers that
// compare a manifest against what is on disk must use this rather than Read,
// which reprices and so never returns quite what was stored.
func (m *localManifests) readRaw(id string) ([]byte, error) {
	data, err := readFile(m.path(id))
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, fs.ErrNotExist) || m.archives == "" {
		return nil, err
	}
	raw, aerr := m.fromPack(id)
	if aerr == nil {
		return raw, nil
	}
	// A pack that simply does not hold it means the manifest is missing, and the
	// loose-file error is the honest one to report. Corruption is not.
	if errors.Is(aerr, fs.ErrNotExist) {
		return nil, err
	}
	return nil, aerr
}

func (m *localManifests) fromPack(id string) ([]byte, error) {
	path, ok := m.locate(id)
	if !ok {
		return nil, errNotInPack
	}
	p, err := openMpack(path)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	return p.Get(id)
}

// locate resolves an exchange id to the pack holding its manifest. Nothing on
// disk maps one to the other — the map is a cache built by reading each pack's own
// directory, so a store with no index.db still resolves every manifest and
// invariant 9 keeps meaning what it says.
//
// A miss is only trusted when the pack set has not changed since the map was
// built. That is what keeps this correct when another process installs one (a
// hand-run `goxray archive` beside a running serve), without rebuilding on every
// lookup for an id that genuinely does not exist.
func (m *localManifests) locate(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locs != nil {
		if p, ok := m.locs[id]; ok {
			return p, true
		}
	}
	gen, err := m.packGen()
	if err == nil && m.locs != nil && gen == m.gen {
		return "", false
	}
	if err := m.reload(gen); err != nil {
		logf("manifest locate: scanning packs failed: %v", err)
		return "", false
	}
	p, ok := m.locs[id]
	return p, ok
}

// packGen fingerprints the pack set without opening anything, so a lookup that is
// going to miss costs a walk rather than a decode of every pack.
func (m *localManifests) packGen() (string, error) {
	var b strings.Builder
	err := m.eachPackPath(func(p string) error {
		gen, err := fileGeneration(p)
		if err != nil {
			return err
		}
		b.WriteString(p)
		b.WriteByte(0)
		b.WriteString(gen)
		b.WriteByte('\n')
		return nil
	})
	return b.String(), err
}

func (m *localManifests) reload(gen string) error {
	locs := map[string]string{}
	err := m.eachPackPath(func(path string) error {
		p, err := openMpack(path)
		if err != nil {
			// One damaged pack must not blind the store to the rest; fsck is what
			// reports it.
			logf("manifest locate: skipping %s: %v", path, err)
			return nil
		}
		defer p.Close()
		for _, id := range p.IDs() {
			locs[id] = path
		}
		return nil
	})
	if err != nil {
		return err
	}
	m.locs, m.gen = locs, gen
	return nil
}

func (m *localManifests) eachPackPath(fn func(path string) error) error {
	if m.archives == "" {
		return nil
	}
	err := filepath.WalkDir(m.archives, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			logf("manifest scan: skipping %s: %v", p, err)
			return nil
		}
		if d.IsDir() || filepath.Ext(p) != mpackExt {
			return nil
		}
		return fn(p)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// EachID yields every manifest id, loose ones first and then packed ones not
// already seen. Unreadable shard dirs are skipped (best-effort); a callback error
// stops the walk.
//
// Yielding each id exactly once is what keeps this usable for an index rebuild:
// during the overlap window a manifest exists both loose and packed, and counting
// it twice would double every projection built from this.
func (m *localManifests) EachID(fn func(id string) error) error {
	loose := map[string]bool{}
	if err := m.eachLooseID(func(id string) error {
		loose[id] = true
		return fn(id)
	}); err != nil {
		return err
	}
	return m.eachPackPath(func(path string) error {
		p, err := openMpack(path)
		if err != nil {
			logf("manifest scan: skipping %s: %v", path, err)
			return nil
		}
		defer p.Close()
		for _, id := range p.IDs() {
			if loose[id] {
				continue
			}
			if err := fn(id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *localManifests) eachLooseID(fn func(id string) error) error {
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
