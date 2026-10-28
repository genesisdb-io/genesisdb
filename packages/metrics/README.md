# github.com/genesisdb-io/genesisdb/v2/packages/metrics

A Prometheus collector that implements `core.Observer`, so the store reports to it without depending on Prometheus. Each collector uses its own registry.

```sh
go get github.com/genesisdb-io/genesisdb/v2/packages/metrics
```

```go
collector := metrics.New(metrics.Options{})
s, err := store.Open("data", store.Options{Observer: collector})
if err != nil {
	return err
}
defer s.Close()
http.Handle("GET /metrics", collector.Handler())
```

Use `core.MultiObserver{collector, exporter}` to report to Prometheus and OpenTelemetry at the same time, see [telemetry](../telemetry).

## Options

| Field | Default | Meaning |
|---|---|---|
| `IncludeLegacyNames` | `false` | Also export every metric with the pre 2.0 prefix `eventstore_` |
| `MaxEventTypeLabels` | `256` | Distinct values of the `type` label. Further types count under `other`. A negative value disables the label |

## Metrics

| Name | Kind | Labels |
|---|---|---|
| `genesisdb_events_total` | counter | |
| `genesisdb_events_by_type` | gauge | `type` |
| `genesisdb_event_type_labels` | gauge | |
| `genesisdb_cache_size` | gauge | `type` |
| `genesisdb_cache_hits_total` | counter | |
| `genesisdb_cache_misses_total` | counter | |
| `genesisdb_operation_duration_seconds` | histogram | `operation` |
| `genesisdb_errors_total` | counter | `type` |

Label cardinality is bounded. Subjects are never labels: every distinct subject would create a time series for the rest of the process and could export personal identifiers to the monitoring system. The legacy per-subject series of 1.x are therefore not exported, even with `IncludeLegacyNames`.

The metrics endpoint has no authentication. Keep it on a trusted network.
