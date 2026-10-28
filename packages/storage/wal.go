package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// WALEntry is a prepared event and its optional reference payload.
type WALEntry struct {
	Event         *core.Event     `json:"event"`
	ReferenceData json.RawMessage `json:"refData,omitempty"`
}

// walBatch is a single checksummed record. A line boundary can never delimit
// a prefix of a batch. A WAL of GenesisDB 1.x (one entry per line, no
// checksum) is still accepted by readLegacyPendingEntries.
type walBatch struct {
	Version  int        `json:"version"`
	Entries  []WALEntry `json:"entries"`
	Checksum string     `json:"checksum"`
}

func batchChecksum(entries []WALEntry) (string, error) {
	encoded, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// WriteAheadLog owns the complete prepared batch of an in-flight commit.
type WriteAheadLog struct {
	directory string
	faults    fsutil.FaultInjector
	mutex     sync.Mutex
}

func OpenWriteAheadLog(dataDirectory string, faults fsutil.FaultInjector) (*WriteAheadLog, error) {
	directory := filepath.Join(dataDirectory, layout.WALDirectory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, err
	}
	return &WriteAheadLog{directory: directory, faults: faults}, nil
}

func (wal *WriteAheadLog) filePath() string { return filepath.Join(wal.directory, layout.WALFile) }

// AppendEntries publishes a complete batch with one synced in-place write.
// The file is kept between commits, so no directory entry changes and a
// single fsync makes the batch durable. The batch is one checksummed line:
// a torn write leaves an unterminated line, which recovery discards, because
// nothing is appended to the event logs before this call returns.
//
// Errors may still mean the write reached the disk, so callers stop writing
// until recovery instead of overwriting a possibly durable batch.
func (wal *WriteAheadLog) AppendEntries(entries []WALEntry) error {
	wal.mutex.Lock()
	defer wal.mutex.Unlock()
	checksum, err := batchChecksum(entries)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(walBatch{Version: 2, Entries: entries, Checksum: checksum})
	if err != nil {
		return err
	}
	if err := wal.faults.Check("wal.write"); err != nil {
		return err
	}
	_, statErr := os.Stat(wal.filePath())
	isNew := errors.Is(statErr, os.ErrNotExist)
	file, err := os.OpenFile(wal.filePath(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := wal.faults.Check("wal.sync"); err != nil {
		file.Close()
		return err
	}
	// Ordered, not flushed: the commit is acknowledged only after the index
	// journal record, whose full flush makes this batch durable too. A crash
	// before that flush loses an unacknowledged commit at most.
	if err := fsutil.SyncOrdered(file); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if isNew {
		return fsutil.SyncDirectory(wal.directory)
	}
	return nil
}

// ReadPendingEntries only accepts a complete, checksummed batch.
//
// An unterminated final line is a torn WAL write. Both this release and every
// earlier one write the WAL before appending anything to the event logs, so a
// torn WAL means the batch was never acknowledged and nothing depends on it.
// It is discarded. A complete but invalid record is corruption and fails
// closed.
func (wal *WriteAheadLog) ReadPendingEntries() ([]WALEntry, error) {
	wal.mutex.Lock()
	defer wal.mutex.Unlock()
	content, err := os.ReadFile(wal.filePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(content) == 0 {
		return nil, nil
	}
	if content[len(content)-1] != '\n' {
		return nil, errTornWriteAheadLog
	}
	if !bytes.HasPrefix(bytes.TrimLeft(content, " \t"), []byte(`{"version"`)) {
		return readLegacyPendingEntries(content)
	}
	// Extra data after the single record is corruption.
	if bytes.Count(content, []byte{'\n'}) != 1 {
		return nil, fmt.Errorf("corrupt WAL: expected one complete batch")
	}
	var batch walBatch
	if err := json.Unmarshal(content, &batch); err != nil {
		return nil, fmt.Errorf("corrupt WAL: %w", err)
	}
	if batch.Version != 2 {
		return nil, fmt.Errorf("unsupported WAL version %d", batch.Version)
	}
	checksum, err := batchChecksum(batch.Entries)
	if err != nil || checksum != batch.Checksum {
		return nil, fmt.Errorf("corrupt WAL batch checksum")
	}
	for _, entry := range batch.Entries {
		if entry.Event == nil || !core.HasValidHash(entry.Event) {
			return nil, fmt.Errorf("corrupt WAL event")
		}
	}
	return batch.Entries, nil
}

// readLegacyPendingEntries decodes the WAL of GenesisDB 1.1.6 and earlier:
// one {"event":...,"refData":...} line per entry, no checksum. 1.1.6 wrote
// the complete file with one write and one fsync before touching any log, so
// a complete file is a complete batch. 1.1.6 itself discarded a torn file,
// and so does this reader. Every event must carry a valid historical hash.
func readLegacyPendingEntries(content []byte) ([]WALEntry, error) {
	var entries []WALEntry
	for _, line := range bytes.Split(content, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry WALEntry
		if err := json.Unmarshal(line, &entry); err != nil || entry.Event == nil {
			return nil, fmt.Errorf("corrupt legacy WAL entry")
		}
		if !core.HasValidHash(entry.Event) {
			return nil, fmt.Errorf("corrupt legacy WAL event %s", entry.Event.ID)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// errTornWriteAheadLog marks a WAL whose single write never completed.
var errTornWriteAheadLog = errors.New("torn write-ahead log")

// IsTorn reports whether err is a torn, never completed WAL write.
func IsTorn(err error) bool { return errors.Is(err, errTornWriteAheadLog) }

// Truncate empties the WAL after its batch is durable in the event logs. The
// truncation is not synced: a stale batch that survives a crash is replayed
// idempotently on the next start, so the extra fsync buys nothing.
func (wal *WriteAheadLog) Truncate() error {
	wal.mutex.Lock()
	defer wal.mutex.Unlock()
	if err := wal.faults.Check("wal.clear"); err != nil {
		return err
	}
	if err := os.Truncate(wal.filePath(), 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Leftover temporary files of earlier releases are unpublished.
	if err := os.Remove(wal.filePath() + layout.TemporaryFileSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (wal *WriteAheadLog) HasPendingEntries() bool {
	// A stat failure must not silently discard recovery work.
	info, err := os.Stat(wal.filePath())
	return !errors.Is(err, os.ErrNotExist) && (err != nil || info.Size() > 0)
}

// ClearWriteAheadLog removes any completed batch and unpublished temporary
// payloads. Only call when the store is healthy and no writer is in flight.
func (engine *Engine) ClearWriteAheadLog() error { return engine.wal.Truncate() }
