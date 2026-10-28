//go:build darwin

package fsutil

import (
	"os"
	"syscall"
)

// fBarrierFullFsync is F_BARRIERFSYNC from <sys/fcntl.h>.
const fBarrierFullFsync = 85

// SyncBeforeBarrier hands the written data of file to the storage device
// without waiting for the device cache to drain.
//
// On Darwin, os.File.Sync issues F_FULLFSYNC, which flushes the whole device
// cache and costs several milliseconds per call. Files that are published
// only by a later record (index shards before their journal record) do not
// need their own device flush: the F_FULLFSYNC of that later record drains
// the device cache, including everything handed to the device before it.
// Plain fsync is therefore sufficient for them and roughly fifty times
// cheaper.
func SyncBeforeBarrier(file *os.File) error {
	for {
		err := syscall.Fsync(int(file.Fd()))
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return &os.PathError{Op: "fsync", Path: file.Name(), Err: err}
		}
		return nil
	}
}

// SyncOrdered makes the written data of file reach stable storage before any
// write issued after this call, without draining the device cache now.
//
// Use it for data that must be ordered before later writes but whose
// durability is established by a later full flush, for example the
// write-ahead log and the event log before the index journal record of the
// same commit. On Darwin it issues F_BARRIERFSYNC, about ten times cheaper
// than F_FULLFSYNC. Filesystems without barrier support fall back to
// F_FULLFSYNC.
func SyncOrdered(file *os.File) error {
	for {
		_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, file.Fd(), fBarrierFullFsync, 0)
		switch errno {
		case 0:
			return nil
		case syscall.EINTR:
			continue
		}
		return file.Sync()
	}
}
