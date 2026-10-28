package query

import (
	"math/big"
	"strings"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// groupEvents applies GROUP BY, the projection per group and HAVING. Groups
// are returned in the order of their first event.
func groupEvents(events []*core.Event, q *Query, alias string) []Row {
	var groupKeys []string
	groups := map[string][]*core.Event{}
	for _, event := range events {
		key := formatValue(resolveFieldPath(event, q.Grouping.Field, alias))
		if _, exists := groups[key]; !exists {
			groupKeys = append(groupKeys, key)
		}
		groups[key] = append(groups[key], event)
	}
	rows := make([]Row, 0, len(groupKeys))
	for _, key := range groupKeys {
		members := groups[key]
		row := Row{q.Grouping.Field: key}
		for name, field := range q.Projection.Fields {
			if field.IsAggregate {
				row[name] = computeAggregate(field.Expression, members, alias)
			} else if len(members) > 0 {
				row[name] = resolveFieldPath(members[0], field.Expression, alias)
			}
		}
		if q.Grouping.Having != "" && !matchesHaving(row, members, q.Grouping.Having, alias) {
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

// matchesHaving evaluates a HAVING condition such as COUNT() > 10. A
// condition that cannot be parsed lets the group through.
func matchesHaving(row Row, members []*core.Event, condition, alias string) bool {
	for _, operator := range []string{"!=", "==", ">=", "<=", ">", "<"} {
		if !strings.Contains(condition, operator) {
			continue
		}
		parts := strings.SplitN(condition, operator, 2)
		if len(parts) != 2 {
			continue
		}
		leftExpression := strings.TrimSpace(parts[0])
		upper := strings.ToUpper(leftExpression)
		var left Value
		switch {
		case strings.Contains(upper, "COUNT()"):
			left = len(members)
		case strings.HasPrefix(upper, "SUM("), strings.HasPrefix(upper, "AVG("), strings.HasPrefix(upper, "MIN("), strings.HasPrefix(upper, "MAX("):
			left = computeAggregate(leftExpression, members, alias)
		default:
			if value, exists := row[leftExpression]; exists {
				left = value
			} else if strings.HasPrefix(leftExpression, alias+".") {
				if len(members) > 0 {
					left = resolveFieldPath(members[0], leftExpression, alias)
				}
			} else {
				left = leftExpression
			}
		}
		right := parseLiteral(parts[1])
		switch operator {
		case "==":
			return valueEquals(left, right)
		case "!=":
			return !valueEquals(left, right)
		}
		return compareOrdered(left, operator, right)
	}
	return true
}

// computeAggregate evaluates COUNT, SUM, AVG, MIN or MAX over events. Sums
// and averages are computed exactly and rendered as int when integral, so
// large integer amounts do not lose precision in the accumulator.
func computeAggregate(expression string, events []*core.Event, alias string) Value {
	original := strings.TrimSpace(expression)
	upper := strings.ToUpper(original)
	switch {
	case upper == "COUNT()", strings.HasPrefix(upper, "COUNT("):
		return len(events)
	case strings.HasPrefix(upper, "SUM("):
		field := aggregateArgument(original)
		sum := new(big.Rat)
		for _, event := range events {
			if number, ok := numberOf(resolveFieldPath(event, field, alias)); ok {
				sum.Add(sum, number)
			}
		}
		return aggregateNumber(sum)
	case strings.HasPrefix(upper, "AVG("):
		field := aggregateArgument(original)
		sum, count := new(big.Rat), 0
		for _, event := range events {
			if number, ok := numberOf(resolveFieldPath(event, field, alias)); ok {
				sum.Add(sum, number)
				count++
			}
		}
		if count > 0 {
			return aggregateNumber(sum.Quo(sum, new(big.Rat).SetInt64(int64(count))))
		}
		return 0.0
	case strings.HasPrefix(upper, "MIN("):
		return extremeValue(aggregateArgument(original), events, alias, func(candidate, current Value) bool { return compareValues(candidate, current) < 0 })
	case strings.HasPrefix(upper, "MAX("):
		return extremeValue(aggregateArgument(original), events, alias, func(candidate, current Value) bool { return compareValues(candidate, current) > 0 })
	}
	return nil
}

// aggregateNumber renders an exact aggregate: integral results as int,
// everything else as float64, which matches how JSON renders them.
func aggregateNumber(number *big.Rat) Value {
	if number.IsInt() {
		return ratValue(number)
	}
	result, _ := number.Float64()
	return result
}

// extremeValue implements MIN and MAX. When any value is a number, only
// numbers take part and are compared exactly. Otherwise values compare as
// text. Empty values are skipped.
func extremeValue(field string, events []*core.Event, alias string, isBetter func(candidate, current Value) bool) Value {
	if field == "" {
		return nil
	}
	var bestNumber, bestText Value
	for _, event := range events {
		value := resolveFieldPath(event, field, alias)
		if value == nil || formatValue(value) == "" {
			continue
		}
		if number, ok := numberOf(value); ok {
			candidate := aggregateNumber(number)
			if bestNumber == nil || isBetter(candidate, bestNumber) {
				bestNumber = candidate
			}
			continue
		}
		text := formatValue(value)
		if bestText == nil || isBetter(text, bestText) {
			bestText = text
		}
	}
	if bestNumber != nil {
		return bestNumber
	}
	return bestText
}

// aggregateArgument returns the argument of SUM(e.data.amount).
func aggregateArgument(expression string) string {
	start := strings.Index(expression, "(")
	end := strings.LastIndex(expression, ")")
	if start >= 0 && end > start {
		return strings.TrimSpace(expression[start+1 : end])
	}
	return ""
}
