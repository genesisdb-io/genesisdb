package core

import (
	"encoding/json"
	"fmt"
)

// Precondition types understood by commits.
const (
	// PreconditionIsSubjectNew requires that neither the subject nor any of
	// its descendants has events. Payload: {"subject": "/user/1"}.
	PreconditionIsSubjectNew = "isSubjectNew"

	// PreconditionIsSubjectExisting requires that the subject or one of its
	// descendants has events. Payload: {"subject": "/user/1"}.
	PreconditionIsSubjectExisting = "isSubjectExisting"

	// PreconditionIsQueryResultTrue requires that a GenesisDB Query Language
	// (GDBQL) query evaluates to true. Payload: {"query": "..."}.
	PreconditionIsQueryResultTrue = "isQueryResultTrue"
)

// Precondition is checked atomically with a commit. If it fails, nothing is
// written.
type Precondition struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// PreconditionFailedError reports a precondition that did not hold. Its JSON
// form is returned to HTTP clients unchanged.
type PreconditionFailedError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (e *PreconditionFailedError) Error() string {
	return fmt.Sprintf("precondition error (%s): %s", e.Type, e.Message)
}
