package store

// fsyncDir is a no-op on Windows. Win32 has no way to flush a directory: the
// only candidate, FlushFileBuffers on a handle opened with
// FILE_FLAG_BACKUP_SEMANTICS (what os.Open gives for a directory), fails with
// ERROR_ACCESS_DENIED — it wants a volume handle, which needs Administrator and
// would flush the whole volume. NTFS instead journals metadata, so a rename is
// ordered after the renamed file's own FlushFileBuffers without a second call.
// Returning the error here broke every write: PersistRequest failed, and the
// proxy is fail-closed, so nothing was forwarded.
func fsyncDir(string) error { return nil }
