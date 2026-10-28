package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// System events are an isolated audit trail, for example of erasures. They
// have their own hash chain, are never indexed, cached or returned by reads,
// and are only exported in backups.

// AppendSystemEvent appends one system event. Writers must be serialized by
// the caller.
func (engine *Engine) AppendSystemEvent(eventType, subject string, data json.RawMessage) (*core.Event, error) {
	if err := engine.checkHealthy(); err != nil {
		return nil, err
	}
	event := &core.Event{
		Source:          core.SystemEventSource,
		Subject:         subject,
		Type:            eventType,
		SpecVersion:     core.EventSpecVersion,
		ID:              engine.newEventID(),
		Time:            engine.now(),
		DataContentType: core.EventDataContentType,
		Data:            data,
	}
	predecessorHash, err := engine.lastSystemEventHash()
	if err != nil {
		return nil, err
	}
	event.PredecessorHash = predecessorHash
	if engine.format == layout.FormatCurrent {
		event.HashVersion = core.CurrentHashVersion
	}
	event.Hash = core.HashEvent(event)

	line, err := EncodeEventLine(event)
	if err != nil {
		return nil, fmt.Errorf("failed to encode system event: %w", err)
	}
	if err := engine.appendToLog(engine.systemEventLogPath(), append(line, '\n')); err != nil {
		return nil, fmt.Errorf("failed to write system event: %w", err)
	}
	return event, nil
}

func (engine *Engine) lastSystemEventHash() (string, error) {
	events, err := engine.ListSystemEvents()
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return core.ZeroHash, nil
	}
	return events[len(events)-1].Hash, nil
}

// ListSystemEvents returns every system event in log order.
func (engine *Engine) ListSystemEvents() ([]*core.Event, error) {
	return engine.ListSystemEventsContext(context.Background())
}

func (engine *Engine) ListSystemEventsContext(ctx context.Context) ([]*core.Event, error) {
	file, err := os.Open(engine.systemEventLogPath())
	if err != nil {
		return nil, err
	}
	defer file.Close()
	events := []*core.Event{}
	expectsHeader := true
	limits := core.ReadLimitsFromContext(ctx)
	var bytes int64
	err = fsutil.ScanLines(file, 0, func(line []byte, offset int64, isTerminated bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !isTerminated {
			return fmt.Errorf("incomplete system record at offset %d", offset)
		}
		if expectsHeader {
			expectsHeader = false
			if !layout.IsLogHeader(line) {
				return fmt.Errorf("invalid system log header")
			}
			return nil
		}
		event, err := DecodeEventLine(line)
		if err != nil {
			return fmt.Errorf("invalid system event at offset %d: %w", offset, err)
		}
		bytes += int64(len(line))
		if (limits.MaxEvents > 0 && len(events) >= limits.MaxEvents) || (limits.MaxBytes > 0 && bytes > limits.MaxBytes) {
			return core.ErrResourceLimit
		}
		events = append(events, event)
		return nil
	})
	return events, err
}
