package httpserver

// The MCP endpoint lets AI agents work with the store through the Model
// Context Protocol. It implements the Streamable HTTP transport without
// sessions and without server initiated messages: every POST carries one
// JSON-RPC request, notification or batch and is answered with
// application/json. Tools call the same code as the HTTP API, so validation,
// schemas, preconditions and read budgets apply unchanged.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// mcpPath is the path of the MCP endpoint, outside the versioned API like
// /ping.
const mcpPath = "/mcp"

// mcpProtocolVersions lists the supported protocol revisions, newest first.
var mcpProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC 2.0 error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
)

// mcpRole is the permission of the token that called the endpoint. It
// decides which tools are listed and callable.
type mcpRole int

const (
	mcpRoleRead mcpRole = iota
	mcpRoleWrite
	mcpRoleAdmin
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (request *rpcRequest) isNotification() bool { return len(request.ID) == 0 }

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func rpcFailure(id json.RawMessage, code int, message string) *rpcResponse {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

func rpcSuccess(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

// mcpRoleOf returns the role of an Authorization header, or false when the
// header carries no valid token.
func (server *Server) mcpRoleOf(header string) (mcpRole, bool) {
	switch {
	case server.verifier.VerifyToken(header) == nil:
		return mcpRoleAdmin, true
	case server.writeVerifier.VerifyToken(header) == nil:
		return mcpRoleWrite, true
	case server.readVerifier.VerifyToken(header) == nil:
		return mcpRoleRead, true
	}
	return mcpRoleRead, false
}

// isAllowedMCPOrigin protects against DNS rebinding: a browser request is
// only accepted from a loopback page or from the origin of the server
// itself. Requests without an Origin header (agents, CLIs) pass.
func isAllowedMCPOrigin(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	if strings.EqualFold(parsed.Host, request.Host) {
		return true
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (server *Server) handleMCP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodOptions {
		// Browser based clients such as the MCP Inspector send a preflight.
		writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Accept, MCP-Protocol-Version, Mcp-Session-Id")
		writer.WriteHeader(http.StatusOK)
		return
	}
	if request.Method != http.MethodPost {
		// No server initiated stream and no sessions: GET and DELETE are
		// not offered, as the transport allows.
		writer.Header().Set("Allow", http.MethodPost)
		http.Error(writer, "Method not allowed, the MCP endpoint accepts POST", http.StatusMethodNotAllowed)
		return
	}
	if !isAllowedMCPOrigin(request) {
		http.Error(writer, "Origin not allowed", http.StatusForbidden)
		return
	}
	role, isAuthorized := server.mcpRoleOf(request.Header.Get("Authorization"))
	if !isAuthorized {
		message := "Invalid token"
		if request.Header.Get("Authorization") == "" {
			message = "No token provided"
		}
		writer.Header().Set("WWW-Authenticate", `Bearer realm="genesisdb"`)
		http.Error(writer, message, http.StatusUnauthorized)
		return
	}
	if version := request.Header.Get("MCP-Protocol-Version"); version != "" && !isSupportedMCPVersion(version) {
		http.Error(writer, "Unsupported MCP protocol version "+version, http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		requestBodyError(writer, err)
		return
	}
	if !utf8.Valid(body) {
		writeJSON(writer, http.StatusBadRequest, rpcFailure(nil, rpcParseError, "Request body must be valid UTF-8"))
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			writeJSON(writer, http.StatusBadRequest, rpcFailure(nil, rpcParseError, "Invalid JSON"))
			return
		}
		if len(batch) == 0 {
			writeJSON(writer, http.StatusBadRequest, rpcFailure(nil, rpcInvalidRequest, "Empty batch"))
			return
		}
		responses := make([]*rpcResponse, 0, len(batch))
		for _, message := range batch {
			if response := server.handleMCPMessage(request, role, message); response != nil {
				responses = append(responses, response)
			}
		}
		if len(responses) == 0 {
			writer.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(writer, http.StatusOK, responses)
		return
	}
	response := server.handleMCPMessage(request, role, trimmed)
	if response == nil {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	status := http.StatusOK
	if response.Error != nil && (response.Error.Code == rpcParseError || response.Error.Code == rpcInvalidRequest) {
		status = http.StatusBadRequest
	}
	writeJSON(writer, status, response)
}

func isSupportedMCPVersion(version string) bool {
	for _, supported := range mcpProtocolVersions {
		if version == supported {
			return true
		}
	}
	return false
}

// handleMCPMessage answers one JSON-RPC message. Notifications and client
// responses return nil.
func (server *Server) handleMCPMessage(httpRequest *http.Request, role mcpRole, message json.RawMessage) *rpcResponse {
	var request rpcRequest
	decoder := json.NewDecoder(bytes.NewReader(message))
	decoder.UseNumber()
	if err := decoder.Decode(&request); err != nil {
		return rpcFailure(nil, rpcParseError, "Invalid JSON")
	}
	if request.JSONRPC != "2.0" {
		return rpcFailure(request.ID, rpcInvalidRequest, `jsonrpc must be "2.0"`)
	}
	if request.Method == "" {
		// A response to a server request. The server sends none, so there
		// is nothing to correlate.
		return nil
	}
	if request.isNotification() {
		return nil
	}
	ctx := httpRequest.Context()
	switch request.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(request.Params, &params)
		version := mcpProtocolVersions[0]
		if isSupportedMCPVersion(params.ProtocolVersion) {
			version = params.ProtocolVersion
		}
		return rpcSuccess(request.ID, map[string]any{
			"protocolVersion": version,
			"capabilities": map[string]any{
				"tools":     map[string]any{"listChanged": false},
				"resources": map[string]any{"listChanged": false},
			},
			"serverInfo":   map[string]any{"name": "genesisdb", "title": "GenesisDB", "version": core.Version},
			"instructions": mcpInstructions,
		})
	case "ping":
		return rpcSuccess(request.ID, map[string]any{})
	case "tools/list":
		return rpcSuccess(request.ID, map[string]any{"tools": mcpToolsFor(role)})
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil || params.Name == "" {
			return rpcFailure(request.ID, rpcInvalidParams, "tools/call requires a tool name")
		}
		tool, isKnown := mcpToolByName(params.Name, role)
		if !isKnown {
			return rpcFailure(request.ID, rpcInvalidParams, "Unknown tool: "+params.Name)
		}
		arguments := params.Arguments
		if len(arguments) == 0 || string(arguments) == "null" {
			arguments = json.RawMessage("{}")
		}
		return rpcSuccess(request.ID, tool.call(server, ctx, arguments))
	case "resources/list":
		return rpcSuccess(request.ID, map[string]any{"resources": mcpResourceList()})
	case "resources/templates/list":
		return rpcSuccess(request.ID, map[string]any{"resourceTemplates": []any{}})
	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil || params.URI == "" {
			return rpcFailure(request.ID, rpcInvalidParams, "resources/read requires a uri")
		}
		resource, isKnown := mcpResourceByURI(params.URI)
		if !isKnown {
			return rpcFailure(request.ID, rpcInvalidParams, "Unknown resource: "+params.URI)
		}
		return rpcSuccess(request.ID, map[string]any{"contents": []any{map[string]any{
			"uri": resource.URI, "mimeType": resource.MimeType, "text": resource.Text,
		}}})
	}
	return rpcFailure(request.ID, rpcMethodNotFound, "Method not found: "+request.Method)
}

// mcpInstructions are returned on initialize and tell the agent how the
// store is organized.
const mcpInstructions = `GenesisDB is an event sourcing database. Every event is an immutable CloudEvents 1.0 record with source, subject, type and data. Subjects form a tree like /customer/42/address, reading a subject always includes its descendants, in commit order.

Start with list_subjects and list_event_types to learn the shape of the store, then read with fetch_events or ask questions with query (GDBQL, read the resource genesisdb://docs/gdbql first). Results are capped, follow the paging hints instead of widening the selection. Never put personal data into subjects, types or sources, use data with options.storeDataAsReference so it can be erased later.`
