package httpserver

import (
	"encoding/json"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// EventResponse is the API form of an event. The field order is part of the
// API. The reference data key and the options are internal and omitted.
type EventResponse struct {
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
}

func newEventResponse(event *core.Event) EventResponse {
	return EventResponse{
		Source:          event.Source,
		Subject:         event.Subject,
		Type:            event.Type,
		SpecVersion:     event.SpecVersion,
		ID:              event.ID,
		Time:            event.Time,
		DataContentType: event.DataContentType,
		PredecessorHash: event.PredecessorHash,
		Data:            event.Data,
		Hash:            event.Hash,
	}
}

// newEventResponses converts events. System events never leave the API.
func newEventResponses(events []*core.Event) []EventResponse {
	responses := make([]EventResponse, 0, len(events))
	for _, event := range events {
		if !event.IsSystemEvent() {
			responses = append(responses, newEventResponse(event))
		}
	}
	return responses
}
