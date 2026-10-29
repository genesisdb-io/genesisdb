# HTTP API

The API is versioned by path. `/api/v2` is the current API and the one the 2.x SDKs use. `/api/v1` is the API of GenesisDB 1.x, kept unchanged so 1.x clients keep working, see [API v1](#api-v1). Infrastructure routes (`/ping`, `/metrics`, `/mcp`) carry no version.

Protected routes require `Authorization: Bearer <token>`. `GENESISDB_AUTH_TOKEN` is the administrator token. Optional `GENESISDB_READ_TOKEN` permits fetch, subscribe, query, status, catalog and schema listing. `GENESISDB_WRITE_TOKEN` adds commit. Every route under `/api/v2/admin` and schema registration require the administrator token, in both versions. `GET /ping`, `GET /api/v2/ping` and `GET /api/v1/status/ping` need no token.

The CLI requires distinct tokens of at least 32 bytes. Use TLS or a trusted TLS proxy for remote access. Requests accept exactly one JSON value in valid UTF-8. By default nothing is capped: request bodies, commit batch size, selection size, concurrency and request duration are unbounded, and every response is complete. Deployments can opt into caps with the `GENESISDB_MAX_*` and `GENESISDB_REQUEST_TIMEOUT_SECONDS` variables, a hit cap fails the request explicitly rather than truncating it. Query `LIMIT` and fetch bounds are applied after loading. See README configuration and SECURITY.md.

A [Postman collection](postman/README.md) with a request for every route is in `docs/postman`.

List responses are NDJSON (`application/x-ndjson`, every object followed by an empty line). Send the header `x-disable-ndjson: true` to receive one JSON array instead. Errors are plain text with the status codes listed at the end, in both versions.

## Routes

| Method | Path | Token | Purpose |
|---|---|---|---|
| GET | `/ping` | none | Liveness for probes and load balancers, answers `Pong` |
| GET | `/api/v2/ping` | none | Liveness, answers `Pong`. Clients use it to detect API v2 |
| GET | `/api/v2/status` | read | Engine, system, event and storage status |
| POST | `/api/v2/commit` | write | Commit events with optional preconditions |
| POST | `/api/v2/fetch` | read | Fetch the events of a subject |
| POST | `/api/v2/subscribe` | read | Subscribe to a subject (Server-Sent Events) |
| POST | `/api/v2/query` | read | Run a GDBQL query |
| GET | `/api/v2/subjects` | read | List top level subjects |
| GET | `/api/v2/types` | read | List event types |
| GET | `/api/v2/schemas` | read | List all event schemas |
| POST | `/api/v2/schemas` | admin | Register an event schema |
| GET | `/api/v2/admin/audit` | admin | Verify the hash chain |
| GET | `/api/v2/admin/backup` | admin | Export a backup |
| POST | `/api/v2/admin/restore` | admin | Restore a backup into an empty store |
| POST | `/api/v2/admin/erase` | admin | Erase reference payloads of a subject |
| POST | `/api/v2/admin/index` | admin | Rebuild all indexes |

The administrator token passes every route. Administration lives under `/api/v2/admin`, so a proxy can restrict it by path.

## Commit

```json
{
  "events": [
    {
      "source": "io.genesisdb.app",
      "subject": "/user/456",
      "type": "io.genesisdb.app.user-added",
      "data": { "firstName": "John", "email": "john.doe@example.com" },
      "options": { "storeDataAsReference": true }
    }
  ],
  "preconditions": [
    { "type": "isSubjectNew", "payload": { "subject": "/user/456" } }
  ]
}
```

- `subject` must start with `/`. A subject with one segment (`/customer`) gets a generated ID appended.
- `data` must be a JSON object, at most about 64 KB per event. Instead of `data`, the legacy field `ref` stores the payload as reference data.
- `options.storeDataAsReference` stores the payload as reference data, so it can be erased later.
- All events of one request are published atomically. There is no limit on the number of events or preconditions per request.
- Legacy `ref` and `options.storeDataAsReference` may be combined without storing the payload twice. `data` and `ref` together are rejected.
- Schema enforcement is repeated under the writer lock, serialized with schema registration.

Preconditions are checked atomically with the commit:

| Type | Payload | Holds when |
|---|---|---|
| `isSubjectNew` | `{"subject": "/user/456"}` | neither the subject nor a descendant has events |
| `isSubjectExisting` | `{"subject": "/user/456"}` | the subject or a descendant has events |
| `isQueryResultTrue` | `{"query": "<GDBQL>"}` | the query result is true, see [gdbql.md](gdbql.md) |

A failed precondition answers `412` with `{"type": "...", "message": "..."}`.

If a commit fails after its events reached the event log, the events are durable and become visible after a restart. The store then rejects writes until it is restarted. Do not retry a failed commit blindly, check whether it was applied first.

## Fetch

```json
{
  "subject": "/customer",
  "options": {
    "lowerBound": "<event id>",
    "includeLowerBoundEvent": true,
    "upperBound": "<event id>",
    "includeUpperBoundEvent": false,
    "latestByEventType": "io.genesisdb.app.customer-added"
  }
}
```

Returns the events of the subject and all of its descendants in commit order (physical log order). Timestamps do not influence the order, so a clock correction never reorders a subject's history. All options are optional. `latestByEventType` returns only the newest event of that type below the subject.

Bounds are event IDs. `lowerBound` skips every event before it, `upperBound` stops at it. The bound events themselves are only included with `includeLowerBoundEvent` or `includeUpperBoundEvent`. A `lowerBound` that does not occur below the subject yields an empty result, not an error, so a client that persists the ID of its last processed event should check that the event still belongs to the subject before using it as a bound.

## Subscribe

```json
{ "subject": "/customer", "interval": 0.5, "options": { "lowerBound": "<event id>" } }
```

Answers with `text/event-stream`. Every event is sent as `data: <event>`. The existing history comes first (narrowed by the bound options), then new events in commit order. When a poll finds nothing new, a heartbeat `data: {"payload":""}` is sent. `interval` is the poll interval in seconds, default 1, between 0.1 and 86400.

If a read budget is configured (`GENESISDB_MAX_READ_EVENTS`, `GENESISDB_MAX_READ_BYTES`), each poll is bounded by it and a history or commit burst larger than one budget is delivered as several consecutive batches without waiting for the interval, so a subscriber that falls behind catches up instead of failing. Without a budget every poll delivers everything new. A subscription whose polls fail five times in a row (for example a storage error) ends with `event: error` followed by `data: {"error": "..."}` and closes the stream. Clients must treat a closed stream as a reason to reconnect or alert, never as an idle subject.

## Query

```json
{ "query": "STREAM e FROM events WHERE e.subject UNDER '/customer' MAP { id: e.id }" }
```

Answers NDJSON by default, one row per line. A query that returns a single value (`MAP COUNT()`) answers that value as its only line. With `x-disable-ndjson: true` the answer is one JSON array instead, also for a single value (`[3]`) and an empty result (`[]`). `/api/v1/query` and `/api/v1/q` always answer NDJSON, as in 1.1.6.

## Backup and restore

`GET /api/v2/admin/backup` collects a consistent snapshot of events, reference data, system events and schemas while excluding concurrent writers. When auxiliary data or schemas exist, it uses the extended form `{"events": [...], "refs": [...], "system": [...], "schemas": {"<filename>.json": {...}}}`. Otherwise it uses a plain JSON event array. Both forms are accepted by `POST /api/v2/admin/restore`. Unreadable auxiliary data fails the backup rather than being omitted. If encoding fails after response headers were sent, the connection is aborted, never treat an incomplete download as a usable backup.

Backups are complete by default and materialized in memory before they are sent, so the process needs memory proportional to the store size during a backup. **Backups stall writes:** the writer lock is held while the backup is collected, so every commit waits (or fails on its deadline) until the collection finishes. Reads continue. The same applies to `GET /api/v2/admin/audit` and `POST /api/v2/admin/erase`. Schedule them in low-traffic windows or, for large stores, take backups from a filesystem snapshot of a stopped instance. Test restoration, protect backup confidentiality and apply independent retention/erasure policies.

Restore requires a completely empty store (no events, reference data or system events), otherwise it answers `409`. Restoring schemas additionally requires an empty schema directory. Invalid structural backup data answers `400` before writing. Schema filenames are path-checked and their JSON must be valid, but restore is not a full schema-semantic validation pass. Event IDs and times are preserved, hash chains are recomputed. A durable maintenance marker prevents a failed/interrupted restore from silently serving partial data. Preserve a failed directory for diagnosis and restore into a separate empty directory.

## Erase

```json
{ "subject": "/user/456" }
```

Replaces each entire reference payload of the subject and its descendants with `{}`, removing keys as well as values. Erasure is not bounded by the read budget: payloads are never loaded, only reference keys are collected from the event records, and the reference log is rewritten in one streaming pass. A subject with more events than one fetch may return is still erased completely. The request holds the writer lock for the duration and observes cancellation until the rewrite starts. Inline data and immutable metadata are not erased. All matching reference-log entries are rewritten, including duplicates. The hash chain stays valid because reference payloads are outside it.

A requested audit event precedes the rewrite, and a completed event is written only after durable replacement. They use the neutral subject `/`, not the personal subject being erased. A `500` can mean the request was logged but replacement failed, or that replacement succeeded but the completion audit failed. Preserve and investigate that distinction. Retrying removes payloads again and creates additional audit events. External backups, filesystem snapshots and physical remnants are outside this operation.

## Schemas

```json
{
  "type": "io.genesisdb.app.customer-added",
  "schema": {
    "properties": {
      "firstName": { "type": "string", "required": true },
      "age": { "type": "number" }
    },
    "allowAdditionalProperties": true
  }
}
```

A registration answers `201 Created` with `{"status": "success", "message": "Schema registered successfully", "type": "..."}`, a second registration of the same type answers `409`, as in 1.x.

HTTP commits of that exact event type are validated against a strict subset, not a complete JSON Schema draft. Supported types are `string`, `number`, `integer`, `boolean`, `null`, `object` and `array`. Supported keywords are `type`, `properties`, `required`, `additionalProperties`, `allowAdditionalProperties`, `items`, `description` and `title`. Unsupported keywords/types are rejected at registration. Schema size is capped at 1 MiB and nesting at 32 levels.

`required` supports an array of field names on an object, or the per-property boolean shown above. `items` accepts one schema object. Additional object fields are rejected by default, unlike standard JSON Schema. Enable them explicitly with `additionalProperties: true` or the alias `allowAdditionalProperties: true`. When both are present, `additionalProperties` takes precedence. `description` and `title` are annotations. `$ref`, formats, patterns, numeric bounds, enum and schema composition are not implemented.

Schemas are immutable registrations. Schema files use SHA-256-derived names and store the exact event type in an envelope. Embedded applications must use `CommitValidated` if they want schema enforcement and `UpdateSchemas` for serialized registration.

## API v1

`/api/v1` is the API of GenesisDB 1.1.6 and is frozen: it gains no routes and answers byte for byte like 1.1.6, which the compatibility suite verifies. Clients and SDKs of 1.x work against GenesisDB 2 without changes. New functionality is only added to `/api/v2`.

Every v1 response carries `Deprecation: true` and `Link: <successor>; rel="successor-version"`. Requests, responses and token permissions are the same as on the successor.

| API v1 | Successor in API v2 |
|---|---|
| GET `/api/v1/status/ping` | GET `/api/v2/ping` |
| GET `/api/v1/status` | GET `/api/v2/status` |
| GET `/api/v1/status/audit` | GET `/api/v2/admin/audit` |
| POST `/api/v1/commit` | POST `/api/v2/commit` |
| POST `/api/v1/stream` | POST `/api/v2/fetch` |
| POST `/api/v1/observe` | POST `/api/v2/subscribe` |
| POST `/api/v1/q`, POST `/api/v1/query` | POST `/api/v2/query` |
| GET `/api/v1/subjects` | GET `/api/v2/subjects` |
| GET `/api/v1/types` | GET `/api/v2/types` |
| GET `/api/v1/schema/get` | GET `/api/v2/schemas` |
| POST `/api/v1/schema/register` | POST `/api/v2/schemas` |
| GET `/api/v1/backup/create` | GET `/api/v2/admin/backup` |
| POST `/api/v1/backup/restore` | POST `/api/v2/admin/restore` |
| POST `/api/v1/erase` | POST `/api/v2/admin/erase` |
| POST `/api/v1/index` | POST `/api/v2/admin/index` |

The names of API v2 do not exist under `/api/v1` and the names of API v1 do not exist under `/api/v2`.

## Trust boundary of restore

`POST /api/v2/admin/restore` is an administrator operation that writes the complete history. It checks the structure of the backup (event records, reference keys, schema file names), not the provenance: event IDs, times, subjects and payloads are taken as given and the hash chains are recomputed from scratch. An administrator token can therefore create any history with a chain that passes `GET /api/v2/admin/audit`. Hash chains detect accidental or unprivileged modification, not actions of the administrator. Keep backups and their checksums under separate control if that distinction matters.

## Status codes

| Code | Meaning |
|---|---|
| 400 | Invalid request, schema violation or invalid backup |
| 401 | Missing or wrong token |
| 403 | Valid token without permission |
| 408 | Request cancellation/deadline observed |
| 413 | Request body exceeds a configured cap (`GENESISDB_MAX_REQUEST_BYTES`, `GENESISDB_MAX_RESTORE_BYTES`) |
| 422 | Read selection exceeds a configured cap (`GENESISDB_MAX_READ_EVENTS`, `GENESISDB_MAX_READ_BYTES`), or erase of a subject whose events only hold immutable inline data |
| 429 | More active requests than a configured `GENESISDB_MAX_CONCURRENT_REQUESTS` |
| 404 | No events for the subject (erase) |
| 409 | Restore into a store that is not empty, or a schema that already exists |
| 412 | A precondition failed |
| 503 | The store is not ready, for example during an index rebuild |
