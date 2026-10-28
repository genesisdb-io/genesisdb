package httpserver

// gdbqlReference is the GDBQL reference served to agents as the MCP resource
// genesisdb://docs/gdbql. It is written for a model that writes queries:
// syntax, operators and examples only. docs/gdbql.md is the full reference
// for people. Every line of the examples block is a complete query that a
// test runs through the parser.
const gdbqlReference = `# GDBQL reference

GDBQL is the query language of GenesisDB. Use it with the query tool and in isQueryResultTrue preconditions.

## Shape

STREAM e FROM events
[WHERE condition]
[ORDER BY field [ASC|DESC]]
[GROUP BY field [HAVING condition]]
[LIMIT n]
MAP { name: expression, ... }

e is the event handle and can be any name. MAP is required and ends the query. A subquery can replace events: STREAM o FROM (STREAM e FROM events WHERE ... MAP { ... }) WHERE ... MAP { ... }

## Fields

- e.id, e.subject, e.type, e.source, e.specversion, e.time: event metadata
- e.data: the whole payload, e.data.a.b: a nested payload field

Literals: 'text', "text", numbers, true, false, null.

Comparisons are typed: e.data.n == 1 matches the number 1, not the string "1". Use == null for missing fields.

## Conditions

- ==, !=
- >, <, >=, <= (numbers with numbers, text with text)
- IN ('a', 'b')
- BETWEEN 10 AND 100
- UNDER '/customer': direct children of /customer, not /customer itself
- DESCENDANTS '/customer': all subjects below /customer, not /customer itself
- AND binds stronger than OR. Parentheses do not group conditions.

Conditions on e.subject and e.type (==, IN, UNDER, DESCENDANTS) use indexes and are fast. Everything else reads the selected events, so always narrow by subject or type first.

## Projections and aggregates

- MAP { id: e.id, city: e.data.address.city }
- MAP { name: e.data.firstName + ' ' + e.data.lastName }
- MAP { * } returns whole events, MAP e.data.firstName returns one field
- Aggregates: COUNT(), SUM(field), AVG(field), MIN(field), MAX(field)
- A single aggregate returns a value, several return one object, GROUP BY returns one object per group including the group field.
- COUNT() == n, COUNT() > n and COUNT() < n return a boolean.

COUNT() on e.subject and e.type conditions is answered from the index without reading events. Prefer aggregates and projections over MAP { * }.

## Preconditions

isQueryResultTrue commits only when the result is true: a true boolean, a number other than 0, the text true or 1, or a non-empty list of rows.

## Examples

` + "```" + `
STREAM e FROM events WHERE e.subject == '/customer/42' MAP { * }
STREAM e FROM events WHERE e.subject DESCENDANTS '/customer' AND e.type == 'io.example.shop.customer-registered' MAP { id: e.subject, email: e.data.email }
STREAM e FROM events WHERE e.type == 'io.example.shop.order-placed' MAP COUNT()
STREAM e FROM events WHERE e.type == 'io.example.shop.order-placed' GROUP BY e.data.category MAP { orders: COUNT(), revenue: SUM(e.data.amount) } HAVING COUNT() > 10
STREAM e FROM events WHERE e.type == 'io.example.shop.order-placed' AND e.data.amount BETWEEN 100 AND 500 ORDER BY e.data.amount DESC LIMIT 10 MAP { order: e.subject, amount: e.data.amount }
STREAM e FROM events WHERE e.type IN ('io.example.shop.order-placed', 'io.example.shop.order-cancelled') ORDER BY e.time DESC LIMIT 20 MAP { type: e.type, at: e.time }
STREAM e FROM events WHERE e.type == 'io.example.shop.customer-registered' AND e.data.email == 'a@example.com' MAP COUNT() == 0
` + "```" + `

The LINQ-style form FROM e IN events WHERE ... TOP n PROJECT INTO { ... } is accepted as well, write new queries in the STREAM form.
`
