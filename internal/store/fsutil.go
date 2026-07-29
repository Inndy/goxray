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

// normalizeSessionID is applied once, where a session id enters the store, so
// that everything downstream of the recorder can treat it as a name.
//
// A session id is the one identifier in the store that goxray does not mint. It
// is whatever the recorded request put in metadata.session_id, and the proxy port
// has no Host guard, so it is not even reliably the user's own client's — a
// CORS-simple POST from a web page is recorded like any other request. Archives
// are found by name rather than through an index, so an id used verbatim as a
// path component lets that request choose where the archiver writes:
// "../../../x" resolves clear of ArchivesDir.
//
// Normalizing at the boundary rather than at each use is what makes the property
// structural: a stored session id *is* a usable name, so no read path has to
// remember to sanitize one. Real ids are uuids and pass through unchanged, which
// keeps archive files identifiable by eye and reversible — the filename is the
// session id. Anything else becomes a digest of itself, prefixed so it is
// obvious in `goxray ls` that the id was rewritten. The raw value is not lost:
// it is in the recorded request body, which is stored verbatim.
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

// archiveFile returns the archive file a session's bodies live in, or "" if the
// session cannot have one — no session id, or one recorded before
// normalizeSessionID existed and therefore not usable as a name. The archiver
// skips such a session and leaves its blobs live; a read reports the body as
// missing from the archive, which is what it is.
//
// This is the only place a session id becomes a path.
func archiveFile(dir, session string) string {
	if session == "" || !isPlainToken(session) {
		return ""
	}
	return shardPath(dir, session, archiveExt)
}

// winReserved are the DOS device names, which Windows still resolves ahead of
// any file in any directory, extension or not: opening "nul.gxa" opens the null
// device. Cheap to exclude, and a legitimate session id never looks like one.
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
