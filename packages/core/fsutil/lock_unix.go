//go:build linux || darwin || freebsd || netbsd || openbsd

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// DirectoryLock is an exclusive advisory lock on a data directory. Locking
// the descriptor of an existing directory needs no spare inode, so ownership
// can be acquired even when the volume ran out of inodes.
type DirectoryLock struct {
	directory *os.File
}

// LockDirectory takes exclusive single-writer ownership of a directory. It
// fails immediately if another process holds the lock.
func LockDirectory(path string) (*DirectoryLock, error) {
	directory, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open data directory for locking: %w", err)
	}
	if err := syscall.Flock(int(directory.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		directory.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("data directory %s is already in use by another GenesisDB process; stop it before starting a new one", path)
		}
		return nil, fmt.Errorf("lock data directory %s (the filesystem must support flock): %w", path, err)
	}
	return &DirectoryLock{directory: directory}, nil
}

// Unlock releases the lock. It is safe to call more than once.
func (lock *DirectoryLock) Unlock() error {
	if lock == nil || lock.directory == nil {
		return nil
	}
	_ = syscall.Flock(int(lock.directory.Fd()), syscall.LOCK_UN)
	err := lock.directory.Close()
	lock.directory = nil
	return err
}
