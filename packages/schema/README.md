# github.com/genesisdb-io/genesisdb/v2/packages/schema

Validates event data against schemas registered per event type. Schemas are stored as JSON files in a directory, one per type. Standard library only.

```sh
go get github.com/genesisdb-io/genesisdb/v2/packages/schema
```

```go
registry := schema.NewRegistry(s.SchemasDirectory())

err := registry.Register("io.genesisdb.app.customer-added", schema.Schema{
	"type":     "object",
	"required": []any{"firstName"},
	"properties": map[string]any{
		"firstName": map[string]any{"type": "string"},
		"age":       map[string]any{"type": "integer"},
	},
})
if errors.Is(err, schema.ErrAlreadyRegistered) {
	// registrations are immutable
}

err = registry.ValidateEvent("io.genesisdb.app.customer-added", json.RawMessage(`{"firstName":"Bruce"}`))
schemas, err := registry.List()
```

`ValidateEvent` accepts any data for an event type without a schema.

## Supported subset

This is a strict subset of JSON Schema, not a complete draft. Unsupported keywords and types are rejected at registration with `ErrInvalidSchema`.

- Types: `string`, `number`, `integer`, `boolean`, `null`, `object`, `array`
- Keywords: `type`, `properties`, `required`, `additionalProperties`, `allowAdditionalProperties`, `items`, `description`, `title`
- `required` is an array of field names on an object, or a boolean on a property
- Additional object fields are rejected unless `additionalProperties` or `allowAdditionalProperties` is `true`, unlike standard JSON Schema
- Schemas are capped at 1 MiB and 32 nesting levels
- `$ref`, formats, patterns, numeric bounds, `enum` and schema composition are not implemented

## Files

File names are derived from the SHA-256 of the exact event type, and every file stores the type in an envelope, so similar type names never collide. Registration publishes atomically and refuses to overwrite. `List` and `Find` fail on unreadable or invalid files rather than hiding them. `MigrateLegacyFiles` converts the schema files of GenesisDB 1.x and is safe to run again after an interruption.

The registry does not serialize concurrent registration with commits. In an embedded store use `store.UpdateSchemas` for registration and `store.CommitValidated` for enforcement. See [docs/http-api.md](../../docs/http-api.md#schemas).
