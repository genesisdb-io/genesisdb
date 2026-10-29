# GenesisDB Query Language (GDBQL)

GDBQL is the query language of GenesisDB. It filters, orders, groups, aggregates and projects events. Queries run through `POST /api/v2/query`, in `isQueryResultTrue` preconditions, or in Go with `query.Run`.

## Syntax

```sql
STREAM e FROM events
[WHERE condition]
[ORDER BY field [ASC|DESC]]
[GROUP BY field [HAVING condition]]
[LIMIT n]
MAP { name: expression, ... }
```

`STREAM` names the handle of an event, `e` here, and can be any name. `FROM events` is the source. `MAP` ends the query with a projection. A subquery can replace `events`: `STREAM o FROM (STREAM e FROM events WHERE ... MAP { ... }) WHERE ... MAP { ... }`.

## Fields

| Field | Value |
|---|---|
| `e.id`, `e.subject`, `e.type`, `e.source`, `e.specversion` | Event metadata |
| `e.time` | Event time |
| `e.data` | The whole payload |
| `e.data.a.b` | A nested payload field |

Literals are `'text'`, `"text"`, numbers, `true`, `false` and `null`.

Comparisons are typed. A JSON string never equals a number or a boolean: `e.data.n == 1` matches the number `1`, not the string `"1"`, and `e.data.b == 'true'` does not match the boolean `true`. Use `== null` to test for missing or null fields. Numbers are compared exactly with arbitrary precision, so integers beyond 2^53 (for example `9007199254740993`) are neither rounded nor confused with their neighbours. `SUM` and `AVG` accumulate exactly, `MIN` and `MAX` prefer numbers when any value is numeric. Ordering operators (`>`, `<`, `>=`, `<=`, `BETWEEN`) compare numbers with number literals and text with text literals, mixed types never match.

## Conditions

| Operator | Example |
|---|---|
| `==`, `!=` | `e.type == 'user.created'` |
| `>`, `<`, `>=`, `<=` | `e.data.age >= 18` (numeric) |
| `IN` | `e.type IN ('a', 'b')` |
| `BETWEEN` | `e.data.amount BETWEEN 10 AND 100`, also for timestamps as text |
| `UNDER` | `e.subject UNDER '/user'`: direct children only, not the parent |
| `DESCENDANTS` | `e.subject DESCENDANTS '/org'`: all descendants, not the parent |
| `AND`, `OR` | `AND` binds stronger than `OR` |

`UNDER '/'` matches top level subjects, `DESCENDANTS '/'` matches every subject. Parentheses do not group conditions. `BETWEEN` consumes its first `AND` as the range separator and can be combined with further `AND`/`OR` predicates. Quoted keywords, operators and commas are literal text, not clause boundaries. `IN ('a', 'b')` trims whitespace around entries and supports quoted commas.

Every word of a query must belong to a clause. A misspelled keyword (`MAPP`, `LIMT`), a repeated clause, text after a value or projection, and `HAVING` without `GROUP BY` are rejected with `400` and a message that names the problem, they never change the result silently. Queries are capped at 64 KiB with at most 32 nested parentheses. Integer overflow in `LIMIT` is rejected. Only the `events` source or a supported subquery is accepted.

Queries are never limited by store size. `LIMIT`, projections and aggregates run after event selection/loading, so a query such as `MAP COUNT()` over an unindexable predicate loads every matching event into memory. Shard scans are linear and an unindexable `OR` scans the whole event set. There is no storage-level pagination or streaming aggregate API. Deployments that want a ceiling can configure optional read caps, see the [HTTP API](http-api.md).

Conditions on `e.subject` and `e.type` (`==`, `IN`, `UNDER`, `DESCENDANTS`) use the indexes. GenesisDB picks the most selective one in an `AND` chain, independent of the order. An `OR` with a branch that cannot use an index reads all events.

## Projections

```sql
MAP { id: e.id, city: e.data.address.city }
MAP { fullName: e.data.firstName + ' ' + e.data.lastName }
MAP { * }
MAP e.data.firstName
```

`+` with a quoted literal concatenates text. Arithmetic works on number literals only (`MAP 2 + 3`), not on fields. Without a name, a field is named after its last path segment.

## Aggregates

`COUNT()`, `SUM(field)`, `AVG(field)`, `MIN(field)`, `MAX(field)`.

```sql
STREAM e FROM events WHERE e.type == 'order.placed' MAP COUNT()
STREAM e FROM events WHERE e.type == 'order.placed' MAP { total: SUM(e.data.amount), average: AVG(e.data.amount) }
STREAM e FROM events WHERE e.type == 'order.placed' GROUP BY e.data.category MAP { orders: COUNT(), total: SUM(e.data.amount) } HAVING COUNT() > 10
```

A single aggregate returns a value. Several aggregates return one object. `GROUP BY` returns one object per group, in the order of each group's first event. The group value is stored under the `GROUP BY` expression exactly as written, so `GROUP BY e.data.category MAP { orders: COUNT() }` answers rows like `{"e.data.category": "books", "orders": 3}`. `COUNT() == n`, `COUNT() > n` and `COUNT() < n` return a boolean, which is useful in preconditions.

## Preconditions

`isQueryResultTrue` commits only when the query result is true:

| Result | True when |
|---|---|
| boolean | it is true |
| number | it is not 0 |
| text | it is `true` or `1` |
| list of rows | it is not empty |

```json
{ "type": "isQueryResultTrue", "payload": { "query": "STREAM e FROM events WHERE e.data.email == 'john@example.com' MAP COUNT() == 0" } }
```

## LINQ-style compatibility syntax

GDBQL also accepts the LINQ-style query syntax that several event stores and .NET developers use, so existing queries and migrated applications run unchanged:

```sql
FROM e IN events [WHERE ...] [ORDER BY ...] [GROUP BY ... [HAVING ...]] [TOP n] PROJECT INTO { ... }
```

The mapping is one to one: `FROM e IN events` is `STREAM e FROM events`, `TOP` is `LIMIT`, `PROJECT INTO` is `MAP`. Conditions, fields, literals, aggregates and subqueries are identical in both spellings, and the two can be mixed freely. This form is kept for compatibility, new documentation and examples use the `STREAM ... MAP` form.

## In Go

The engine runs against any `core.EventSource`, not only a GenesisDB store:

```go
result, err := query.Run(ctx, source, "STREAM e FROM events WHERE e.subject UNDER '/user' MAP { id: e.id }")

parsed, err := query.Parse(text)
selector := query.PlanQuery(parsed).Selector() // which index selection the query needs
```
