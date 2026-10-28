package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

// Backup is a logical backup: the raw events, the reference data and the
// system events. Index files are never part of a backup.
//
// Two JSON forms exist and both stay readable forever:
//   - plain: a JSON array of events, used when there is neither reference
//     data nor any system event
//   - extended: {"events": [...], "refs": [...], "system": [...]}.
type Backup struct {
	Events        []*Event              `json:"events"`
	ReferenceData []*ReferenceDataEntry `json:"refs,omitempty"`
	SystemEvents  []*Event              `json:"system,omitempty"`
	// Schemas contains exact schema file names and JSON contents.
	Schemas map[string]json.RawMessage `json:"schemas,omitempty"`

	// Extended selects the extended JSON form when the backup is encoded.
	Extended bool `json:"-"`
}

// EncodeJSON writes the backup in its plain or extended JSON form, followed
// by a newline.
func (b *Backup) EncodeJSON(w io.Writer) error {
	if b.Extended {
		return json.NewEncoder(w).Encode(b)
	}
	events := b.Events
	if events == nil {
		events = []*Event{}
	}
	return json.NewEncoder(w).Encode(events)
}

// DecodeBackup reads a backup in either JSON form.
func DecodeBackup(data []byte) (*Backup, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || (trimmed[0] != '[' && trimmed[0] != '{') {
		return nil, fmt.Errorf("%w: expected an event array or backup object", ErrInvalidBackup)
	}
	// encoding/json would replace invalid bytes with U+FFFD, and restore
	// recomputes the hash chain, so the corruption would become permanent.
	if !utf8.Valid(trimmed) {
		return nil, fmt.Errorf("%w: backup is not valid UTF-8", ErrInvalidBackup)
	}
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var backup Backup
		if err := json.Unmarshal(trimmed, &backup); err != nil {
			return nil, err
		}
		backup.Extended = true
		return &backup, nil
	}
	var events []*Event
	if err := json.Unmarshal(trimmed, &events); err != nil {
		return nil, err
	}
	return &Backup{Events: events}, nil
}

// String summarizes the backup content.
func (b *Backup) String() string {
	return fmt.Sprintf("%d events, %d reference data entries, %d system events", len(b.Events), len(b.ReferenceData), len(b.SystemEvents))
}
