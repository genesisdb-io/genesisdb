package httpserver

// mcpResource is one readable document of the MCP endpoint.
type mcpResource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
	Text        string `json:"-"`
}

var mcpResources = []mcpResource{
	{
		URI:         "genesisdb://docs/gdbql",
		Name:        "gdbql",
		Title:       "GDBQL reference",
		Description: "Syntax of the GenesisDB Query Language: conditions, fields, projections, aggregates and preconditions",
		MimeType:    "text/markdown",
		Text:        gdbqlReference,
	},
	{
		URI:         "genesisdb://docs/events",
		Name:        "events",
		Title:       "Events in GenesisDB",
		Description: "How events, subjects, types and preconditions work, and how to model them",
		MimeType:    "text/markdown",
		Text:        eventsGuide,
	},
}

func mcpResourceList() []mcpResource { return mcpResources }

func mcpResourceByURI(uri string) (mcpResource, bool) {
	for _, resource := range mcpResources {
		if resource.URI == uri {
			return resource, true
		}
	}
	return mcpResource{}, false
}

const eventsGuide = `# Events in GenesisDB

Every event is an immutable CloudEvents 1.0 record:

| Field | Meaning |
|---|---|
| source | Origin, for example io.example.shop |
| subject | Path in the subject tree, for example /customer/42/address |
| type | What happened, in past tense, for example io.example.shop.customer-registered |
| data | JSON object with the payload |
| id, time, hash, predecessorhash | Assigned by GenesisDB on commit |

## Subjects

Subjects form a tree. Reading /customer returns the events of /customer and of every subject below it, in commit order. A subject with a single segment such as /customer gets a generated ID on commit (/customer/<uuid>), so commit to /customer/42 when the ID is known.

## Commits

A commit writes a batch of events atomically: all or none. Preconditions guard a commit:

- {"type": "isSubjectNew", "payload": {"subject": "/customer/42"}}: neither the subject nor a descendant has events.
- {"type": "isSubjectExisting", "payload": {"subject": "/customer/42"}}: the subject or a descendant has events.
- {"type": "isQueryResultTrue", "payload": {"query": "STREAM e FROM events WHERE e.data.email == 'a@example.com' MAP COUNT() == 0"}}: the GDBQL result is true.

When a precondition fails nothing is written. Events cannot be changed or deleted afterwards. To correct a mistake commit a new event that compensates it.

## Personal data

Never put personal data into source, subject or type, they are immutable. Put it into data and set options.storeDataAsReference to true. An administrator can then erase the payload later while the event, its hash and the history stay intact.

## Schemas

An event type can have a registered JSON schema. Commits of that type must match it, list_schemas shows them.

## Modeling tips

- One subject per entity (/customer/42, /order/1337), children for parts of it (/order/1337/shipment).
- Event types name facts in past tense, not commands.
- Keep events small and focused, one fact per event.
`
