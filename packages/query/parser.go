package query

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Query is a parsed GDBQL query.
type Query struct {
	From       From
	Filter     *Filter
	Ordering   *Ordering
	Grouping   *Grouping
	Limit      *int
	Projection Projection
}

// From is the data source: the events stream or a subquery.
type From struct {
	// Alias is the handle of an event, for example e in "STREAM e FROM events".
	Alias string
	// Source is the name of the stream, normally "events".
	Source string
	// Subquery is set for FROM e IN (subquery).
	Subquery *Query
}

// Filter is the WHERE clause.
type Filter struct {
	Condition string
}

// Ordering is the ORDER BY clause.
type Ordering struct {
	Field        string
	IsDescending bool
}

// Grouping is the GROUP BY clause with its optional HAVING condition.
type Grouping struct {
	Field  string
	Having string
}

// Projection is the MAP or PROJECT INTO clause.
type Projection struct {
	// IsFullEvent returns whole events (MAP { * } or no projection).
	IsFullEvent bool
	// Fields maps the output name to its expression.
	Fields map[string]ProjectedField
}

// ProjectedField is one output field of a projection.
type ProjectedField struct {
	Expression   string
	IsAggregate  bool
	IsCalculated bool
}

// Alias returns the event handle, "e" by default.
func (q *Query) Alias() string {
	if q.From.Alias == "" {
		return "e"
	}
	return q.From.Alias
}

var (
	limitClausePattern      = regexp.MustCompile(`(?i)(TOP|LIMIT)\s+(\d+)(?:\s+|$)`)
	nextClausePattern       = regexp.MustCompile(`(?i)\s+(WHERE|ORDER\s+BY|GROUP\s+BY|PROJECT\s+INTO|MAP|$)`)
	whereClausePattern      = regexp.MustCompile(`(?i)WHERE\s+(.+?)(?:\s+ORDER\s+BY|\s+GROUP\s+BY|\s+PROJECT\s+INTO|\s+MAP|\s+TOP\s+|\s+LIMIT\s+|$)`)
	orderByClausePattern    = regexp.MustCompile(`(?i)ORDER\s+BY\s+([\w\.]+)(?:\s+(DESC|ASC))?`)
	groupByClausePattern    = regexp.MustCompile(`(?i)GROUP\s+BY\s+([\w\.]+)(?:\s+HAVING\s+(.+?))?(?:\s+ORDER\s+BY|\s+PROJECT\s+INTO|\s+MAP|$)`)
	projectionClausePattern = regexp.MustCompile(`(?i)(PROJECT\s+INTO|MAP)\s+(.+?)(?:\s+HAVING\s+|$)`)
	havingClausePattern     = regexp.MustCompile(`(?i)HAVING\s+(.+)$`)
)

// Parse parses GDBQL text. Two equivalent forms exist:
//
//	STREAM e FROM events [WHERE ...] [ORDER BY ...] [GROUP BY ... [HAVING ...]] [LIMIT n] MAP {...}
//	FROM e IN events     [WHERE ...] [ORDER BY ...] [GROUP BY ... [HAVING ...]] [TOP n]   PROJECT INTO {...}
//
// Every word must belong to a clause, a misspelled keyword fails with an
// error instead of changing the result, see strict.go.
func Parse(text string) (*Query, error) {
	mask, err := clauseMask(text)
	if err != nil {
		return nil, err
	}
	if err := checkStructure(text); err != nil {
		return nil, err
	}
	q := &Query{Projection: Projection{Fields: map[string]ProjectedField{}}}

	// LIMIT and TOP may appear anywhere, also before FROM.
	if match := limitClausePattern.FindStringSubmatchIndex(mask); match != nil {
		limit, err := strconv.Atoi(text[match[4]:match[5]])
		if err != nil {
			return nil, fmt.Errorf("invalid query limit: %w", err)
		}
		q.Limit = &limit
		text = text[:match[0]] + " " + text[match[1]:]
	}

	rest, err := parseFrom(q, text)
	if err != nil {
		return nil, err
	}
	text = rest
	mask, err = clauseMask(text)
	if err != nil {
		return nil, err
	}
	clause := func(pattern *regexp.Regexp) []string {
		indices := pattern.FindStringSubmatchIndex(mask)
		if indices == nil {
			return nil
		}
		values := make([]string, len(indices)/2)
		for position := range values {
			if indices[2*position] >= 0 {
				values[position] = text[indices[2*position]:indices[2*position+1]]
			}
		}
		return values
	}

	if match := clause(whereClausePattern); match != nil {
		q.Filter = &Filter{Condition: strings.TrimSpace(match[1])}
	}
	if match := clause(orderByClausePattern); match != nil {
		q.Ordering = &Ordering{Field: match[1], IsDescending: strings.ToUpper(match[2]) == "DESC"}
	}
	if match := clause(groupByClausePattern); match != nil {
		q.Grouping = &Grouping{Field: match[1]}
		if len(match) > 2 && match[2] != "" {
			q.Grouping.Having = strings.TrimSpace(match[2])
		}
	}
	if match := clause(projectionClausePattern); match != nil {
		parseProjection(&q.Projection, strings.TrimSpace(match[2]))
	}
	if q.Grouping != nil && q.Grouping.Having == "" {
		if match := clause(havingClausePattern); match != nil {
			q.Grouping.Having = strings.TrimSpace(match[1])
		}
	}
	if q.Filter != nil {
		if err := validateCondition(q.Filter.Condition); err != nil {
			return nil, err
		}
	}
	if q.Grouping != nil && q.Grouping.Having != "" {
		if _, _, _, ok := splitComparison(q.Grouping.Having); !ok {
			return nil, fmt.Errorf("invalid HAVING condition")
		}
	}
	return q, nil
}

// ErrMissingSource reports a query without FROM or STREAM.
var ErrMissingSource = errors.New("invalid query: missing FROM or STREAM clause")

// parseFrom parses the data source and returns the text without it.
func parseFrom(q *Query, text string) (string, error) {
	var clauseStart, sourceStart int
	mask, err := clauseMask(text)
	if err != nil {
		return "", err
	}
	upper := upperASCII(mask)
	if streamStart := strings.Index(upper, "STREAM "); streamStart != -1 {
		afterStream := text[streamStart+len("STREAM "):]
		words := strings.Fields(afterStream)
		if len(words) < 3 || strings.ToUpper(words[1]) != "FROM" {
			return "", errors.New("invalid STREAM clause format, expected: STREAM alias FROM source")
		}
		q.From.Alias = words[0]
		fromPosition := strings.Index(upperASCII(afterStream), " FROM ")
		if fromPosition == -1 {
			return "", errors.New("missing FROM keyword in STREAM clause")
		}
		sourceStart = streamStart + len("STREAM ") + fromPosition + len(" FROM ")
		clauseStart = streamStart
	} else {
		clauseStart = strings.Index(upper, "FROM ")
		if clauseStart == -1 {
			return "", ErrMissingSource
		}
		afterFrom := text[clauseStart+len("FROM "):]
		words := strings.Fields(afterFrom)
		if len(words) < 3 || strings.ToUpper(words[1]) != "IN" {
			return "", errors.New("invalid FROM clause format")
		}
		q.From.Alias = words[0]
		inPosition := strings.Index(upperASCII(afterFrom), " IN ")
		if inPosition == -1 {
			return "", errors.New("missing IN keyword in FROM clause")
		}
		sourceStart = clauseStart + len("FROM ") + inPosition + len(" IN ")
	}

	var source string
	var sourceEnd int
	if sourceStart < len(text) && text[sourceStart] == '(' {
		depth := 0
		var quote byte
		for position := sourceStart; position < len(text); position++ {
			if quote != 0 {
				if text[position] == '\\' && position+1 < len(text) {
					position++
				} else if text[position] == quote {
					quote = 0
				}
				continue
			}
			if text[position] == '\'' || text[position] == '"' {
				quote = text[position]
				continue
			}
			if text[position] == '(' {
				depth++
			} else if text[position] == ')' {
				depth--
				if depth == 0 {
					source = strings.TrimSpace(text[sourceStart : position+1])
					sourceEnd = position + 1
					break
				}
			}
		}
	} else if sourceStart <= len(text) {
		if match := nextClausePattern.FindStringIndex(text[sourceStart:]); match != nil {
			source = strings.TrimSpace(text[sourceStart : sourceStart+match[0]])
			sourceEnd = sourceStart + match[0]
		} else {
			source = strings.TrimSpace(text[sourceStart:])
			sourceEnd = len(text)
		}
	}
	rest := text[:clauseStart] + text[sourceEnd:]

	if strings.HasPrefix(source, "(") && strings.HasSuffix(source, ")") {
		subquery, err := Parse(strings.TrimSpace(source[1 : len(source)-1]))
		if err != nil {
			return "", fmt.Errorf("failed to parse subquery: %w", err)
		}
		q.From.Subquery = subquery
	} else {
		if source != "events" {
			return "", fmt.Errorf("unsupported query source %q", source)
		}
		q.From.Source = source
	}
	return rest, nil
}

func parseProjection(projection *Projection, clause string) {
	switch {
	case clause == "*" || clause == "{ * }":
		projection.IsFullEvent = true
	case strings.HasPrefix(clause, "{") && strings.HasSuffix(clause, "}"):
		fields := strings.TrimSpace(clause[1 : len(clause)-1])
		if isAggregateCall(fields) {
			projection.Fields["result"] = ProjectedField{Expression: fields, IsAggregate: true}
			return
		}
		for _, field := range splitTopLevel(fields, ',') {
			field = strings.TrimSpace(field)
			if field == "" {
				continue
			}
			name, expression := "", ""
			if parts := strings.SplitN(field, ":", 2); len(parts) == 2 {
				name, expression = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			} else {
				expression = strings.TrimSpace(parts[0])
				name = defaultFieldAlias(expression)
			}
			projection.Fields[name] = ProjectedField{
				Expression:   expression,
				IsAggregate:  isAggregateCall(expression),
				IsCalculated: isComputedExpression(expression),
			}
		}
	case isAggregateCall(clause):
		projection.Fields["result"] = ProjectedField{Expression: clause, IsAggregate: true}
	default:
		projection.Fields[defaultFieldAlias(clause)] = ProjectedField{Expression: clause}
	}
}

// isAggregateCall reports whether an expression starts with an aggregate
// function call.
func isAggregateCall(expression string) bool {
	expression = strings.ToUpper(strings.TrimSpace(expression))
	return strings.HasPrefix(expression, "COUNT(") || strings.HasPrefix(expression, "SUM(") ||
		strings.HasPrefix(expression, "AVG(") || strings.HasPrefix(expression, "MIN(") ||
		strings.HasPrefix(expression, "MAX(")
}

// isComputedExpression reports whether an expression contains an arithmetic
// or concatenation operator.
func isComputedExpression(expression string) bool {
	return strings.ContainsAny(expression, "+-*/")
}

// defaultFieldAlias names an unnamed projection field after the last path
// segment of its expression.
func defaultFieldAlias(expression string) string {
	if strings.Contains(expression, ".") {
		segments := strings.Split(expression, ".")
		return segments[len(segments)-1]
	}
	return expression
}
