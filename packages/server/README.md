# github.com/genesisdb-io/genesisdb/v2/packages/server

The GenesisDB HTTP API in package `server/httpserver`. It serves the current API under `/api/v2` and the frozen API of GenesisDB 1.x under `/api/v1`, one route table each in `routes.go`. The executable in `cmd/genesisdb` wires it to a store. The full API reference is [docs/http-api.md](../../docs/http-api.md).

```go
import (
	"github.com/genesisdb-io/genesisdb/v2/packages/query"
	"github.com/genesisdb-io/genesisdb/v2/packages/server/httpserver"
	"github.com/genesisdb-io/genesisdb/v2/packages/store"
)

s, err := store.Open("data", store.Options{EvaluateGDBQLPrecondition: query.EvaluatePrecondition})
if err != nil {
	return err
}
defer s.Close()
server := httpserver.New(s, httpserver.Options{AuthToken: adminToken})
return http.ListenAndServe("127.0.0.1:8080", server.Routes())
```

## Options

| Field | Default | Meaning |
|---|---|---|
| `AuthToken` | required | Administrator token, passes every route |
| `ReadToken`, `WriteToken` | disabled | Least-privilege tokens for read and read plus commit |
| `MaxRequestBytes`, `MaxRestoreBytes` | `0` (unbounded) | Request body caps, a hit cap answers `413` |
| `MaxConcurrentRequests` | `0` (unbounded) | Active requests including subscriptions, a hit cap answers `429` |
| `MaxReadEvents`, `MaxReadBytes` | `0` (unbounded) | Read budget per request, a hit cap answers `422` |
| `RequestTimeout` | `0` (none) | Deadline for non-streaming requests |
| `Logger` | `slog.Default()` | Request and error logs |
| `MetricsHandler` | none | Served at `GET /metrics` on the API port when set |
| `EnableMCP` | `false` | Serve the MCP endpoint at `/mcp`, see [docs/mcp.md](../../docs/mcp.md) |

Limits are opt-in and fail explicitly, they never truncate a response.

## Contributing

- `/api/v1` is frozen and answers byte for byte like 1.1.6. Never add routes to it. The compatibility suite in `tests/` verifies it.
- New functionality belongs to `/api/v2`. Administration routes live under `/api/v2/admin` so a proxy can restrict them by path.
- Document every route change in [docs/http-api.md](../../docs/http-api.md) and the Postman collection in `docs/postman`.
- The server does not terminate TLS by itself. The executable configures TLS, or a reverse proxy does.
