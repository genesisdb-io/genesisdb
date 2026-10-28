# Security policy

## Reporting

Report suspected vulnerabilities privately to mail@genesisdb.io. Include the affected revision, reproduction steps, environment and likely impact. Do not send live credentials or personal event data. Coordinate disclosure before opening a public issue. No response-time SLA or independent security certification is implied.

Only the latest release is eligible for fixes. A fix ships as the next patch version.

## Deployment boundaries

- Run one process per local data directory. Do not share a writable directory over a network filesystem or bypass its exclusive lock.
- Use an unprivileged account and restrict directory and backup permissions. New files and directories are created owner-only (`0600`/`0700`). Existing file modes, including those of a migrated 1.x directory, are not automatically tightened. GenesisDB does not encrypt data at rest.
- Keep the API on loopback or a private network. Use TLS before transmitting bearer tokens remotely. A trusted reverse proxy should enforce request rates, connection limits and origin policy.
- Generate distinct administrator, writer and reader tokens with `openssl rand -hex 32`. Restart to rotate environment-configured tokens. Tokens are store-wide, not tenant-specific. There are no per-subject ACLs.
- Administrator tokens can restore, erase, rebuild indexes, register schemas, export backups and audit hashes. Writer tokens can read and commit. Reader tokens can read. API v1 and API v2 enforce the same roles.
- Ping and Prometheus metrics are unauthenticated. Metrics may reveal workload/system information. Restrict both the separate metrics listener and `/metrics` on the API listener. TLS settings for the API do not secure the separate metrics listener.
- The API permits cross-origin requests without cookies. Do not store administrator tokens in untrusted web origins. Bearer authentication is not an application authorization model.
- Nothing is capped by default: any authenticated client can request a selection or backup as large as the store, which is materialized in memory. The optional `GENESISDB_MAX_*` and `GENESISDB_REQUEST_TIMEOUT_SECONDS` caps bound selections, bodies, concurrency and duration for deployments that need them, they do not bound all process memory or CPU. Index shards and reference logs can require linear scans. Use OS/container memory, CPU and disk quotas and monitoring.
- The MCP endpoint (`GENESISDB_MCP=true`) is off by default. It uses the same tokens and roles, offers no erase, restore, backup, schema registration or index rebuild, and rejects browser requests from foreign origins. Event payloads read by an agent go to its model provider and are untrusted input for the agent. Give agents a reader token unless they must commit. See [docs/mcp.md](docs/mcp.md).
- Embedded users must configure their own transport, token policy and `core.WithReadLimits`. Direct `store.Commit` does not enforce registered schemas. Use `CommitValidated` and serialize schema publication through `UpdateSchemas` if schema enforcement is required.

## Integrity and durability

Version-3 hashes bind the predecessor, version and metadata digest. Records without `hashversion` are accepted only before version-3 records in a chain and reported separately by the audit. Neither hash chains nor WAL checksums are signatures. Full-history rewriting, tail truncation and replacement of the whole data directory require trusted external checkpoints to detect.

Reference payloads are intentionally mutable and outside the immutable event hash. Protect their files and backups from unauthorized writes. Erasure replaces whole reference objects, not inline payloads, event metadata, external backups, snapshots or recoverable disk blocks. The immutable reference key (`ref`) that survives an erasure is a SHA-256 digest over the event metadata and the original payload. It cannot be reversed, but the metadata (ID, time, subject, type, source) stays readable next to it, so a reader who can guess a low-entropy payload (for example `{"email":"a@b.c"}` from a small candidate list) can confirm the guess against the key. Treat the key as a pseudonymous identifier that may allow confirmation of known values, not as anonymization. Keep guessable identifying values out of reference payloads that must be fully erasable, or combine them with high-entropy fields. Manage those through application design and retention controls. No automatic GDPR compliance is claimed.

A storage error after durable preparation may leave a commit recoverable even when the client saw an error. Do not retry blindly. Recovery relies on atomic rename and file/directory synchronization. Fault-injection tests do not prove power-loss behavior on every filesystem or device.

## Release checks

Use Go 1.26.10 or a newer supported patched release. Run `make lint`, `make test`, `make fuzz` and `make vuln`. The release workflow runs these, gitleaks over the full Git history and a Trivy scan of the container image before anything is tagged. A clean scanner result is evidence, not a security guarantee.
