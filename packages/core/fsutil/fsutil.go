// Package fsutil contains the durable file operations shared by the storage
// and index engines: synced appends, atomic replacement, synced truncation,
// directory syncs, line scanning and test fault injection.
package fsutil

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FaultInjector lets tests fail I/O at named points, for example
// "append:<file name>" or "shard.sync". A nil FaultInjector never fails.
// Production code never sets one.
type FaultInjector func(point string) error

// Check returns the injected error for point, or nil.
func (inject FaultInjector) Check(point string) error {
	if inject == nil {
		return nil
	}
	return inject(point)
}

// CheckCreate is called before a new file or directory is created at path.
// Existing paths never fail.
func (inject FaultInjector) CheckCreate(path string) error {
	if inject == nil {
		return nil
	}
	if _, err := os.Lstat(path); err == nil {
		return nil
	}
	return inject("create:" + path)
}

// AppendAndSync appends data to an existing file and syncs it.
func AppendAndSync(path string, data []byte, faults FaultInjector) error {
	return appendAndSync(path, data, faults, (*os.File).Sync)
}

// AppendAndSyncBeforeBarrier appends data to an existing file and hands it
// to the device, see SyncBeforeBarrier.
func AppendAndSyncBeforeBarrier(path string, data []byte, faults FaultInjector) error {
	return appendAndSync(path, data, faults, SyncBeforeBarrier)
}

func appendAndSync(path string, data []byte, faults FaultInjector, sync func(*os.File) error) error {
	name := filepath.Base(path)
	if err := faults.Check("append:" + name); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := faults.Check("sync:" + name); err != nil {
		file.Close()
		return err
	}
	if err := sync(file); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// ReplaceAtomically durably replaces path with data. A temporary file next to
// path is written and synced, renamed over path, and the directory is
// synced. A crash leaves either the complete old or the complete new file.
// beforeRename and afterRename bracket the rename, so callers can exclude
// readers for that moment only. Both may be nil.
func ReplaceAtomically(path string, data []byte, beforeRename, afterRename func(), faults FaultInjector) error {
	temporaryPath := path + ".tmp"
	if err := faults.Check("replace.write:" + filepath.Base(path)); err != nil {
		return err
	}
	if err := WriteAndSync(temporaryPath, data); err != nil {
		os.Remove(temporaryPath)
		return err
	}
	if err := faults.Check("replace.rename:" + filepath.Base(path)); err != nil {
		os.Remove(temporaryPath)
		return err
	}
	if beforeRename != nil {
		beforeRename()
	}
	err := os.Rename(temporaryPath, path)
	if afterRename != nil {
		afterRename()
	}
	if err != nil {
		os.Remove(temporaryPath)
		return err
	}
	return SyncDirectory(filepath.Dir(path))
}

// WriteAndSync creates or truncates path, writes data and syncs the file.
func WriteAndSync(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// CreateAndSync creates a new file with data. It fails if path exists. The
// file and its directory are synced.
func CreateAndSync(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return SyncDirectory(filepath.Dir(path))
}

// TruncateAndSync truncates path to size and syncs it.
func TruncateAndSync(path string, size int64) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Truncate(size); err != nil {
		return fmt.Errorf("truncate %s: %w", path, err)
	}
	return file.Sync()
}

// SyncDirectory syncs a directory, which makes created, renamed and removed
// entries durable.
func SyncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open directory %s for sync: %w", directory, err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("sync directory %s: %w", directory, err)
	}
	return nil
}

// RemoveAndSync removes path, ignoring a missing file, and syncs the parent
// directory.
func RemoveAndSync(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return SyncDirectory(filepath.Dir(path))
}

// ReadFirstLine returns the first line of a file including a trailing
// newline if present, at most 4 KiB.
func ReadFirstLine(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(io.LimitReader(file, 4096), 4096)
	line, err := reader.ReadSlice('\n')
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return nil, err
	}
	return append([]byte(nil), line...), nil
}

// VisitDirectory streams the entries of a directory in bounded batches, so
// directories with millions of entries never load into memory at once.
func VisitDirectory(directory string, visit func(name string, mode os.FileMode) error) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	for {
		entries, err := handle.ReadDir(1024)
		for _, entry := range entries {
			if visitErr := visit(entry.Name(), entry.Type()); visitErr != nil {
				return visitErr
			}
		}
		if err == io.EOF || len(entries) == 0 {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// ErrStopScan can be returned by visitors to end a scan early without error.
var ErrStopScan = errors.New("stop scan")
