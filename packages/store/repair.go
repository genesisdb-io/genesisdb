package store

import (
	"context"
	"errors"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// Index self-repair. The indexes are derived data: the event logs are the
// only authority. Startup validates the published index files by size and
// state journal, which catches torn and missing writes but not bytes that
// were changed in place (bit rot, a stray write, a damaged copy). Such
// damage surfaces later as core.ErrIndexCorrupt while reading. Instead of
// failing every following read until an administrator rebuilds manually,
// the store rebuilds the indexes from the event logs and retries the read
// once.
//
// A rebuild never changes events, it only replaces derived files. If the
// rebuild fails (for example because the event log itself is damaged), the
// index is marked failed and the store answers core.ErrNotReady until a
// restart, as after any other failed rebuild.

type writeLockHeldKey struct{}

// withWriteLockHeld marks ctx as used by a caller that holds writeMutex.
// Reads under such a context cannot rebuild synchronously, because the
// rebuild needs writeMutex itself. They fail with the corruption error and
// schedule the rebuild, which runs as soon as the caller releases the lock.
func withWriteLockHeld(ctx context.Context) context.Context {
	return context.WithValue(ctx, writeLockHeldKey{}, true)
}

func holdsWriteLock(ctx context.Context) bool {
	held, _ := ctx.Value(writeLockHeldKey{}).(bool)
	return held
}

// handleIndexCorruption decides how a read that failed on build reacts to
// err. It reports whether the read should run again.
func (s *Store) handleIndexCorruption(ctx context.Context, build string, err error) bool {
	if build == "" || !errors.Is(err, core.ErrIndexCorrupt) {
		return false
	}
	if holdsWriteLock(ctx) {
		go s.repairIndexAfterCorruption(build, err)
		return false
	}
	return s.repairIndexAfterCorruption(build, err)
}

// repairIndexAfterCorruption rebuilds the indexes unless another caller
// already replaced the damaged build. It reports whether a usable index is
// published afterwards. The caller must not hold writeMutex or
// maintenanceMutex.
func (s *Store) repairIndexAfterCorruption(damagedBuild string, cause error) bool {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	s.maintenanceMutex.Lock()
	defer s.maintenanceMutex.Unlock()
	if s.isClosed {
		return false
	}
	if snapshot, err := s.index.AcquireSnapshot(); err == nil && snapshot.Build() != damagedBuild {
		return true
	}
	if err := s.checkStorageHealthy(); err != nil {
		return false
	}
	s.logger.Warn("index corruption detected, rebuilding indexes from the event logs", "error", cause)
	s.observer.ErrorOccurred("index_repair")
	s.cache.Clear()
	if err := s.index.RebuildContext(context.Background()); err != nil {
		s.index.MarkFailed(err)
		s.logger.Error("index repair failed, the store is unavailable until restart", "error", err)
		return false
	}
	s.cache.Clear()
	s.logger.Info("index repair finished", "events", s.index.EventCount())
	return true
}
