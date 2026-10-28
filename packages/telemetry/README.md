# github.com/genesisdb-io/genesisdb/v2/packages/telemetry

An OpenTelemetry exporter that implements `core.Observer`. Pushes GenesisDB metrics with OTLP over HTTP to any OpenTelemetry collector or backend.

```go
exporter, err := telemetry.New(ctx, telemetry.Options{EndpointURL: "http://collector:4318"})
if err != nil {
	return err
}
defer exporter.Shutdown(ctx)
s, err := store.Open("data", store.Options{Observer: exporter})
```

Use `core.MultiObserver{collector, exporter}` to export to Prometheus and OpenTelemetry at the same time.

The standard `OTEL_EXPORTER_OTLP_*`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` and `OTEL_METRIC_EXPORT_INTERVAL` environment variables apply.

| Instrument | Kind | Attributes |
|---|---|---|
| `genesisdb.events` | counter | `type` |
| `genesisdb.cache.hits` | counter | |
| `genesisdb.cache.misses` | counter | |
| `genesisdb.cache.entries` | gauge | |
| `genesisdb.operation.duration` | histogram, seconds | `operation` |
| `genesisdb.errors` | counter | `operation` |

Attribute cardinality is bounded: subjects are never attributes, and `type` admits at most `MaxEventTypeLabels` distinct values (default 256) before further types are reported as `other`.
