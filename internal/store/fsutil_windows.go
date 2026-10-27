package store

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// fsyncDir is a no-op on Windows. Win32 has no way to flush a directory: the
// only candidate, FlushFileBuffers on a handle opened with
// FILE_FLAG_BACKUP_SEMANTICS (what os.Open gives for a directory), fails with
// ERROR_ACCESS_DENIED — it wants a volume handle, which needs Administrator and
// would flush the whole volume. NTFS instead journals metadata, so a rename is
// ordered after the renamed file's own FlushFileBuffers without a second call.
// Returning the error here broke every write: PersistRequest failed, and the
// proxy is fail-closed, so nothing was forwarded.
func fsyncDir(string) error { return nil }

// renameOver is os.Rename with a short, bounded retry. Go opens files without
// FILE_SHARE_DELETE, so while any reader holds the target open — the dashboard
// reading the manifest being superseded, an indexer, antivirus — a rename onto
// it fails with ERROR_ACCESS_DENIED or a sharing violation. Those readers hold
// it for microseconds; without the retry a turn's final manifest write failed,
// and the proxy, being fail-closed, dropped the connection.
func renameOver(from, to string) error {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.Rename(from, to); err == nil || !transientRenameErr(err) {
			return err
		}
		time.Sleep(time.Duration(i+1) * 5 * time.Millisecond)
	}
	return err
}

func transientRenameErr(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	const errorSharingViolation = 32
	return errno == syscall.ERROR_ACCESS_DENIED || errno == errorSharingViolation
}
