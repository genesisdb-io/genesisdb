package metrics

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, collector *Collector) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	collector.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(recorder.Body)
	return string(body)
}

func record(collector *Collector) {
	collector.EventCommitted("/a", "t")
	collector.CacheHit()
	collector.CacheMiss()
	collector.CacheSizeChanged(3)
	collector.OperationCompleted("fetch", time.Millisecond)
	collector.ErrorOccurred("commit")
}

func TestCollector_CurrentNames(t *testing.T) {
	collector := New(Options{})
	record(collector)
	exported := scrape(t, collector)
	for _, name := range []string{"genesisdb_events_total", "genesisdb_cache_hits_total", "genesisdb_cache_size", "genesisdb_operation_duration_seconds", "genesisdb_errors_total"} {
		if !strings.Contains(exported, name) {
			t.Errorf("%s missing", name)
		}
	}
	if strings.Contains(exported, "eventstore_") {
		t.Error("legacy names exported without IncludeLegacyNames")
	}
}

func TestCollector_LegacyNames(t *testing.T) {
	collector := New(Options{IncludeLegacyNames: true})
	record(collector)
	exported := scrape(t, collector)
	for _, name := range []string{"eventstore_events_total", "eventstore_cache_misses_total", "genesisdb_events_total"} {
		if !strings.Contains(exported, name) {
			t.Errorf("%s missing", name)
		}
	}
}

// TestCollector_BoundedCardinality checks that subjects never become labels
// and that the type label is capped, so the series count stays bounded.
func TestCollector_BoundedCardinality(t *testing.T) {
	collector := New(Options{MaxEventTypeLabels: 3, IncludeLegacyNames: true})
	for position := 0; position < 1000; position++ {
		collector.EventCommitted(fmt.Sprintf("/user/%d", position), fmt.Sprintf("type-%d", position%10))
	}
	exported := scrape(t, collector)
	if strings.Contains(exported, "/user/") {
		t.Fatal("subjects exported as label values")
	}
	if strings.Contains(exported, "events_by_subject") {
		t.Fatal("per-subject metric still registered")
	}
	typeSeries := 0
	for _, line := range strings.Split(exported, "\n") {
		if strings.HasPrefix(line, "genesisdb_events_by_type{") {
			typeSeries++
		}
	}
	if typeSeries != 4 {
		t.Fatalf("%d type series, want 3 plus %q", typeSeries, OverflowLabel)
	}
	if !strings.Contains(exported, `genesisdb_events_by_type{type="other"} 700`) {
		t.Fatalf("overflow not counted under %q:\n%s", OverflowLabel, exported)
	}
	if !strings.Contains(exported, "genesisdb_events_total 1000") {
		t.Fatal("total events missing")
	}
	if !strings.Contains(exported, "genesisdb_event_type_labels 4") {
		t.Fatal("label budget gauge missing")
	}
}
