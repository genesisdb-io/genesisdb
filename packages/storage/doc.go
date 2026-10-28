// Package storage is the GenesisDB storage engine: append-only event logs
// with a SHA-256 hash chain, a write-ahead log for crash safety, reference
// data for erasable payloads, and an isolated system event log.
//
// The event logs are the authoritative data of a GenesisDB data directory.
// Everything else (indexes, caches) is derived from them.
//
// A minimal standalone use:
//
//	engine, err := storage.Open(directory, storage.Options{})
//	stored, err := engine.Append(ctx, events)
//	_, _, err = engine.ScanEventLog(engine.MainEventLogFile(), 0, visit)
//
// Writers must be serialized by the caller. Readers may run concurrently with
// one writer. An engine holds no open files between calls, so there is
// nothing to close.
package storage
