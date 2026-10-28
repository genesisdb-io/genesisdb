package httpserver

import (
	"errors"
	"net/http"
	"strings"

	"github.com/genesisdb-io/genesisdb/v2/packages/schema"
)

// RegisterSchemaRequest is the body of POST /api/v2/schemas and
// /api/v1/schema/register.
type RegisterSchemaRequest struct {
	Type   string         `json:"type"`
	Schema map[string]any `json:"schema"`
}

// RegisterSchemaResponse confirms a registered schema.
type RegisterSchemaResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
}

func (server *Server) handleRegisterSchema(writer http.ResponseWriter, request *http.Request) {
	var body RegisterSchemaRequest
	if !decodeRequestBody(writer, request, &body) {
		return
	}
	if strings.TrimSpace(body.Type) == "" {
		http.Error(writer, "Type cannot be empty", http.StatusBadRequest)
		return
	}
	if body.Schema == nil {
		http.Error(writer, "Schema cannot be empty", http.StatusBadRequest)
		return
	}
	switch err := server.store.UpdateSchemas(request.Context(), func(directory string) error {
		return schema.NewRegistry(directory).Register(body.Type, body.Schema)
	}); {
	case errors.Is(err, schema.ErrInvalidSchema):
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, schema.ErrAlreadyRegistered):
		http.Error(writer, "Schema for this type already exists", http.StatusConflict)
		return
	case err != nil:
		server.logger.Error("failed to save schema", "type", body.Type, "error", err)
		http.Error(writer, "Failed to save schema", http.StatusInternalServerError)
		return
	}
	writeJSON(writer, http.StatusCreated, RegisterSchemaResponse{Status: "success", Message: "Schema registered successfully", Type: body.Type})
}

type schemaResponse struct {
	Schema map[string]any `json:"schema"`
	Type   string         `json:"type"`
}

func (server *Server) handleListSchemas(writer http.ResponseWriter, request *http.Request) {
	registered, err := server.schemas.List()
	if err != nil {
		server.logger.Error("failed to read schemas", "error", err)
		http.Error(writer, "Failed to read schemas", http.StatusInternalServerError)
		return
	}
	responses := make([]schemaResponse, 0, len(registered))
	for _, entry := range registered {
		responses = append(responses, schemaResponse{Schema: entry.Schema, Type: entry.EventType})
	}
	writeNDJSON(writer, responses)
}
