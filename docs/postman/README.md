# Postman collection

`GenesisDB-2.postman_collection.json` holds a request for every route of the HTTP API with test assertions. Requests use API v2, the 1.x routes of API v1 are included where they have a different name and are marked "(API v1 ...)". `GenesisDB-2-local.postman_environment.json` is a matching environment for a local instance.

## Import

1. Postman, File, Import, drop both files.
2. Select the environment "GenesisDB 2 local" (top right).
3. Set `adminToken` to the value of `GENESISDB_AUTH_TOKEN`. `readToken` and `writeToken` are optional and only needed for folder 9.
4. `baseUrl` defaults to `http://127.0.0.1:8080`.

## Use

The folders are numbered in a working order. Run them top to bottom with the collection runner (right click the collection, Run) for a complete smoke test of a fresh or existing store:

| Folder | Content |
|---|---|
| 1 Health | ping (unversioned, API v2 and API v1) and status, no token for ping |
| 2 Commit | sample data, preconditions, expected 412 and 400. Saves the customer subject and event IDs in collection variables for later folders |
| 3 Fetch | subject with descendants, bounds, latest by type, NDJSON form, API v1 `/stream` |
| 4 Query (GDBQL) | projections, filters, aggregates, GROUP BY, subquery, both syntaxes, API v1 `/q` |
| 5 Catalog | subjects and types |
| 6 Subscribe (SSE) | streams until you cancel, excluded from runner runs |
| 7 Schemas | register, list, valid and invalid commit against the schema |
| 8 Administration | routes under `/api/v2/admin`: hash audit, backup, erase, rebuild, restore (answers 409 against a non-empty store) |
| 9 Authorization | 401 and 403 cases, needs read and write tokens |

Every request can be repeated. Requests that only work once (`isSubjectNew`, schema registration) accept both the first-run and the repeat status.

List routes answer NDJSON, one object per line, and the test scripts parse that form. One request in folder 3 shows the 1.x compatibility header `x-disable-ndjson` that turns a list into a JSON array, nothing else uses it.

To test a real restore, start a second instance with an empty data directory, point `baseUrl` at it and run "Restore" from folder 8, the `backup` variable holds the body from the last "Backup" request.

## Command line

The same collection runs headless with [newman](https://github.com/postmanlabs/newman):

```sh
npx newman run docs/postman/GenesisDB-2.postman_collection.json \
  --env-var baseUrl=http://127.0.0.1:8080 \
  --env-var adminToken="$GENESISDB_AUTH_TOKEN" \
  --env-var readToken="$GENESISDB_READ_TOKEN" \
  --env-var writeToken="$GENESISDB_WRITE_TOKEN" \
  --folder "1 Health" --folder "2 Commit" --folder "3 Fetch" --folder "4 Query (GDBQL)" \
  --folder "5 Catalog" --folder "7 Schemas" --folder "8 Administration" --folder "9 Authorization"
```

Folder 6 is left out because its requests stream until cancelled.
