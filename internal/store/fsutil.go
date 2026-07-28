package store

import (
	"os"
	"path/filepath"
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

// fsyncDir flushes a directory's metadata so a rename/create is durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// shardPath returns dir/<shard>/<id><ext>, sharding by the LAST two chars of
// the id so no single directory accumulates 100k+ entries. The tail is used
// because uuidv7 ids lead with a (near-constant) timestamp and carry their
// entropy in the trailing bytes — sharding on the prefix would give ~no fanout.
func shardPath(dir, id, ext string) string {
	shard := id
	if len(id) >= 2 {
		shard = id[len(id)-2:]
	}
	return filepath.Join(dir, shard, id+ext)
}
