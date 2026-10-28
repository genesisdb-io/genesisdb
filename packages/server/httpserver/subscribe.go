package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/store"
)

// SubscribeRequest is the body of POST /api/v2/subscribe (and of POST
// /api/v1/observe).
type SubscribeRequest struct {
	Subject string `json:"subject"`
	// Interval is the poll interval in seconds, integer or fraction (for
	// example 0.5). Default 1 second, clamped to 0.1 seconds .. 24 hours.
	Interval float64              `json:"interval"`
	Options  *FetchRequestOptions `json:"options,omitempty"`
}

// heartbeatPayload is sent when a poll found no new events.
var heartbeatPayload = []byte(`{"payload":""}`)

func (server *Server) handleSubscribe(writer http.ResponseWriter, request *http.Request) {
	var body SubscribeRequest
	if !decodeRequestBody(writer, request, &body) {
		return
	}
	_, canStream := writer.(http.Flusher)
	if !canStream {
		http.Error(writer, "Streaming unsupported", http.StatusInternalServerError)
		return
	}
	options := store.SubscribeOptions{FetchOptions: body.Options.toStoreOptions()}
	// Subscriptions never filter by latest event type.
	options.LatestByEventType = ""
	if body.Interval > 0 {
		if body.Interval >= store.MaximumPollInterval.Seconds() {
			options.PollInterval = store.MaximumPollInterval
		} else {
			options.PollInterval = time.Duration(body.Interval * float64(time.Second))
		}
	}
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	subscription, err := server.store.Subscribe(ctx, body.Subject, options)
	if err != nil {
		server.logger.Warn("subscription failed", "subject", body.Subject, "error", err)
		http.Error(writer, "Failed to get initial events", httpStatusForError(err))
		return
	}

	header := writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("Access-Control-Allow-Origin", "*")

	controller := http.NewResponseController(writer)
	defer controller.SetWriteDeadline(time.Time{})
	for batch := range subscription.Batches() {
		_ = controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if batch.Err != nil {
			// The subscription gave up. Tell the client explicitly and end the
			// stream, a silent stall would look like an idle subject.
			server.logger.Warn("subscription ended", "subject", body.Subject, "error", batch.Err)
			encoded, _ := json.Marshal(map[string]string{"error": batch.Err.Error()})
			fmt.Fprintf(writer, "event: error\ndata: %s\n\n", encoded)
			controller.Flush()
			return
		}
		if !batch.IsHistory && len(batch.Events) == 0 && !batch.HasMore {
			if _, err := fmt.Fprintf(writer, "data: %s\n\n", heartbeatPayload); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
			continue
		}
		for _, event := range newEventResponses(batch.Events) {
			encoded, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(writer, "data: %s\n\n", encoded); err != nil {
				return
			}
		}
		if err := controller.Flush(); err != nil {
			return
		}
	}
}
