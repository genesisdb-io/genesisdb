package httpserver

import "net/http"

// The API is versioned by path prefix. Each version is one complete route
// table, there are no aliases inside a version.
//
//   - /api/v1 is the API of GenesisDB 1.x, frozen. It answers byte for byte
//     like 1.1.6 and marks every response as deprecated with a Link to the
//     successor route in /api/v2.
//   - /api/v2 is the current API.
//
// Infrastructure routes (/ping, /metrics, /mcp) carry no version.
const (
	v1Prefix = "/api/v1"
	v2Prefix = "/api/v2"
)

// access is the least token role a route requires. The administrator token
// passes every route.
type access int

const (
	accessRead access = iota
	accessWrite
	accessAdmin
)

// route is one API endpoint, its path relative to the version prefix.
type route struct {
	method  string
	path    string
	access  access
	handler http.HandlerFunc
	// successor is the /api/v2 path of a /api/v1 route, sent as deprecation
	// Link. Empty for /api/v2 routes.
	successor string
	// restore routes are capped by MaxRestoreBytes instead of
	// MaxRequestBytes.
	restore bool
	// stream routes are long-lived and exempt from the request timeout.
	stream bool
}

// v2Routes lists every route of the current API. Administration lives
// under /admin, so proxies can guard it by path.
func (server *Server) v2Routes() []route {
	return []route{
		{method: http.MethodGet, path: "/status", access: accessRead, handler: server.handleDetailedStatus},
		{method: http.MethodPost, path: "/commit", access: accessWrite, handler: server.handleCommit},
		{method: http.MethodPost, path: "/fetch", access: accessRead, handler: server.handleFetch},
		{method: http.MethodPost, path: "/subscribe", access: accessRead, handler: server.handleSubscribe, stream: true},
		{method: http.MethodPost, path: "/query", access: accessRead, handler: server.handleQuery},
		{method: http.MethodGet, path: "/subjects", access: accessRead, handler: server.handleListSubjects},
		{method: http.MethodGet, path: "/types", access: accessRead, handler: server.handleListEventTypes},
		{method: http.MethodGet, path: "/schemas", access: accessRead, handler: server.handleListSchemas},
		{method: http.MethodPost, path: "/schemas", access: accessAdmin, handler: server.handleRegisterSchema},
		{method: http.MethodGet, path: "/admin/audit", access: accessAdmin, handler: server.handleVerifyHashChain},
		{method: http.MethodGet, path: "/admin/backup", access: accessAdmin, handler: server.handleBackup},
		{method: http.MethodPost, path: "/admin/restore", access: accessAdmin, handler: server.handleRestore, restore: true},
		{method: http.MethodPost, path: "/admin/erase", access: accessAdmin, handler: server.handleErase},
		{method: http.MethodPost, path: "/admin/index", access: accessAdmin, handler: server.handleRebuildIndexes},
	}
}

// v1Routes lists every route of GenesisDB 1.x. This table is frozen: it
// must not gain routes, new functionality belongs to /api/v2.
func (server *Server) v1Routes() []route {
	return []route{
		{method: http.MethodGet, path: "/status", access: accessRead, handler: server.handleDetailedStatus, successor: "/status"},
		{method: http.MethodGet, path: "/status/audit", access: accessAdmin, handler: server.handleVerifyHashChain, successor: "/admin/audit"},
		{method: http.MethodPost, path: "/commit", access: accessWrite, handler: server.handleCommit, successor: "/commit"},
		{method: http.MethodPost, path: "/stream", access: accessRead, handler: server.handleFetch, successor: "/fetch"},
		{method: http.MethodPost, path: "/observe", access: accessRead, handler: server.handleSubscribe, successor: "/subscribe", stream: true},
		{method: http.MethodPost, path: "/q", access: accessRead, handler: server.handleQuery, successor: "/query"},
		{method: http.MethodPost, path: "/query", access: accessRead, handler: server.handleQuery, successor: "/query"},
		{method: http.MethodGet, path: "/subjects", access: accessRead, handler: server.handleListSubjects, successor: "/subjects"},
		{method: http.MethodGet, path: "/types", access: accessRead, handler: server.handleListEventTypes, successor: "/types"},
		{method: http.MethodGet, path: "/schema/get", access: accessRead, handler: server.handleListSchemas, successor: "/schemas"},
		{method: http.MethodPost, path: "/schema/register", access: accessAdmin, handler: server.handleRegisterSchema, successor: "/schemas"},
		{method: http.MethodGet, path: "/backup/create", access: accessAdmin, handler: server.handleBackup, successor: "/admin/backup"},
		{method: http.MethodPost, path: "/backup/restore", access: accessAdmin, handler: server.handleRestore, successor: "/admin/restore", restore: true},
		{method: http.MethodPost, path: "/erase", access: accessAdmin, handler: server.handleErase, successor: "/admin/erase"},
		{method: http.MethodPost, path: "/index", access: accessAdmin, handler: server.handleRebuildIndexes, successor: "/admin/index"},
	}
}

// Routes returns the complete HTTP handler. Every registered route is
// logged once, as in every earlier release.
func (server *Server) Routes() http.Handler {
	protected := http.NewServeMux()
	hasPreflight := map[string]bool{}
	restores := map[string]bool{}
	streams := map[string]bool{}
	register := func(prefix string, r route) {
		path := prefix + r.path
		var handler http.Handler = r.handler
		if r.successor != "" {
			handler = markDeprecated(v2Prefix+r.successor, handler)
		}
		protected.Handle(r.method+" "+path, server.authorize(r.access, handler))
		server.logger.Info("Registered route: " + r.method + " " + path)
		if !hasPreflight[path] {
			hasPreflight[path] = true
			protected.Handle("OPTIONS "+path, http.HandlerFunc(answerPreflight))
		}
		restores[path] = restores[path] || r.restore
		streams[path] = streams[path] || r.stream
	}
	root := http.NewServeMux()
	ping := func(path string, handler http.Handler) {
		root.Handle("GET "+path, handler)
		server.logger.Info("Registered route: GET " + path)
	}
	ping("/ping", http.HandlerFunc(server.handlePing))
	ping(v1Prefix+"/status/ping", markDeprecated(v2Prefix+"/ping", http.HandlerFunc(server.handlePing)))
	ping(v2Prefix+"/ping", http.HandlerFunc(server.handlePing))
	for _, r := range server.v1Routes() {
		register(v1Prefix, r)
	}
	for _, r := range server.v2Routes() {
		register(v2Prefix, r)
	}
	if server.metrics != nil {
		root.Handle("GET /metrics", server.metrics)
		server.logger.Info("Registered route: GET /metrics")
	}
	if server.options.EnableMCP {
		root.HandleFunc(mcpPath, server.handleMCP)
		server.logger.Info("Registered route: POST " + mcpPath + " (MCP)")
	}
	root.Handle(v1Prefix+"/", protected)
	root.Handle(v2Prefix+"/", protected)
	return server.logRequests(server.boundRequests(restores, streams, allowCrossOrigin(root)))
}

// authorize admits the administrator token everywhere, a read token on read
// routes and a write token on read and write routes.
func (server *Server) authorize(required access, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header := request.Header.Get("Authorization")
		if server.verifier.VerifyToken(header) == nil {
			next.ServeHTTP(writer, request)
			return
		}
		read := server.readVerifier.VerifyToken(header) == nil
		write := server.writeVerifier.VerifyToken(header) == nil
		if !read && !write {
			message := "Invalid token"
			if header == "" {
				message = "No token provided"
			}
			http.Error(writer, message, http.StatusUnauthorized)
			return
		}
		if required == accessAdmin || (required == accessWrite && !write) {
			http.Error(writer, "Token does not permit this operation", http.StatusForbidden)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

// markDeprecated adds the deprecation headers of a /api/v1 route.
func markDeprecated(successor string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Deprecation", "true")
		writer.Header().Set("Link", "<"+successor+`>; rel="successor-version"`)
		next.ServeHTTP(writer, request)
	})
}
