package store

import (
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
)

// BlobStore stores opaque byte payloads and returns an opaque ref. The MVP
// backend writes one zstd file per payload keyed by a fresh uuidv7 (no dedup);
// a future content-addressed backend can return a hash ref with no contract
// change — callers never interpret the ref.
type BlobStore interface {
	Put(b []byte) (ref string, err error)
	Get(ref string) ([]byte, error)
}

// localBlobs is the filesystem+zstd backend. EncodeAll/DecodeAll on a shared
// encoder/decoder are safe for concurrent use, so no per-call allocation.
type localBlobs struct {
	dir string
	enc *zstd.Encoder
	dec *zstd.Decoder
}

func newLocalBlobs(dir string) (*localBlobs, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	return &localBlobs{dir: dir, enc: enc, dec: dec}, nil
}

func (s *localBlobs) Put(b []byte) (string, error) {
	ref := uuid.Must(uuid.NewV7()).String()
	comp := s.enc.EncodeAll(b, nil)
	if err := writeFileSync(shardPath(s.dir, ref, ".zst"), comp); err != nil {
		return "", err
	}
	return ref, nil
}

func (s *localBlobs) Get(ref string) ([]byte, error) {
	comp, err := readFile(shardPath(s.dir, ref, ".zst"))
	if err != nil {
		return nil, err
	}
	return s.dec.DecodeAll(comp, nil)
}
