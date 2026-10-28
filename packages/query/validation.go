package query

import (
	"fmt"
	"strings"
)

// validateCondition rejects unsupported expressions rather than interpreting
// malformed preconditions as valid predicates with surprising results.
func validateCondition(condition string) error {
	for _, operator := range []string{" OR ", " AND "} {
		parts := conditionParts(condition, operator)
		if len(parts) > 1 {
			for _, part := range parts {
				if err := validateCondition(part); err != nil {
					return err
				}
			}
			return nil
		}
	}
	left, operator, right, ok := splitComparison(condition)
	if !ok || left == "" || right == "" {
		return fmt.Errorf("invalid WHERE condition %q", condition)
	}
	if operator == " BETWEEN " && !strings.Contains(strings.ToUpper(right), " AND ") {
		return fmt.Errorf("BETWEEN requires two bounds")
	}
	return nil
}
