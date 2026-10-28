package storage

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// ValidateBackup checks a backup completely before anything is written, so a
// malformed backup never leaves a half restored data directory.
func ValidateBackup(backup *core.Backup) error {
	if backup == nil {
		return fmt.Errorf("%w: backup is nil", core.ErrInvalidBackup)
	}
	for name, content := range backup.Schemas {
		if name == "" || strings.ContainsAny(name, "/\\") || !strings.HasSuffix(name, ".json") || !json.Valid(content) {
			return fmt.Errorf("%w: invalid schema file %q", core.ErrInvalidBackup, name)
		}
	}
	referenceDataKeys := make(map[string]bool, len(backup.ReferenceData))
	for _, entry := range backup.ReferenceData {
		if entry != nil {
			referenceDataKeys[entry.Key] = true
		}
	}
	for position, event := range backup.Events {
		switch {
		case event == nil:
			return fmt.Errorf("%w: event %d is empty", core.ErrInvalidBackup, position)
		case event.IsSystemEvent():
			return fmt.Errorf("%w: event %d: %v", core.ErrInvalidBackup, position, core.ErrSystemEventNotAllowed)
		}
		// An event whose payload lives only in the reference data log must
		// come with its entry, otherwise every later read would be corrupt.
		if event.HasReferenceData() && !referenceDataKeys[event.ReferenceDataKey] && core.IsReferenceDataPlaceholder(event.Data) {
			return fmt.Errorf("%w: event %d (%s): reference data %s is missing from the backup", core.ErrInvalidBackup, position, event.ID, event.ReferenceDataKey)
		}
		if _, err := EncodeEventLine(event); err != nil {
			return fmt.Errorf("%w: event %d (%s): %v", core.ErrInvalidBackup, position, event.ID, err)
		}
	}
	for position, entry := range backup.ReferenceData {
		if entry == nil {
			return fmt.Errorf("%w: reference data entry %d is empty", core.ErrInvalidBackup, position)
		}
		if _, err := json.Marshal(entry); err != nil {
			return fmt.Errorf("%w: reference data entry %d: %v", core.ErrInvalidBackup, position, err)
		}
	}
	for position, event := range backup.SystemEvents {
		if event == nil {
			return fmt.Errorf("%w: system event %d is empty", core.ErrInvalidBackup, position)
		}
		if _, err := EncodeEventLine(event); err != nil {
			return fmt.Errorf("%w: system event %d: %v", core.ErrInvalidBackup, position, err)
		}
	}
	return nil
}

// WriteBackup appends a validated backup to empty logs. Event IDs and times
// are preserved. Events keep the order of the backup, which a GenesisDB
// backup exports in commit order, and their hash chain is recomputed.
// Timestamps are not used for ordering: a clock correction in the source
// must not reorder a subject's history. Payloads that are stored as
// reference data are never written into the immutable event data.
func (engine *Engine) WriteBackup(backup *core.Backup) error {
	if err := engine.checkHealthy(); err != nil {
		return err
	}
	referenceDataKeys := map[string]bool{}
	var referenceDataBuffer []byte
	for _, entry := range backup.ReferenceData {
		referenceDataKeys[entry.Key] = true
		encoded, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		referenceDataBuffer = append(append(referenceDataBuffer, encoded...), '\n')
	}
	if len(referenceDataBuffer) > 0 {
		if err := engine.appendToLog(engine.referenceDataLogPath(), referenceDataBuffer); err != nil {
			return err
		}
	}

	var systemEventBuffer []byte
	previousSystem := core.ZeroHash
	for _, original := range backup.SystemEvents {
		event := original.Clone()
		event.PredecessorHash = previousSystem
		event.HashVersion = 0
		if engine.format == layout.FormatCurrent {
			event.HashVersion = core.CurrentHashVersion
		}
		event.Hash = core.HashEvent(event)
		previousSystem = event.Hash
		line, err := EncodeEventLine(event)
		if err != nil {
			return err
		}
		systemEventBuffer = append(append(systemEventBuffer, line...), '\n')
	}
	if len(systemEventBuffer) > 0 {
		if err := engine.appendToLog(engine.systemEventLogPath(), systemEventBuffer); err != nil {
			return err
		}
	}

	ordered := make([]*core.Event, len(backup.Events))
	for position, event := range backup.Events {
		ordered[position] = event.Clone()
	}
	predecessorHash := core.ZeroHash
	var eventBuffer []byte
	for _, event := range ordered {
		event.SpecVersion = core.EventSpecVersion
		event.DataContentType = core.EventDataContentType
		if event.HasReferenceData() && referenceDataKeys[event.ReferenceDataKey] {
			event.Data = core.EmptyEventData()
		}
		event.PredecessorHash = predecessorHash
		event.HashVersion = 0
		if engine.format == layout.FormatCurrent {
			event.HashVersion = core.CurrentHashVersion
		}
		event.Hash = core.HashEvent(event)
		predecessorHash = event.Hash
		line, err := EncodeEventLine(event)
		if err != nil {
			return err
		}
		eventBuffer = append(append(eventBuffer, line...), '\n')
	}
	if len(eventBuffer) > 0 {
		if err := engine.appendToLog(engine.mainEventLogPath(), eventBuffer); err != nil {
			return err
		}
	}
	return engine.loadMainEventLogState()
}
