package store

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// writeFileSync durably writes data to path: temp file in the same dir, fsync
// the data, atomic rename, then fsync the directory so the new dirent survives
// a crash (on Linux, fsyncing the file alone does not persist its directory
// entry). This is the durability primitive behind every blob and manifest write.
func writeFileSync(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := ensureDir(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before rename.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return fsyncDir(dir)
}

// ensureDir creates dir if absent and fsyncs its parent. A freshly created
// shard dir is itself a dirent in its parent; without fsyncing the parent, a
// crash can drop the whole directory — and with it a manifest PersistRequest
// already reported durable.
func ensureDir(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return fsyncDir(filepath.Dir(dir))
}

// readFile reads a whole file (thin wrapper for symmetry with writeFileSync).
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// shardPath returns dir/<shard>/<id><ext>, sharding by the LAST two chars of
// the id so no single directory accumulates 100k+ entries. The tail is used
// because uuidv7 ids lead with a (near-constant) timestamp and carry their
// entropy in the trailing bytes — sharding on the prefix would give ~no fanout.
//
// id must be a name this package minted or has checked: filepath.Join cleans
// the result, so a "../" in id escapes dir entirely.
func shardPath(dir, id, ext string) string {
	shard := id
	if len(id) >= 2 {
		shard = id[len(id)-2:]
	}
	return filepath.Join(dir, shard, id+ext)
}

// normalizeSessionID is applied once, where a session id enters the store.
//
// A session id is the one identifier in the store that goxray does not mint: it
// is whatever the recorded request put in metadata.session_id, and the proxy
// port has no Host guard, so it is not even reliably the user's own client's — a
// CORS-simple POST from a web page is recorded like any other request.
//
// It is no longer what keeps such an id out of the filesystem; archiveFile does
// that, for every id, by construction. What is left is narrower and not a safety
// property: a recorded id can be 46 KB of anything, and it is carried in an index
// column and in the dashboard's URL path. Real ids are uuids and pass through
// unchanged. Anything else becomes a digest of itself, prefixed so it is obvious
// in `goxray ls` that the id was rewritten; the raw value is still in the
// recorded request body, which is stored verbatim.
//
// Rewriting rather than blanking keeps two unrelated things honest: distinct
// sessions stay distinct (a lossy substitution would merge them into one
// archive), and an empty session id already means "subagent turn" to
// wire.ParseRequest.
func normalizeSessionID(session string) string {
	if session == "" || isPlainToken(session) {
		return session
	}
	sum := sha256.Sum256([]byte(session))
	return "h-" + hex.EncodeToString(sum[:16])
}

// archiveNamePrefix marks a filename as one this package derived, so that a file
// still named after its session — written before the digest naming, or moved by
// hand — is recognisable as needing migration rather than merely unfamiliar.
const archiveNamePrefix = "s-"

// archiveName is the filename stem for a session's archive.
//
// It is a digest, and the point is that it is *total*: every byte string has
// exactly one valid name, so nothing about a recorded session id has to be
// checked, trusted, or rejected before it becomes a path. The predicate this
// replaces was the whole hazard — it had to be right, it had to be applied at
// the one boundary that mattered, and the ids it refused could not be archived
// at all.
//
// A digest is only affordable because a session's archive is still found by
// computing this rather than by consulting a mapping: reads stay index-free and
// installing a rebuilt archive stays one atomic rename over a known path. What
// it costs is a filename nobody can read, which the archive pays back by
// recording its own session id (see archiveReader.Session).
func archiveName(session string) string {
	sum := sha256.Sum256([]byte(session))
	return archiveNamePrefix + hex.EncodeToString(sum[:16])
}

// isArchiveName reports whether a filename stem is one archiveName produced.
// Only fsck asks, to tell a pre-migration file from a current one.
func isArchiveName(stem string) bool {
	if len(stem) != len(archiveNamePrefix)+32 || !strings.HasPrefix(stem, archiveNamePrefix) {
		return false
	}
	_, err := hex.DecodeString(stem[len(archiveNamePrefix):])
	return err == nil
}

// archiveFile returns the archive file a session's bodies live in, or "" for a
// sessionless turn.
//
// The one empty case is about grouping, not safety: a turn with no session id
// has no locality group to be folded into, and collecting every such turn into
// one shared file would give the store its first archive that is not per-session
// — unbounded, rebuilt on every pass, and shared blast radius. They stay live.
//
// This is the only place a session id becomes a path.
func archiveFile(dir, session string) string {
	if session == "" {
		return ""
	}
	return shardPath(dir, archiveName(session), archiveExt)
}

// mpackNamePrefix distinguishes a manifest pack from the body archive of the same
// session: both are digests of the same string, so only the prefix and extension
// tell them apart.
const mpackNamePrefix = "m-"

// mpackFile returns the file a session's manifests live in, or "" for a
// sessionless turn — which, having no group to be folded into, keeps its loose
// manifest for the same reason it keeps its live blobs.
//
// It sits beside the session's archive rather than inside it. Manifests are the
// audit record and the mutable half of the store; sharing a file would make a
// damaged pool cost the record too, and make `migrate-usage` rewriting one field
// rebuild the whole body pool. See mpack.go.
func mpackFile(dir, session string) string {
	if session == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(session))
	return shardPath(dir, mpackNamePrefix+hex.EncodeToString(sum[:16]), mpackExt)
}

// winReserved are the DOS device names, which Windows still resolves ahead of
// any file in any directory, extension or not: opening "nul.zst" opens the null
// device. Cheap to exclude, and a legitimate ref never looks like one.
var winReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// isPlainToken reports whether s is safe to use as one path component on every
// platform goxray builds for: bounded, no separators, no "." or "..", not hidden
// (so it cannot collide with the lock file or a build temp file), and not a
// Windows device name.
func isPlainToken(s string) bool {
	if s == "" || len(s) > 64 || s[0] == '.' || s[len(s)-1] == '.' {
		return false // a trailing dot is dropped by Windows, so "a." would alias "a"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	stem := s
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	return !winReserved[strings.ToLower(stem)]
}
