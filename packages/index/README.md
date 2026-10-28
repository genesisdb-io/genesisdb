# github.com/genesisdb-io/genesisdb/v2/packages/index

Fixed-shard file indexes over any append-only event log that implements `core.EventLogReader`. At most 1282 index files, atomic batch publication through a state journal, crash recovery and migration of older layouts. Depends on `core` only.

```sh
go get github.com/genesisdb-io/genesisdb/v2/packages/index
```

```go
idx := index.New("data", eventLogReader, index.Options{})
err := idx.Open()
err = idx.Publish(appendedEvents, logSize)
snapshot, err := idx.AcquireSnapshot()
set := index.NewPostingSet()
err = idx.Select(snapshot, core.EventSelector{EventTypes: []string{"registered"}}, set)
```

Shard scans are linear in shard size. The fixed file count is not a constant-time lookup guarantee. Use `snapshot.WithContext(ctx)` and `NewPostingSetContext(ctx)` for cancellation and selection budgets. See [docs/storage-format.md](../../docs/storage-format.md).
