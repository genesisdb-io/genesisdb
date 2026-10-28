// Package query is the engine of the GenesisDB Query Language (GDBQL).
//
//	STREAM e FROM events
//	WHERE e.subject UNDER '/customer' AND e.data.age > 18
//	ORDER BY e.time DESC
//	LIMIT 10
//	MAP { id: e.id, name: e.data.firstName + ' ' + e.data.lastName }
//
// Parse turns GDBQL text into a Query. Plan derives a core.EventSelector
// that selects a safe superset of the matching events through indexes.
// Execute runs a query against any core.EventSource, for example a
// *store.Store, or a source of your own.
//
// The package depends on the Go standard library and package core only.
package query

// Value is a value produced by GDBQL: a string, a number, a bool, nil, a
// time.Time, a map[string]any (event data) or a Row.
type Value = any

// Row is one projected result row.
type Row map[string]Value
