// Package store is the GenesisDB facade. It wires the storage engine, the
// index engine and the cache together and adds commits with preconditions,
// fetches, subscriptions, backups, restores and erasure.
//
//	s, err := store.Open(directory, store.Options{})
//	defer s.Close()
//	err = s.Commit(ctx, events, preconditions...)
//	events, err := s.Fetch(ctx, "/customer", store.FetchOptions{})
//
// A Store owns its data directory exclusively: a second process that opens
// the same directory is rejected.
package store

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/cache"
	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
	"github.com/genesisdb-io/genesisdb/v2/packages/index"
	"github.com/genesisdb-io/genesisdb/v2/packages/storage"
)

// FormatPolicy decides what happens to a data directory in the legacy
// storage format (GenesisDB 1.1.6 and earlier).
type FormatPolicy int

const (
	// ConvertToCurrentFormat converts a legacy directory once on open. This
	// is the default: a 1.1.6 data directory is a drop-in input, the
	// migration is automatic, crash safe and resumable.
	ConvertToCurrentFormat FormatPolicy = iota
	// KeepExistingFormat opens a legacy directory in the legacy format
	// without renaming anything. Intended for inspection and for the
	// compatibility tests, not for operation.
	KeepExistingFormat
)

// ParseFormatPolicy parses "auto" (convert on open, the default) or "keep".
func ParseFormatPolicy(name string) (FormatPolicy, error) {
	switch name {
	case "", "auto", "current":
		return ConvertToCurrentFormat, nil
	case "keep", "legacy":
		return KeepExistingFormat, nil
	}
	return ConvertToCurrentFormat, fmt.Errorf("unknown storage format policy %q (expected auto or keep)", name)
}

// Options configure a store.
type Options struct {
	// CacheSize is the number of loaded events kept in memory. 0 uses the
	// default (10000), a negative value disables the cache. The cache never
	// changes results, it only saves reads.
	CacheSize int
	// FormatPolicy decides whether legacy directories are converted.
	FormatPolicy FormatPolicy
	// EvaluateGDBQLPrecondition evaluates isQueryResultTrue preconditions.
	// Pass query.EvaluatePrecondition. Without it such preconditions fail.
	EvaluateGDBQLPrecondition core.GDBQLPreconditionEvaluator
	// Observer receives metrics signals. Defaults to core.NoopObserver.
	Observer core.Observer
	// Logger receives operational messages. Defaults to slog.Default().
	Logger *slog.Logger
	// Faults injects I/O failures in tests. Leave nil in production.
	Faults fsutil.FaultInjector
	// Signer signs every new event with an Ed25519 key. Nil commits
	// unsigned events exactly as without signing support. Directories kept
	// in the legacy format are never signed.
	Signer *core.EventSigner
	// TrustedSigningKeys pins the public keys the audit verifies signatures
	// with. Empty uses the keys recorded in the data directory, which
	// detects damage but not a writer of the directory who adds a key.
	TrustedSigningKeys []core.SigningPublicKey
	// RevokedSigningKeys withdraws trust from compromised keys. They are
	// configured, not stored in the data directory, so a writer of the
	// directory cannot lift a revocation. Signer must not be revoked.
	RevokedSigningKeys []core.SigningKeyRevocation
}

// Store is an open GenesisDB data directory. It is safe for concurrent use.
//
// Locking: writeMutex serializes every writer (commits, system events,
// reference data updates, maintenance). maintenanceMutex is held shared by
// readers and exclusively by maintenance (rebuild, restore, close), so
// readers never see a partly rebuilt index. Maintenance always takes
// writeMutex before maintenanceMutex.
type Store struct {
	dataDirectory string
	format        layout.Format
	lock          *fsutil.DirectoryLock
	storage       *storage.Engine
	index         *index.Index
	cache         *cache.Cache[core.RecordLocation, *core.Event]
	observer      core.Observer
	logger        *slog.Logger
	faults        fsutil.FaultInjector

	evaluateGDBQLPrecondition core.GDBQLPreconditionEvaluator
	signer                    *core.EventSigner
	trustedSigningKeys        []core.SigningPublicKey
	revokedSigningKeys        core.SigningKeyRevocations

	writeMutex       sync.Mutex
	maintenanceMutex sync.RWMutex
	isClosed         bool
}

// Open opens a data directory, creating it if needed. It acquires exclusive
// ownership, detects and if requested converts the storage format, recovers
// the write-ahead log and validates or rebuilds the indexes. Any failure is
// returned before the store is usable.
func Open(dataDirectory string, options Options) (*Store, error) {
	if err := os.MkdirAll(dataDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}
	lock, err := fsutil.LockDirectory(dataDirectory)
	if err != nil {
		return nil, err
	}
	s := &Store{dataDirectory: dataDirectory, lock: lock, observer: options.Observer, logger: options.Logger, faults: options.Faults}
	if s.observer == nil {
		s.observer = core.NoopObserver{}
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	s.evaluateGDBQLPrecondition = options.EvaluateGDBQLPrecondition
	s.signer = options.Signer
	s.trustedSigningKeys = options.TrustedSigningKeys
	if _, err := core.NewSigningKeySet(s.trustedSigningKeys); err != nil {
		lock.Unlock()
		return nil, err
	}
	if s.revokedSigningKeys, err = core.NewSigningKeyRevocations(options.RevokedSigningKeys); err != nil {
		lock.Unlock()
		return nil, err
	}
	if s.signer != nil && s.revokedSigningKeys.IsRevoked(s.signer.PublicKey().KeyID) {
		lock.Unlock()
		return nil, fmt.Errorf("%w: the signing key %s is revoked, configure a new key", core.ErrInvalidSigningKey, s.signer.PublicKey().KeyID)
	}
	if options.CacheSize >= 0 {
		s.cache = cache.New[core.RecordLocation, *core.Event](cache.Options[*core.Event]{
			MaxEntries:   options.CacheSize,
			Clone:        (*core.Event).Clone,
			OnHit:        s.observer.CacheHit,
			OnMiss:       s.observer.CacheMiss,
			OnSizeChange: s.observer.CacheSizeChanged,
		})
	}
	if err := s.open(options.FormatPolicy); err != nil {
		lock.Unlock()
		return nil, err
	}
	return s, nil
}

func (s *Store) open(policy FormatPolicy) error {
	format, err := s.prepareStorageFormat(policy)
	if err != nil {
		return err
	}
	s.format = format
	if err := os.MkdirAll(s.SchemasDirectory(), 0o700); err != nil {
		return fmt.Errorf("failed to create schemas directory: %w", err)
	}
	s.index = index.New(s.dataDirectory, nil, index.Options{Format: format, Logger: s.logger, Faults: s.faults})
	// Reject future index formats before anything is written.
	if err := s.index.CheckCompatibility(); err != nil {
		return err
	}
	// The public key is recorded before the first signed write, so every
	// signature in the directory names a recorded key.
	if s.signer != nil {
		if format != layout.FormatCurrent {
			s.logger.Warn("signing key ignored: directories kept in the legacy storage format are not signed")
		} else if err := s.registerSigningKeys([]core.SigningPublicKey{s.signer.PublicKey()}); err != nil {
			return fmt.Errorf("failed to record the signing key: %w", err)
		}
	}
	engine, err := storage.Open(s.dataDirectory, storage.Options{Format: format, Logger: s.logger, Faults: s.faults, Signer: s.signer})
	if err != nil {
		return err
	}
	s.storage = engine
	s.index = index.New(s.dataDirectory, engine, index.Options{Format: format, Logger: s.logger, Faults: s.faults})
	return s.index.Open()
}

// Close releases the data directory. Running writers finish first.
func (s *Store) Close() error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	s.maintenanceMutex.Lock()
	defer s.maintenanceMutex.Unlock()
	if s.isClosed {
		return nil
	}
	s.isClosed = true
	s.index.Close()
	s.cache.Clear()
	return s.lock.Unlock()
}

// IsReady reports whether the store serves reads and writes.
func (s *Store) IsReady() bool { return s.index.IsReady() && s.storage.Failure() == nil }

// DataDirectory returns the data directory of the store.
func (s *Store) DataDirectory() string { return s.dataDirectory }

// SchemasDirectory returns the directory that holds event schemas.
func (s *Store) SchemasDirectory() string {
	return s.dataDirectory + string(os.PathSeparator) + layout.SchemasDirectory
}

// StorageFormat returns the storage format of the open data directory.
func (s *Store) StorageFormat() layout.Format { return s.format }

// ClearCache drops every cached event.
func (s *Store) ClearCache() { s.cache.Clear() }

// CountEvents returns the number of committed regular events.
func (s *Store) CountEvents() (int64, error) {
	s.maintenanceMutex.RLock()
	defer s.maintenanceMutex.RUnlock()
	if _, err := s.index.AcquireSnapshot(); err != nil {
		return 0, err
	}
	return s.index.EventCount(), nil
}

// EventLogSizeBytes returns the indexed size of all regular event logs.
func (s *Store) EventLogSizeBytes() int64 {
	var total int64
	for _, logFile := range s.index.IndexedEventLogs() {
		total += s.index.CheckpointForLog(logFile).Size
	}
	return total
}

// measure reports the duration or the failure of an operation.
func (s *Store) measure(operation string, started time.Time, err error) {
	if err != nil {
		s.observer.ErrorOccurred(operation)
		return
	}
	s.observer.OperationCompleted(operation, time.Since(started))
}

func (s *Store) checkWritable() error {
	if s.isClosed {
		return core.ErrClosed
	}
	if err := s.checkStorageHealthy(); err != nil {
		return err
	}
	if !s.index.IsReady() {
		return core.ErrUnhealthy
	}
	return nil
}

// checkStorageHealthy reports a sticky storage failure. Unlike an index
// failure it cannot be cleared by a rebuild: the event log may hold a batch
// that only WAL replay on restart reconciles, and the in-memory log state
// (size, predecessor hash) is untrustworthy.
func (s *Store) checkStorageHealthy() error {
	if err := s.storage.Failure(); err != nil {
		return fmt.Errorf("%w: storage failure requires a restart: %v", core.ErrUnhealthy, err)
	}
	return nil
}
