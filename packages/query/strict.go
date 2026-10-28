package query

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Structural validation. The clause extraction in Parse is regular
// expression based and tolerates text it does not understand: a misspelled
// keyword such as MAPP or LIMT ends up inside the WHERE condition or is
// dropped, and the query silently returns a different result. checkStructure
// splits the query into its clauses first and rejects every word that does
// not belong to a clause, so a typo fails with a message instead.

// errUnexpectedStart reports text before the first clause. Without any
// source the 1.1.6 message is kept, see checkStructure.
var errUnexpectedStart = errors.New("invalid query: a query starts with STREAM or FROM")

// queryClause is one top level clause and its content.
type queryClause struct {
	keyword string
	content string
}

// clauseKeywords are the top level keywords, longest first. Multi-word
// keywords are matched word by word.
var clauseKeywords = [][]string{
	{"ORDER", "BY"}, {"GROUP", "BY"}, {"PROJECT", "INTO"},
	{"STREAM"}, {"FROM"}, {"WHERE"}, {"HAVING"}, {"LIMIT"}, {"TOP"}, {"MAP"},
}

// canonicalClause maps the LINQ-style spellings onto the STREAM form, so a
// clause cannot appear twice under two names.
var canonicalClause = map[string]string{"TOP": "LIMIT", "PROJECT INTO": "MAP"}

type maskedWord struct {
	upper      string
	start, end int
}

// topLevelWords splits the masked text at whitespace. Quoted text and
// parenthesized groups are masked, so they stay inside one word.
func topLevelWords(text, mask string) []maskedWord {
	var words []maskedWord
	for position := 0; position < len(mask); {
		for position < len(mask) && isSpace(mask[position]) {
			position++
		}
		start := position
		for position < len(mask) && !isSpace(mask[position]) {
			position++
		}
		if position > start {
			words = append(words, maskedWord{upper: upperASCII(text[start:position]), start: start, end: position})
		}
	}
	return words
}

// maskBraces hides the content of projection braces, so a field named
// like a keyword ({ limit : 3 }) does not start a clause.
func maskBraces(mask string) string {
	masked := []byte(mask)
	depth := 0
	for position, character := range masked {
		switch {
		case character == '{':
			depth++
		case character == '}' && depth > 0:
			depth--
		case depth > 0:
			masked[position] = '~'
		}
	}
	return string(masked)
}

func isSpace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\n' || character == '\r'
}

// splitClauses returns the clauses of a query in their order. Text before
// the first clause is an error.
func splitClauses(text string) ([]queryClause, error) {
	mask, err := clauseMask(text)
	if err != nil {
		return nil, err
	}
	words := topLevelWords(text, maskBraces(mask))
	var clauses []queryClause
	contentStart := -1
	wordsInClause := 0
	closeClause := func(end int) {
		if len(clauses) > 0 && contentStart >= 0 {
			clauses[len(clauses)-1].content = strings.TrimSpace(text[contentStart:end])
		}
	}
	for position := 0; position < len(words); {
		keyword, length := matchClauseKeyword(words[position:])
		// FROM inside "STREAM e FROM source" belongs to STREAM.
		if keyword == "FROM" && len(clauses) > 0 && clauses[len(clauses)-1].keyword == "STREAM" && wordsInClause == 1 {
			keyword = ""
		}
		if keyword == "" {
			if len(clauses) == 0 {
				return nil, errUnexpectedStart
			}
			wordsInClause++
			position++
			continue
		}
		wordsInClause = 0
		closeClause(words[position].start)
		if canonical, isAlias := canonicalClause[keyword]; isAlias {
			keyword = canonical
		}
		clauses = append(clauses, queryClause{keyword: keyword})
		contentStart = words[position+length-1].end
		position += length
	}
	closeClause(len(text))
	return clauses, nil
}

func matchClauseKeyword(words []maskedWord) (string, int) {
	for _, keyword := range clauseKeywords {
		if len(words) < len(keyword) {
			continue
		}
		matches := true
		for offset, part := range keyword {
			if words[offset].upper != part {
				matches = false
				break
			}
		}
		if matches {
			return strings.Join(keyword, " "), len(keyword)
		}
	}
	return "", 0
}

// checkStructure rejects queries with unknown words, repeated clauses or
// clause content that is not a single expression.
func checkStructure(text string) error {
	clauses, err := splitClauses(text)
	if errors.Is(err, errUnexpectedStart) {
		upper := upperASCII(text)
		if !strings.Contains(upper, "STREAM ") && !strings.Contains(upper, "FROM ") {
			return ErrMissingSource
		}
	}
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for position, clause := range clauses {
		if seen[clause.keyword] {
			return fmt.Errorf("invalid query: %s appears more than once", clause.keyword)
		}
		seen[clause.keyword] = true
		if clause.keyword == "STREAM" || clause.keyword == "FROM" {
			// LIMIT and TOP may precede the source, as in every earlier release.
			if position != 0 && !(position == 1 && clauses[0].keyword == "LIMIT") {
				return fmt.Errorf("invalid query: %s must start the query", clause.keyword)
			}
		}
		if err := checkClauseContent(clause); err != nil {
			return err
		}
	}
	if !seen["STREAM"] && !seen["FROM"] {
		return ErrMissingSource
	}
	if seen["HAVING"] && !seen["GROUP BY"] {
		return fmt.Errorf("invalid query: HAVING requires GROUP BY")
	}
	return nil
}

var (
	sourcePattern    = regexp.MustCompile(`(?s)^\S+\s+(?i:FROM|IN)\s+(events|\(.*\))$`)
	orderByPattern   = regexp.MustCompile(`^[\w.]+(\s+(?i:ASC|DESC))?$`)
	fieldPattern     = regexp.MustCompile(`^[\w.]+$`)
	limitPattern     = regexp.MustCompile(`^\d+$`)
	fieldNamePattern = regexp.MustCompile(`^[\p{L}\p{N}_.$-]+$`)
)

func checkClauseContent(clause queryClause) error {
	content := clause.content
	if content == "" {
		return fmt.Errorf("invalid query: %s without content", clause.keyword)
	}
	switch clause.keyword {
	case "STREAM", "FROM":
		if !sourcePattern.MatchString(content) {
			return fmt.Errorf("invalid query: expected STREAM alias FROM events or FROM alias IN events, have %q", clause.keyword+" "+content)
		}
	case "WHERE", "HAVING":
		return checkConditionOperands(clause.keyword, content)
	case "ORDER BY":
		if !orderByPattern.MatchString(content) {
			return fmt.Errorf("invalid query: ORDER BY expects one field and ASC or DESC, have %q", content)
		}
	case "GROUP BY":
		if !fieldPattern.MatchString(content) {
			return fmt.Errorf("invalid query: GROUP BY expects one field, have %q", content)
		}
	case "LIMIT":
		if !limitPattern.MatchString(content) {
			return fmt.Errorf("invalid query: LIMIT expects a number, have %q", content)
		}
	case "MAP":
		return checkProjection(content)
	}
	return nil
}

// checkConditionOperands requires every comparison of a condition to have
// single operands, so trailing words are not swallowed by a literal.
func checkConditionOperands(keyword, condition string) error {
	for _, operator := range []string{" OR ", " AND "} {
		if parts := conditionParts(condition, operator); len(parts) > 1 {
			for _, part := range parts {
				if err := checkConditionOperands(keyword, part); err != nil {
					return err
				}
			}
			return nil
		}
	}
	left, operator, right, ok := splitComparison(condition)
	if !ok {
		return fmt.Errorf("invalid %s condition %q", keyword, condition)
	}
	if !isSingleOperand(left) {
		return fmt.Errorf("invalid %s condition %q: unexpected text before the operator", keyword, condition)
	}
	if operator == " BETWEEN " {
		mask, _ := clauseMask(right)
		separator := strings.Index(upperASCII(mask), " AND ")
		if separator < 0 || !isSingleOperand(right[:separator]) || !isSingleOperand(right[separator+len(" AND "):]) {
			return fmt.Errorf("invalid %s condition %q: BETWEEN expects two values", keyword, condition)
		}
		return nil
	}
	if !isSingleOperand(right) {
		return fmt.Errorf("invalid %s condition %q: unexpected text after the value", keyword, condition)
	}
	return nil
}

// isSingleOperand reports whether text is one literal, field, aggregate call
// or parenthesized list, without further words.
func isSingleOperand(text string) bool {
	mask, err := clauseMask(strings.TrimSpace(text))
	return err == nil && mask != "" && !strings.ContainsAny(mask, " \t\r\n")
}

var expressionOperatorPattern = regexp.MustCompile(`\s*(==|!=|>=|<=|[-+*/<>])\s*`)

// checkProjection accepts MAP *, MAP { * }, MAP { name: expression, ... }
// and MAP expression.
func checkProjection(content string) error {
	if content == "*" {
		return nil
	}
	if strings.HasPrefix(content, "{") && strings.HasSuffix(content, "}") {
		inner := strings.TrimSpace(content[1 : len(content)-1])
		if inner == "*" {
			return nil
		}
		if inner == "" {
			return fmt.Errorf("invalid query: empty projection")
		}
		for _, field := range splitTopLevel(inner, ',') {
			field = strings.TrimSpace(field)
			if field == "" {
				return fmt.Errorf("invalid query: empty field in projection %q", content)
			}
			expression := field
			if name, value, hasName := cutTopLevelColon(field); hasName {
				if !fieldNamePattern.MatchString(name) {
					return fmt.Errorf("invalid query: invalid field name %q in projection", name)
				}
				expression = value
			}
			if !isSingleExpression(expression) {
				return fmt.Errorf("invalid query: invalid projection field %q", field)
			}
		}
		return nil
	}
	if !isSingleExpression(content) {
		return fmt.Errorf("invalid query: unexpected text in projection %q", content)
	}
	return nil
}

// isSingleExpression reports whether text is operands joined by operators,
// for example e.data.a + ' ' + e.data.b or COUNT() == 0.
func isSingleExpression(text string) bool {
	mask, err := clauseMask(strings.TrimSpace(text))
	if err != nil || mask == "" {
		return false
	}
	joined := expressionOperatorPattern.ReplaceAllString(mask, "$1")
	return !strings.ContainsAny(joined, " \t\r\n")
}

// cutTopLevelColon splits "name: expression" at the first colon outside
// quotes and parentheses.
func cutTopLevelColon(field string) (string, string, bool) {
	mask, err := clauseMask(field)
	if err != nil {
		return "", "", false
	}
	position := strings.IndexByte(mask, ':')
	if position < 0 {
		return "", "", false
	}
	return strings.TrimSpace(field[:position]), strings.TrimSpace(field[position+1:]), true
}
