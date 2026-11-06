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
	if _, err := core.NewSigningKeySet(backup.SigningKeys); err != nil {
		return fmt.Errorf("%w: %v", core.ErrInvalidBackup, err)
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
//
// Signed events are checked before anything is written, see
// checkBackupSignatures. A signature is carried over only when the restored
// event has exactly the hash it signed. Every record is encoded before the
// first write, so a rejected backup leaves the logs untouched.
func (engine *Engine) WriteBackup(backup *core.Backup, signingKeys core.SigningKeySet) error {
	if err := engine.checkHealthy(); err != nil {
		return err
	}
	if err := ValidateBackupSignatures(backup, signingKeys); err != nil {
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

	var systemEventBuffer []byte
	previousSystem := core.ZeroHash
	for _, original := range backup.SystemEvents {
		event := original.Clone()
		event.PredecessorHash = previousSystem
		event.HashVersion = 0
		if engine.format == layout.FormatCurrent {
			event.HashVersion = core.CurrentHashVersion
		}
		rehashForRestore(event, original.Hash)
		previousSystem = event.Hash
		line, err := EncodeEventLine(event)
		if err != nil {
			return err
		}
		systemEventBuffer = append(append(systemEventBuffer, line...), '\n')
	}

	ordered := make([]*core.Event, len(backup.Events))
	for position, event := range backup.Events {
		ordered[position] = event.Clone()
	}
	predecessorHash := core.ZeroHash
	var eventBuffer []byte
	for position, event := range ordered {
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
		rehashForRestore(event, backup.Events[position].Hash)
		predecessorHash = event.Hash
		line, err := EncodeEventLine(event)
		if err != nil {
			return err
		}
		eventBuffer = append(append(eventBuffer, line...), '\n')
	}
	if len(referenceDataBuffer) > 0 {
		if err := engine.appendToLog(engine.referenceDataLogPath(), referenceDataBuffer); err != nil {
			return err
		}
	}
	if len(systemEventBuffer) > 0 {
		if err := engine.appendToLog(engine.systemEventLogPath(), systemEventBuffer); err != nil {
			return err
		}
	}
	if len(eventBuffer) > 0 {
		if err := engine.appendToLog(engine.mainEventLogPath(), eventBuffer); err != nil {
			return err
		}
	}
	return engine.loadMainEventLogState()
}

// ValidateBackupSignatures checks every signed event and system event of a
// backup against keys, see checkBackupSignatures. It writes nothing.
func ValidateBackupSignatures(backup *core.Backup, keys core.SigningKeySet) error {
	if err := checkBackupSignatures(backup.Events, keys, "event"); err != nil {
		return err
	}
	return checkBackupSignatures(backup.SystemEvents, keys, "system event")
}

// checkBackupSignatures rejects a backup whose signed events were changed
// after they were signed. For every signed event, its hash must match its
// content and its predecessor in the backup, and the signature must verify
// with one of keys. Together this proves that each signed event, and the
// history before it, is the one the key signed. Unsigned events are not
// checked, a restore never signs them.
func checkBackupSignatures(events []*core.Event, keys core.SigningKeySet, kind string) error {
	predecessorHash := core.ZeroHash
	for position, event := range events {
		if event.Signature != "" {
			switch {
			case !core.HasValidHash(event):
				return fmt.Errorf("%w: signed %s %d (%s) does not match its hash", core.ErrInvalidBackup, kind, position, event.ID)
			case event.PredecessorHash != predecessorHash:
				return fmt.Errorf("%w: signed %s %d (%s) does not follow its predecessor in the backup", core.ErrInvalidBackup, kind, position, event.ID)
			}
			switch keys.VerifySignature(event) {
			case core.SignatureUnknownKey:
				return fmt.Errorf("%w: %s %d (%s) is signed by an unknown key", core.ErrInvalidBackup, kind, position, event.ID)
			case core.SignatureInvalid:
				return fmt.Errorf("%w: %s %d (%s) has an invalid signature", core.ErrInvalidBackup, kind, position, event.ID)
			}
		}
		predecessorHash = event.Hash
	}
	return nil
}

// rehashForRestore recomputes the hash of a restored event. A signature
// stays when the recomputed hash equals the hash it signed. When the hash
// changes, the signature is dropped. The backup's signatures were verified
// before anything was written, so the change comes from the restore itself:
// legacy events earlier in the chain get the current hash version, which
// shifts every later predecessor hash, and directories kept in the legacy
// format cannot hold signatures at all. A restore never signs anything: it
// does not claim that a key committed history it did not sign.
func rehashForRestore(event *core.Event, originalHash string) {
	event.Hash = core.HashEvent(event)
	if event.Signature != "" && event.Hash != originalHash {
		event.Signature = ""
	}
}
