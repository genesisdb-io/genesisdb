//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package fsutil

import "errors"

// DirectoryLock is an exclusive advisory lock on a data directory.
type DirectoryLock struct{}

// LockDirectory is not supported on this platform.
func LockDirectory(path string) (*DirectoryLock, error) {
	return nil, errors.New("locking a data directory is only supported on Linux, macOS and BSD")
}

// Unlock releases the lock.
func (lock *DirectoryLock) Unlock() error { return nil }
