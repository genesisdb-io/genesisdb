# github.com/genesisdb-io/genesisdb/v2/packages/storage

The GenesisDB storage engine: append-only event logs with a SHA-256 hash chain, a write-ahead log for atomic batches, erasable reference data and an isolated system event log. Depends on `core` only.

```sh
go get github.com/genesisdb-io/genesisdb/v2/packages/storage
```

```go
engine, err := storage.Open("data", storage.Options{})
appended, err := engine.Append(ctx, events)
event, err := engine.ReadEventAt(appended.Events[0].Location)
report, err := engine.VerifyHashChain()
```

Writers must be serialized by the caller. New hashes bind their predecessor. A pending 1.x WAL is replayed on open. Hashes are not signatures and do not detect whole-chain rewriting without external checkpoints. See [docs/engines.md](../../docs/engines.md).
