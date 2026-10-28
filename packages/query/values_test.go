package query

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

type payloadSource []string

func (source payloadSource) SelectEvents(context.Context, core.EventSelector) ([]*core.Event, error) {
	events := make([]*core.Event, 0, len(source))
	for _, payload := range source {
		events = append(events, &core.Event{Subject: "/x", Type: "t", Data: json.RawMessage(payload)})
	}
	return events, nil
}

func count(t *testing.T, source core.EventSource, condition string) int {
	t.Helper()
	result, err := Run(context.Background(), source, "STREAM e FROM events WHERE "+condition+" MAP COUNT()")
	if err != nil {
		t.Fatalf("%s: %v", condition, err)
	}
	return result.(int)
}

// TestTypedComparisons checks that strings, numbers and booleans are
// distinct and that integers beyond 2^53 compare exactly.
func TestTypedComparisons(t *testing.T) {
	source := payloadSource{
		`{"n":9007199254740993,"s":"1","b":true,"t":"true","z":0,"f":1.5,"big":123456789012345678901234567890}`,
		`{"n":1,"s":"a","b":false,"t":"false","z":"0","f":2,"nested":{"v":"7"}}`,
	}
	cases := map[string]int{
		// Exact integers.
		"e.data.n > 9007199254740992":                  1,
		"e.data.n == 9007199254740993":                 1,
		"e.data.n == 9007199254740992":                 0,
		"e.data.big == 123456789012345678901234567890": 1,
		"e.data.big > 123456789012345678901234567889":  1,
		// Strings are not numbers.
		"e.data.s == 1":   0,
		"e.data.s == '1'": 1,
		"e.data.z == 0":   1,
		"e.data.z == '0'": 1,
		"e.data.z != 0":   1,
		// Booleans are not strings.
		"e.data.b == true":     1,
		"e.data.b == 'true'":   0,
		"e.data.t == 'true'":   1,
		"e.data.t == true":     0,
		"e.data.b IN (true)":   1,
		"e.data.b IN ('true')": 0,
		// Ordering only between compatible types.
		"e.data.s > 0":                        0,
		"e.data.f >= 1.5":                     2,
		"e.data.f BETWEEN 1.5 AND 2":          2,
		"e.data.f BETWEEN '1.5' AND '2'":      0,
		"e.data.s BETWEEN '1' AND 'b'":        2,
		"e.data.nested.v == 7":                0,
		"e.data.nested.v == '7'":              1,
		"e.data.missing == null":              2,
		"e.data.missing != null":              0,
		"e.data.n != null":                    2,
		"e.subject == '/x' AND e.data.n == 1": 1,
	}
	for condition, expected := range cases {
		if got := count(t, source, condition); got != expected {
			t.Errorf("%s: %d, want %d", condition, got, expected)
		}
	}
}

// TestExactAggregates checks that sums and extremes keep integer precision
// and that projections render large integers unchanged.
func TestExactAggregates(t *testing.T) {
	source := payloadSource{
		`{"amount":9007199254740993,"label":"b"}`,
		`{"amount":1,"label":"a"}`,
		`{"amount":"ignored","label":"c"}`,
	}
	row, err := Run(context.Background(), source, "STREAM e FROM events MAP { s: SUM(e.data.amount), hi: MAX(e.data.amount), lo: MIN(e.data.amount), avg: AVG(e.data.amount), first: MIN(e.data.label) }")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(row)
	if string(encoded) != `{"avg":4503599627370497,"first":"a","hi":9007199254740993,"lo":1,"s":9007199254740994}` {
		t.Fatalf("aggregates %s", encoded)
	}
	rows, err := Run(context.Background(), source, "STREAM e FROM events ORDER BY e.data.amount DESC MAP { a: e.data.amount }")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(rows)
	// Non-numeric values sort as text after numbers in descending order of
	// their rendering, numbers keep every digit.
	if string(encoded) != `[{"a":"ignored"},{"a":9007199254740993},{"a":1}]` {
		t.Fatalf("projection %s", encoded)
	}
	having, err := Run(context.Background(), source, "STREAM e FROM events GROUP BY e.data.label MAP { c: COUNT(), s: SUM(e.data.amount) } HAVING SUM(e.data.amount) > 9007199254740992")
	if err != nil {
		t.Fatal(err)
	}
	if groups := having.([]Row); len(groups) != 1 || groups[0]["e.data.label"] != "b" {
		t.Fatalf("having %v", having)
	}
}

// TestPreconditionTypedCount guards the precondition path, which shares the
// comparison code.
func TestPreconditionTypedCount(t *testing.T) {
	source := payloadSource{`{"email":"a@example.invalid","n":1}`}
	for query, expected := range map[string]bool{
		"STREAM e FROM events WHERE e.data.email == 'a@example.invalid' MAP COUNT() == 1": true,
		"STREAM e FROM events WHERE e.data.n == '1' MAP COUNT() == 0":                     true,
		"STREAM e FROM events WHERE e.data.n == 1 MAP COUNT() == 0":                       false,
	} {
		got, err := EvaluatePrecondition(context.Background(), source, query)
		if err != nil || got != expected {
			t.Errorf("%s: %v %v", query, got, err)
		}
	}
}
