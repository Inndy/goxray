package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.inndy.tw/goxray/internal/model"
)

// FsckOptions configures an integrity check.
type FsckOptions struct {
	BlobsDir     string
	ManifestsDir string
	ArchivesDir  string
	Session      string // restrict to one session; empty checks everything
}

// archivedRef is a body proven intact inside one particular session's archive.
type archivedRef struct {
	session string // the archive's own name, which is the session id it holds
	ref     string
}

// FsckMissing is a manifest that points at a body nothing can produce.
type FsckMissing struct {
	Exchange string
	Session  string
	Side     string // "request" or "response"
	Ref      string
	Err      string
}

// FsckStats reports what the check found.
type FsckStats struct {
	Archives  int
	Chunks    int
	Bodies    int
	Manifests int
	Problems  []string // archive-level failures
	Missing   []FsckMissing
}

// Fsck verifies every archive against the content hashes it carries, then
// cross-checks the manifests: every recorded body must be produced by either a
// live blob or an archive.
//
// Content addressing is what makes this possible at all — an archive can prove
// its own chunks are the bytes that went in, so damage surfaces as a reported
// failure rather than as a body that silently reads back wrong. For a tool
// whose entire purpose is a faithful record, detectability is the property that
// matters most.
func Fsck(o FsckOptions) (FsckStats, error) {
	var st FsckStats

	blobs, err := newLocalBlobs(o.BlobsDir, o.ArchivesDir)
	if err != nil {
		return st, err
	}
	defer blobs.Close()

	// Pass 1: verify archives, and remember which refs they hold so the
	// manifest cross-check does not have to re-decode a pool per manifest.
	//
	// Keyed by (session, ref), not by ref alone. A ref proven to be intact
	// *somewhere* is not the property a manifest needs: reads resolve a body
	// through its own session's archive path, so an archive moved or renamed
	// under another session holds refs nothing can reach. Keying globally passed
	// that store as clean while every read from it failed.
	archived := map[archivedRef]bool{}
	err = filepath.WalkDir(o.ArchivesDir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || filepath.Ext(p) != archiveExt {
			return nil //nolint:nilerr // an unreadable shard is reported below, not fatal
		}
		sid := d.Name()[:len(d.Name())-len(archiveExt)]
		if o.Session != "" && sid != o.Session {
			return nil
		}
		st.Archives++
		// A read finds an archive by computing archiveFile() from the session id on
		// the manifest, so a file whose own name does not produce its own path
		// holds bodies no read can reach: an id recorded before normalization, or a
		// file moved between shards by hand. Verify it anyway — the bytes are
		// intact and `--unarchive` can still get them out, which is worth saying —
		// but do not credit its refs. Crediting them is how a store where every
		// read failed came out of pass 2 looking clean.
		reachable := archiveFile(o.ArchivesDir, sid) == filepath.Clean(p)
		if !reachable {
			st.Problems = append(st.Problems, fmt.Sprintf(
				"%s: no session id resolves to this file, so nothing can read it; recover it with `goxray archive --unarchive --apply`", sid))
		}
		a, err := openArchive(p)
		if err != nil {
			st.Problems = append(st.Problems, fmt.Sprintf("%s: %v", sid, err))
			return nil
		}
		defer a.Close()
		chunks, err := a.Verify()
		st.Chunks += chunks
		if err != nil {
			st.Problems = append(st.Problems, fmt.Sprintf("%s: %v", sid, err))
			return nil
		}
		if !reachable {
			return nil
		}
		for _, ref := range a.Refs() {
			archived[archivedRef{session: sid, ref: ref}] = true
			st.Bodies++
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return st, err
	}

	// Pass 2: every manifest's bodies must be reachable. A ref already verified
	// in pass 1 is known good; anything else has to be read to prove it.
	manifests := newLocalManifests(o.ManifestsDir)
	err = manifests.EachID(func(id string) error {
		ex, err := manifests.Read(id)
		if err != nil {
			st.Problems = append(st.Problems, fmt.Sprintf("%s: unreadable manifest: %v", id, err))
			return nil
		}
		if o.Session != "" && ex.Req.SessionID != o.Session {
			return nil
		}
		st.Manifests++
		// An inflight turn legitimately has no response body yet.
		check := []struct {
			side string
			ref  string
		}{{"request", ex.ReqRef}, {"response", ex.ResRef}}
		for _, c := range check {
			if c.ref == "" || archived[archivedRef{session: ex.Req.SessionID, ref: c.ref}] {
				continue
			}
			if _, err := blobs.Get(ex.Req.SessionID, c.ref); err != nil {
				if errors.Is(err, fs.ErrNotExist) && ex.State == model.StateInflight {
					continue
				}
				st.Missing = append(st.Missing, FsckMissing{
					Exchange: id, Session: ex.Req.SessionID, Side: c.side, Ref: c.ref, Err: err.Error(),
				})
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return st, err
	}
	return st, nil
}
