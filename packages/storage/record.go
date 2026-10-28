package storage

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// eventLine is the on-disk wrapper of one event record: {"event": {...}}.
type eventLine struct {
	Event *core.Event `json:"event"`
}

// EncodeEventLine encodes an event as one event log record without the
// trailing newline.
func EncodeEventLine(event *core.Event) ([]byte, error) {
	return json.Marshal(eventLine{Event: event})
}

// DecodeEventLine decodes one event log record.
func DecodeEventLine(line []byte) (*core.Event, error) {
	var record eventLine
	if err := json.Unmarshal(line, &record); err != nil {
		return nil, err
	}
	if record.Event == nil {
		return nil, errors.New("record has no event")
	}
	return record.Event, nil
}

// newCorruptionError wraps core.ErrEventLogCorrupt with details.
func newCorruptionError(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", core.ErrEventLogCorrupt, fmt.Sprintf(format, arguments...))
}

// ErrNoRecordAtLocation reports a record location that does not point to a
// complete event record.
var ErrNoRecordAtLocation = errors.New("no event record at location")
