package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/query"
)

// Result caps of the MCP tools. An agent copies results into its context,
// so a tool never returns an unbounded list: it returns one page and says
// how to get the next one.
const (
	mcpDefaultLimit = 100
	mcpMaximumLimit = 1000
)

// mcpTool is one tool of the MCP endpoint.
type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
	role        mcpRole
	run         func(server *Server, ctx context.Context, arguments json.RawMessage) (any, error)
}

// mcpToolError is a failure the agent can act on. It is returned as a tool
// result with isError, not as a protocol error, so the agent sees it.
type mcpToolError struct{ message string }

func (err *mcpToolError) Error() string { return err.message }

func (tool *mcpTool) call(server *Server, ctx context.Context, arguments json.RawMessage) map[string]any {
	result, err := tool.run(server, ctx, arguments)
	if err != nil {
		var failed *core.PreconditionFailedError
		var invalid *invalidCommitError
		var toolError *mcpToolError
		message := err.Error()
		switch {
		case errors.As(err, &failed):
			message = fmt.Sprintf("Precondition %s failed: %s. Nothing was committed.", failed.Type, failed.Message)
		case errors.As(err, &invalid):
			message = invalid.message + ". Nothing was committed."
		case errors.As(err, &toolError):
		case errors.Is(err, core.ErrResourceLimit):
			message = "The selection exceeds the read budget of this server. Narrow the subject or the query."
		default:
			message = "Failed: " + message
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": message}}, "isError": true}
	}
	text, _ := json.MarshalIndent(result, "", "  ")
	response := map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}}
	if _, isObject := result.(map[string]any); isObject {
		response["structuredContent"] = result
	}
	return response
}

// decodeArguments decodes tool arguments strictly, unknown fields are
// rejected so a misspelled option is not silently ignored.
func decodeArguments(arguments json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &mcpToolError{"Invalid arguments: " + err.Error()}
	}
	return nil
}

func pageLimit(requested int) (int, error) {
	switch {
	case requested == 0:
		return mcpDefaultLimit, nil
	case requested < 0 || requested > mcpMaximumLimit:
		return 0, &mcpToolError{fmt.Sprintf("limit must be between 1 and %d", mcpMaximumLimit)}
	}
	return requested, nil
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

var readOnlyAnnotations = map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}

var limitProperty = map[string]any{"type": "integer", "minimum": 1, "maximum": mcpMaximumLimit, "description": fmt.Sprintf("Maximum number of results, default %d", mcpDefaultLimit)}

var mcpTools = []*mcpTool{
	{
		Name:        "list_subjects",
		Title:       "List subjects",
		Description: "Lists the subjects of the store. Without arguments it returns the top level subjects, with under it returns every subject below that subject. Use it first to learn the shape of the store.",
		InputSchema: objectSchema(map[string]any{
			"under": map[string]any{"type": "string", "description": "Return all subjects below this subject, for example /customer"},
			"limit": limitProperty,
		}),
		Annotations: readOnlyAnnotations,
		role:        mcpRoleRead,
		run:         runListSubjects,
	},
	{
		Name:        "list_event_types",
		Title:       "List event types",
		Description: "Lists every event type in the store.",
		InputSchema: objectSchema(map[string]any{}),
		Annotations: readOnlyAnnotations,
		role:        mcpRoleRead,
		run: func(server *Server, ctx context.Context, arguments json.RawMessage) (any, error) {
			var input struct{}
			if err := decodeArguments(arguments, &input); err != nil {
				return nil, err
			}
			types, err := server.store.ListEventTypes(ctx)
			if err != nil {
				return nil, err
			}
			return map[string]any{"types": nonNil(types)}, nil
		},
	},
	{
		Name:  "fetch_events",
		Title: "Fetch events",
		Description: "Returns the events of a subject and all of its descendants in commit order, one page at a time. " +
			"When more events exist the result contains nextLowerBound, pass it as lowerBound to read the next page.",
		InputSchema: objectSchema(map[string]any{
			"subject":           map[string]any{"type": "string", "description": "Subject to read, for example /customer/42. / reads the whole store."},
			"lowerBound":        map[string]any{"type": "string", "description": "Event ID, only events after it are returned"},
			"latestByEventType": map[string]any{"type": "string", "description": "Return only the newest event of this type below the subject"},
			"limit":             limitProperty,
		}, "subject"),
		Annotations: readOnlyAnnotations,
		role:        mcpRoleRead,
		run:         runFetchEvents,
	},
	{
		Name:  "query",
		Title: "Run a GDBQL query",
		Description: "Runs a GDBQL query, for example STREAM e FROM events WHERE e.type == 'order.placed' GROUP BY e.data.category MAP { category: e.data.category, orders: COUNT() }. " +
			"Read the resource genesisdb://docs/gdbql for the syntax. Prefer aggregates and projections over returning whole events.",
		InputSchema: objectSchema(map[string]any{
			"query": map[string]any{"type": "string", "description": "GDBQL query text"},
			"limit": limitProperty,
		}, "query"),
		Annotations: readOnlyAnnotations,
		role:        mcpRoleRead,
		run:         runQuery,
	},
	{
		Name:        "list_schemas",
		Title:       "List schemas",
		Description: "Lists the JSON schemas registered per event type. Commits of a type with a schema are validated against it.",
		InputSchema: objectSchema(map[string]any{}),
		Annotations: readOnlyAnnotations,
		role:        mcpRoleRead,
		run: func(server *Server, ctx context.Context, arguments json.RawMessage) (any, error) {
			var input struct{}
			if err := decodeArguments(arguments, &input); err != nil {
				return nil, err
			}
			registered, err := server.schemas.List()
			if err != nil {
				return nil, err
			}
			schemas := make([]any, 0, len(registered))
			for _, entry := range registered {
				schemas = append(schemas, map[string]any{"type": entry.EventType, "schema": entry.Schema})
			}
			return map[string]any{"schemas": schemas}, nil
		},
	},
	{
		Name:        "get_status",
		Title:       "Get status",
		Description: "Returns engine version, event, subject and type counts, storage format and readiness.",
		InputSchema: objectSchema(map[string]any{}),
		Annotations: readOnlyAnnotations,
		role:        mcpRoleRead,
		run: func(server *Server, ctx context.Context, arguments json.RawMessage) (any, error) {
			var input struct{}
			if err := decodeArguments(arguments, &input); err != nil {
				return nil, err
			}
			status := server.status(ctx)
			return map[string]any{
				"engine":  status.Engine,
				"events":  status.Events,
				"storage": status.Storage,
			}, nil
		},
	},
	{
		Name:  "commit_events",
		Title: "Commit events",
		Description: "Commits one or more events atomically, optionally guarded by preconditions (isSubjectNew, isSubjectExisting, isQueryResultTrue). " +
			"Events are immutable once committed. Use past tense types like io.example.order-placed, and store personal data with options.storeDataAsReference so it can be erased.",
		InputSchema: objectSchema(map[string]any{
			"events": map[string]any{
				"type":     "array",
				"minItems": 1,
				"items": objectSchema(map[string]any{
					"source":  map[string]any{"type": "string", "description": "Origin of the event, for example io.example.shop"},
					"subject": map[string]any{"type": "string", "description": "Subject path starting with /, for example /customer/42"},
					"type":    map[string]any{"type": "string", "description": "Event type in past tense, for example io.example.shop.customer-registered"},
					"data":    map[string]any{"type": "object", "description": "Payload, a JSON object"},
					"options": objectSchema(map[string]any{
						"storeDataAsReference": map[string]any{"type": "boolean", "description": "Store the payload as erasable reference data"},
					}),
				}, "source", "subject", "type", "data"),
			},
			"preconditions": map[string]any{
				"type": "array",
				"items": objectSchema(map[string]any{
					"type":    map[string]any{"type": "string", "enum": []string{"isSubjectNew", "isSubjectExisting", "isQueryResultTrue"}},
					"payload": map[string]any{"type": "object", "description": `{"subject": "/..."} or {"query": "<GDBQL>"}`},
				}, "type", "payload"),
			},
		}, "events"),
		Annotations: map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false},
		role:        mcpRoleWrite,
		run:         runCommitEvents,
	},
	{
		Name:        "verify_hash_chain",
		Title:       "Verify the hash chain",
		Description: "Verifies the SHA-256 hash chain of the whole store. Commits wait while it runs, avoid it under heavy write load.",
		InputSchema: objectSchema(map[string]any{}),
		Annotations: map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
		role:        mcpRoleAdmin,
		run: func(server *Server, ctx context.Context, arguments json.RawMessage) (any, error) {
			var input struct{}
			if err := decodeArguments(arguments, &input); err != nil {
				return nil, err
			}
			report, err := server.store.VerifyHashChain(ctx)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"valid":         report.IsValid(),
				"checkedEvents": report.CheckedEvents,
				"subjects":      nonNil(report.Subjects),
				"problem":       report.Problem,
			}, nil
		},
	},
}

// mcpToolsFor lists the tools a role may call.
func mcpToolsFor(role mcpRole) []*mcpTool {
	tools := make([]*mcpTool, 0, len(mcpTools))
	for _, tool := range mcpTools {
		if role >= tool.role {
			tools = append(tools, tool)
		}
	}
	return tools
}

func mcpToolByName(name string, role mcpRole) (*mcpTool, bool) {
	for _, tool := range mcpToolsFor(role) {
		if tool.Name == name {
			return tool, true
		}
	}
	return nil, false
}

func nonNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

func runListSubjects(server *Server, ctx context.Context, arguments json.RawMessage) (any, error) {
	var input struct {
		Under string `json:"under"`
		Limit int    `json:"limit"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return nil, err
	}
	limit, err := pageLimit(input.Limit)
	if err != nil {
		return nil, err
	}
	var subjects []string
	under := strings.TrimRight(strings.TrimSpace(input.Under), "/")
	if under == "" {
		subjects, err = server.store.ListTopLevelSubjects(ctx)
	} else {
		var all []string
		all, err = server.store.ListSubjects(ctx)
		for _, subject := range all {
			if strings.HasPrefix(subject, under+"/") {
				subjects = append(subjects, subject)
			}
		}
	}
	if err != nil {
		return nil, err
	}
	sort.Strings(subjects)
	result := map[string]any{"total": len(subjects)}
	if len(subjects) > limit {
		result["truncated"] = true
		result["hint"] = "More subjects exist, pass a deeper subject as under"
		subjects = subjects[:limit]
	}
	result["subjects"] = nonNil(subjects)
	return result, nil
}

func runFetchEvents(server *Server, ctx context.Context, arguments json.RawMessage) (any, error) {
	var input struct {
		Subject           string `json:"subject"`
		LowerBound        string `json:"lowerBound"`
		LatestByEventType string `json:"latestByEventType"`
		Limit             int    `json:"limit"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(strings.TrimSpace(input.Subject), "/") {
		return nil, &mcpToolError{"subject must start with /"}
	}
	limit, err := pageLimit(input.Limit)
	if err != nil {
		return nil, err
	}
	result := map[string]any{}
	var responses []EventResponse
	if input.LatestByEventType != "" {
		latest, err := server.store.FetchLatestOfType(ctx, input.Subject, input.LatestByEventType)
		if err != nil {
			return nil, err
		}
		responses = []EventResponse{}
		if latest != nil {
			responses = newEventResponses([]*core.Event{latest})
		}
	} else {
		events, next, err := server.store.FetchPage(ctx, input.Subject, input.LowerBound, limit)
		if err != nil {
			return nil, err
		}
		responses = newEventResponses(events)
		if next != "" {
			result["nextLowerBound"] = next
			result["hint"] = "More events exist, call fetch_events again with lowerBound set to nextLowerBound"
		}
	}
	result["count"] = len(responses)
	result["events"] = responses
	return result, nil
}

func runQuery(server *Server, ctx context.Context, arguments json.RawMessage) (any, error) {
	var input struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := decodeArguments(arguments, &input); err != nil {
		return nil, err
	}
	limit, err := pageLimit(input.Limit)
	if err != nil {
		return nil, err
	}
	parsed, err := query.Parse(input.Query)
	if err != nil {
		return nil, &mcpToolError{"Invalid GDBQL: " + err.Error() + ". Read the resource genesisdb://docs/gdbql for the syntax."}
	}
	output, err := query.Execute(ctx, server.store, parsed)
	if err != nil {
		return nil, err
	}
	items := make([]any, 0)
	for _, item := range queryResultItems(output) {
		if event, isEvent := item.(*core.Event); isEvent {
			// System events never leave the API.
			if event.IsSystemEvent() {
				continue
			}
			item = newEventResponse(event)
		}
		items = append(items, item)
	}
	result := map[string]any{"total": len(items)}
	if len(items) > limit {
		items = items[:limit]
		result["truncated"] = true
		result["hint"] = "The result was cut, add WHERE conditions, LIMIT or an aggregate to the query"
	}
	result["results"] = items
	return result, nil
}

func runCommitEvents(server *Server, ctx context.Context, arguments json.RawMessage) (any, error) {
	var body CommitRequest
	if err := decodeArguments(arguments, &body); err != nil {
		return nil, err
	}
	for position, event := range body.Events {
		if len(event.ReferenceData) > 0 {
			return nil, &mcpToolError{fmt.Sprintf("Event %d: use data with options.storeDataAsReference instead of ref", position)}
		}
	}
	committed, err := server.commit(ctx, &body)
	if err != nil {
		return nil, err
	}
	responses := newEventResponses(committed)
	return map[string]any{"committed": len(responses), "events": responses}, nil
}
