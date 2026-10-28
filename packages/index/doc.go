// Package index is the GenesisDB index engine: fixed-shard, file based
// indexes over append-only event logs.
//
// Every regular event is posted into five hash-sharded categories (exact
// subject, subject with ancestors, event type, subject and type pair, event
// ID) and one subject catalog. Each posting stores the exact key and the
// byte location of the event record, so reads go straight to the record
// without scanning the log. A state journal publishes batches atomically:
// readers see a whole commit or nothing of it.
//
// The number of index files is bounded (5 * 256 shards + catalog + state
// journal) regardless of how many events, subjects or types exist.
//
// The engine reads event logs through core.EventLogReader, so it works with
// any log implementation, not only package storage.
package index
