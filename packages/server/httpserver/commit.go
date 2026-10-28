package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// maximumEventSizeBytes bounds the estimated size of one committed event.
const maximumEventSizeBytes = 64 * 1024

// CommitRequest is the body of POST /api/v2/commit and /api/v1/commit.
type CommitRequest struct {
	Events []struct {
		Source  string          `json:"source"`
		Subject string          `json:"subject"`
		Type    string          `json:"type"`
		Data    json.RawMessage `json:"data,omitempty"`
		// ReferenceData is the payload of an event stored as reference
		// data. It is the legacy alternative to options.storeDataAsReference.
		ReferenceData json.RawMessage   `json:"ref,omitempty"`
		Options       core.EventOptions `json:"options,omitempty"`
	} `json:"events"`
	Preconditions []core.Precondition `json:"preconditions"`
}

func (server *Server) handleCommit(writer http.ResponseWriter, request *http.Request) {
	var body CommitRequest
	if !decodeRequestBody(writer, request, &body) {
		return
	}
	committed, err := server.commit(request.Context(), &body)
	if err != nil {
		var failed *core.PreconditionFailedError
		if errors.As(err, &failed) {
			writeJSON(writer, http.StatusPreconditionFailed, failed)
			return
		}
		var invalid *invalidCommitError
		if errors.As(err, &invalid) {
			http.Error(writer, invalid.message, http.StatusBadRequest)
			return
		}
		// After the event log was written an error may still mean the events
		// are durable. Clients must not retry blindly.
		http.Error(writer, fmt.Sprintf("Failed to commit events: %v", err), httpStatusForError(err))
		return
	}
	writeNDJSONOrArray(writer, request, newEventResponses(committed))
}

// invalidCommitError is a client error in a commit request, reported
// before anything is written.
type invalidCommitError struct{ message string }

func (err *invalidCommitError) Error() string { return err.message }

// commit validates and commits the events of body. It is shared by the
// HTTP API and the MCP endpoint. A failed precondition is returned as
// *core.PreconditionFailedError, a client error as *invalidCommitError.
func (server *Server) commit(ctx context.Context, body *CommitRequest) ([]*core.Event, error) {
	if len(body.Events) == 0 {
		return nil, &invalidCommitError{"Commit requires at least one event"}
	}
	for position, event := range body.Events {
		if problem := server.validateCommittedEvent(position, event.Source, event.Subject, event.Type, event.Data, event.ReferenceData); problem != "" {
			return nil, &invalidCommitError{problem}
		}
	}

	// ID, time and hashes are assigned by storage during the commit.
	committed := make([]*core.Event, 0, len(body.Events))
	for _, event := range body.Events {
		subject := event.Subject
		// Subjects with a single segment get a generated ID, for example
		// "/customer" becomes "/customer/<uuid>".
		if len(strings.Split(subject, "/")) < 3 {
			subject += "/" + core.NewUUID()
		}
		newEvent := &core.Event{
			Source:  event.Source,
			Subject: subject,
			Type:    event.Type,
			Data:    event.Data,
			Options: event.Options,
		}
		if len(event.ReferenceData) > 0 {
			newEvent.Data = event.ReferenceData
			newEvent.ReferenceDataKey = core.ReferenceDataPending
		}
		committed = append(committed, newEvent)
	}

	validate := func(event *core.Event) error {
		return server.schemas.ValidateEvent(event.Type, event.Data)
	}
	if err := server.store.CommitValidated(ctx, committed, validate, body.Preconditions...); err != nil {
		return nil, err
	}
	return committed, nil
}

// validateCommittedEvent returns a client error message, or "" if the event
// is valid.
func (server *Server) validateCommittedEvent(position int, source, subject, eventType string, data, referenceData json.RawMessage) string {
	switch {
	case strings.TrimSpace(source) == "":
		return fmt.Sprintf("Event %d: source cannot be empty", position)
	case strings.TrimSpace(subject) == "":
		return fmt.Sprintf("Event %d: subject cannot be empty", position)
	case !strings.HasPrefix(strings.TrimSpace(subject), "/"):
		return fmt.Sprintf("Event %d: subject must start with /", position)
	case strings.TrimSpace(eventType) == "":
		return fmt.Sprintf("Event %d: type cannot be empty", position)
	}
	hasData, hasReferenceData := len(data) > 0, len(referenceData) > 0
	if !hasData && !hasReferenceData {
		return fmt.Sprintf("Event %d: either data or ref must be provided", position)
	}
	if hasData && hasReferenceData {
		return fmt.Sprintf("Event %d: cannot provide both data and ref", position)
	}
	payload, fieldName := data, "data"
	if hasReferenceData {
		payload, fieldName = referenceData, "ref"
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return fmt.Sprintf("Event %d: %s must be valid JSON: %v", position, fieldName, err)
	}
	if decoded == nil {
		return fmt.Sprintf("Event %d: %s cannot be null", position, fieldName)
	}
	if _, isObject := decoded.(map[string]any); !isObject {
		return fmt.Sprintf("Event %d: %s must be a JSON object", position, fieldName)
	}
	// Estimated size: the payload plus overhead for ID, time, hashes and
	// the JSON structure.
	estimatedSize := len(source) + len(subject) + len(eventType) + len(payload) + 500
	if estimatedSize > maximumEventSizeBytes {
		return fmt.Sprintf("Event %d: event size exceeds 64KB limit (estimated: %d bytes)", position, estimatedSize)
	}
	if err := server.schemas.ValidateEvent(eventType, payload); err != nil {
		// The error names fields of the payload, which may be personal data.
		// It goes to the client, not to the server log.
		server.logger.Debug("schema validation failed", "event", position, "type", eventType)
		return fmt.Sprintf("Event %d: schema validation failed: %v", position, err)
	}
	return ""
}
