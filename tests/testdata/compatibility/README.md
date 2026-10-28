# Compatibility fixtures

Everything in this directory was produced by GenesisDB 1.1.6. It must never be edited by hand: the compatibility suite in `tests/compatibility_test.go` compares the new implementation against it byte for byte.

| Path | Content |
|---|---|
| `v116/` | A data directory as 1.1.6 leaves it: fixed-shard indexes, reference data, an erasure with its system event, a schema |
| `v115-legacy-indexes/` | The same events with the per-key index layout of releases before 1.1.6 |
| `golden.json` | Requests against `v116/` and the exact responses of 1.1.6 |
| `backup-extended-v116.json`, `backup-plain-v116.json` | Backups exported by 1.1.6 |
