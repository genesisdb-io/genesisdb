package query

import (
	"strings"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// Plan is a safe superset of the events that can satisfy a filter. Execution
// always applies the full filter afterwards.
type Plan struct {
	selector core.EventSelector
	// cost ranks selectivity, lower is better.
	cost int
}

// Selector returns the index selection of the plan.
func (plan Plan) Selector() core.EventSelector { return plan.selector }

var planSelectAll = Plan{selector: core.EventSelector{All: true}, cost: 1 << 30}

func (plan Plan) union(other Plan) Plan {
	if plan.selector.All || other.selector.All {
		return planSelectAll
	}
	return Plan{
		selector: core.EventSelector{
			ExactSubjects:           append(append([]string{}, plan.selector.ExactSubjects...), other.selector.ExactSubjects...),
			SubjectsWithDescendants: append(append([]string{}, plan.selector.SubjectsWithDescendants...), other.selector.SubjectsWithDescendants...),
			EventTypes:              append(append([]string{}, plan.selector.EventTypes...), other.selector.EventTypes...),
		},
		cost: plan.cost + other.cost,
	}
}

// PlanQuery derives the index selection of a query.
func PlanQuery(q *Query) Plan {
	if q.Filter == nil {
		return planSelectAll
	}
	return planCondition(q.Filter.Condition, q.Alias())
}

// planCondition mirrors evaluateCondition with OR split before AND and
// quote-aware operators, so the plan
// is always a superset of the final matches:
//   - AND picks the most selective branch that can be narrowed
//   - OR unions all branches and selects everything if one branch cannot be
//     narrowed
//   - anything else (data predicates, NOT, unknown syntax) selects all.
func planCondition(condition, alias string) Plan {
	condition = strings.TrimSpace(condition)
	if condition == "" {
		return planSelectAll
	}
	if hasTopLevelAnd(condition) && !hasTopLevelOr(condition) {
		best := planSelectAll
		for _, part := range splitConditionTopLevel(condition, " AND ") {
			candidate := planCondition(part, alias)
			if !candidate.selector.All && candidate.cost < best.cost {
				best = candidate
			}
		}
		return best
	}
	if hasTopLevelOr(condition) {
		var combined Plan
		for position, part := range splitConditionTopLevel(condition, " OR ") {
			candidate := planCondition(part, alias)
			if candidate.selector.All {
				return planSelectAll
			}
			if position == 0 {
				combined = candidate
			} else {
				combined = combined.union(candidate)
			}
		}
		return combined
	}
	left, operator, right, ok := splitComparison(condition)
	if !ok {
		return planSelectAll
	}
	subjectField, typeField := alias+".subject", alias+".type"
	switch {
	case operator == "==" && left == subjectField:
		return Plan{selector: core.EventSelector{ExactSubjects: []string{unquote(right)}}, cost: 1}
	case (operator == " UNDER " || operator == " DESCENDANTS ") && left == subjectField:
		parent := unquote(right)
		if parent == "" {
			// No ancestor key is ever indexed for "".
			return planSelectAll
		}
		if parent == "/" {
			// Every subject starts with "/", so the root key selects the
			// whole store. Ranked like a full selection, any other indexed
			// branch of an AND is chosen instead.
			return Plan{selector: core.EventSelector{SubjectsWithDescendants: []string{parent}}, cost: planSelectAll.cost - 1}
		}
		return Plan{selector: core.EventSelector{SubjectsWithDescendants: []string{parent}}, cost: 2}
	case operator == "==" && left == typeField:
		return Plan{selector: core.EventSelector{EventTypes: []string{unquote(right)}}, cost: 3}
	case operator == " IN " && left == subjectField:
		values := splitValueList(right)
		return Plan{selector: core.EventSelector{ExactSubjects: values}, cost: 1 + len(values)}
	case operator == " IN " && left == typeField:
		values := splitValueList(right)
		return Plan{selector: core.EventSelector{EventTypes: values}, cost: 3 + len(values)}
	}
	return planSelectAll
}
