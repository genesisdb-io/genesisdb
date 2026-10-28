package query

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// sliceSource is a core.EventSource over a slice. It applies selectors like
// the store does, so plans and filters are tested without any storage.
type sliceSource struct {
	events    []*core.Event
	selectors []core.EventSelector
}

func (source *sliceSource) SelectEvents(ctx context.Context, selector core.EventSelector) ([]*core.Event, error) {
	source.selectors = append(source.selectors, selector)
	var selected []*core.Event
	for _, event := range source.events {
		if selector.All || contains(selector.ExactSubjects, event.Subject) || contains(selector.EventTypes, event.Type) || isBelowAny(event.Subject, selector.SubjectsWithDescendants) {
			selected = append(selected, event)
		}
	}
	return selected, nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func isBelowAny(subject string, parents []string) bool {
	for _, parent := range parents {
		for _, key := range core.SubjectAndAncestors(subject) {
			if key == parent {
				return true
			}
		}
	}
	return false
}

func newSource() *sliceSource {
	source := &sliceSource{}
	add := func(subject, eventType, data string) {
		source.events = append(source.events, &core.Event{
			ID: fmt.Sprintf("id-%d", len(source.events)), Subject: subject, Type: eventType,
			Time: time.Date(2026, 1, 1, 0, 0, len(source.events), 0, time.UTC), Data: json.RawMessage(data),
		})
	}
	add("/u/1", "a", `{"amount":10,"name":"Bruce","address":{"city":"Gotham"}}`)
	add("/u/1/x", "b", `{"amount":20,"name":"Alfred"}`)
	add("/v/1", "c", `{"amount":30,"category":"z"}`)
	add("/w", "d", `{"amount":40,"category":"z"}`)
	return source
}

func run(t *testing.T, source *sliceSource, text string) any {
	t.Helper()
	result, err := Run(context.Background(), source, text)
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	return result
}

func TestFilters_CountMatchingEvents(t *testing.T) {
	cases := map[string]int{
		"FROM e IN events WHERE e.subject == '/u/1' PROJECT INTO { * }":                     1,
		"FROM e IN events WHERE e.subject == '/u/1' OR e.type == 'c' PROJECT INTO { * }":    2,
		"FROM e IN events WHERE e.type == 'c' OR e.data.category == 'z' PROJECT INTO { * }": 2,
		"STREAM e FROM events WHERE e.subject UNDER '/u' MAP { * }":                         1,
		"STREAM e FROM events WHERE e.subject DESCENDANTS '/u' MAP { * }":                   2,
		"STREAM e FROM events WHERE e.subject UNDER '/' MAP { * }":                          1,
		"STREAM e FROM events WHERE e.subject DESCENDANTS '/' MAP { * }":                    4,
		"STREAM e FROM events WHERE e.type == 'a' AND e.subject DESCENDANTS '/u' MAP { * }": 1,
		"STREAM e FROM events WHERE e.type == 'b' AND e.subject UNDER '/u' MAP { * }":       0,
		"STREAM e FROM events WHERE e.type IN ('a','d') MAP { * }":                          2,
		"STREAM e FROM events WHERE e.type != 'a' MAP { * }":                                3,
		"STREAM e FROM events WHERE e.data.amount BETWEEN 15 AND 35 MAP { * }":              2,
		"STREAM e FROM events WHERE e.data.amount >= 30 MAP { * }":                          2,
		"STREAM e FROM events WHERE e.data.address.city == 'Gotham' MAP { * }":              1,
	}
	for text, want := range cases {
		if got := len(run(t, newSource(), text).([]*core.Event)); got != want {
			t.Errorf("%s: got %d want %d", text, got, want)
		}
	}
}

func TestPlanner_SelectsSafeSupersets(t *testing.T) {
	if plan := planCondition("e.subject == '/a' OR e.data.x == 1", "e"); !plan.selector.All {
		t.Error("OR with a branch that cannot use an index must select all")
	}
	if plan := planCondition("e.type == 'x' AND e.subject == '/a'", "e"); plan.selector.All || len(plan.selector.ExactSubjects) != 1 {
		t.Error("AND must pick the most selective branch")
	}
	if plan := planCondition("e.subject UNDER ''", "e"); !plan.selector.All {
		t.Error("an empty parent cannot use the index")
	}
	// The root matches every subject, an indexed type is far more selective.
	for _, condition := range []string{
		"e.subject DESCENDANTS '/' AND e.type == 'x'",
		"e.type == 'x' AND e.subject UNDER '/'",
	} {
		if plan := planCondition(condition, "e"); plan.selector.All || len(plan.selector.EventTypes) != 1 || len(plan.selector.SubjectsWithDescendants) != 0 {
			t.Errorf("%s: plan %+v, want the type index", condition, plan.selector)
		}
	}
	if plan := planCondition("e.subject DESCENDANTS '/' AND e.data.x == 1", "e"); len(plan.selector.SubjectsWithDescendants) != 1 {
		t.Errorf("the root key is still better than a full selection: %+v", plan.selector)
	}
	source := newSource()
	run(t, source, "STREAM e FROM events WHERE e.subject UNDER '/u' MAP { * }")
	if !reflect.DeepEqual(source.selectors[0].SubjectsWithDescendants, []string{"/u"}) {
		t.Errorf("selector %+v", source.selectors[0])
	}
}

func TestProjections_AndAggregates(t *testing.T) {
	rows := run(t, newSource(), "STREAM e FROM events WHERE e.subject DESCENDANTS '/u' ORDER BY e.data.amount DESC MAP { n: e.data.name, a: e.data.amount }").([]Row)
	if len(rows) != 2 || rows[0]["n"] != "Alfred" {
		t.Fatalf("rows %v", rows)
	}
	if count := run(t, newSource(), "STREAM e FROM events MAP COUNT()"); count != 4 {
		t.Fatalf("count %v", count)
	}
	if isEmpty := run(t, newSource(), "STREAM e FROM events WHERE e.type == 'none' MAP COUNT() == 0"); isEmpty != true {
		t.Fatal("COUNT() == 0")
	}
	aggregates := run(t, newSource(), "STREAM e FROM events MAP { s: SUM(e.data.amount), lo: MIN(e.data.amount), hi: MAX(e.data.amount), avg: AVG(e.data.amount) }").(Row)
	// Integral aggregates are exact integers, which JSON renders identically
	// to the historical float results (100, not 100.0).
	if aggregates["s"] != 100 || aggregates["lo"] != 10 || aggregates["hi"] != 40 || aggregates["avg"] != 25 {
		t.Fatalf("aggregates %v", aggregates)
	}
	groups := run(t, newSource(), "STREAM e FROM events WHERE e.data.category == 'z' GROUP BY e.data.category MAP { c: COUNT() }").([]Row)
	if len(groups) != 1 || groups[0]["c"] != 2 {
		t.Fatalf("groups %v", groups)
	}
	limited := run(t, newSource(), "STREAM e FROM events LIMIT 2 MAP { id: e.id }").([]Row)
	if len(limited) != 2 {
		t.Fatal("LIMIT")
	}
	// A trailing LIMIT without a projection was silently ignored by 1.1.6.
	limitedAtEnd := run(t, newSource(), "STREAM e FROM events ORDER BY e.time LIMIT 1").([]*core.Event)
	if len(limitedAtEnd) != 1 {
		t.Fatalf("trailing LIMIT returned %d events", len(limitedAtEnd))
	}
	concatenated := run(t, newSource(), "STREAM e FROM events WHERE e.subject == '/u/1' MAP { text: 'Name: ' + e.data.name }").([]Row)
	if concatenated[0]["text"] != "Name: Bruce" {
		t.Fatalf("concatenation %v", concatenated)
	}
}

// Computed fields behave the same for every alias: literal arithmetic is
// evaluated, arithmetic on event fields yields null (a 1.1.6 quirk that is
// kept), concatenation reads the fields.
func TestProjection_ComputedFieldsAreAliasIndependent(t *testing.T) {
	for _, alias := range []string{"e", "ev", "x"} {
		rows := run(t, newSource(), fmt.Sprintf("STREAM %s FROM events WHERE %s.subject == '/u/1' MAP { sum: 2 + 3, field: %s.data.amount * 2, text: %s.data.name + '!' }", alias, alias, alias, alias)).([]Row)
		if len(rows) != 1 || rows[0]["sum"] != 5 || rows[0]["field"] != nil || rows[0]["text"] != "Bruce!" {
			t.Errorf("alias %s: %v", alias, rows)
		}
	}
}

func TestParse_RejectsQueriesWithoutSource(t *testing.T) {
	if _, err := Parse("SELECT nothing"); err == nil || !strings.Contains(err.Error(), "missing FROM or STREAM") {
		t.Fatalf("got %v", err)
	}
}

func TestEvaluatePrecondition(t *testing.T) {
	cases := map[string]bool{
		"STREAM e FROM events WHERE e.subject == '/u/1' MAP COUNT() == 1":  true,
		"STREAM e FROM events WHERE e.subject == '/none' MAP COUNT() == 0": true,
		"STREAM e FROM events WHERE e.subject == '/u/1' MAP COUNT() > 5":   false,
		"STREAM e FROM events WHERE e.subject == '/u/1' MAP { id: e.id }":  true,
		"STREAM e FROM events WHERE e.subject == '/none' MAP { id: e.id }": false,
	}
	for text, want := range cases {
		got, err := EvaluatePrecondition(context.Background(), newSource(), text)
		if err != nil || got != want {
			t.Errorf("%s: got %v %v want %v", text, got, err, want)
		}
	}
}

// countingSource adds core.EventCounter to sliceSource and records whether
// the index count was used.
type countingSource struct {
	*sliceSource
	counts int
}

func (source *countingSource) CountSelectedEvents(ctx context.Context, selector core.EventSelector) (int, error) {
	source.counts++
	events, err := source.sliceSource.SelectEvents(ctx, selector)
	return len(events), err
}

// TestCount_FromTheIndexMatchesTheFullEvaluation runs every COUNT query
// against a source that can count and one that cannot. The results must be
// identical, and the index count must only be used where it is exact.
func TestCount_FromTheIndexMatchesTheFullEvaluation(t *testing.T) {
	cases := []struct {
		query     string
		fromIndex bool
	}{
		{"STREAM e FROM events MAP COUNT()", true},
		{"STREAM e FROM events WHERE e.type == 'a' MAP COUNT()", true},
		{"STREAM e FROM events WHERE e.type IN ('a', 'c') MAP COUNT()", true},
		{"STREAM e FROM events WHERE e.subject == '/u/1' MAP COUNT()", true},
		{"STREAM e FROM events WHERE e.subject IN ('/u/1', '/w') MAP COUNT()", true},
		{"STREAM e FROM events WHERE e.type == 'none' MAP COUNT() == 0", true},
		{"STREAM e FROM events WHERE e.type == 'a' MAP COUNT() > 0", true},
		{"FROM e IN events WHERE e.type == 'a' PROJECT INTO COUNT()", true},
		// Not exact: the operators exclude the parent, the index key includes it.
		{"STREAM e FROM events WHERE e.subject DESCENDANTS '/u' MAP COUNT()", false},
		{"STREAM e FROM events WHERE e.subject UNDER '/' MAP COUNT()", false},
		{"STREAM e FROM events WHERE e.subject DESCENDANTS '/' MAP COUNT()", false},
		// Not exact: residual filters, other aggregates, grouping, limits.
		{"STREAM e FROM events WHERE e.type == 'a' AND e.data.amount > 5 MAP COUNT()", false},
		{"STREAM e FROM events WHERE e.type == 'a' OR e.type == 'b' MAP COUNT()", false},
		{"STREAM e FROM events WHERE e.data.category == 'z' MAP COUNT()", false},
		{"STREAM e FROM events WHERE e.type == 1 MAP COUNT()", false},
		{"STREAM e FROM events MAP SUM(e.data.amount)", false},
		{"STREAM e FROM events MAP { n: COUNT(), s: SUM(e.data.amount) }", false},
		{"STREAM e FROM events GROUP BY e.type MAP { n: COUNT() }", false},
		{"STREAM e FROM events LIMIT 2 MAP COUNT()", false},
		{"STREAM o FROM (STREAM e FROM events MAP { * }) MAP COUNT()", false},
	}
	for _, testCase := range cases {
		plain := run(t, newSource(), testCase.query)
		counting := &countingSource{sliceSource: newSource()}
		counted, err := Run(context.Background(), counting, testCase.query)
		if err != nil {
			t.Fatalf("%s: %v", testCase.query, err)
		}
		if !reflect.DeepEqual(plain, counted) {
			t.Errorf("%s: %v with the index, %v without", testCase.query, counted, plain)
		}
		if usedIndex := counting.counts > 0; usedIndex != testCase.fromIndex {
			t.Errorf("%s: index count used %v, want %v", testCase.query, usedIndex, testCase.fromIndex)
		}
	}
}
