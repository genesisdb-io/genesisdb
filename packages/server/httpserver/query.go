package httpserver

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/query"
)

// QueryRequest is the body of POST /api/v2/query (and of POST /api/v1/q
// and /api/v1/query). Query is GenesisDB Query Language (GDBQL) text.
type QueryRequest struct {
	Query string `json:"query"`
}

func (server *Server) handleQuery(writer http.ResponseWriter, request *http.Request) {
	var body QueryRequest
	if !decodeRequestBody(writer, request, &body) {
		return
	}
	parsed, err := query.Parse(body.Query)
	if err != nil {
		http.Error(writer, fmt.Sprintf("Failed to parse query: %v", err), http.StatusBadRequest)
		return
	}
	result, err := query.Execute(request.Context(), server.store, parsed)
	if err != nil {
		http.Error(writer, fmt.Sprintf("Failed to execute query: %v", err), httpStatusForError(err))
		return
	}
	items := queryResultItems(result)
	// NDJSON is the default. /api/v2 honors x-disable-ndjson like every other
	// list route, /api/v1 keeps the NDJSON answer of 1.1.6 unchanged.
	if strings.HasPrefix(request.URL.Path, v2Prefix+"/") {
		writeNDJSONOrArray(writer, request, items)
		return
	}
	writeNDJSON(writer, items)
}

// queryResultItems flattens a query result into NDJSON items. Full events
// keep their persisted form, which matches every earlier release.
func queryResultItems(result any) []any {
	switch typed := result.(type) {
	case []query.Row:
		items := make([]any, len(typed))
		for position, row := range typed {
			items[position] = map[string]any(row)
		}
		return items
	case []*core.Event:
		items := make([]any, len(typed))
		for position, event := range typed {
			items[position] = event
		}
		return items
	case query.Row:
		return []any{map[string]any(typed)}
	}
	return []any{result}
}
