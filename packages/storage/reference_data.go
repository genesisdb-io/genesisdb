package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// ScanReferenceData visits every entry of the reference data log in order.
// visit returns false to stop.
func (engine *Engine) ScanReferenceData(visit func(entry *core.ReferenceDataEntry) bool) error {
	return engine.scanReferenceDataContext(context.Background(), visit)
}

func (engine *Engine) scanReferenceDataContext(ctx context.Context, visit func(entry *core.ReferenceDataEntry) bool) error {
	file, err := os.Open(engine.referenceDataLogPath())
	if err != nil {
		return err
	}
	defer file.Close()
	logFile := filepath.Base(engine.referenceDataLogPath())
	expectsHeader := true
	err = fsutil.ScanLines(file, 0, func(line []byte, offset int64, isTerminated bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if expectsHeader {
			expectsHeader = false
			if !layout.IsLogHeader(line) {
				return newCorruptionError("%s has an invalid header", logFile)
			}
			return nil
		}
		if len(line) == 0 && isTerminated {
			return nil
		}
		if !isTerminated {
			return newCorruptionError("%s ends inside a record at offset %d", logFile, offset)
		}
		var entry core.ReferenceDataEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return newCorruptionError("invalid reference data entry in %s at offset %d: %v", logFile, offset, err)
		}
		if !visit(&entry) {
			return fsutil.ErrStopScan
		}
		return nil
	})
	if errors.Is(err, fsutil.ErrStopScan) {
		return nil
	}
	return err
}

// ListReferenceData returns every entry of the reference data log.
func (engine *Engine) ListReferenceData() ([]*core.ReferenceDataEntry, error) {
	return engine.ListReferenceDataContext(context.Background())
}

// ListReferenceDataContext is ListReferenceData observing cancellation and
// the read limits of ctx.
func (engine *Engine) ListReferenceDataContext(ctx context.Context) ([]*core.ReferenceDataEntry, error) {
	entries := []*core.ReferenceDataEntry{}
	limits := core.ReadLimitsFromContext(ctx)
	var bytes int64
	var limitErr error
	err := engine.scanReferenceDataContext(ctx, func(entry *core.ReferenceDataEntry) bool {
		bytes += int64(len(entry.Data) + len(entry.Key) + 128)
		if (limits.MaxEvents > 0 && len(entries) >= limits.MaxEvents) || (limits.MaxBytes > 0 && bytes > limits.MaxBytes) {
			limitErr = core.ErrResourceLimit
			return false
		}
		entries = append(entries, entry)
		return true
	})
	if err != nil {
		return nil, err
	}
	if limitErr != nil {
		return nil, limitErr
	}
	return entries, nil
}

// LoadReferenceData returns the payload stored under key, or core.ErrNotFound.
func (engine *Engine) LoadReferenceData(key string) (json.RawMessage, error) {
	var data json.RawMessage
	if err := engine.ScanReferenceData(func(entry *core.ReferenceDataEntry) bool {
		if entry.Key == key {
			data = entry.Data
			return false
		}
		return true
	}); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, fmt.Errorf("reference data %s: %w", key, core.ErrNotFound)
	}
	return data, nil
}

// ExistingReferenceDataKeys returns which of keys have an entry in the
// reference data log, in one streaming pass that never retains payloads.
// It observes cancellation of ctx.
func (engine *Engine) ExistingReferenceDataKeys(ctx context.Context, keys map[string]struct{}) (map[string]bool, error) {
	found := make(map[string]bool, len(keys))
	if len(keys) == 0 {
		return found, nil
	}
	remaining := len(keys)
	err := engine.scanReferenceDataContext(ctx, func(entry *core.ReferenceDataEntry) bool {
		if _, wanted := keys[entry.Key]; wanted && !found[entry.Key] {
			found[entry.Key] = true
			remaining--
		}
		return remaining > 0
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// LoadReferenceDataInto fills the nil values of payloads with the first
// matching entry of the reference data log. Keys without an entry stay nil.
func (engine *Engine) LoadReferenceDataInto(payloads map[string]json.RawMessage) error {
	return engine.LoadReferenceDataIntoContext(context.Background(), payloads)
}

func (engine *Engine) LoadReferenceDataIntoContext(ctx context.Context, payloads map[string]json.RawMessage) error {
	if len(payloads) == 0 {
		return nil
	}
	remaining := len(payloads)
	var size int64
	var limitErr error
	limits := core.ReadLimitsFromContext(ctx)
	err := engine.scanReferenceDataContext(ctx, func(entry *core.ReferenceDataEntry) bool {
		if value, wanted := payloads[entry.Key]; wanted && value == nil {
			size += int64(len(entry.Data))
			if limits.MaxBytes > 0 && size > limits.MaxBytes {
				limitErr = core.ErrResourceLimit
				return false
			}
			payloads[entry.Key] = entry.Data
			remaining--
		}
		return remaining > 0
	})
	if err != nil {
		return err
	}
	return limitErr
}

// ReplaceReferenceData replaces the payloads of several entries with one
// atomic rewrite of the reference data log and returns the keys that were
// found. Nothing is written if no key exists. beforeRename and afterRename
// bracket the moment the new file becomes visible, so callers can exclude
// readers for that moment only. Both may be nil.
func (engine *Engine) ReplaceReferenceData(updates map[string]json.RawMessage, beforeRename, afterRename func()) (map[string]bool, error) {
	return engine.ReplaceReferenceDataContext(context.Background(), updates, beforeRename, afterRename)
}

// ReplaceReferenceDataContext is ReplaceReferenceData observing ctx while
// the new file is written. The rename itself is not interruptible.
func (engine *Engine) ReplaceReferenceDataContext(ctx context.Context, updates map[string]json.RawMessage, beforeRename, afterRename func()) (map[string]bool, error) {
	replacedKeys := map[string]bool{}
	if len(updates) == 0 {
		return replacedKeys, nil
	}
	if err := engine.checkHealthy(); err != nil {
		return nil, err
	}
	now := engine.now()
	err := fsutil.ReplaceStreamAtomically(engine.referenceDataLogPath(), func(writer io.Writer) error {
		if _, err := writer.Write(engine.format.LogHeaderLine()); err != nil {
			return err
		}
		encoder := json.NewEncoder(writer)
		var writeErr error
		err := engine.scanReferenceDataContext(ctx, func(entry *core.ReferenceDataEntry) bool {
			if data, wanted := updates[entry.Key]; wanted {
				entry.Data = data
				entry.Timestamp = now
				replacedKeys[entry.Key] = true
			}
			writeErr = encoder.Encode(entry)
			return writeErr == nil
		})
		if err != nil {
			return err
		}
		return writeErr
	}, beforeRename, afterRename, engine.faults)
	if err != nil {
		return nil, fmt.Errorf("rewrite reference data: %w", err)
	}
	return replacedKeys, nil
}

func (engine *Engine) appendToLog(path string, data []byte) error {
	return fsutil.AppendAndSync(path, data, engine.faults)
}
