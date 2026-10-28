# github.com/genesisdb-io/genesisdb/v2/packages/core

The shared foundation of every GenesisDB engine. Standard library only.

```sh
go get github.com/genesisdb-io/genesisdb/v2/packages/core
```

- `Event`, `ReferenceDataEntry`, `Precondition`, `Backup`: data types with additive version/schema fields, see compatibility documentation
- `HashEvent`, `HasValidHash`: the event hash chain
- `SubjectAndAncestors`, `IsDirectChild`, `IsStrictDescendant`: the subject hierarchy
- `EventLogReader`, `EventSource`, `Observer`: the interfaces the engines meet through
- `core/layout`: file names and storage formats of a data directory
- `core/fsutil`: durable file operations and the data directory lock
