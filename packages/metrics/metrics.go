// Package metrics exports Prometheus metrics. Collector implements
// core.Observer, so the store reports to it without depending on Prometheus.
//
// Label cardinality is bounded. Event subjects are never used as labels:
// every distinct subject would create a new time series that lives for the
// rest of the process, so a store with UUID subjects (or a writer who picks
// subjects deliberately) would grow the process memory and the scrape size
// without limit, and would export personal identifiers to the monitoring
// system. Event types are exported as a label only up to MaxEventTypeLabels
// distinct values, further types are counted under the label value "other".
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// DefaultMaxEventTypeLabels bounds the distinct event type label values.
const DefaultMaxEventTypeLabels = 256

// OverflowLabel is the label value used once the type budget is exhausted.
const OverflowLabel = "other"

// Options configure a collector.
type Options struct {
	// IncludeLegacyNames also exports every metric under its pre 2.0 name
	// with the prefix "eventstore_" instead of "genesisdb_". The legacy
	// per-subject series are not exported, their cardinality is unbounded.
	IncludeLegacyNames bool
	// MaxEventTypeLabels bounds the distinct values of the type label. 0
	// uses DefaultMaxEventTypeLabels, a negative value disables the type
	// label entirely (every event counts under OverflowLabel).
	MaxEventTypeLabels int
}

// Collector records GenesisDB metrics in its own Prometheus registry.
type Collector struct {
	registry *prometheus.Registry
	families []*metricFamily

	maxEventTypeLabels int
	mutex              sync.Mutex
	eventTypeLabels    map[string]struct{}
}

// metricFamily holds one metric under its current and, optionally, its
// legacy name.
type metricFamily struct {
	counters   []*prometheus.CounterVec
	gauges     []*prometheus.GaugeVec
	histograms []*prometheus.HistogramVec
}

// Metric indexes into Collector.families.
const (
	eventsTotal = iota
	eventsByType
	eventTypeLabelsTotal
	cacheSize
	cacheHits
	cacheMisses
	operationDuration
	errorsTotal
)

// New returns a collector with a fresh registry.
func New(options Options) *Collector {
	collector := &Collector{registry: prometheus.NewRegistry(), eventTypeLabels: map[string]struct{}{}}
	collector.maxEventTypeLabels = options.MaxEventTypeLabels
	if collector.maxEventTypeLabels == 0 {
		collector.maxEventTypeLabels = DefaultMaxEventTypeLabels
	}
	prefixes := []string{"genesisdb_"}
	if options.IncludeLegacyNames {
		prefixes = append(prefixes, "eventstore_")
	}
	counter := func(name, help string, labels ...string) *metricFamily {
		family := &metricFamily{}
		for _, prefix := range prefixes {
			vector := prometheus.NewCounterVec(prometheus.CounterOpts{Name: prefix + name, Help: help}, labels)
			collector.registry.MustRegister(vector)
			family.counters = append(family.counters, vector)
		}
		return family
	}
	gauge := func(name, help string, labels ...string) *metricFamily {
		family := &metricFamily{}
		for _, prefix := range prefixes {
			vector := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: prefix + name, Help: help}, labels)
			collector.registry.MustRegister(vector)
			family.gauges = append(family.gauges, vector)
		}
		return family
	}
	histogram := func(name, help string, labels ...string) *metricFamily {
		family := &metricFamily{}
		for _, prefix := range prefixes {
			vector := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: prefix + name, Help: help, Buckets: prometheus.DefBuckets}, labels)
			collector.registry.MustRegister(vector)
			family.histograms = append(family.histograms, vector)
		}
		return family
	}
	collector.families = []*metricFamily{
		eventsTotal:          counter("events_total", "Total number of committed events"),
		eventsByType:         gauge("events_by_type", "Number of committed events per type, bounded distinct types, overflow under \"other\"", "type"),
		eventTypeLabelsTotal: gauge("event_type_labels", "Distinct event type label values in use, including \"other\" once the budget is exhausted"),
		cacheSize:            gauge("cache_size", "Size of the cache", "type"),
		cacheHits:            counter("cache_hits_total", "Total number of cache hits"),
		cacheMisses:          counter("cache_misses_total", "Total number of cache misses"),
		operationDuration:    histogram("operation_duration_seconds", "Duration of operations in seconds", "operation"),
		errorsTotal:          counter("errors_total", "Total number of errors", "type"),
	}
	return collector
}

// Handler serves the metrics in the Prometheus text format.
func (collector *Collector) Handler() http.Handler {
	return promhttp.HandlerFor(collector.registry, promhttp.HandlerOpts{})
}

func (collector *Collector) increment(metric int, labels ...string) {
	for _, vector := range collector.families[metric].counters {
		vector.WithLabelValues(labels...).Inc()
	}
	for _, vector := range collector.families[metric].gauges {
		vector.WithLabelValues(labels...).Inc()
	}
}

// eventTypeLabel returns the label value for an event type, admitting new
// distinct values only within the budget.
func (collector *Collector) eventTypeLabel(eventType string) string {
	if collector.maxEventTypeLabels < 0 {
		return OverflowLabel
	}
	collector.mutex.Lock()
	defer collector.mutex.Unlock()
	if _, isKnown := collector.eventTypeLabels[eventType]; isKnown {
		return eventType
	}
	if len(collector.eventTypeLabels) >= collector.maxEventTypeLabels {
		if _, hasOverflow := collector.eventTypeLabels[OverflowLabel]; !hasOverflow {
			collector.eventTypeLabels[OverflowLabel] = struct{}{}
			collector.setGauge(eventTypeLabelsTotal, float64(len(collector.eventTypeLabels)))
		}
		return OverflowLabel
	}
	collector.eventTypeLabels[eventType] = struct{}{}
	collector.setGauge(eventTypeLabelsTotal, float64(len(collector.eventTypeLabels)))
	return eventType
}

func (collector *Collector) setGauge(metric int, value float64, labels ...string) {
	for _, vector := range collector.families[metric].gauges {
		vector.WithLabelValues(labels...).Set(value)
	}
}

// EventCommitted implements core.Observer. The subject is intentionally not
// recorded, see the package documentation.
func (collector *Collector) EventCommitted(subject, eventType string) {
	collector.increment(eventsTotal)
	collector.increment(eventsByType, collector.eventTypeLabel(eventType))
}

// CacheHit implements core.Observer.
func (collector *Collector) CacheHit() { collector.increment(cacheHits) }

// CacheMiss implements core.Observer.
func (collector *Collector) CacheMiss() { collector.increment(cacheMisses) }

// CacheSizeChanged implements core.Observer.
func (collector *Collector) CacheSizeChanged(entries int) {
	collector.setGauge(cacheSize, float64(entries), "events")
}

// OperationCompleted implements core.Observer.
func (collector *Collector) OperationCompleted(operation string, duration time.Duration) {
	for _, vector := range collector.families[operationDuration].histograms {
		vector.WithLabelValues(operation).Observe(duration.Seconds())
	}
}

// ErrorOccurred implements core.Observer.
func (collector *Collector) ErrorOccurred(operation string) {
	collector.increment(errorsTotal, operation)
}
