# github.com/genesisdb-io/genesisdb/v2/packages/cache

A generic, bounded, concurrency-safe LRU cache with generations. A generation prevents a reader from storing a value it loaded before the cache was cleared. Standard library only.

```sh
go get github.com/genesisdb-io/genesisdb/v2/packages/cache
```

```go
c := cache.New[string, []byte](cache.Options[[]byte]{MaxEntries: 100_000})
generation := c.Generation()
value := load(key)
c.PutIfGenerationMatches(generation, key, value)
cached, isCached := c.Get(key)
```
