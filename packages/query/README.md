# github.com/genesisdb-io/genesisdb/v2/packages/query

The engine of the GenesisDB Query Language (GDBQL): parser, index planner and executor over any `core.EventSource`. Depends on `core` only.

```sh
go get github.com/genesisdb-io/genesisdb/v2/packages/query
```

```go
result, err := query.Run(ctx, source, "STREAM e FROM events WHERE e.subject UNDER '/user' MAP { id: e.id }")
```

See [docs/gdbql.md](../../docs/gdbql.md).
