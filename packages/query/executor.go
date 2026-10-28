package query

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// Execute runs a query against source. The result is one of:
//   - []*core.Event for full event projections
//   - []Row for field projections and GROUP BY
//   - Row for several aggregates without GROUP BY
//   - a single Value for one aggregate, or a bool for an aggregate
//     comparison such as COUNT() == 0.
func Execute(ctx context.Context, source core.EventSource, q *Query) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q == nil {
		return nil, fmt.Errorf("query is nil")
	}
	if q.Limit != nil && *q.Limit < 0 {
		return nil, fmt.Errorf("query limit must not be negative")
	}
	if result, isCounted, err := countFromIndex(ctx, source, q); isCounted || err != nil {
		return result, err
	}
	events, err := loadEvents(ctx, source, q)
	if err != nil {
		return nil, err
	}
	alias := q.Alias()
	if q.Filter != nil && q.Filter.Condition != "" {
		matching := make([]*core.Event, 0, len(events))
		for _, event := range events {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if evaluateCondition(event, q.Filter.Condition, alias) {
				matching = append(matching, event)
			}
		}
		events = matching
	}
	if q.Grouping != nil {
		return groupEvents(events, q, alias), nil
	}
	if q.Ordering != nil {
		sortEvents(events, q.Ordering, alias)
	}
	if q.Limit != nil && *q.Limit < len(events) {
		events = events[:*q.Limit]
	}
	return project(events, q.Projection, alias), nil
}

// Run parses and executes GDBQL text.
func Run(ctx context.Context, source core.EventSource, text string) (any, error) {
	q, err := Parse(text)
	if err != nil {
		return nil, err
	}
	return Execute(ctx, source, q)
}

// countFromIndex answers a query whose result is a bare COUNT(), or a COUNT()
// comparison, from the index when the source can count and the index
// selection is exactly the set of matching events: no subquery, no GROUP BY,
// no LIMIT, and a WHERE clause that the plan resolves without a residual
// filter. Everything else returns isCounted false and runs normally.
func countFromIndex(ctx context.Context, source core.EventSource, q *Query) (result any, isCounted bool, err error) {
	counter, canCount := source.(core.EventCounter)
	if !canCount || q.From.Subquery != nil || q.Grouping != nil || q.Limit != nil || len(q.Projection.Fields) != 1 {
		return nil, false, nil
	}
	var expression string
	for _, field := range q.Projection.Fields {
		if !field.IsAggregate {
			return nil, false, nil
		}
		expression = strings.TrimSpace(field.Expression)
	}
	upper := strings.ToUpper(expression)
	if !strings.HasPrefix(upper, "COUNT()") {
		return nil, false, nil
	}
	selector, isExact := exactSelector(q)
	if !isExact {
		return nil, false, nil
	}
	count, err := counter.CountSelectedEvents(ctx, selector)
	if err != nil {
		return nil, true, fmt.Errorf("failed to count events: %w", err)
	}
	if upper == "COUNT()" {
		return count, true, nil
	}
	return evaluateCountComparison(expression, count), true, nil
}

// exactSelector returns the index selection of q when it matches exactly
// the events the WHERE clause accepts, so no event has to be loaded to
// filter. That holds for no WHERE clause and for one comparison on e.subject
// or e.type with == or IN. It does not hold for UNDER and DESCENDANTS: the
// index key includes the parent itself, the operators exclude it, and a
// store opened in Go may hold an event with the subject "/".
func exactSelector(q *Query) (core.EventSelector, bool) {
	if q.Filter == nil || strings.TrimSpace(q.Filter.Condition) == "" {
		return core.EventSelector{All: true}, true
	}
	condition := strings.TrimSpace(q.Filter.Condition)
	if hasTopLevelAnd(condition) || hasTopLevelOr(condition) {
		return core.EventSelector{}, false
	}
	left, operator, right, ok := splitComparison(condition)
	if !ok {
		return core.EventSelector{}, false
	}
	alias := q.Alias()
	switch {
	case left == alias+".subject" && operator == "==":
		return core.EventSelector{ExactSubjects: []string{unquote(right)}}, isQuoted(right)
	case left == alias+".subject" && operator == " IN ":
		return core.EventSelector{ExactSubjects: splitValueList(right)}, allQuoted(right)
	case left == alias+".type" && operator == "==":
		return core.EventSelector{EventTypes: []string{unquote(right)}}, isQuoted(right)
	case left == alias+".type" && operator == " IN ":
		return core.EventSelector{EventTypes: splitValueList(right)}, allQuoted(right)
	}
	return core.EventSelector{}, false
}

// isQuoted reports whether a literal is a quoted text. A comparison of the
// subject or type with a number, boolean or null never matches, which the
// index cannot express.
func isQuoted(text string) bool {
	text = strings.TrimSpace(text)
	return len(text) >= 2 && (text[0] == '\'' || text[0] == '"') && text[len(text)-1] == text[0]
}

func allQuoted(list string) bool {
	values := splitTopLevel(strings.Trim(strings.TrimSpace(list), "()"), ',')
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if !isQuoted(value) {
			return false
		}
	}
	return true
}

func loadEvents(ctx context.Context, source core.EventSource, q *Query) ([]*core.Event, error) {
	if q.From.Subquery != nil {
		subqueryResult, err := Execute(ctx, source, q.From.Subquery)
		if err != nil {
			return nil, fmt.Errorf("failed to execute subquery: %w", err)
		}
		events, err := resultsAsEvents(subqueryResult)
		if err != nil {
			return nil, fmt.Errorf("failed to convert subquery results: %w", err)
		}
		return events, nil
	}
	events, err := source.SelectEvents(ctx, PlanQuery(q).Selector())
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve events: %w", err)
	}
	return events, nil
}

// evaluateCondition evaluates a WHERE condition against an event.
func evaluateCondition(event *core.Event, condition, alias string) bool {
	if hasTopLevelOr(condition) {
		for _, part := range splitConditionTopLevel(condition, " OR ") {
			if evaluateCondition(event, strings.TrimSpace(part), alias) {
				return true
			}
		}
		return false
	}
	if hasTopLevelAnd(condition) {
		for _, part := range splitConditionTopLevel(condition, " AND ") {
			if !evaluateCondition(event, strings.TrimSpace(part), alias) {
				return false
			}
		}
		return true
	}
	left, operator, right, ok := splitComparison(condition)
	if !ok {
		return false
	}
	leftValue := resolveFieldPath(event, left, alias)
	switch operator {
	case "==":
		return valueEquals(leftValue, parseLiteral(right))
	case "!=":
		return !valueEquals(leftValue, parseLiteral(right))
	case " IN ":
		for _, candidate := range splitTopLevel(strings.Trim(strings.TrimSpace(right), "()"), ',') {
			if valueEquals(leftValue, parseLiteral(candidate)) {
				return true
			}
		}
		return false
	case " UNDER ":
		return core.IsDirectChild(event.Subject, unquote(right))
	case " DESCENDANTS ":
		return core.IsStrictDescendant(event.Subject, unquote(right))
	case " BETWEEN ":
		bounds := conditionParts(right, " AND ")
		if len(bounds) != 2 {
			return false
		}
		return valueBetween(leftValue, parseLiteral(bounds[0]), parseLiteral(bounds[1]))
	}
	return compareOrdered(leftValue, operator, parseLiteral(right))
}

// resolveFieldPath returns the value of a field path such as e.subject,
// e.data.address.city, a literal or a projected field name.
func resolveFieldPath(event *core.Event, field, alias string) Value {
	field = strings.TrimSpace(field)
	if strings.HasPrefix(field, "'") && strings.HasSuffix(field, "'") {
		return strings.Trim(field, "'")
	}
	if strings.HasPrefix(field, "\"") && strings.HasSuffix(field, "\"") {
		return strings.Trim(field, "\"")
	}
	if number, ok := parseRational(field); ok {
		return ratValue(number)
	}
	switch field {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	switch field {
	case alias + ".id":
		return event.ID
	case alias + ".subject":
		return event.Subject
	case alias + ".type":
		return event.Type
	case alias + ".source":
		return event.Source
	case alias + ".time":
		return event.Time
	case alias + ".specversion":
		return event.SpecVersion
	case alias + ".data":
		return decodeEventData(event)
	}
	// Projected results of a subquery keep their fields at the top level.
	if strings.HasPrefix(field, alias+".") {
		if data := decodeEventData(event); data != nil {
			if value, exists := data[strings.TrimPrefix(field, alias+".")]; exists {
				return value
			}
		}
	}
	if strings.HasPrefix(field, alias+".data.") {
		path := strings.TrimPrefix(field, alias+".data.")
		data := decodeEventData(event)
		if data == nil {
			return nil
		}
		if value, exists := data[path]; exists {
			return value
		}
		var current Value = data
		for _, segment := range strings.Split(path, ".") {
			object, isObject := current.(map[string]any)
			if !isObject {
				return nil
			}
			current = object[segment]
		}
		return current
	}
	return nil
}

// decodeEventData decodes a payload with exact numbers: every JSON number
// is a json.Number, so large integers keep their digits. The result encodes
// back to the same JSON text.
func decodeEventData(event *core.Event) map[string]any {
	var data map[string]any
	decoder := json.NewDecoder(bytes.NewReader(event.Data))
	decoder.UseNumber()
	if err := decoder.Decode(&data); err != nil {
		return nil
	}
	return data
}

// formatValue renders a value as text for projection, grouping and text
// comparisons. nil renders as "<nil>", which never equals a literal. Times
// render in RFC 3339 with nanoseconds, numbers with their original digits.
func formatValue(value Value) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case time.Time:
		return typed.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%v", value)
}

// sortKey is the precomputed ordering value of one event. Resolving a data
// field decodes the payload and parsing a number allocates a big.Rat, so
// both happen once per event here, not once per comparison.
type sortKey struct {
	number *big.Rat
	time   time.Time
	text   string
	kind   uint8 // 0 missing, 1 number, 2 time, 3 text
}

func newSortKey(value Value) sortKey {
	if value == nil {
		return sortKey{}
	}
	if number, ok := numberOf(value); ok {
		return sortKey{kind: 1, number: number}
	}
	if at, ok := value.(time.Time); ok {
		return sortKey{kind: 2, time: at}
	}
	return sortKey{kind: 3, text: formatValue(value)}
}

// compare orders two keys like compareValues orders their values: numbers
// with numbers exactly, times with times, everything else as text.
func (key sortKey) compare(other sortKey) int {
	if key.kind == 1 && other.kind == 1 {
		return key.number.Cmp(other.number)
	}
	if key.kind == 2 && other.kind == 2 {
		return key.time.Compare(other.time)
	}
	return strings.Compare(key.asText(), other.asText())
}

func (key sortKey) asText() string {
	switch key.kind {
	case 1:
		return formatValue(ratValue(key.number))
	case 2:
		return key.time.Format(time.RFC3339Nano)
	}
	return key.text
}

// sortEvents applies ORDER BY. Missing values sort last in ascending order.
// Numbers compare exactly, times chronologically, everything else as text.
// The ordering field is resolved once per event before sorting.
func sortEvents(events []*core.Event, ordering *Ordering, alias string) {
	keys := make([]sortKey, len(events))
	for position, event := range events {
		keys[position] = newSortKey(resolveFieldPath(event, ordering.Field, alias))
	}
	indexes := make([]int, len(events))
	for position := range indexes {
		indexes[position] = position
	}
	sort.SliceStable(indexes, func(i, j int) bool {
		first, second := keys[indexes[i]], keys[indexes[j]]
		if first.kind == 0 && second.kind == 0 {
			return false
		}
		if first.kind == 0 {
			return ordering.IsDescending
		}
		if second.kind == 0 {
			return !ordering.IsDescending
		}
		if ordering.IsDescending {
			return first.compare(second) > 0
		}
		return first.compare(second) < 0
	})
	sorted := make([]*core.Event, len(events))
	for position, index := range indexes {
		sorted[position] = events[index]
	}
	copy(events, sorted)
}

// resultsAsEvents turns subquery results back into events.
func resultsAsEvents(results any) ([]*core.Event, error) {
	switch typed := results.(type) {
	case []*core.Event:
		return typed, nil
	case []Row:
		events := make([]*core.Event, 0, len(typed))
		for _, row := range typed {
			event := &core.Event{}
			event.ID, _ = row["id"].(string)
			event.Subject, _ = row["subject"].(string)
			event.Type, _ = row["type"].(string)
			event.Source, _ = row["source"].(string)
			event.SpecVersion, _ = row["specversion"].(string)
			if text, isText := row["time"].(string); isText {
				if parsed, err := time.Parse(time.RFC3339, text); err == nil {
					event.Time = parsed
				}
			}
			data := map[string]any{}
			for key, value := range row {
				switch key {
				case "id", "subject", "type", "source", "time", "specversion":
				default:
					data[key] = value
				}
			}
			if object, isObject := row["data"].(map[string]any); isObject {
				data = object
			}
			if len(data) > 0 {
				if encoded, err := json.Marshal(data); err == nil {
					event.Data = encoded
				}
			}
			events = append(events, event)
		}
		return events, nil
	}
	return nil, fmt.Errorf("unsupported result type for conversion to events: %T", results)
}
