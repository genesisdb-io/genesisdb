// Package httpserver is the HTTP API of GenesisDB. Every route lives under
// a version prefix and requires a bearer token, except the pings.
//
// The route tables of /api/v1 (frozen 1.x API) and /api/v2 (current API)
// are in routes.go.
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/genesisdb-io/genesisdb/v2/packages/auth"
	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/schema"
	"github.com/genesisdb-io/genesisdb/v2/packages/store"
)

// Options configure a server.
type Options struct {
	// AuthToken is the bearer token every protected route requires.
	AuthToken string
	// ReadToken and WriteToken are optional least-privilege application tokens.
	ReadToken  string
	WriteToken string
	// MaxRequestBytes and MaxRestoreBytes cap request bodies. Zero means
	// unbounded, which is the default.
	MaxRequestBytes int64
	MaxRestoreBytes int64
	// MaxConcurrentRequests caps active requests including subscriptions.
	// Zero means unbounded, which is the default.
	MaxConcurrentRequests int
	// Read budgets are optional. Zero means unbounded, which is the default:
	// a selection, backup or query never fails because the store grew.
	MaxReadEvents int
	MaxReadBytes  int64
	// RequestTimeout bounds non-streaming requests. Zero means no deadline,
	// which is the default: large backups and queries run to completion.
	RequestTimeout time.Duration
	// Logger receives request and error logs. Defaults to slog.Default().
	Logger *slog.Logger
	// MetricsHandler is served at GET /metrics on the API port when set.
	MetricsHandler http.Handler
	// EnableMCP serves the Model Context Protocol endpoint at /mcp, so AI
	// agents can use the store with the same tokens. Off by default.
	EnableMCP bool
}

// Server serves the HTTP API for one store.
type Server struct {
	store         *store.Store
	schemas       *schema.Registry
	verifier      *auth.TokenVerifier
	logger        *slog.Logger
	metrics       http.Handler
	readVerifier  *auth.TokenVerifier
	writeVerifier *auth.TokenVerifier
	options       Options
}

// New returns a server for s.
func New(s *store.Store, options Options) *Server {
	server := &Server{
		store:         s,
		schemas:       schema.NewRegistry(s.SchemasDirectory()),
		verifier:      auth.NewTokenVerifier(options.AuthToken),
		logger:        options.Logger,
		metrics:       options.MetricsHandler,
		readVerifier:  auth.NewTokenVerifier(options.ReadToken),
		writeVerifier: auth.NewTokenVerifier(options.WriteToken),
		options:       options,
	}
	if server.logger == nil {
		server.logger = slog.Default()
	}
	// Schema files of GenesisDB 1.x are migrated to the current envelope the
	// first time the directory is served. A failure keeps the legacy files,
	// which remain readable, and is logged.
	migrated, err := server.schemas.MigrateLegacyFiles()
	if err != nil {
		server.logger.Warn("failed to migrate legacy schema files, they stay readable in place", "error", err)
	}
	if len(migrated) > 0 {
		server.logger.Info("migrated schema files from GenesisDB 1.x", "types", migrated)
	}
	return server
}

// allowCrossOrigin answers every origin. Authentication is a bearer token,
// not a cookie, so credentials are never involved and the
// Access-Control-Allow-Credentials header is not sent (browsers ignore it
// together with a wildcard origin anyway).
func allowCrossOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header := writer.Header()
		header.Set("Access-Control-Allow-Origin", "*")
		header.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		header.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Disable-Ndjson")
		next.ServeHTTP(writer, request)
	})
}

func answerPreflight(writer http.ResponseWriter, request *http.Request) {
	writer.WriteHeader(http.StatusOK)
}

// logRequests logs every incoming request at info level, as every earlier
// release did. Only method and path are logged, never headers or bodies.
func (server *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		server.logger.Info("Incoming request: " + request.Method + " " + request.URL.Path)
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) handlePing(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "text/plain")
	writer.Write([]byte("Pong"))
}

// httpStatusForError maps store errors to HTTP status codes.
func httpStatusForError(err error) int {
	switch {
	case errors.Is(err, core.ErrNotReady), errors.Is(err, core.ErrUnhealthy), errors.Is(err, core.ErrClosed):
		return http.StatusServiceUnavailable
	case errors.Is(err, core.ErrResourceLimit):
		return http.StatusUnprocessableEntity
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusRequestTimeout
	case errors.Is(err, core.ErrNotEmpty):
		return http.StatusConflict
	case errors.Is(err, core.ErrInvalidEvent), errors.Is(err, core.ErrSystemEventNotAllowed):
		return http.StatusBadRequest
	case errors.Is(err, core.ErrInvalidBackup):
		return http.StatusBadRequest
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// wantsJSONArray reports whether the client disabled NDJSON responses.
func wantsJSONArray(request *http.Request) bool {
	return request.Header.Get("x-disable-ndjson") != ""
}

// writeNDJSONOrArray writes items as NDJSON (one object followed by an empty
// line, as in every earlier release) or, if the client disabled NDJSON, as
// one JSON array.
func writeNDJSONOrArray[T any](writer http.ResponseWriter, request *http.Request, items []T) {
	if wantsJSONArray(request) {
		writer.Header().Set("Content-Type", "application/json")
		if items == nil {
			items = []T{}
		}
		json.NewEncoder(writer).Encode(items)
		return
	}
	writeNDJSON(writer, items)
}

func writeNDJSON[T any](writer http.ResponseWriter, items []T) {
	writer.Header().Set("Content-Type", "application/x-ndjson")
	encoder := json.NewEncoder(writer)
	for _, item := range items {
		if err := encoder.Encode(item); err != nil {
			return
		}
		if _, err := writer.Write([]byte("\n")); err != nil {
			return
		}
	}
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	json.NewEncoder(writer).Encode(value)
}

// decodeRequestBody decodes exactly one JSON value. The body must be valid
// UTF-8: encoding/json silently replaces invalid bytes in strings with
// U+FFFD, which would persist a subject or type the client never sent.
func decodeRequestBody(writer http.ResponseWriter, request *http.Request, target any) bool {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		requestBodyError(writer, err)
		return false
	}
	if !utf8.Valid(body) {
		http.Error(writer, "Request body must be valid UTF-8", http.StatusBadRequest)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		requestBodyError(writer, err)
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		requestBodyError(writer, err)
		return false
	}
	return true
}

func requestBodyError(writer http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(writer, "Request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(writer, "Invalid request body", http.StatusBadRequest)
}

// boundRequests applies the opt-in caps. restores and streams name the
// paths with the restore body cap and the paths exempt from the timeout.
func (server *Server) boundRequests(restores, streams map[string]bool, next http.Handler) http.Handler {
	// A concurrency cap is opt-in. Without one every request is served, as
	// in every earlier release.
	var slots chan struct{}
	if server.options.MaxConcurrentRequests > 0 {
		slots = make(chan struct{}, server.options.MaxConcurrentRequests)
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodOptions {
			next.ServeHTTP(writer, request)
			return
		}
		if slots != nil {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				http.Error(writer, "Too many concurrent requests", http.StatusTooManyRequests)
				return
			}
		}
		// Body caps are opt-in. Zero means unbounded, so a large commit batch
		// or a large restore is never refused.
		maximum := server.options.MaxRequestBytes
		if restores[request.URL.Path] {
			maximum = server.options.MaxRestoreBytes
		}
		if maximum > 0 {
			request.Body = http.MaxBytesReader(writer, request.Body, maximum)
		}
		controller := http.NewResponseController(writer)
		// Read budgets are opt-in. Zero means unbounded: selections, backups
		// and queries never fail because the store grew.
		if server.options.MaxReadEvents > 0 || server.options.MaxReadBytes > 0 {
			request = request.WithContext(core.WithReadLimits(request.Context(), core.ReadLimits{MaxEvents: server.options.MaxReadEvents, MaxBytes: server.options.MaxReadBytes}))
		}
		if !streams[request.URL.Path] && server.options.RequestTimeout > 0 {
			ctx, cancel := context.WithTimeout(request.Context(), server.options.RequestTimeout)
			defer cancel()
			request = request.WithContext(ctx)
			_ = controller.SetWriteDeadline(time.Now().Add(server.options.RequestTimeout))
			defer controller.SetWriteDeadline(time.Time{})
		}
		next.ServeHTTP(writer, request)
	})
}
