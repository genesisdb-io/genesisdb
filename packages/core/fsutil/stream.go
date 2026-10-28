package fsutil

import (
	"io"
	"os"
	"path/filepath"
)

// ReplaceStreamAtomically rewrites a file without buffering it in memory.
// The caller must serialize writers. Readers see either complete version.
func ReplaceStreamAtomically(path string, write func(io.Writer) error, beforeRename, afterRename func(), faults FaultInjector) error {
	if err := faults.Check("replace.write:" + filepath.Base(path)); err != nil {
		return err
	}
	temporaryPath := path + ".tmp"
	file, err := os.OpenFile(temporaryPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(temporaryPath)
	err = write(file)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := faults.Check("replace.rename:" + filepath.Base(path)); err != nil {
		return err
	}
	if beforeRename != nil {
		beforeRename()
	}
	err = os.Rename(temporaryPath, path)
	if afterRename != nil {
		afterRename()
	}
	if err != nil {
		return err
	}
	return SyncDirectory(filepath.Dir(path))
}
