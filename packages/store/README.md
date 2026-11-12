# github.com/genesisdb-io/genesisdb/v2/packages/store

The complete embedded GenesisDB: storage, indexes and cache wired together, with commits and preconditions, fetches, subscriptions, backups, restores, erasure and storage format conversion. Depends on `core`, `storage`, `index` and `cache`.

```sh
go get github.com/genesisdb-io/genesisdb/v2/packages/store
```

```go
s, err := store.Open("data", store.Options{EvaluateGDBQLPrecondition: query.EvaluatePrecondition})
defer s.Close()
err = s.Commit(ctx, events, preconditions...)
events, err := s.Fetch(ctx, "/user", store.FetchOptions{})
```

Embedded reads are unbounded by default. Use `core.WithReadLimits` for untrusted selections. Registered schemas are not automatically enforced by `Commit`, use `CommitValidated` and serialize schema publication with `UpdateSchemas`. Backups hold the writer lock and include schemas, reference erasure replaces entire reference payloads with `{}`. A commit error can still mean durable data, check before retrying. Concurrent commits are written together as one group and share one device flush (`Options.MaxCommitsPerGroup`, default 256, `1` writes every commit on its own, `MaxEventsPerGroup` and `MaxBytesPerGroup` split groups by events and payload). Each commit stays atomic, the outcome equals executing them one after another, and a storage failure reaches every commit of its group. The `validate` callback of `CommitValidated` may run on the goroutine of another caller. Maintenance takes the writer lock ahead of waiting commits, which write one group before the next maintenance call. `Options.Signer` (`core.ParseSigningKey`) signs new events, `Options.TrustedSigningKeys` pins the keys `VerifyHashChain` trusts, see [docs/signatures.md](../../docs/signatures.md). See [docs/engines.md](../../docs/engines.md) and [SECURITY.md](../../SECURITY.md).
