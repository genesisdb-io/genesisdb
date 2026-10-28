# Contributing to GenesisDB

Contributions are welcome: bug reports, documentation, tests and code.

## Reporting a bug

Use the bug report template on GitHub. Include the GenesisDB version (`genesisdb version`), the operating system, the smallest sequence of requests that triggers the problem, and the expected and actual result. Remove tokens and private event data from logs.

## Before you write code

Open an issue first for changes that alter stored data, the HTTP API, a public Go API, or that span several packages. Data compatibility is the most important property of GenesisDB: a change must never make an existing data directory or backup unreadable.

## Workflow

Branch from `main`, name the branch as you like (`feat/signatures`, `fix/erase-status`), open a pull request. The CI runs the full gate set on every pull request: lint, race tests, disk-full tests, vulnerability scan, fuzzing, secret scan. Every pull request is reviewed before it is merged. `main` is protected, releases are cut from it by the maintainer.

## Repository layout

```text
cmd/genesisdb/        the binary: configuration, wiring, convert command
packages/<engine>/    one package per engine, see docs/engines.md
docs/                 user documentation
scripts/              seed.py loads a running instance with sample data, third-party-licenses.sh writes THIRD_PARTY_LICENSES during every build
tests/                repository wide checks: architecture rules and the 1.1.6 compatibility suite, with its fixtures in tests/testdata
```

The repository is one Go module, `github.com/genesisdb-io/genesisdb/v2`. Imports always include `/v2`. There is no `go.work` and no `replace` directive.

## Rules

- `storage`, `index`, `cache` and `query` never import each other. They meet through interfaces in `core`. `tests/architecture_test.go` enforces the dependency rules.
- Engines use the standard library only. Third-party imports belong to `metrics`, `telemetry` and `cmd/genesisdb`. `tests/architecture_test.go` enforces this.
- Engines never read environment variables or global state. Configuration comes through `Options`.
- File names and log headers are built only in `core/layout`.
- Names are descriptive and without abbreviations (`referenceDataKey`, not `ref`). Functions that need a held lock end in `WhileLocked`.
- Persisted JSON field names never change.
- Every bug fix comes with a test. Changes to storage or indexes need a fault injection test.
- Keep files below about 600 lines.

## Checks

```sh
make lint            # gofmt, go vet, staticcheck
make test            # all packages with the race detector
make fuzz            # bounded fuzz runs
make compatibility   # the 1.1.6 compatibility suite
```

`make build` produces `bin/genesisdb` reporting version `0.0.0-dev`. Release builds inject the version, see the Makefile.

## Disk-full tests

`packages/store/diskfull_test.go` runs against a real small volume instead of injected faults, so the ENOSPC paths of the operating system are exercised. The tests skip unless `GENESISDB_FULL_VOLUME` points to a dedicated volume of a few MiB. They delete `data`, `filler` and `reserve` inside it. The CI runs them on a 4 MiB tmpfs.

```sh
# macOS: 4 MiB RAM disk with HFS+ (APFS has a larger minimum size)
DEV=$(hdiutil attach -nomount ram://8192 | tr -d ' ')
newfs_hfs -v gdbfull "$DEV" && mkdir -p /tmp/gdbfull && mount -t hfs "$DEV" /tmp/gdbfull
GENESISDB_FULL_VOLUME=/tmp/gdbfull go test -count=1 -run TestDiskFull -v ./packages/store/
umount /tmp/gdbfull && hdiutil detach "$DEV"

# Linux: 4 MiB tmpfs
mkdir -p /tmp/gdbfull && sudo mount -t tmpfs -o size=4m tmpfs /tmp/gdbfull
GENESISDB_FULL_VOLUME=/tmp/gdbfull go test -count=1 -run TestDiskFull -v ./packages/store/
sudo umount /tmp/gdbfull
```

A volume that was filled to the last byte by a previous run may refuse `unlink` (HFS+). Reformat it between runs.

## Commit messages

Write the subject in the imperative mood, at most 72 characters, and explain the reason in the body when it is not obvious.
