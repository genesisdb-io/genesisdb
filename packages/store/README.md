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

Embedded reads are unbounded by default. Use `core.WithReadLimits` for untrusted selections. Registered schemas are not automatically enforced by `Commit`, use `CommitValidated` and serialize schema publication with `UpdateSchemas`. Backups hold the writer lock and include schemas, reference erasure replaces entire reference payloads with `{}`. A commit error can still mean durable data, check before retrying. See [docs/engines.md](../../docs/engines.md) and [SECURITY.md](../../SECURITY.md).
