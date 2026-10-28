package query

import (
	"strconv"
	"strings"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// project applies the projection to the filtered and ordered events.
func project(events []*core.Event, projection Projection, alias string) any {
	if projection.IsFullEvent || len(projection.Fields) == 0 {
		return events
	}
	if len(projection.Fields) == 1 {
		for _, field := range projection.Fields {
			if field.IsAggregate {
				result := computeAggregate(field.Expression, events, alias)
				if strings.Contains(field.Expression, "==") || strings.Contains(field.Expression, ">") || strings.Contains(field.Expression, "<") {
					return evaluateCountComparison(field.Expression, result)
				}
				return result
			}
		}
	}
	hasAggregate := false
	for _, field := range projection.Fields {
		if field.IsAggregate {
			hasAggregate = true
			break
		}
	}
	if hasAggregate {
		// Aggregates are computed once over all events. Plain fields take
		// the value of the first event.
		row := Row{}
		for name, field := range projection.Fields {
			if field.IsAggregate {
				row[name] = computeAggregate(field.Expression, events, alias)
			} else if len(events) > 0 {
				row[name] = resolveFieldPath(events[0], field.Expression, alias)
			}
		}
		return row
	}
	rows := make([]Row, 0, len(events))
	for _, event := range events {
		row := Row{}
		for name, field := range projection.Fields {
			if field.IsCalculated {
				row[name] = evaluateExpression(event, field.Expression, alias)
			} else {
				row[name] = resolveFieldPath(event, field.Expression, alias)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// evaluateCountComparison evaluates COUNT() == n, COUNT() > n and
// COUNT() < n. Every other comparison is false.
func evaluateCountComparison(expression string, count Value) bool {
	countValue, isCount := count.(int)
	for _, operator := range []string{"==", ">", "<"} {
		marker := "COUNT() " + operator + " "
		if !strings.Contains(expression, marker) {
			continue
		}
		parts := strings.Split(expression, marker)
		if len(parts) < 2 {
			return false
		}
		expected, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || !isCount {
			return false
		}
		switch operator {
		case "==":
			return countValue == expected
		case ">":
			return countValue > expected
		case "<":
			return countValue < expected
		}
	}
	return false
}

// evaluateExpression evaluates a computed projection field: string
// concatenation with quoted literals, or arithmetic on number literals.
func evaluateExpression(event *core.Event, expression, alias string) Value {
	if strings.Contains(expression, "+") && strings.Contains(expression, "'") {
		var builder strings.Builder
		for _, part := range strings.Split(expression, "+") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "'") && strings.HasSuffix(part, "'") {
				builder.WriteString(strings.Trim(part, "'"))
				continue
			}
			if value := resolveFieldPath(event, part, alias); value != nil {
				builder.WriteString(formatValue(value))
			}
		}
		return builder.String()
	}
	// Arithmetic is only evaluated between literals. An expression that
	// references a field of the event yields null, as in 1.1.6.
	if strings.ContainsAny(expression, "+-*/") && !strings.Contains(expression, "'") && !strings.Contains(expression, alias+".") {
		return evaluateArithmetic(expression)
	}
	return resolveFieldPath(event, expression, alias)
}

// evaluateArithmetic evaluates one binary operation on number literals, for
// example 2+3 or 10/4.
func evaluateArithmetic(expression string) Value {
	expression = strings.ReplaceAll(expression, " ", "")
	apply := func(operator string, operation func(left, right float64) (float64, bool)) (Value, bool) {
		if !strings.Contains(expression, operator) {
			return nil, false
		}
		parts := strings.Split(expression, operator)
		if len(parts) != 2 {
			return nil, false
		}
		left, leftErr := strconv.ParseFloat(parts[0], 64)
		right, rightErr := strconv.ParseFloat(parts[1], 64)
		if leftErr != nil || rightErr != nil {
			return nil, false
		}
		result, ok := operation(left, right)
		if !ok {
			return nil, false
		}
		return result, true
	}
	asInteger := func(value Value) Value {
		number := value.(float64)
		if number == float64(int(number)) {
			return int(number)
		}
		return number
	}
	if value, ok := apply("+", func(l, r float64) (float64, bool) { return l + r, true }); ok {
		return asInteger(value)
	}
	if !strings.HasPrefix(expression, "-") {
		if value, ok := apply("-", func(l, r float64) (float64, bool) { return l - r, true }); ok {
			return asInteger(value)
		}
	}
	if value, ok := apply("*", func(l, r float64) (float64, bool) { return l * r, true }); ok {
		return asInteger(value)
	}
	if value, ok := apply("/", func(l, r float64) (float64, bool) { return l / r, r != 0 }); ok {
		return value
	}
	if number, err := strconv.ParseFloat(expression, 64); err == nil {
		return asInteger(number)
	}
	return nil
}
