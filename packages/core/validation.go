package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ValidateNewEvent checks untrusted new events, not historical records.
func ValidateNewEvent(event *Event) error {
	if event == nil {
		return fmt.Errorf("%w: event is nil", ErrInvalidEvent)
	}
	if event.IsSystemEvent() {
		return ErrSystemEventNotAllowed
	}
	if strings.TrimSpace(event.Source) == "" || len(event.Source) > 1024 || strings.TrimSpace(event.Type) == "" || len(event.Type) > 1024 {
		return fmt.Errorf("%w: source and type must contain 1 to 1024 bytes", ErrInvalidEvent)
	}
	if !strings.HasPrefix(event.Subject, "/") || strings.TrimSpace(event.Subject) != event.Subject || len(event.Subject) > 4096 || strings.Count(event.Subject, "/") > 64 {
		return fmt.Errorf("%w: subject must start with /, have no surrounding whitespace, and fit in 4096 bytes and 64 segments", ErrInvalidEvent)
	}
	// json.Marshal replaces invalid UTF-8 in strings with U+FFFD. The hash is
	// computed over the original bytes, so an invalid string would be
	// persisted with a hash that never verifies and an index key that never
	// matches. Raw data passes through unchanged, but JSON is UTF-8 by
	// definition and clients must never receive invalid text.
	if !utf8.ValidString(event.Source) || !utf8.ValidString(event.Subject) || !utf8.ValidString(event.Type) || !utf8.Valid(event.Data) {
		return fmt.Errorf("%w: source, subject, type and data must be valid UTF-8", ErrInvalidEvent)
	}
	if len(event.Source)+len(event.Subject)+len(event.Type)+len(event.Data)+500 > 64*1024 || !json.Valid(event.Data) {
		return fmt.Errorf("%w: invalid JSON or event exceeds 64 KiB", ErrInvalidEvent)
	}
	if event.ReferenceDataKey != "" && event.ReferenceDataKey != ReferenceDataPending {
		return fmt.Errorf("%w: reference keys are assigned by storage", ErrInvalidEvent)
	}
	return nil
}
