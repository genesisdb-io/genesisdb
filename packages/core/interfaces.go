package core

import (
	"context"
	"time"
)

// EventSelector describes a union of index selections. Engines use it to ask
// for a safe superset of the events that can match a query.
type EventSelector struct {
	// All selects every regular event. The other fields are ignored.
	All bool
	// ExactSubjects selects events whose subject equals one of the values.
	ExactSubjects []string
	// SubjectsWithDescendants selects events whose subject equals one of the
	// values or is a descendant of it.
	SubjectsWithDescendants []string
	// EventTypes selects events whose type equals one of the values.
	EventTypes []string
}

// EventSource returns events for a selector, deduplicated and ordered by
// time. The store implements it, the GDBQL engine consumes it.
type EventSource interface {
	SelectEvents(ctx context.Context, selector EventSelector) ([]*Event, error)
}

// EventCounter is optionally implemented by an EventSource that can count a
// selection from its index without loading events. The count must equal
// len(SelectEvents(selector)) for the same snapshot.
type EventCounter interface {
	CountSelectedEvents(ctx context.Context, selector EventSelector) (int, error)
}

// GDBQLPreconditionEvaluator evaluates the GenesisDB Query Language (GDBQL)
// query of an isQueryResultTrue precondition against source.
type GDBQLPreconditionEvaluator func(ctx context.Context, source EventSource, gdbql string) (bool, error)

// RecordLocation is the physical location of one record in an event log.
type RecordLocation struct {
	// LogFile is the file name of the event log inside the events directory.
	LogFile string
	// Offset is the byte offset of the record line.
	Offset int64
	// Length is the record line length in bytes, without the newline.
	Length int64
}

// StoredEvent is an event together with the location of its record.
type StoredEvent struct {
	Event    *Event
	Location RecordLocation
}

// EventLogReader gives read access to the authoritative event logs. The
// storage engine implements it, the index engine consumes it.
type EventLogReader interface {
	// ListEventLogs returns the sorted file names of all regular event logs.
	ListEventLogs() ([]string, error)
	// EventLogSize returns the current size of an event log in bytes.
	EventLogSize(logFile string) (int64, error)
	// ScanEventLog visits every record from startOffset on. A start offset
	// of 0 expects the log header first. It returns the end offset and the
	// last visited record.
	ScanEventLog(logFile string, startOffset int64, visit func(StoredEvent) error) (endOffset int64, last *StoredEvent, err error)
	// ReadEventAt reads one record by its location.
	ReadEventAt(location RecordLocation) (*Event, error)
}

// Observer receives operational signals, for example to export metrics.
// Implementations must be safe for concurrent use.
type Observer interface {
	EventCommitted(subject, eventType string)
	CacheHit()
	CacheMiss()
	CacheSizeChanged(entries int)
	OperationCompleted(operation string, duration time.Duration)
	ErrorOccurred(operation string)
}

// NoopObserver ignores every signal.
type NoopObserver struct{}

func (NoopObserver) EventCommitted(string, string)            {}
func (NoopObserver) CacheHit()                                {}
func (NoopObserver) CacheMiss()                               {}
func (NoopObserver) CacheSizeChanged(int)                     {}
func (NoopObserver) OperationCompleted(string, time.Duration) {}
func (NoopObserver) ErrorOccurred(string)                     {}

// MultiObserver forwards every signal to each of its observers in order, for
// example to Prometheus and OpenTelemetry at the same time.
type MultiObserver []Observer

func (observers MultiObserver) EventCommitted(subject, eventType string) {
	for _, observer := range observers {
		observer.EventCommitted(subject, eventType)
	}
}

func (observers MultiObserver) CacheHit() {
	for _, observer := range observers {
		observer.CacheHit()
	}
}

func (observers MultiObserver) CacheMiss() {
	for _, observer := range observers {
		observer.CacheMiss()
	}
}

func (observers MultiObserver) CacheSizeChanged(entries int) {
	for _, observer := range observers {
		observer.CacheSizeChanged(entries)
	}
}

func (observers MultiObserver) OperationCompleted(operation string, duration time.Duration) {
	for _, observer := range observers {
		observer.OperationCompleted(operation, duration)
	}
}

func (observers MultiObserver) ErrorOccurred(operation string) {
	for _, observer := range observers {
		observer.ErrorOccurred(operation)
	}
}
