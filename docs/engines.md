# Engines

GenesisDB is a single Go module, `github.com/genesisdb-io/genesisdb/v2`. The `/v2` suffix is required by Go for release tags `v2.x.y`. Each engine is a separate package under `packages/`, released together with one repository tag. Importing one engine compiles only that package and its GenesisDB dependencies, not the HTTP server or metrics code. Architecture tests enforce the dependency rules between packages and keep the engines free of third-party imports. Package APIs follow semantic versioning from `v2.0.0` on.

```sh
go get github.com/genesisdb-io/genesisdb/v2/packages/store
```

| Package | Depends on | Purpose |
|---|---|---|
| [`core`](../packages/core) | standard library | Events, hashes, subjects, errors, shared interfaces, data directory layout, durable file helpers |
| [`storage`](../packages/storage) | core | Append-only event logs with hash chain, write-ahead log, reference data, system events |
| [`index`](../packages/index) | core | Fixed-shard file indexes over any `core.EventLogReader` |
| [`cache`](../packages/cache) | standard library | Generic bounded LRU cache with generations |
| [`query`](../packages/query) | core | GenesisDB Query Language (GDBQL): parser, planner, executor over any `core.EventSource` |
| [`store`](../packages/store) | core, storage, index, cache | The complete embedded database |
| [`schema`](../packages/schema) | standard library | Event data validation against registered schemas |
| [`auth`](../packages/auth) | standard library | Bearer token verification |
| [`metrics`](../packages/metrics) | Prometheus | Prometheus collector implementing `core.Observer` |
| [`telemetry`](../packages/telemetry) | OpenTelemetry | OTLP metric exporter implementing `core.Observer` |
| [`server`](../packages/server) | the packages above | The HTTP API |

`metrics` and `telemetry` import third-party packages. `server` composes the engine packages. The engines import the standard library only. Because the module has one `go.mod`, `go get` of an engine package still records the module's third-party requirements in the consumer's module graph, but unused packages are neither downloaded for compilation nor linked.

## How the engines connect

```text
core <- storage ----\
core <- index ------+-- store <- server <- cmd/genesisdb
         cache -----/
core <- query         (store implements core.EventSource)
```

`storage`, `index`, `cache` and `query` never import each other. They meet through interfaces in `core`:

- `core.EventLogReader`: the index engine reads event logs through it. `storage.Engine` implements it.
- `core.EventSource`: the GDBQL engine selects events through it. `store.Store` implements it.
- `core.Observer`: engines report metrics through it. `metrics.Collector` (Prometheus) and `telemetry.Exporter` (OpenTelemetry) implement it. `core.MultiObserver` forwards to several observers.

## Storage engine only

```go
import (
	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/storage"
)

engine, err := storage.Open("data", storage.Options{})
appended, err := engine.Append(ctx, []*core.Event{{Source: "app", Subject: "/a/1", Type: "t", Data: data}})
err = engine.SyncDurable() // before acknowledging, see below
_, _, err = engine.ScanEventLog(engine.MainEventLogFile(), 0, func(stored core.StoredEvent) error {
	fmt.Println(stored.Event.Subject, stored.Location.Offset)
	return nil
})
event, err := engine.ReadEventAt(appended.Events[0].Location)
report, err := engine.VerifyHashChain()
```

Writers must be serialized by the caller. Use `fsutil.LockDirectory` when several processes could open the same directory.

On macOS `Append` orders its writes but leaves the final device flush to the caller, the store issues it once per commit for the index journal. Call `SyncDurable` before acknowledging a write when the storage engine is used alone. On Linux `Append` is durable on return and `SyncDurable` costs one more `fsync`.

## Cache engine only

```go
import "github.com/genesisdb-io/genesisdb/v2/packages/cache"

c := cache.New[string, []byte](cache.Options[[]byte]{MaxEntries: 100_000})
generation := c.Generation()      // capture before loading
value := load(key)
c.PutIfGenerationMatches(generation, key, value) // ignored if Clear ran meanwhile
cached, isCached := c.Get(key)
c.Clear()
```

## Index engine only

```go
import "github.com/genesisdb-io/genesisdb/v2/packages/index"

idx := index.New("data", myEventLogReader, index.Options{})
err := idx.Open()                          // recovers or rebuilds from the event log
err = idx.Publish(appendedEvents, logSize) // after every append
snapshot, err := idx.AcquireSnapshot()
set := index.NewPostingSet()
err = idx.Select(snapshot, core.EventSelector{SubjectsWithDescendants: []string{"/user"}}, set)
for _, posting := range set.InLogOrder() {
	event, err := myEventLogReader.ReadEventAt(posting.Location())
}
```

## GDBQL engine only

```go
import "github.com/genesisdb-io/genesisdb/v2/packages/query"

parsed, err := query.Parse("STREAM e FROM events WHERE e.type == 'x' MAP COUNT()")
selector := query.PlanQuery(parsed).Selector()
result, err := query.Execute(ctx, mySource, parsed) // mySource implements core.EventSource
```

## Everything embedded

```go
import (
	"github.com/genesisdb-io/genesisdb/v2/packages/query"
	"github.com/genesisdb-io/genesisdb/v2/packages/store"
)

s, err := store.Open("data", store.Options{EvaluateGDBQLPrecondition: query.EvaluatePrecondition})
defer s.Close()
err = s.Commit(ctx, events, preconditions...)
events, err := s.Fetch(ctx, "/user", store.FetchOptions{})
subscription, err := s.Subscribe(ctx, "/user", store.SubscribeOptions{})
for batch := range subscription.Batches() { ... }
result, err := query.Run(ctx, s, "STREAM e FROM events MAP COUNT()")
backup, err := s.Backup(ctx)
```
