package httpserver

import (
	"encoding/json"
	"net/http"
	"strings"
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
	// Signature is present on signed events only, so unsigned events answer
	// byte for byte as before. The frozen /api/v1 never sends it.
	Signature string `json:"signature,omitempty"`
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
		Signature:       event.Signature,
	}
}

// newEventResponsesFor converts events for the API version of request.
func newEventResponsesFor(request *http.Request, events []*core.Event) []EventResponse {
	responses := newEventResponses(events)
	if !isVersion2Request(request) {
		for position := range responses {
			responses[position].Signature = ""
		}
	}
	return responses
}

// isVersion2Request reports whether request uses the current API.
func isVersion2Request(request *http.Request) bool {
	return strings.HasPrefix(request.URL.Path, v2Prefix+"/")
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
