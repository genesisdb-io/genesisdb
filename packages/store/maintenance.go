package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/index"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/storage"
)

// RebuildIndexes rebuilds every index file in place from the event logs and
// returns the number of indexed events. Reads and writes wait meanwhile. If
// it fails, the store stays unavailable until a restart recovers it. A
// rebuild never clears a storage failure: after one, only a restart that
// replays the write-ahead log makes the store writable again.
func (s *Store) RebuildIndexes(ctx context.Context) (int64, error) {
	if err := acquireContext(ctx, s.writeMutex.TryLock); err != nil {
		return 0, err
	}
	defer s.writeMutex.Unlock()
	if err := acquireContext(ctx, s.maintenanceMutex.TryLock); err != nil {
		return 0, err
	}
	defer s.maintenanceMutex.Unlock()
	if s.isClosed {
		return 0, core.ErrClosed
	}
	if err := s.checkStorageHealthy(); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.cache.Clear()
	if err := s.index.RebuildContext(ctx); err != nil {
		s.index.MarkFailed(err)
		return 0, err
	}
	s.cache.Clear()
	return s.index.EventCount(), nil
}

// VerifyHashChain checks the hash chain of the authoritative event log.
func (s *Store) VerifyHashChain(ctx context.Context) (*storage.HashChainReport, error) {
	if err := acquireContext(ctx, s.writeMutex.TryLock); err != nil {
		return nil, err
	}
	defer s.writeMutex.Unlock()
	if err := s.checkWritable(); err != nil {
		return nil, err
	}
	s.maintenanceMutex.RLock()
	defer s.maintenanceMutex.RUnlock()
	return s.storage.VerifyHashChainContext(ctx)
}

// Backup returns a logical backup. Payloads stored as reference data are
// exported only as reference data, never embedded in event data. The
// extended form is chosen when reference data or system events exist.
func (s *Store) Backup(ctx context.Context) (*core.Backup, error) {
	if err := acquireContext(ctx, s.writeMutex.TryLock); err != nil {
		return nil, err
	}
	defer s.writeMutex.Unlock()
	if err := acquireContext(ctx, s.maintenanceMutex.TryRLock); err != nil {
		return nil, err
	}
	defer s.maintenanceMutex.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.checkWritable(); err != nil {
		return nil, err
	}
	snapshot, err := s.index.AcquireSnapshot()
	if err != nil {
		return nil, err
	}
	snapshot = snapshot.WithContext(ctx)
	set := index.NewPostingSetContext(ctx)
	if err := s.index.Select(snapshot, core.EventSelector{All: true}, set); err != nil {
		return nil, err
	}
	events, err := s.loadEventsForPostings(ctx, set.InLogOrder(), false)
	if err != nil {
		return nil, err
	}
	backup := &core.Backup{Events: events}
	for _, event := range events {
		if event.HasReferenceData() {
			backup.Extended = true
			break
		}
	}
	systemEvents, err := s.storage.ListSystemEventsContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup system events: %w", err)
	}
	if len(systemEvents) > 0 {
		backup.Extended = true
	}
	if backup.Extended {
		backup.SystemEvents = systemEvents
		referenceData, err := s.storage.ListReferenceDataContext(ctx)
		if err != nil {
			return nil, fmt.Errorf("backup reference data: %w", err)
		}
		backup.ReferenceData = referenceData
	}
	limits := core.ReadLimitsFromContext(ctx)
	records := len(backup.Events) + len(backup.SystemEvents) + len(backup.ReferenceData)
	var size int64
	for _, section := range [][]*core.Event{backup.Events, backup.SystemEvents} {
		for _, event := range section {
			size += int64(len(event.Data) + len(event.Subject) + len(event.Type) + len(event.Source) + 512)
		}
	}
	for _, entry := range backup.ReferenceData {
		size += int64(len(entry.Data) + len(entry.Key) + 128)
	}
	withinBudget := func() bool {
		return (limits.MaxEvents <= 0 || records <= limits.MaxEvents) && (limits.MaxBytes <= 0 || size <= limits.MaxBytes)
	}
	if !withinBudget() {
		return nil, core.ErrResourceLimit
	}
	files, err := os.ReadDir(s.SchemasDirectory())
	if err != nil {
		return nil, err
	}
	backup.Schemas = map[string]json.RawMessage{}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		if !file.Type().IsRegular() {
			return nil, fmt.Errorf("schema %s is not a regular file", file.Name())
		}
		info, err := file.Info()
		if err != nil {
			return nil, err
		}
		if info.Size() > 1024*1024 {
			return nil, fmt.Errorf("schema %s exceeds 1 MiB", file.Name())
		}
		records++
		size += info.Size() + int64(len(file.Name()))
		if !withinBudget() {
			return nil, core.ErrResourceLimit
		}
		content, err := os.ReadFile(filepath.Join(s.SchemasDirectory(), file.Name()))
		if err != nil {
			return nil, err
		}
		if !json.Valid(content) {
			return nil, fmt.Errorf("schema %s contains invalid JSON", file.Name())
		}
		backup.Schemas[file.Name()] = content
	}
	if len(backup.Schemas) > 0 {
		backup.Extended = true
	}
	if err := storage.ValidateBackup(backup); err != nil {
		return nil, err
	}
	return backup, ctx.Err()
}

// Restore writes a backup into an empty store and rebuilds the indexes.
// Event IDs and times are preserved, the hash chain is recomputed. A store
// that holds any event, reference data or system event is rejected with
// core.ErrNotEmpty, an invalid backup with core.ErrInvalidBackup. In both
// cases nothing is written.
func (s *Store) Restore(ctx context.Context, backup *core.Backup) error {
	if err := acquireContext(ctx, s.writeMutex.TryLock); err != nil {
		return err
	}
	defer s.writeMutex.Unlock()
	if err := acquireContext(ctx, s.maintenanceMutex.TryLock); err != nil {
		return err
	}
	defer s.maintenanceMutex.Unlock()
	if err := s.checkWritable(); err != nil {
		return err
	}
	isEmpty, err := s.isEmptyWhileLocked()
	if err != nil {
		return err
	}
	if !isEmpty {
		return core.ErrNotEmpty
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := storage.ValidateBackup(backup); err != nil {
		return err
	}
	if len(backup.Schemas) > 0 {
		existing, err := os.ReadDir(s.SchemasDirectory())
		if err != nil {
			return err
		}
		if len(existing) > 0 {
			return fmt.Errorf("%w: schema directory is not empty", core.ErrNotEmpty)
		}
	}
	// The durable marker makes an interrupted restore visible on restart.
	// If writing it fails, nothing changed and the store stays usable.
	if err := s.index.MarkRestoreStarted(); err != nil {
		return fmt.Errorf("restore could not start, store unchanged: %w", err)
	}
	s.cache.Clear()
	if err := s.storage.WriteBackup(backup); err != nil {
		s.index.MarkFailed(err)
		return fmt.Errorf("restore failed, store left in maintenance state (restart refuses to serve this data directory): %w", err)
	}
	for name, content := range backup.Schemas {
		if err := fsutil.ReplaceAtomically(filepath.Join(s.SchemasDirectory(), name), content, nil, nil, s.faults); err != nil {
			s.index.MarkFailed(err)
			return fmt.Errorf("restore schema failed: %w", err)
		}
	}
	if err := s.index.Rebuild(); err != nil {
		s.index.MarkFailed(err)
		return fmt.Errorf("restore index rebuild failed: %w", err)
	}
	s.cache.Clear()
	return nil
}

func (s *Store) isEmptyWhileLocked() (bool, error) {
	if s.index.EventCount() > 0 {
		return false, nil
	}
	return s.storage.IsEmpty()
}

// ListReferenceData returns every reference data entry.
func (s *Store) ListReferenceData() ([]*core.ReferenceDataEntry, error) {
	s.maintenanceMutex.RLock()
	defer s.maintenanceMutex.RUnlock()
	return s.storage.ListReferenceData()
}

// LoadReferenceData returns the payload stored under key.
func (s *Store) LoadReferenceData(key string) (json.RawMessage, error) {
	s.maintenanceMutex.RLock()
	defer s.maintenanceMutex.RUnlock()
	return s.storage.LoadReferenceData(key)
}

// ReplaceReferenceData replaces several reference data payloads with one
// atomic rewrite and returns the keys that existed. Readers are excluded
// only while the new file becomes visible. The cache is cleared.
func (s *Store) ReplaceReferenceData(ctx context.Context, updates map[string]json.RawMessage) (map[string]bool, error) {
	if err := acquireContext(ctx, s.writeMutex.TryLock); err != nil {
		return nil, err
	}
	defer s.writeMutex.Unlock()
	if err := s.checkWritable(); err != nil {
		return nil, err
	}
	return s.storage.ReplaceReferenceData(updates, s.maintenanceMutex.Lock, func() {
		s.cache.Clear()
		s.maintenanceMutex.Unlock()
	})
}

// ErasureSystemEventType is the type of the system event that records an
// erasure.
const ErasureSystemEventType = "io.genesisdb.system.ref-erased"

// ErrNothingToErase reports a subject whose events exist but hold their
// payload inline. Inline event data is immutable and cannot be erased.
var ErrNothingToErase = errors.New("subject has no erasable reference data, inline event data is immutable")

// EraseReport describes an erasure.
type EraseReport struct {
	// ErasedEntries is the number of reference data entries that were
	// redacted.
	ErasedEntries int
}

// Erase replaces complete reference payloads below subject with {}.
// Requested and completed audit records distinguish interrupted erasures.
// Inline data, metadata, external backups and physical disk remnants are not
// erased. Applications must manage those separately.
//
// Erasure is a write operation whose scope is the subject, not a response
// to a client, so the selection read budget of ctx does not apply: a subject
// with more events than one fetch may return is still erased completely.
// Payloads are never loaded, only reference keys are collected from the
// event records, and the reference log is scanned twice in streaming mode
// (once to check the keys exist, once to rewrite it). Cancellation of ctx
// is observed until the rewrite starts.
func (s *Store) Erase(ctx context.Context, subject string) (*EraseReport, error) {
	if err := acquireContext(ctx, s.writeMutex.TryLock); err != nil {
		return nil, err
	}
	defer s.writeMutex.Unlock()
	if err := s.checkWritable(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	unbounded := withWriteLockHeld(core.WithReadLimits(ctx, core.ReadLimits{}))
	keys := map[string]struct{}{}
	eventCount := 0
	err := s.withIndexSnapshot(unbounded, func(snapshot indexSnapshot) error {
		set := index.NewPostingSetContext(unbounded)
		if err := s.index.Select(snapshot.WithContext(unbounded), core.EventSelector{SubjectsWithDescendants: []string{subject}}, set); err != nil {
			return err
		}
		postings := set.InLogOrder()
		eventCount = len(postings)
		if eventCount == 0 {
			return nil
		}
		reader := s.storage.NewRecordReader()
		defer reader.Close()
		for _, posting := range postings {
			if err := ctx.Err(); err != nil {
				return err
			}
			event, err := reader.Read(posting.Location())
			if err != nil {
				return fmt.Errorf("%w: %v", core.ErrIndexCorrupt, err)
			}
			if !posting.Matches(event) {
				return fmt.Errorf("%w: record %s@%d does not match its index posting", core.ErrIndexCorrupt, posting.LogFile, posting.Offset)
			}
			if event.HasReferenceData() {
				keys[event.ReferenceDataKey] = struct{}{}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if eventCount == 0 {
		return nil, fmt.Errorf("subject %s: %w", subject, core.ErrNotFound)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("subject %s: %w", subject, ErrNothingToErase)
	}
	s.maintenanceMutex.RLock()
	existing, err := s.storage.ExistingReferenceDataKeys(ctx, keys)
	s.maintenanceMutex.RUnlock()
	if err != nil {
		return nil, err
	}
	updates := make(map[string]json.RawMessage, len(keys))
	for key := range keys {
		if !existing[key] {
			return nil, fmt.Errorf("reference data %s: %w", key, core.ErrNotFound)
		}
		// Keys can contain personal data too. Remove the entire payload.
		updates[key] = core.EmptyEventData()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Remove residual prepared payloads before reporting successful erasure.
	if err := s.storage.ClearWriteAheadLog(); err != nil {
		return nil, err
	}
	if _, err := s.storage.AppendSystemEvent(ErasureSystemEventType+".requested", "/", core.EmptyEventData()); err != nil {
		return nil, err
	}
	replaced, err := s.storage.ReplaceReferenceDataContext(ctx, updates, s.maintenanceMutex.Lock, func() {
		s.cache.Clear()
		s.maintenanceMutex.Unlock()
	})
	if err != nil {
		return nil, fmt.Errorf("erasure requested but payload replacement failed: %w", err)
	}
	if len(replaced) != len(updates) {
		return nil, fmt.Errorf("erasure incomplete: replaced %d of %d payloads", len(replaced), len(updates))
	}
	// A completion record is only written after the replacement is durable.
	if _, err := s.storage.AppendSystemEvent(ErasureSystemEventType, "/", core.EmptyEventData()); err != nil {
		return nil, fmt.Errorf("payloads erased but completion audit failed: %w", err)
	}
	return &EraseReport{ErasedEntries: len(replaced)}, nil
}
