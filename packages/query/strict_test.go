package query

import (
	"context"
	"strings"
	"testing"
)

// A typo must fail with an error instead of silently changing the result.
// Every query here was accepted by 1.1.6 and returned a wrong answer.
func TestStrict_RejectsUnknownWords(t *testing.T) {
	for _, text := range []string{
		"STREAM e FROM events WHERE e.type == 'a' MAPP COUNT()",
		"STREAM e FROM events WHERE e.type == 'a' LIMT 5 MAP { id: e.id }",
		"STREAM e FROM events MAP COUNT() garbage",
		"STREAM e FROM events WHERE e.type == 'a' extra MAP COUNT()",
		"STREAM e FROM events WHERE e.type == 'a' ORDERBY e.time MAP { id: e.id }",
		"STREAM e FROM events WHERE e.type == 'a' ORDER BY e.time DESCENDING MAP { id: e.id }",
		"STREAM e FROM events WHERE e.type == 'a' GROUP BY e.data.c x MAP { n: COUNT() }",
		"STREAM e FROM events WHERE e.data.n BETWEEN 1 AND 2 3 MAP COUNT()",
		"STREAM e FROM events WHERE e.data.n BETWEEN 1 MAP COUNT()",
		"STREAM e FROM events WHERE e.type == 'a' AND MAP COUNT()",
		"STREAM e FROM events MAP { id: e.id e.type }",
		"STREAM e FROM events MAP { id e.id }",
		"STREAM e FROM events MAP {}",
		"STREAM e FROM events LIMIT x MAP { id: e.id }",
		"STREAM e FROM events LIMIT 5 LIMIT 6 MAP { id: e.id }",
		"STREAM e FROM events TOP 5 LIMIT 6 MAP { id: e.id }",
		"STREAM e FROM events MAP { id: e.id } PROJECT INTO { * }",
		"STREAM e FROM events WHERE e.type == 'a' WHERE e.type == 'b' MAP COUNT()",
		"STREAM e FROM events HAVING COUNT() > 1 MAP COUNT()",
		"STREAM e FROM other MAP COUNT()",
		"STREAM e events MAP COUNT()",
		"STREAM e FROM events WHERE MAP COUNT()",
		"COUNT STREAM e FROM events MAP COUNT()",
		"STREAM e FROM events MAP COUNT() STREAM e FROM events",
	} {
		if _, err := Parse(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

// Valid spellings keep working, including the forms of earlier releases.
func TestStrict_AcceptsValidQueries(t *testing.T) {
	for _, text := range []string{
		"STREAM e FROM events",
		"STREAM e FROM events WHERE e.type == 'a'",
		"stream e from events where e.type == 'a' map count()",
		"STREAM e FROM events WHERE e.type == 'MAP LIMIT 5' MAP COUNT()",
		"STREAM e FROM events WHERE e.data.note == 'a  b' MAP { id: e.id }",
		"STREAM e FROM events WHERE e.type IN ('a', 'b c', 'd,e') MAP COUNT()",
		"STREAM e FROM events WHERE e.data.n BETWEEN 1 AND 2 AND e.type == 'a' OR e.type == 'b' MAP COUNT()",
		"STREAM e FROM events WHERE e.data.n BETWEEN '2026-01-01' AND '2026-12-31' MAP COUNT()",
		"STREAM e FROM events WHERE e.data.n >= -1.5 MAP COUNT()",
		"STREAM e FROM events WHERE e.subject UNDER '/' MAP { s: e.subject }",
		"STREAM e FROM events WHERE e.subject DESCENDANTS '/org' MAP { s: e.subject }",
		"STREAM e FROM events ORDER BY e.time MAP { id: e.id }",
		"STREAM e FROM events ORDER BY e.data.amount desc LIMIT 2 MAP { a: e.data.amount }",
		"STREAM e FROM events GROUP BY e.data.c MAP { c: COUNT(), s: SUM(e.data.amount) } HAVING COUNT() > 1",
		"STREAM e FROM events GROUP BY e.data.c HAVING COUNT() > 1 MAP { c: COUNT() }",
		"STREAM e FROM events MAP { * }",
		"STREAM e FROM events MAP *",
		"STREAM e FROM events MAP e.data.firstName",
		"STREAM e FROM events MAP 2+3",
		"STREAM e FROM events MAP 2 + 3",
		"STREAM e FROM events MAP COUNT() == 0",
		"STREAM e FROM events MAP COUNT()",
		"STREAM e FROM events MAP { full: e.data.firstName + ' ' + e.data.lastName }",
		"STREAM e FROM events MAP { total: SUM(e.data.amount), avg: AVG(e.data.amount), lo: MIN(e.data.amount), hi: MAX(e.data.amount) }",
		"STREAM e FROM events MAP { limit: e.data.limit, top: e.data.top }",
		"STREAM e\n  FROM events\n  WHERE e.type == 'a'\n  MAP { id: e.id }",
		"STREAM e FROM events TOP 3 MAP { id: e.id }",
		"LIMIT 5 STREAM e FROM events MAP { id: e.id }",
		"FROM e IN events WHERE e.type == 'a' TOP 3 PROJECT INTO { amount: e.data.amount }",
		"FROM e IN events PROJECT INTO { * }",
		"STREAM o FROM (STREAM e FROM events WHERE e.type == 'a' MAP { amount: e.data.amount }) WHERE o.amount > 5 MAP COUNT()",
		"FROM x IN (STREAM e FROM events MAP {*}) MAP {*}",
	} {
		if _, err := Parse(text); err != nil {
			t.Errorf("rejected %q: %v", text, err)
		}
	}
}

// The error names what is wrong, so a client can fix the query.
func TestStrict_ErrorsExplainTheProblem(t *testing.T) {
	for text, fragment := range map[string]string{
		"STREAM e FROM events MAP COUNT() garbage":                    "unexpected text in projection",
		"STREAM e FROM events WHERE e.type == 'a' LIMT 5 MAP COUNT()": "unexpected text after the value",
		"STREAM e FROM events LIMIT 5 LIMIT 6 MAP COUNT()":            "LIMIT appears more than once",
		"STREAM e FROM events HAVING COUNT() > 1 MAP COUNT()":         "HAVING requires GROUP BY",
		"SELECT nothing": "missing FROM or STREAM clause",
		"STREAM e FROM events WHERE e.type == 'a' ORDERBY e.time MAP { * }": "unexpected text after the value",
	} {
		_, err := Parse(text)
		if err == nil || !strings.Contains(err.Error(), fragment) {
			t.Errorf("%q: error %v, want %q", text, err, fragment)
		}
	}
}

// A precondition with a typo must fail the commit with an error, never be
// evaluated as false or true.
func TestStrict_PreconditionWithTypoFails(t *testing.T) {
	if _, err := EvaluatePrecondition(context.Background(), newSource(), "STREAM e FROM events WHERE e.type == 'a' MAPP COUNT() == 0"); err == nil {
		t.Fatal("precondition with a typo was evaluated")
	}
}
