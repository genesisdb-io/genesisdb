//go:build !darwin

package fsutil

import "os"

// SyncBeforeBarrier syncs file. On this platform os.File.Sync already is the
// cheapest durable flush, see sync_darwin.go for the Darwin special case.
func SyncBeforeBarrier(file *os.File) error {
	return file.Sync()
}

// SyncOrdered syncs file. See sync_darwin.go for the Darwin barrier.
func SyncOrdered(file *os.File) error {
	return file.Sync()
}
