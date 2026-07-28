//go:build !windows

package store

import "os"

// fsyncDir flushes a directory's metadata so a rename/create is durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
