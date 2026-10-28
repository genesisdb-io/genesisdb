package core

import (
	"bytes"
	"encoding/json"
	"time"
)

// Fixed values written into every event and every hash chain.
const (
	// SystemEventSource is the source of internal system events (for example
	// the audit trail of erasures). System events are never returned by reads.
	SystemEventSource = "tag:system.genesisdb.io"

	// EventSpecVersion is the CloudEvents specification version of all events.
	EventSpecVersion = "1.0"

	// EventDataContentType is the content type of all event payloads.
	EventDataContentType = "application/json"

	// ZeroHash is the predecessor hash of the first event of a hash chain.
	ZeroHash = "0000000000000000000000000000000000000000000000000000000000000000"

	// ReferenceDataPending marks an event whose Data must be moved into the
	// reference data log during the commit (the legacy "ref" request field).
	ReferenceDataPending = "pending"

	// emptyEventData replaces a payload that is stored as reference data.
	emptyEventData = "{}"
)

// Event is a single CloudEvents compatible event.
//
// The JSON field names and their order are part of the on-disk format and of
// every backup. They must never change.
type Event struct {
	Source          string          `json:"source"`
	Subject         string          `json:"subject"`
	Type            string          `json:"type"`
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Time            time.Time       `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	PredecessorHash string          `json:"predecessorhash"`
	Data            json.RawMessage `json:"data"`
	Hash            string          `json:"hash"`
	// HashVersion is absent on historical events. Version 3 binds the predecessor.
	HashVersion int `json:"hashversion,omitempty"`

	// ReferenceDataKey identifies the payload in the reference data log when
	// the payload is stored as reference data (GDPR erasure support).
	ReferenceDataKey string `json:"ref,omitempty"`

	Options EventOptions `json:"options,omitempty"`
}

// EventOptions are per-event commit options. They are persisted unchanged.
type EventOptions struct {
	// StoreDataAsReference moves the payload into the reference data log, so
	// it can be erased later without breaking the hash chain.
	StoreDataAsReference bool `json:"storeDataAsReference,omitempty"`
}

// Clone returns a deep copy of the event.
func (e *Event) Clone() *Event {
	if e == nil {
		return nil
	}
	clone := *e
	if e.Data != nil {
		clone.Data = append(json.RawMessage(nil), e.Data...)
	}
	return &clone
}

// IsSystemEvent reports whether the event is an internal system event.
func (e *Event) IsSystemEvent() bool {
	return e.Source == SystemEventSource
}

// HasReferenceData reports whether the payload of a persisted event lives in
// the reference data log.
func (e *Event) HasReferenceData() bool {
	return e.ReferenceDataKey != "" && e.ReferenceDataKey != ReferenceDataPending
}

// IsReferenceDataPlaceholder reports whether persisted event data is the
// placeholder written in place of a payload that is stored as reference data.
func IsReferenceDataPlaceholder(data json.RawMessage) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) == 0 || string(trimmed) == emptyEventData
}

// EmptyEventData returns the placeholder payload of events whose data is
// stored as reference data.
func EmptyEventData() json.RawMessage {
	return json.RawMessage(emptyEventData)
}

// ReferenceDataEntry is one record of the reference data log.
//
// The JSON field names are part of the on-disk format and of every backup.
type ReferenceDataEntry struct {
	// Key equals Event.ReferenceDataKey of the event that owns the payload.
	Key string `json:"eventHash"`
	// Data is the payload of the event.
	Data json.RawMessage `json:"refData"`
	// Timestamp is the time the entry was written or last replaced.
	Timestamp time.Time `json:"timestamp"`
}
