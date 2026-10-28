package query

import "strings"

// splitTopLevel splits outside quoted strings and nested expressions.
func splitTopLevel(text string, separator rune) []string {
	var parts []string
	start, depth := 0, 0
	var quote byte
	for position := 0; position < len(text); position++ {
		character := text[position]
		if quote != 0 {
			if character == '\\' && position+1 < len(text) {
				position++
			} else if character == quote {
				quote = 0
			}
			continue
		}
		switch character {
		case '\'', '"':
			quote = character
		case '(', '{', '[':
			depth++
		case ')', '}', ']':
			depth--
		default:
			if depth == 0 && rune(character) == separator {
				parts = append(parts, text[start:position])
				start = position + 1
			}
		}
	}
	return append(parts, text[start:])
}

func splitConditionTopLevel(condition, separator string) []string {
	return conditionParts(condition, separator)
}

var comparisonOperators = []string{" BETWEEN ", " DESCENDANTS ", " UNDER ", " IN ", "!=", "==", ">=", "<=", ">", "<"}

func splitComparison(condition string) (left, operator, right string, ok bool) {
	mask, err := clauseMask(condition)
	if err != nil {
		return "", "", "", false
	}
	for _, candidate := range comparisonOperators {
		position := strings.Index(upperASCII(mask), candidate)
		if position >= 0 {
			return strings.TrimSpace(condition[:position]), candidate, strings.TrimSpace(condition[position+len(candidate):]), true
		}
	}
	return "", "", "", false
}

func unquote(text string) string {
	text = strings.TrimSpace(text)
	if len(text) >= 2 && (text[0] == '\'' || text[0] == '"') && text[len(text)-1] == text[0] {
		text = text[1 : len(text)-1]
		text = strings.ReplaceAll(text, "\\'", "'")
		text = strings.ReplaceAll(text, `\"`, `"`)
		text = strings.ReplaceAll(text, `\\`, `\`)
	}
	return text
}

func splitValueList(text string) []string {
	var values []string
	for _, value := range splitTopLevel(strings.Trim(strings.TrimSpace(text), "()"), ',') {
		values = append(values, unquote(value))
	}
	return values
}

func hasTopLevelAnd(condition string) bool { return len(conditionParts(condition, " AND ")) > 1 }
func hasTopLevelOr(condition string) bool  { return len(conditionParts(condition, " OR ")) > 1 }
