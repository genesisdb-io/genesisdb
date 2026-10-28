// Package telemetry exports GenesisDB metrics with OpenTelemetry. Exporter
// implements core.Observer, so the store reports to it without depending on
// OpenTelemetry.
//
// Metrics are pushed with OTLP over HTTP. The endpoint, headers, TLS and
// resource attributes follow the standard OTEL_EXPORTER_OTLP_* and
// OTEL_RESOURCE_ATTRIBUTES environment variables unless Options set them.
//
// The instruments follow the OpenTelemetry naming conventions and mirror the
// Prometheus collector of package metrics: genesisdb.events (type),
// genesisdb.cache.hits, genesisdb.cache.misses, genesisdb.cache.entries,
// genesisdb.operation.duration (operation) and genesisdb.errors (operation).
//
// Attribute cardinality is bounded like in package metrics: subjects are
// never attributes, and the type attribute admits at most MaxEventTypeLabels
// distinct values before further types are reported as "other".
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// ScopeName is the instrumentation scope of every instrument.
const ScopeName = "genesisdb.io/genesisdb"

// metricsPath is the OTLP HTTP path of metric exports.
const metricsPath = "/v1/metrics"

// DefaultMaxEventTypeLabels bounds the distinct event type attribute values.
const DefaultMaxEventTypeLabels = 256

// OverflowLabel is the attribute value used once the type budget is exhausted.
const OverflowLabel = "other"

// Options configure an exporter.
type Options struct {
	// EndpointURL is the base URL of the OTLP HTTP receiver, for example
	// "http://collector:4318". Like OTEL_EXPORTER_OTLP_ENDPOINT, the path
	// /v1/metrics is appended. Empty uses OTEL_EXPORTER_OTLP_ENDPOINT or
	// OTEL_EXPORTER_OTLP_METRICS_ENDPOINT, and the OTLP default
	// https://localhost:4318 if neither is set.
	EndpointURL string
	// Headers are sent with every export, for example an API key. They are
	// merged with OTEL_EXPORTER_OTLP_HEADERS.
	Headers map[string]string
	// ExportInterval between two pushes. 0 uses OTEL_METRIC_EXPORT_INTERVAL
	// or the OpenTelemetry default of 60 seconds.
	ExportInterval time.Duration
	// ServiceVersion is reported as the service.version resource attribute.
	ServiceVersion string
	// Reader replaces the OTLP exporter, for example with a
	// sdkmetric.ManualReader in tests. When set, EndpointURL, Headers and
	// ExportInterval are ignored.
	Reader sdkmetric.Reader
	// MaxEventTypeLabels bounds the distinct values of the type attribute.
	// 0 uses DefaultMaxEventTypeLabels, a negative value disables the type
	// attribute entirely.
	MaxEventTypeLabels int
}

// Exporter records GenesisDB metrics with its own OpenTelemetry meter
// provider. It never touches the global provider. It is safe for
// concurrent use.
type Exporter struct {
	provider *sdkmetric.MeterProvider

	events            metric.Int64Counter
	cacheHits         metric.Int64Counter
	cacheMisses       metric.Int64Counter
	operationDuration metric.Float64Histogram
	errors            metric.Int64Counter

	cacheEntries      sync.Mutex
	cacheEntriesValue int64

	maxEventTypeLabels int
	eventTypeMutex     sync.Mutex
	eventTypeLabels    map[string]struct{}
}

// New returns an exporter that pushes metrics until Shutdown.
func New(ctx context.Context, options Options) (*Exporter, error) {
	reader := options.Reader
	if reader == nil {
		var exporterOptions []otlpmetrichttp.Option
		if options.EndpointURL != "" {
			exporterOptions = append(exporterOptions, otlpmetrichttp.WithEndpointURL(strings.TrimSuffix(options.EndpointURL, "/")+metricsPath))
		}
		if len(options.Headers) > 0 {
			exporterOptions = append(exporterOptions, otlpmetrichttp.WithHeaders(options.Headers))
		}
		otlpExporter, err := otlpmetrichttp.New(ctx, exporterOptions...)
		if err != nil {
			return nil, fmt.Errorf("create OTLP metric exporter: %w", err)
		}
		var readerOptions []sdkmetric.PeriodicReaderOption
		if options.ExportInterval > 0 {
			readerOptions = append(readerOptions, sdkmetric.WithInterval(options.ExportInterval))
		}
		reader = sdkmetric.NewPeriodicReader(otlpExporter, readerOptions...)
	}

	attributes := []attribute.KeyValue{attribute.String("service.name", "genesisdb")}
	if options.ServiceVersion != "" {
		attributes = append(attributes, attribute.String("service.version", options.ServiceVersion))
	}
	// Environment attributes override the defaults, so OTEL_SERVICE_NAME
	// and OTEL_RESOURCE_ATTRIBUTES keep working.
	serviceResource, err := resource.Merge(resource.NewSchemaless(attributes...), resource.EnvironmentWithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("build OpenTelemetry resource: %w", err)
	}

	exporter := &Exporter{
		provider:           sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(serviceResource)),
		maxEventTypeLabels: options.MaxEventTypeLabels,
		eventTypeLabels:    map[string]struct{}{},
	}
	if exporter.maxEventTypeLabels == 0 {
		exporter.maxEventTypeLabels = DefaultMaxEventTypeLabels
	}
	if err := exporter.createInstruments(); err != nil {
		exporter.provider.Shutdown(ctx)
		return nil, err
	}
	return exporter, nil
}

func (exporter *Exporter) createInstruments() error {
	meter := exporter.provider.Meter(ScopeName)
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	var err error
	exporter.events, err = meter.Int64Counter("genesisdb.events", metric.WithDescription("Committed events"), metric.WithUnit("{event}"))
	collect(err)
	exporter.cacheHits, err = meter.Int64Counter("genesisdb.cache.hits", metric.WithDescription("Event cache hits"), metric.WithUnit("{hit}"))
	collect(err)
	exporter.cacheMisses, err = meter.Int64Counter("genesisdb.cache.misses", metric.WithDescription("Event cache misses"), metric.WithUnit("{miss}"))
	collect(err)
	exporter.operationDuration, err = meter.Float64Histogram("genesisdb.operation.duration", metric.WithDescription("Duration of store operations"), metric.WithUnit("s"))
	collect(err)
	exporter.errors, err = meter.Int64Counter("genesisdb.errors", metric.WithDescription("Failed store operations"), metric.WithUnit("{error}"))
	collect(err)
	_, err = meter.Int64ObservableGauge("genesisdb.cache.entries", metric.WithDescription("Events held in the cache"), metric.WithUnit("{event}"),
		metric.WithInt64Callback(func(_ context.Context, observer metric.Int64Observer) error {
			exporter.cacheEntries.Lock()
			defer exporter.cacheEntries.Unlock()
			observer.Observe(exporter.cacheEntriesValue)
			return nil
		}))
	collect(err)
	if len(errs) > 0 {
		return fmt.Errorf("create OpenTelemetry instruments: %w", errors.Join(errs...))
	}
	return nil
}

// Shutdown exports the remaining metrics and stops the exporter.
func (exporter *Exporter) Shutdown(ctx context.Context) error {
	return exporter.provider.Shutdown(ctx)
}

// Observer signals carry no context, so recording uses the background
// context. The OpenTelemetry SDK does not block on it.

// EventCommitted implements core.Observer. The subject is intentionally not
// recorded, see the package documentation.
func (exporter *Exporter) EventCommitted(subject, eventType string) {
	exporter.events.Add(context.Background(), 1, metric.WithAttributes(attribute.String("type", exporter.eventTypeLabel(eventType))))
}

// eventTypeLabel admits new distinct type values only within the budget.
func (exporter *Exporter) eventTypeLabel(eventType string) string {
	if exporter.maxEventTypeLabels < 0 {
		return OverflowLabel
	}
	exporter.eventTypeMutex.Lock()
	defer exporter.eventTypeMutex.Unlock()
	if _, isKnown := exporter.eventTypeLabels[eventType]; isKnown {
		return eventType
	}
	if len(exporter.eventTypeLabels) >= exporter.maxEventTypeLabels {
		return OverflowLabel
	}
	exporter.eventTypeLabels[eventType] = struct{}{}
	return eventType
}

// CacheHit implements core.Observer.
func (exporter *Exporter) CacheHit() { exporter.cacheHits.Add(context.Background(), 1) }

// CacheMiss implements core.Observer.
func (exporter *Exporter) CacheMiss() { exporter.cacheMisses.Add(context.Background(), 1) }

// CacheSizeChanged implements core.Observer.
func (exporter *Exporter) CacheSizeChanged(entries int) {
	exporter.cacheEntries.Lock()
	exporter.cacheEntriesValue = int64(entries)
	exporter.cacheEntries.Unlock()
}

// OperationCompleted implements core.Observer.
func (exporter *Exporter) OperationCompleted(operation string, duration time.Duration) {
	exporter.operationDuration.Record(context.Background(), duration.Seconds(), metric.WithAttributes(attribute.String("operation", operation)))
}

// ErrorOccurred implements core.Observer.
func (exporter *Exporter) ErrorOccurred(operation string) {
	exporter.errors.Add(context.Background(), 1, metric.WithAttributes(attribute.String("operation", operation)))
}
