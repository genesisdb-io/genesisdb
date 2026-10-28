package httpserver

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/query"
	"github.com/genesisdb-io/genesisdb/v2/packages/store"
)

func newMCPTestServer(t *testing.T, enable bool) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := store.Open(t.TempDir(), store.Options{EvaluateGDBQLPrecondition: query.EvaluatePrecondition, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s, Options{AuthToken: "admin", ReadToken: "reader", WriteToken: "writer", Logger: logger, EnableMCP: enable}).Routes()
}

type mcpReply struct {
	status int
	body   string
	result map[string]any
	err    map[string]any
}

func mcpCall(t *testing.T, handler http.Handler, token, body string) mcpReply {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	reply := mcpReply{status: recorder.Code, body: recorder.Body.String()}
	var decoded map[string]any
	if json.Unmarshal(recorder.Body.Bytes(), &decoded) == nil {
		reply.result, _ = decoded["result"].(map[string]any)
		reply.err, _ = decoded["error"].(map[string]any)
	}
	return reply
}

func callTool(t *testing.T, handler http.Handler, token, name, arguments string) (map[string]any, bool, string) {
	t.Helper()
	reply := mcpCall(t, handler, token, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, arguments))
	if reply.result == nil {
		t.Fatalf("%s: no result: %d %s", name, reply.status, reply.body)
	}
	isError, _ := reply.result["isError"].(bool)
	content := reply.result["content"].([]any)[0].(map[string]any)["text"].(string)
	structured, _ := reply.result["structuredContent"].(map[string]any)
	return structured, isError, content
}

func toolNames(t *testing.T, handler http.Handler, token string) []string {
	t.Helper()
	reply := mcpCall(t, handler, token, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	var names []string
	for _, tool := range reply.result["tools"].([]any) {
		names = append(names, tool.(map[string]any)["name"].(string))
	}
	return names
}

func TestMCP_IsOffByDefault(t *testing.T) {
	handler := newMCPTestServer(t, false)
	if reply := mcpCall(t, handler, "admin", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); reply.status != http.StatusNotFound {
		t.Fatalf("disabled endpoint answered %d", reply.status)
	}
}

func TestMCP_Initialize(t *testing.T) {
	handler := newMCPTestServer(t, true)
	reply := mcpCall(t, handler, "reader", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if reply.status != http.StatusOK || reply.result["protocolVersion"] != "2025-03-26" {
		t.Fatalf("initialize: %d %s", reply.status, reply.body)
	}
	if info := reply.result["serverInfo"].(map[string]any); info["name"] != "genesisdb" {
		t.Fatalf("serverInfo %v", info)
	}
	reply = mcpCall(t, handler, "reader", `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if reply.result["protocolVersion"] != mcpProtocolVersions[0] {
		t.Fatal("unknown version not answered with the newest supported one")
	}
	if reply := mcpCall(t, handler, "reader", `{"jsonrpc":"2.0","method":"notifications/initialized"}`); reply.status != http.StatusAccepted || reply.body != "" {
		t.Fatalf("notification: %d %q", reply.status, reply.body)
	}
	if reply := mcpCall(t, handler, "reader", `{"jsonrpc":"2.0","id":3,"method":"nope"}`); reply.err["code"] != float64(rpcMethodNotFound) {
		t.Fatalf("unknown method: %s", reply.body)
	}
}

func TestMCP_Authentication(t *testing.T) {
	handler := newMCPTestServer(t, true)
	if reply := mcpCall(t, handler, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); reply.status != http.StatusUnauthorized {
		t.Fatalf("no token: %d", reply.status)
	}
	if reply := mcpCall(t, handler, "wrong", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); reply.status != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", reply.status)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	request.Header.Set("Authorization", "Bearer reader")
	request.Header.Set("Origin", "https://evil.example")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", recorder.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/mcp", nil)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", recorder.Code)
	}
}

func TestMCP_ToolsFollowTheTokenRole(t *testing.T) {
	handler := newMCPTestServer(t, true)
	reader := strings.Join(toolNames(t, handler, "reader"), ",")
	writer := strings.Join(toolNames(t, handler, "writer"), ",")
	admin := strings.Join(toolNames(t, handler, "admin"), ",")
	if strings.Contains(reader, "commit_events") || strings.Contains(reader, "verify_hash_chain") {
		t.Fatalf("reader tools: %s", reader)
	}
	if !strings.Contains(writer, "commit_events") || strings.Contains(writer, "verify_hash_chain") {
		t.Fatalf("writer tools: %s", writer)
	}
	if !strings.Contains(admin, "commit_events") || !strings.Contains(admin, "verify_hash_chain") {
		t.Fatalf("admin tools: %s", admin)
	}
	for _, forbidden := range []string{"erase", "restore", "backup", "index"} {
		if strings.Contains(admin, forbidden) {
			t.Fatalf("administration tool %s exposed", forbidden)
		}
	}
	reply := mcpCall(t, handler, "reader", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"commit_events","arguments":{"events":[{"source":"s","subject":"/a/1","type":"t","data":{}}]}}}`)
	if reply.err == nil || reply.err["code"] != float64(rpcInvalidParams) {
		t.Fatalf("reader committed: %s", reply.body)
	}
}

func TestMCP_CommitFetchQuery(t *testing.T) {
	handler := newMCPTestServer(t, true)
	result, isError, text := callTool(t, handler, "writer", "commit_events", `{"events":[
		{"source":"io.example","subject":"/customer/1","type":"io.example.registered","data":{"name":"Bruce"},"options":{"storeDataAsReference":true}},
		{"source":"io.example","subject":"/customer/2","type":"io.example.registered","data":{"name":"Selina"}},
		{"source":"io.example","subject":"/order/1","type":"io.example.placed","data":{"total":10}}]}`)
	if isError || result["committed"] != float64(3) {
		t.Fatalf("commit: %s", text)
	}
	_, isError, text = callTool(t, handler, "writer", "commit_events", `{"events":[{"source":"s","subject":"/customer/1","type":"t","data":{}}],"preconditions":[{"type":"isSubjectNew","payload":{"subject":"/customer/1"}}]}`)
	if !isError || !strings.Contains(text, "isSubjectNew") || !strings.Contains(text, "Nothing was committed") {
		t.Fatalf("precondition: %s", text)
	}
	_, isError, text = callTool(t, handler, "writer", "commit_events", `{"events":[{"source":"s","subject":"customer","type":"t","data":{}}]}`)
	if !isError || !strings.Contains(text, "must start with /") {
		t.Fatalf("validation: %s", text)
	}

	result, _, _ = callTool(t, handler, "reader", "list_subjects", `{}`)
	if fmt.Sprint(result["subjects"]) != "[/customer /order]" {
		t.Fatalf("subjects %v", result["subjects"])
	}
	result, _, _ = callTool(t, handler, "reader", "list_subjects", `{"under":"/customer"}`)
	if fmt.Sprint(result["subjects"]) != "[/customer/1 /customer/2]" {
		t.Fatalf("subjects under %v", result["subjects"])
	}
	result, _, _ = callTool(t, handler, "reader", "list_event_types", `{}`)
	if len(result["types"].([]any)) != 2 {
		t.Fatalf("types %v", result["types"])
	}

	result, _, text = callTool(t, handler, "reader", "fetch_events", `{"subject":"/customer","limit":1}`)
	if result["count"] != float64(1) || result["nextLowerBound"] == nil || !strings.Contains(text, "Bruce") {
		t.Fatalf("first page: %s", text)
	}
	result, _, text = callTool(t, handler, "reader", "fetch_events", fmt.Sprintf(`{"subject":"/customer","lowerBound":%q}`, result["nextLowerBound"]))
	if result["count"] != float64(1) || result["nextLowerBound"] != nil || !strings.Contains(text, "Selina") {
		t.Fatalf("second page: %s", text)
	}

	result, isError, text = callTool(t, handler, "reader", "query", `{"query":"STREAM e FROM events WHERE e.subject UNDER '/customer' MAP COUNT()"}`)
	if isError || fmt.Sprint(result["results"]) != "[2]" {
		t.Fatalf("count: %s", text)
	}
	result, _, _ = callTool(t, handler, "reader", "query", `{"query":"STREAM e FROM events MAP { * }","limit":2}`)
	if result["truncated"] != true || result["total"] != float64(3) {
		t.Fatalf("truncation: %v", result)
	}
	_, isError, text = callTool(t, handler, "reader", "query", `{"query":"SELECT * FROM events"}`)
	if !isError || !strings.Contains(text, "genesisdb://docs/gdbql") {
		t.Fatalf("invalid query: %s", text)
	}
	_, isError, text = callTool(t, handler, "reader", "fetch_events", `{"subject":"/customer","nope":1}`)
	if !isError || !strings.Contains(text, "unknown field") {
		t.Fatalf("unknown argument: %s", text)
	}

	result, _, _ = callTool(t, handler, "reader", "get_status", `{}`)
	if result["events"].(map[string]any)["count"] != float64(3) {
		t.Fatalf("status %v", result)
	}
	result, isError, text = callTool(t, handler, "admin", "verify_hash_chain", `{}`)
	if isError || result["valid"] != true {
		t.Fatalf("audit: %s", text)
	}
}

func TestMCP_ResourcesAndBatch(t *testing.T) {
	handler := newMCPTestServer(t, true)
	reply := mcpCall(t, handler, "reader", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`)
	if len(reply.result["resources"].([]any)) != len(mcpResources) {
		t.Fatalf("resources: %s", reply.body)
	}
	reply = mcpCall(t, handler, "reader", `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"genesisdb://docs/gdbql"}}`)
	contents := reply.result["contents"].([]any)[0].(map[string]any)
	if !strings.Contains(contents["text"].(string), "STREAM e FROM events") {
		t.Fatal("GDBQL reference missing")
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`))
	request.Header.Set("Authorization", "Bearer reader")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var batch []map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &batch); err != nil || len(batch) != 2 {
		t.Fatalf("batch: %s", recorder.Body)
	}
	if reply := mcpCall(t, handler, "reader", `{not json`); reply.status != http.StatusBadRequest || reply.err["code"] != float64(rpcParseError) {
		t.Fatalf("parse error: %d %s", reply.status, reply.body)
	}
}

// TestMCP_FetchPagesUnderAReadBudget: a subject far larger than the read
// budget is still readable page by page, because only the page is loaded.
func TestMCP_FetchPagesUnderAReadBudget(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := store.Open(t.TempDir(), store.Options{EvaluateGDBQLPrecondition: query.EvaluatePrecondition, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	handler := New(s, Options{AuthToken: "admin", Logger: logger, EnableMCP: true, MaxReadEvents: 25}).Routes()
	var events []string
	for position := 0; position < 120; position++ {
		events = append(events, fmt.Sprintf(`{"source":"s","subject":"/big/%d","type":"t","data":{"n":%d}}`, position, position))
	}
	if _, isError, text := callTool(t, handler, "admin", "commit_events", `{"events":[`+strings.Join(events, ",")+`]}`); isError {
		t.Fatal(text)
	}
	seen, lowerBound := 0, ""
	for pages := 0; pages < 10; pages++ {
		arguments := `{"subject":"/big","limit":20}`
		if lowerBound != "" {
			arguments = fmt.Sprintf(`{"subject":"/big","limit":20,"lowerBound":%q}`, lowerBound)
		}
		result, isError, text := callTool(t, handler, "admin", "fetch_events", arguments)
		if isError {
			t.Fatalf("page under budget failed: %s", text)
		}
		seen += int(result["count"].(float64))
		next, hasNext := result["nextLowerBound"].(string)
		if !hasNext {
			break
		}
		lowerBound = next
	}
	if seen != 120 {
		t.Fatalf("paged %d events, want 120", seen)
	}
	result, isError, text := callTool(t, handler, "admin", "fetch_events", `{"subject":"/big","latestByEventType":"t"}`)
	if isError || result["count"] != float64(1) || !strings.Contains(text, `"n": 119`) {
		t.Fatalf("latest under budget: %s", text)
	}
}
