package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	metrics := map[string]metricdata.Metrics{}
	for _, scope := range data.ScopeMetrics {
		if scope.Scope.Name != ScopeName {
			t.Errorf("unexpected scope %q", scope.Scope.Name)
		}
		for _, recorded := range scope.Metrics {
			metrics[recorded.Name] = recorded
		}
	}
	return metrics
}

func sumOf(t *testing.T, recorded metricdata.Metrics) int64 {
	t.Helper()
	sum, isSum := recorded.Data.(metricdata.Sum[int64])
	if !isSum {
		t.Fatalf("%s is %T, not a sum", recorded.Name, recorded.Data)
	}
	var total int64
	for _, point := range sum.DataPoints {
		total += point.Value
	}
	return total
}

func TestExporter_RecordsEverySignal(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	exporter, err := New(context.Background(), Options{Reader: reader, ServiceVersion: "2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	defer exporter.Shutdown(context.Background())

	exporter.EventCommitted("/a", "t")
	exporter.EventCommitted("/a", "t")
	exporter.EventCommitted("/b", "u")
	exporter.CacheHit()
	exporter.CacheMiss()
	exporter.CacheMiss()
	exporter.CacheSizeChanged(7)
	exporter.OperationCompleted("fetch", 3*time.Millisecond)
	exporter.ErrorOccurred("commit")

	metrics := collect(t, reader)
	for name, expected := range map[string]int64{"genesisdb.events": 3, "genesisdb.cache.hits": 1, "genesisdb.cache.misses": 2, "genesisdb.errors": 1} {
		recorded, exists := metrics[name]
		if !exists {
			t.Errorf("%s missing", name)
			continue
		}
		if total := sumOf(t, recorded); total != expected {
			t.Errorf("%s = %d, want %d", name, total, expected)
		}
	}

	events := metrics["genesisdb.events"].Data.(metricdata.Sum[int64])
	isFound := false
	for _, point := range events.DataPoints {
		if _, hasSubject := point.Attributes.Value(attribute.Key("subject")); hasSubject {
			t.Error("subjects must not be attributes, their cardinality is unbounded")
		}
		eventType, _ := point.Attributes.Value(attribute.Key("type"))
		if eventType.AsString() == "t" && point.Value == 2 {
			isFound = true
		}
	}
	if !isFound {
		t.Error("events are not attributed by type")
	}

	gauge, isGauge := metrics["genesisdb.cache.entries"].Data.(metricdata.Gauge[int64])
	if !isGauge || len(gauge.DataPoints) != 1 || gauge.DataPoints[0].Value != 7 {
		t.Errorf("cache entries %+v", metrics["genesisdb.cache.entries"].Data)
	}

	histogram, isHistogram := metrics["genesisdb.operation.duration"].Data.(metricdata.Histogram[float64])
	if !isHistogram || len(histogram.DataPoints) != 1 || histogram.DataPoints[0].Count != 1 {
		t.Fatalf("operation duration %+v", metrics["genesisdb.operation.duration"].Data)
	}
	if operation, _ := histogram.DataPoints[0].Attributes.Value("operation"); operation.AsString() != "fetch" {
		t.Error("duration is not attributed by operation")
	}
	if metrics["genesisdb.operation.duration"].Unit != "s" {
		t.Error("duration unit must be seconds")
	}
}

func TestExporter_ServiceResource(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.environment=test")
	reader := sdkmetric.NewManualReader()
	exporter, err := New(context.Background(), Options{Reader: reader, ServiceVersion: "2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	defer exporter.Shutdown(context.Background())
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	for key, expected := range map[attribute.Key]string{"service.name": "genesisdb", "service.version": "2.0.0", "deployment.environment": "test"} {
		if value, exists := data.Resource.Set().Value(key); !exists || value.AsString() != expected {
			t.Errorf("resource %s = %q, want %q", key, value.AsString(), expected)
		}
	}
}

// The real OTLP HTTP exporter delivers to the configured endpoint on
// Shutdown.
func TestExporter_PushesWithOTLPOverHTTP(t *testing.T) {
	var requests atomic.Int64
	var authorization atomic.Value
	collector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/metrics" || request.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Errorf("unexpected export %s %s", request.URL.Path, request.Header.Get("Content-Type"))
		}
		io.Copy(io.Discard, request.Body)
		authorization.Store(request.Header.Get("Authorization"))
		requests.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	exporter, err := New(context.Background(), Options{EndpointURL: collector.URL, Headers: map[string]string{"Authorization": "Bearer key"}, ExportInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	exporter.EventCommitted("/a", "t")
	if err := exporter.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() == 0 {
		t.Fatal("nothing was exported")
	}
	if authorization.Load() != "Bearer key" {
		t.Errorf("headers were not sent: %v", authorization.Load())
	}
}
