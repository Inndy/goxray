// Package store provides goxray's durable recording layer: a zstd blob store,
// immutable per-exchange manifests (the source of truth), a rebuildable SQLite
// index, and the Recorder that sequences fail-closed commits across them.
package store

import (
	"log"
	"os"
	"path/filepath"

	"go.inndy.tw/goxray/internal/model"
)

// Store bundles the durable stores and the recorder. Manifests are the source
// of truth; the SQLite index is a rebuildable projection.
type Store struct {
	Blobs     BlobStore
	Manifests ManifestStore
	Index     *Index
	Recorder  *Recorder
}

// Open wires the stores, brings the index up to date (full rebuild on
// schema/DB loss, else tail reconcile after an unclean shutdown), and starts the
// recorder's async index pool.
func Open(blobsDir, manifestsDir, archivesDir, indexPath string, provider model.Provider, workers int) (*Store, error) {
	// Ensure the home layout exists before SQLite tries to create index.db.
	for _, d := range []string{blobsDir, manifestsDir, archivesDir, filepath.Dir(indexPath)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	blobs, err := newLocalBlobs(blobsDir, archivesDir)
	if err != nil {
		return nil, err
	}
	manifests := newLocalManifests(manifestsDir, archivesDir)

	idx, needRebuild, wasClean, err := OpenIndex(indexPath)
	if err != nil {
		return nil, err
	}
	switch {
	case needRebuild:
		if err := idx.RebuildFrom(manifests); err != nil {
			log.Printf("goxray: index rebuild failed (continuing): %v", err)
		}
	case !wasClean:
		if err := idx.Reconcile(manifests); err != nil {
			log.Printf("goxray: index reconcile failed (continuing): %v", err)
		}
	}

	rec := NewRecorder(blobs, manifests, idx, provider, workers)
	return &Store{Blobs: blobs, Manifests: manifests, Index: idx, Recorder: rec}, nil
}

// ReleaseArchive drops any handle the blob cache holds on one archive file. The
// background sweep calls it immediately before renaming a rebuilt archive over
// that path, because on Windows a file this process still has open cannot be
// renamed over — see ArchiveOptions.Release. Reads reopen it on demand, so the
// only cost is one pool decode.
func (s *Store) ReleaseArchive(path string) {
	if b, ok := s.Blobs.(*localBlobs); ok {
		b.forget(path)
	}
}

// Close drains the recorder then closes the index (marking a clean shutdown).
func (s *Store) Close() error {
	s.Recorder.Close()
	// Release any archive handles the blob store is holding open. Optional
	// rather than part of BlobStore, so a backend with nothing to release does
	// not have to say so.
	if c, ok := s.Blobs.(interface{ Close() error }); ok {
		_ = c.Close()
	}
	return s.Index.Close()
}

func logf(format string, args ...any) {
	log.Printf("goxray: "+format, args...)
}
