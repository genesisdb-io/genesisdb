package httpserver

import (
	"fmt"
	"net/http"

	"github.com/genesisdb-io/genesisdb/v2/packages/store"
)

// FetchRequest is the body of POST /api/v2/fetch (and of POST
// /api/v1/stream).
type FetchRequest struct {
	Subject string               `json:"subject"`
	Options *FetchRequestOptions `json:"options,omitempty"`
}

// FetchRequestOptions narrow a fetch. The field names are part of the API.
type FetchRequestOptions struct {
	LowerBound             string `json:"lowerBound,omitempty"`
	IncludeLowerBoundEvent bool   `json:"includeLowerBoundEvent,omitempty"`
	UpperBound             string `json:"upperBound,omitempty"`
	IncludeUpperBoundEvent bool   `json:"includeUpperBoundEvent,omitempty"`
	LatestByEventType      string `json:"latestByEventType,omitempty"`
}

func (options *FetchRequestOptions) toStoreOptions() store.FetchOptions {
	if options == nil {
		return store.FetchOptions{}
	}
	return store.FetchOptions{
		LowerBound:             options.LowerBound,
		IncludeLowerBoundEvent: options.IncludeLowerBoundEvent,
		UpperBound:             options.UpperBound,
		IncludeUpperBoundEvent: options.IncludeUpperBoundEvent,
		LatestByEventType:      options.LatestByEventType,
	}
}

func (server *Server) handleFetch(writer http.ResponseWriter, request *http.Request) {
	var body FetchRequest
	if !decodeRequestBody(writer, request, &body) {
		return
	}
	events, err := server.store.Fetch(request.Context(), body.Subject, body.Options.toStoreOptions())
	if err != nil {
		http.Error(writer, fmt.Sprintf("Failed to get events: %v", err), httpStatusForError(err))
		return
	}
	writeNDJSONOrArray(writer, request, newEventResponses(events))
}

type subjectResponse struct {
	Subject string `json:"subject"`
}

type eventTypeResponse struct {
	Type string `json:"type"`
}

func (server *Server) handleListSubjects(writer http.ResponseWriter, request *http.Request) {
	subjects, err := server.store.ListTopLevelSubjects(request.Context())
	if err != nil {
		http.Error(writer, fmt.Sprintf("Failed to get subjects: %v", err), httpStatusForError(err))
		return
	}
	responses := make([]subjectResponse, len(subjects))
	for position, subject := range subjects {
		responses[position] = subjectResponse{Subject: subject}
	}
	writeNDJSONOrArray(writer, request, responses)
}

func (server *Server) handleListEventTypes(writer http.ResponseWriter, request *http.Request) {
	eventTypes, err := server.store.ListEventTypes(request.Context())
	if err != nil {
		http.Error(writer, fmt.Sprintf("Failed to get event types: %v", err), httpStatusForError(err))
		return
	}
	responses := make([]eventTypeResponse, len(eventTypes))
	for position, eventType := range eventTypes {
		responses[position] = eventTypeResponse{Type: eventType}
	}
	writeNDJSONOrArray(writer, request, responses)
}
