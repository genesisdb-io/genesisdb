package query

import (
	"context"
	"fmt"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// EvaluatePrecondition runs the GDBQL query of an isQueryResultTrue
// precondition and interprets its result as a boolean. It implements
// core.GDBQLPreconditionEvaluator.
//
// Booleans are taken as they are, numbers are true when not zero, text is
// true for "true" and "1", row lists are true when not empty, and a single
// row is true unless its "result" field is false.
func EvaluatePrecondition(ctx context.Context, source core.EventSource, text string) (bool, error) {
	q, err := Parse(text)
	if err != nil {
		return false, fmt.Errorf("failed to parse query: %w", err)
	}
	result, err := Execute(ctx, source, q)
	if err != nil {
		return false, fmt.Errorf("failed to execute query: %w", err)
	}
	switch typed := result.(type) {
	case []Row:
		return len(typed) > 0, nil
	case Row:
		if value, exists := typed["result"]; exists {
			return isTruthy(value), nil
		}
		return true, nil
	case bool, int, int64, float64, string:
		return isTruthy(typed), nil
	}
	return false, fmt.Errorf("query result cannot be evaluated as boolean: %T", result)
}

// isTruthy interprets a scalar value as a boolean.
func isTruthy(value Value) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	case string:
		return typed == "true" || typed == "1"
	}
	return false
}
