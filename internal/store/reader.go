package store

import (
	"database/sql"
	"os"

	"go.inndy.tw/goxray/internal/model"
)

// Reader is a read-only view for the CLI. It opens the SQLite index in read-only
// mode so it never mutates state a running server owns, and reads manifests/
// blobs directly.
type Reader struct {
	db        *sql.DB // nil when no index exists yet
	blobs     *localBlobs
	manifests *localManifests
}

// OpenReader opens the stores read-only. A missing index (no data recorded yet)
// is not an error — List simply returns nothing.
func OpenReader(blobsDir, manifestsDir, indexPath string) (*Reader, error) {
	blobs, err := newLocalBlobs(blobsDir)
	if err != nil {
		return nil, err
	}
	r := &Reader{blobs: blobs, manifests: newLocalManifests(manifestsDir)}
	if _, err := os.Stat(indexPath); err == nil {
		db, err := sql.Open("sqlite", "file:"+indexPath+"?mode=ro&_pragma=busy_timeout(5000)")
		if err != nil {
			return nil, err
		}
		r.db = db
	}
	return r, nil
}

func (r *Reader) Close() error {
	if r.db == nil {
		return nil
	}
	return r.db.Close()
}

// List queries the index. Returns an empty slice when no index exists yet.
func (r *Reader) List(f Filter) ([]model.Exchange, error) {
	if r.db == nil {
		return nil, nil
	}
	return queryEntries(r.db, f)
}

// Sessions returns per-session rollups. Empty when no index exists yet.
func (r *Reader) Sessions(f Filter) ([]Session, error) {
	if r.db == nil {
		return nil, nil
	}
	return querySessions(r.db, f)
}

// Manifest reads one exchange's full audit record.
func (r *Reader) Manifest(id string) (*model.Exchange, error) {
	return r.manifests.Read(id)
}

// Blob returns the verbatim bytes for a blob ref.
func (r *Reader) Blob(ref string) ([]byte, error) {
	return r.blobs.Get(ref)
}
