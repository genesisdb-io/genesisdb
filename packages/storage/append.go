package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// ErrNothingWritten marks an append that failed before anything durable was
// written. Every other append error means the batch may already be durable
// and becomes visible after recovery.
var ErrNothingWritten = errors.New("nothing was written")

// AppendResult describes a durable append.
type AppendResult struct {
	// Events are the appended events with their record locations, in commit
	// order. Event.Data holds the persisted data: the placeholder "{}" for
	// payloads stored as reference data.
	Events []core.StoredEvent
	// ReferenceData maps an appended event index to its reference data.
	ReferenceData map[int]json.RawMessage
	// MainEventLogSize is the size of the main event log after the append.
	MainEventLogSize int64
}

// Append appends a batch of new events to the main event log. It
// assigns ID, time, spec version, content type, reference data key and the
// hash chain, then writes the write-ahead log, the reference data and the
// events, syncing each once. The batch is atomic: after a crash it is either
// completely present or completely absent.
//
// On Darwin the write-ahead log is ordered with F_BARRIERFSYNC and the logs
// use a plain fsync. The batch is
// durable once a later full flush returns, which the store issues for the
// index journal record of every commit. A standalone user of the engine that
// acknowledges after Append must call SyncDurable.
//
// Events are modified in place. Writers must be serialized by the caller.
func (engine *Engine) Append(ctx context.Context, events []*core.Event) (*AppendResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNothingWritten, err)
	}
	if err := engine.checkHealthy(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNothingWritten, err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("%w: empty batch", ErrNothingWritten)
	}
	for _, event := range events {
		if err := core.ValidateNewEvent(event); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrNothingWritten, err)
		}
	}
	walEntries := engine.prepareEvents(events)

	// From here on a failure may leave a durable batch. The engine then
	// refuses further writes until a restart has replayed the WAL, because
	// neither the in-memory log size nor the predecessor hash can be trusted.
	if err := engine.wal.AppendEntries(walEntries); err != nil {
		return nil, engine.markFailed(fmt.Errorf("failed to publish WAL (restart required): %w", err))
	}
	if err := engine.appendReferenceDataOfEntries(walEntries); err != nil {
		return nil, engine.markFailed(fmt.Errorf("failed to save reference data (WAL retained for recovery): %w", err))
	}
	stored, endOffset, err := engine.appendEventsToLog(walEntries)
	if err != nil {
		return nil, engine.markFailed(fmt.Errorf("failed to append events (WAL retained for recovery): %w", err))
	}
	engine.mainEventLogSize = endOffset
	engine.lastEventHash = walEntries[len(walEntries)-1].Event.Hash
	if err := engine.wal.Truncate(); err != nil {
		engine.logger.Warn("failed to clear WAL after a durable append; recovery is idempotent", "error", err)
	}

	result := &AppendResult{Events: stored, MainEventLogSize: endOffset, ReferenceData: map[int]json.RawMessage{}}
	for position, entry := range walEntries {
		if len(entry.ReferenceData) > 0 {
			result.ReferenceData[position] = entry.ReferenceData
		}
	}
	return result, nil
}

// prepareEvents assigns metadata and the hash chain and splits off reference
// data. The algorithm is part of the data format: the reference data key is
// the event hash computed before the key is set.
func (engine *Engine) prepareEvents(events []*core.Event) []WALEntry {
	entries := make([]WALEntry, 0, len(events))
	predecessorHash := engine.lastEventHash
	if predecessorHash == "" {
		predecessorHash = core.ZeroHash
	}
	for _, event := range events {
		event.HashVersion = 0
		event.SpecVersion = core.EventSpecVersion
		event.DataContentType = core.EventDataContentType
		event.Time = engine.now()
		event.ID = engine.newEventID()

		var referenceData json.RawMessage
		if event.Options.StoreDataAsReference || event.ReferenceDataKey == core.ReferenceDataPending {
			referenceData = event.Data
			event.Data = core.EmptyEventData()
		}
		if len(referenceData) > 0 {
			event.ReferenceDataKey = core.HashEvent(event)
		}
		event.PredecessorHash = predecessorHash
		if engine.format == layout.FormatCurrent {
			event.HashVersion = core.CurrentHashVersion
		}
		event.Hash = core.HashEvent(event)

		entries = append(entries, WALEntry{Event: event, ReferenceData: referenceData})
		predecessorHash = event.Hash
	}
	return entries
}

func (engine *Engine) appendReferenceDataOfEntries(entries []WALEntry) error {
	var buffer []byte
	for _, entry := range entries {
		if len(entry.ReferenceData) == 0 {
			continue
		}
		encoded, err := json.Marshal(core.ReferenceDataEntry{Key: entry.Event.ReferenceDataKey, Data: entry.ReferenceData, Timestamp: engine.now()})
		if err != nil {
			return err
		}
		buffer = append(append(buffer, encoded...), '\n')
	}
	if len(buffer) == 0 {
		return nil
	}
	return fsutil.AppendAndSyncBeforeBarrier(engine.referenceDataLogPath(), buffer, engine.faults)
}

// appendEventsToLog writes all events with one write at the known end of the
// main log and syncs once. The write-ahead log is already ordered before it,
// so a partial batch can always be completed by replay. The full device
// flush of the index journal record that publishes the batch makes the
// lines durable, a plain sync that hands them to the device is enough here.
func (engine *Engine) appendEventsToLog(entries []WALEntry) ([]core.StoredEvent, int64, error) {
	file, err := os.OpenFile(engine.mainEventLogPath(), os.O_RDWR, 0o600)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	startOffset := engine.mainEventLogSize
	if info.Size() != startOffset {
		return nil, 0, fmt.Errorf("event log size %d does not match the expected size %d", info.Size(), startOffset)
	}
	var buffer []byte
	stored := make([]core.StoredEvent, 0, len(entries))
	for _, entry := range entries {
		line, err := EncodeEventLine(entry.Event)
		if err != nil {
			return nil, 0, err
		}
		stored = append(stored, core.StoredEvent{
			Event:    entry.Event,
			Location: core.RecordLocation{LogFile: engine.MainEventLogFile(), Offset: startOffset + int64(len(buffer)), Length: int64(len(line))},
		})
		buffer = append(append(buffer, line...), '\n')
	}
	if err := engine.faults.Check("events.write"); err != nil {
		return nil, 0, err
	}
	if _, err := file.WriteAt(buffer, startOffset); err != nil {
		return nil, 0, err
	}
	if err := engine.faults.Check("events.sync"); err != nil {
		return nil, 0, err
	}
	if err := fsutil.SyncBeforeBarrier(file); err != nil {
		return nil, 0, err
	}
	return stored, startOffset + int64(len(buffer)), nil
}

// SyncDurable drains the device cache of the main event log, making every
// batch appended before durable. The store does not need it: its index
// journal record flushes the device after every commit.
func (engine *Engine) SyncDurable() error {
	file, err := os.OpenFile(engine.mainEventLogPath(), os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
