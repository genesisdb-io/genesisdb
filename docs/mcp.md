# MCP endpoint

GenesisDB has a built-in [Model Context Protocol](https://modelcontextprotocol.io) endpoint. AI agents connect to it directly and can explore subjects and event types, read events, run GDBQL queries and, with a write token, commit events. There is no separate MCP server to install.

## Enable it

The endpoint is off by default. Enable it with:

```bash
GENESISDB_MCP=true
```

It is served at `/mcp` on the API port, with the same TLS settings, and logs `Registered route: POST /mcp (MCP)` on start.

## Connect an agent

The transport is Streamable HTTP, authentication is the usual bearer token.

Claude Code:

```bash
claude mcp add --transport http genesisdb http://localhost:8080/mcp \
  --header "Authorization: Bearer $GENESISDB_READ_TOKEN"
```

Other clients with a JSON configuration:

```json
{
  "mcpServers": {
    "genesisdb": {
      "type": "http",
      "url": "http://localhost:8080/mcp",
      "headers": { "Authorization": "Bearer <token>" }
    }
  }
}
```

Test it with the MCP Inspector:

```bash
npx @modelcontextprotocol/inspector --cli http://localhost:8080/mcp \
  --transport http --header "Authorization: Bearer <token>" --method tools/list
```

## Tools

The token decides which tools an agent sees and can call.

| Tool | Token | Purpose |
|---|---|---|
| `list_subjects` | read | Top level subjects, or every subject below `under` |
| `list_event_types` | read | Every event type |
| `fetch_events` | read | Events of a subject and its descendants, paged with `lowerBound` |
| `query` | read | Run a GDBQL query |
| `list_schemas` | read | Registered JSON schemas |
| `get_status` | read | Version, counts, storage format, readiness |
| `commit_events` | write | Commit events atomically, with preconditions |
| `verify_hash_chain` | admin | Verify the hash chain of the whole store |

Erase, backup, restore, schema registration and index rebuild are not available through MCP, not even with the administrator token. They stay HTTP API operations for operators.

Tools call the same code as the HTTP API: schema validation, preconditions, the 64 KB event limit and the optional read budgets (`GENESISDB_MAX_READ_EVENTS`, `GENESISDB_MAX_READ_BYTES`) apply unchanged.

## Result size

An agent copies every result into its context, so tools return pages, not whole stores. `limit` defaults to 100 and can be raised to 1000.

`fetch_events` returns `nextLowerBound` when more events exist, pass it as `lowerBound` for the next page. Only the events of the requested page are loaded, so a subject of any size can be read page by page, also when a read budget (`GENESISDB_MAX_READ_EVENTS`) is far smaller than the subject. `latestByEventType` loads exactly one event.

`query` and `list_subjects` return `truncated` and a hint to narrow the request. A query evaluates its whole selection, so prefer GDBQL aggregates and projections over returning whole events.

## Resources

| URI | Content |
|---|---|
| `genesisdb://docs/gdbql` | A compact GDBQL reference for agents: syntax, operators and examples. [gdbql.md](gdbql.md) is the full reference |
| `genesisdb://docs/events` | Events, subjects, preconditions, personal data and modeling tips |

The `initialize` answer also carries short instructions, so an agent knows how the store is organized before its first call.

## Security

- Give an agent the least privileged token that does the job. For analysis a read token is enough. A write token lets the agent commit events, and committed events cannot be changed.
- The endpoint rejects browser requests from foreign origins, to prevent DNS rebinding attacks against a local instance. Requests without an `Origin` header, which is what agents send, are accepted.
- Results can contain personal data from event payloads, and that data goes to the model provider of the agent. Keep that in mind before connecting a store with personal data to a hosted model.
- Treat event payloads read by an agent as untrusted input: text inside an event can try to instruct the agent.

## Protocol

Supported revisions are `2025-06-18`, `2025-03-26` and `2024-11-05`. Every `POST /mcp` carries one JSON-RPC message or a batch and is answered with `application/json`. The endpoint has no sessions and sends no server initiated messages, so `GET /mcp` answers `405`. Implemented methods: `initialize`, `ping`, `tools/list`, `tools/call`, `resources/list`, `resources/templates/list`, `resources/read`.
