package query

import (
	"fmt"
	"strings"
)

// upperASCII preserves byte offsets, including for UTF-8 and invalid input.
func upperASCII(text string) string {
	result := []byte(text)
	for i, character := range result {
		if character >= 'a' && character <= 'z' {
			result[i] = character - ('a' - 'A')
		}
	}
	return string(result)
}

// clauseMask preserves byte offsets while hiding quoted and nested text.
// Limits also bound recursive subquery planning and condition evaluation.
func clauseMask(text string) (string, error) {
	if len(text) > 64*1024 {
		return "", fmt.Errorf("query exceeds 64 KiB")
	}
	masked := []byte(text)
	var quote byte
	depth := 0
	for position := 0; position < len(text); position++ {
		character := text[position]
		if quote != 0 {
			masked[position] = '~'
			if character == '\\' && position+1 < len(text) {
				position++
				masked[position] = '~'
			} else if character == quote {
				quote = 0
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			masked[position] = '~'
			continue
		}
		if character == '(' {
			depth++
			if depth > 32 {
				return "", fmt.Errorf("query nesting exceeds 32 levels")
			}
		}
		if depth > 0 {
			masked[position] = '~'
		}
		if character == ')' {
			depth--
			if depth < 0 {
				return "", fmt.Errorf("unmatched closing parenthesis")
			}
		}
	}
	if quote != 0 || depth != 0 {
		return "", fmt.Errorf("unterminated string or parenthesis")
	}
	return string(masked), nil
}

// conditionParts only splits operators outside literals and parentheses.
// The first AND after BETWEEN belongs to its range, not to a conjunction.
func conditionParts(text, operator string) []string {
	masked, err := clauseMask(text)
	if err != nil {
		return []string{text}
	}
	masked = upperASCII(masked)
	var parts []string
	start := 0
	between := false
	for position := 0; position < len(masked); position++ {
		if strings.HasPrefix(masked[position:], " BETWEEN ") {
			between = true
		}
		if strings.HasPrefix(masked[position:], " AND ") && between {
			between = false
			position += len(" AND ") - 1
			continue
		}
		if strings.HasPrefix(masked[position:], operator) {
			parts = append(parts, strings.TrimSpace(text[start:position]))
			position += len(operator) - 1
			start = position + 1
		}
	}
	return append(parts, strings.TrimSpace(text[start:]))
}
