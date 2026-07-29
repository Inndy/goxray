package store

import (
	"errors"
	"io/fs"
	"sync"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
)

// BlobStore stores opaque byte payloads and returns an opaque ref. Live bodies
// are one zstd file per payload keyed by a fresh uuidv7 (no dedup); once a
// session goes quiet, `goxray archive` folds its bodies into a single
// per-session archive and removes the individual files.
type BlobStore interface {
	Put(b []byte) (ref string, err error)

	// Get returns the verbatim bytes for a ref. session is the locality group
	// the blob belongs to — model.Exchange.Req.SessionID — because an archived
	// body lives in a file named after its session. Passing it costs callers
	// nothing (every one of them reads the manifest first) and saves the store
	// from maintaining a ref -> archive index, which would be derived state
	// with nothing to derive it from but the archives themselves.
	//
	// An empty session means "cannot be archived", which is true by
	// construction: sessionless turns are skipped by the archiver.
	Get(session, ref string) ([]byte, error)
}

// archiveCacheMax is how many archives stay open with their pools decoded.
// Browsing is session-local, so a very small number captures nearly all of the
// benefit — and it is a large benefit: measured, resolving a turn from a
// resident pool is 0.38 ms against 10.7 ms when the pool has to be rebuilt.
const archiveCacheMax = 2

// localBlobs is the filesystem+zstd backend. EncodeAll/DecodeAll on a shared
// encoder/decoder are safe for concurrent use, so no per-call allocation.
type localBlobs struct {
	dir      string
	archives string
	enc      *zstd.Encoder
	dec      *zstd.Decoder

	// mu guards the archive cache and is held across archive reads: a reader
	// evicted and closed by another goroutine mid-read would be a use-after-
	// close on its file handle. Live .zst reads never take this lock, so the
	// only thing serialised is archived reads, which are a local dashboard
	// browsing one session.
	mu    sync.Mutex
	cache []*archiveReader // most recently used first
}

func newLocalBlobs(dir, archivesDir string) (*localBlobs, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	return &localBlobs{dir: dir, archives: archivesDir, enc: enc, dec: dec}, nil
}

func (s *localBlobs) Put(b []byte) (string, error) {
	ref := uuid.Must(uuid.NewV7()).String()
	comp := s.enc.EncodeAll(b, nil)
	if err := writeFileSync(shardPath(s.dir, ref, ".zst"), comp); err != nil {
		return "", err
	}
	return ref, nil
}

// Get tries the live file first, then the session's archive.
//
// That order is what makes archiving safe to interleave with reads: during the
// window between an archive being installed and its .zst files being unlinked
// both exist and hold identical bytes, and `--unarchive` restores the .zst
// without having to invalidate anything. The cost once a session is archived is
// one ENOENT per read.
func (s *localBlobs) Get(session, ref string) ([]byte, error) {
	comp, err := readFile(shardPath(s.dir, ref, ".zst"))
	if err == nil {
		return s.dec.DecodeAll(comp, nil)
	}
	if !errors.Is(err, fs.ErrNotExist) || session == "" || s.archives == "" {
		return nil, err
	}

	raw, aerr := s.fromArchive(session, ref)
	if aerr == nil {
		return raw, nil
	}
	// No archive, or an intact archive that simply does not hold this ref:
	// report the missing blob rather than the missing archive, which is what
	// the caller asked about. Anything else — a corrupt or unreadable archive —
	// is a real failure and must not masquerade as "not recorded".
	if errors.Is(aerr, fs.ErrNotExist) {
		return nil, err
	}
	return nil, aerr
}

func (s *localBlobs) fromArchive(session, ref string) ([]byte, error) {
	path := archiveFile(s.archives, session)
	if path == "" {
		return nil, errNotInArchive
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.openLocked(path)
	if err != nil {
		return nil, err
	}
	return a.Get(ref)
}

// openLocked returns a cached reader for path, reopening it if the file behind
// it has been replaced. A resumed session is re-archived by renaming a new file
// over the old one, so a cached reader can otherwise keep serving an inode that
// no longer has the turns the caller is asking for.
func (s *localBlobs) openLocked(path string) (*archiveReader, error) {
	for i, a := range s.cache {
		if a.path != path {
			continue
		}
		s.cache = append(s.cache[:i], s.cache[i+1:]...)
		if a.stale() {
			a.Close()
			break
		}
		s.cache = append([]*archiveReader{a}, s.cache...)
		return a, nil
	}
	a, err := openArchive(path)
	if err != nil {
		return nil, err
	}
	s.cache = append([]*archiveReader{a}, s.cache...)
	for len(s.cache) > archiveCacheMax {
		s.cache[len(s.cache)-1].Close()
		s.cache = s.cache[:len(s.cache)-1]
	}
	return a, nil
}

// Close releases cached archive handles. Safe to call more than once.
func (s *localBlobs) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.cache {
		a.Close()
	}
	s.cache = nil
	return nil
}
