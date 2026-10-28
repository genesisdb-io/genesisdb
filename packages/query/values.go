package query

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// Typed value handling for GDBQL. Comparisons are typed: a JSON string never
// equals a number or a boolean, and numbers are compared exactly, so
// integers beyond 2^53 are not rounded. Payloads are decoded with
// json.Number to keep the original digits.

// literal is a parsed right-hand side of a comparison.
type literal struct {
	kind   literalKind
	text   string
	number *big.Rat
	isTrue bool
}

type literalKind int

const (
	literalText literalKind = iota
	literalNumber
	literalBool
	literalNull
	// literalBare is an unquoted token that is neither a number, a boolean
	// nor null. For compatibility it compares as text.
	literalBare
)

// parseLiteral classifies a comparison operand that is not a field path.
func parseLiteral(text string) literal {
	text = strings.TrimSpace(text)
	if len(text) >= 2 && (text[0] == '\'' || text[0] == '"') && text[len(text)-1] == text[0] {
		return literal{kind: literalText, text: unquote(text)}
	}
	switch text {
	case "true":
		return literal{kind: literalBool, isTrue: true}
	case "false":
		return literal{kind: literalBool}
	case "null":
		return literal{kind: literalNull}
	}
	if number, ok := parseRational(text); ok {
		return literal{kind: literalNumber, number: number, text: text}
	}
	return literal{kind: literalBare, text: text}
}

// parseRational parses a decimal number exactly. Text that strconv would
// not accept as a float is rejected, so "0x10" or "Inf" are not numbers.
func parseRational(text string) (*big.Rat, bool) {
	if text == "" {
		return nil, false
	}
	if _, err := strconv.ParseFloat(text, 64); err != nil {
		// Out of range values are still decimal numbers.
		numError, isNumError := err.(*strconv.NumError)
		if !isNumError || numError.Err != strconv.ErrRange {
			return nil, false
		}
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "inf") || strings.Contains(lower, "nan") || strings.Contains(lower, "x") || strings.Contains(lower, "_") {
		return nil, false
	}
	number, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, false
	}
	return number, true
}

// numberOf returns the exact numeric value of a decoded value, if it is a
// number. Strings are not numbers.
func numberOf(value Value) (*big.Rat, bool) {
	switch typed := value.(type) {
	case json.Number:
		return parseRational(typed.String())
	case int:
		return new(big.Rat).SetInt64(int64(typed)), true
	case int64:
		return new(big.Rat).SetInt64(typed), true
	case float64:
		rational := new(big.Rat)
		if rational.SetFloat64(typed) == nil {
			return nil, false
		}
		return rational, true
	}
	return nil, false
}

// valueEquals compares a decoded field value with a literal. Types must
// match: "1" is not 1, true is not "true". A bare literal compares as text
// with the rendered value, which keeps unquoted identifiers working.
func valueEquals(value Value, right literal) bool {
	switch right.kind {
	case literalNull:
		return value == nil
	case literalBool:
		flag, isBool := value.(bool)
		return isBool && flag == right.isTrue
	case literalNumber:
		number, isNumber := numberOf(value)
		return isNumber && number.Cmp(right.number) == 0
	case literalText:
		switch typed := value.(type) {
		case string:
			return typed == right.text
		case time.Time:
			return formatValue(typed) == right.text
		}
		return false
	}
	if value == nil {
		return false
	}
	return formatValue(value) == right.text
}

// compareOrdered applies >, <, >= or <= between a value and a literal.
// Numbers compare exactly, text compares lexically, other combinations never
// match.
func compareOrdered(value Value, operator string, right literal) bool {
	var ordering int
	switch right.kind {
	case literalNumber:
		number, isNumber := numberOf(value)
		if !isNumber {
			return false
		}
		ordering = number.Cmp(right.number)
	case literalText, literalBare:
		text, isText := value.(string)
		if moment, isTime := value.(time.Time); isTime {
			text, isText = formatValue(moment), true
		}
		if !isText {
			return false
		}
		ordering = strings.Compare(text, right.text)
	default:
		return false
	}
	switch operator {
	case ">":
		return ordering > 0
	case "<":
		return ordering < 0
	case ">=":
		return ordering >= 0
	case "<=":
		return ordering <= 0
	}
	return false
}

// valueBetween implements BETWEEN with the same typing rules as
// compareOrdered.
func valueBetween(value Value, lower, upper literal) bool {
	return compareOrdered(value, ">=", lower) && compareOrdered(value, "<=", upper)
}

// compareValues orders two decoded values for ORDER BY and MIN/MAX: numbers
// numerically, times chronologically, otherwise by rendered text. Mixed
// types fall back to text.
func compareValues(first, second Value) int {
	if firstNumber, ok := numberOf(first); ok {
		if secondNumber, ok := numberOf(second); ok {
			return firstNumber.Cmp(secondNumber)
		}
	}
	if firstTime, ok := first.(time.Time); ok {
		if secondTime, ok := second.(time.Time); ok {
			return firstTime.Compare(secondTime)
		}
	}
	return strings.Compare(formatValue(first), formatValue(second))
}

// ratValue renders an exact number as the narrowest Go value: int for
// integers that fit, float64 otherwise.
func ratValue(number *big.Rat) Value {
	if number.IsInt() && number.Num().IsInt64() {
		value := number.Num().Int64()
		if int64(int(value)) == value {
			return int(value)
		}
		return value
	}
	result, _ := number.Float64()
	return result
}
