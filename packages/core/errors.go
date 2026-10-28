package core

import "errors"

// Sentinel errors shared by all engines. Use errors.Is to test for them.
var (
	// ErrInvalidEvent reports invalid event input before a durable write.
	ErrInvalidEvent = errors.New("invalid event")
	// ErrNotReady reports that the index set is not published, for example
	// during a rebuild or after a failed commit.
	ErrNotReady = errors.New("index set not ready")

	// ErrUnhealthy reports that writes are rejected because an earlier
	// failure left the store in recovery state.
	ErrUnhealthy = errors.New("store is unhealthy: indexes are not published; restart GenesisDB to recover. A previously failed request may already be durable, do not retry blindly")

	// ErrClosed reports an operation on a closed store.
	ErrClosed = errors.New("store is closed")

	// ErrNotEmpty reports a restore into a store that already holds data.
	ErrNotEmpty = errors.New("store is not empty")

	// ErrNotFound reports a missing event, subject or reference data entry.
	ErrNotFound = errors.New("not found")

	// ErrInvalidBackup reports a backup that was rejected before anything was
	// written.
	ErrInvalidBackup = errors.New("invalid backup")

	// ErrSystemEventNotAllowed reports an attempt to commit a system event.
	ErrSystemEventNotAllowed = errors.New("you do not have permission to create system events")

	// ErrEventLogCorrupt reports invalid authoritative event log data.
	ErrEventLogCorrupt = errors.New("event log corruption")

	// ErrIndexCorrupt reports invalid or missing index data.
	ErrIndexCorrupt = errors.New("index corruption")

	// ErrUnsupportedIndexFormat reports an index written by a newer or
	// unknown GenesisDB version.
	ErrUnsupportedIndexFormat = errors.New("unsupported index format")

	// ErrUnsupportedStorageFormat reports a data directory whose storage
	// format this version cannot open.
	ErrUnsupportedStorageFormat = errors.New("unsupported storage format")
)
