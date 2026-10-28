package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
)

// LogCheckpoint records how much of an event log is indexed.
type LogCheckpoint struct {
	Size       int64  `json:"size"`
	LastOffset int64  `json:"lastOff"`
	LastLength int64  `json:"lastLen"`
	LastHash   string `json:"lastHash"`
}

func newLogCheckpoint(endOffset int64, last *core.StoredEvent) LogCheckpoint {
	checkpoint := LogCheckpoint{Size: endOffset}
	if last != nil {
		checkpoint.LastOffset = last.Location.Offset
		checkpoint.LastLength = last.Location.Length
		checkpoint.LastHash = last.Event.Hash
	}
	return checkpoint
}

// StateJournalRecord is one line of the index state journal.
type StateJournalRecord struct {
	Type        string                   `json:"type"`
	Build       string                   `json:"build,omitempty"`
	CommitGroup string                   `json:"group,omitempty"`
	Files       map[string]int64         `json:"files,omitempty"`
	EventLogs   map[string]LogCheckpoint `json:"sources,omitempty"`
	EventCount  int64                    `json:"count"`
	Checksum    string                   `json:"sum"`
}

// State journal record types.
const (
	recordBuildStarted   = "build-start"
	recordBuildCompleted = "build-complete"
	recordCommit         = "commit"
	recordRestoreStarted = "restore-start"
)

func (record *StateJournalRecord) computeChecksum() string {
	unsigned := *record
	unsigned.Checksum = ""
	encoded, _ := json.Marshal(&unsigned)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (record *StateJournalRecord) encode() []byte {
	record.Checksum = record.computeChecksum()
	encoded, _ := json.Marshal(record)
	return append(encoded, '\n')
}

// journalState is the result of replaying the state journal.
type journalState struct {
	exists             bool
	isReady            bool
	build              string
	files              map[string]int64
	eventLogs          map[string]LogCheckpoint
	eventCount         int64
	isRestoring        bool
	endOfLastValidLine int64
	size               int64
	corruption         error
}

func (state *journalState) apply(record *StateJournalRecord) {
	switch record.Type {
	case recordBuildStarted, recordRestoreStarted:
		state.isReady = false
		state.isRestoring = state.isRestoring || record.Type == recordRestoreStarted
		state.build = record.Build
		state.files = map[string]int64{}
		state.eventLogs = map[string]LogCheckpoint{}
		state.eventCount = 0
	case recordBuildCompleted:
		if record.Build != state.build {
			state.isReady = false
			return
		}
		state.files = copyFileSizes(record.Files)
		state.eventLogs = copyCheckpoints(record.EventLogs)
		state.eventCount = record.EventCount
		state.isReady = true
		state.isRestoring = false
	case recordCommit:
		if !state.isReady || record.Build != state.build {
			state.isReady = false
			return
		}
		for path, size := range record.Files {
			state.files[path] = size
		}
		for logFile, checkpoint := range record.EventLogs {
			state.eventLogs[logFile] = checkpoint
		}
		state.eventCount = record.EventCount
	}
}

func copyFileSizes(source map[string]int64) map[string]int64 {
	result := make(map[string]int64, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func copyCheckpoints(source map[string]LogCheckpoint) map[string]LogCheckpoint {
	result := make(map[string]LogCheckpoint, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (index *Index) stateJournalPath() string {
	return filepath.Join(index.root, index.format.IndexStateFile())
}

// loadStateJournal replays the journal without modifying it. A future format
// is an error. Interior corruption is reported in state.corruption, which
// requires a rebuild.
func (index *Index) loadStateJournal() (*journalState, error) {
	state := &journalState{files: map[string]int64{}, eventLogs: map[string]LogCheckpoint{}}
	info, err := os.Lstat(index.stateJournalPath())
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("index state %s is not a regular file", index.stateJournalPath())
	}
	state.exists = true
	state.size = info.Size()
	file, err := os.Open(index.stateJournalPath())
	if err != nil {
		return nil, err
	}
	defer file.Close()

	expectsHeader := true
	var pendingProblem error
	err = fsutil.ScanLines(file, 0, func(line []byte, offset int64, isTerminated bool) error {
		if pendingProblem != nil {
			// A bad record followed by more data is interior corruption.
			state.corruption = pendingProblem
			return fsutil.ErrStopScan
		}
		if expectsHeader {
			expectsHeader = false
			if !isTerminated {
				state.corruption = newCorruptionError("state journal header incomplete")
				return fsutil.ErrStopScan
			}
			kind, header := classifyShardHeader(line)
			if kind == shardHeaderFuture {
				return fmt.Errorf("%w: index state version %d shardCount %d (this binary supports versions up to %d with %d shards); refusing to modify it",
					core.ErrUnsupportedIndexFormat, header.Version, header.ShardCount, newestRecordVersion, ShardCount)
			}
			if problem := validateShardHeader(line, index.codec.version(), categoryIndexState, -1); problem != nil {
				state.corruption = problem
				return fsutil.ErrStopScan
			}
			state.endOfLastValidLine = offset + int64(len(line)) + 1
			return nil
		}
		if !isTerminated {
			pendingProblem = newCorruptionError("incomplete state record")
			return nil
		}
		var record StateJournalRecord
		if err := json.Unmarshal(line, &record); err != nil || record.Checksum != record.computeChecksum() {
			pendingProblem = newCorruptionError("invalid state record at %d", offset)
			return nil
		}
		state.apply(&record)
		state.endOfLastValidLine = offset + int64(len(line)) + 1
		return nil
	})
	if err != nil && !errors.Is(err, fsutil.ErrStopScan) {
		return nil, err
	}
	if expectsHeader {
		state.corruption = newCorruptionError("empty state journal")
	}
	// A problem in the final record only is a verified incomplete tail. It
	// is discarded.
	return state, nil
}

// resetStateJournal durably replaces the journal with header and record in
// place. Truncating in place avoids allocating a new inode.
func (index *Index) resetStateJournal(record *StateJournalRecord) error {
	if err := index.faults.Check("journal.reset"); err != nil {
		return err
	}
	if err := index.faults.CheckCreate(index.stateJournalPath()); err != nil {
		return err
	}
	file, err := os.OpenFile(index.stateJournalPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open index state: %w", err)
	}
	defer file.Close()
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("truncate index state: %w", err)
	}
	content := append(encodeShardHeader(index.codec.version(), categoryIndexState, -1), record.encode()...)
	if _, err := file.WriteAt(content, 0); err != nil {
		return fmt.Errorf("write index state: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync index state: %w", err)
	}
	if err := fsutil.SyncDirectory(index.root); err != nil {
		return err
	}
	index.journalSize = int64(len(content))
	return nil
}

// appendToStateJournal appends and syncs one record at the published end.
func (index *Index) appendToStateJournal(record *StateJournalRecord) error {
	if err := index.faults.Check("journal.append"); err != nil {
		return err
	}
	file, err := os.OpenFile(index.stateJournalPath(), os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open index state: %w", err)
	}
	defer file.Close()
	encoded := record.encode()
	if _, err := file.WriteAt(encoded, index.journalSize); err != nil {
		return fmt.Errorf("append index state: %w", err)
	}
	if err := index.faults.Check("journal.sync"); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync index state: %w", err)
	}
	index.journalSize += int64(len(encoded))
	return nil
}

// compactStateJournal rewrites the journal as one completed build, using the
// fixed temporary file name, sync, rename and directory sync.
func (index *Index) compactStateJournal() error {
	index.mutex.RLock()
	completed := &StateJournalRecord{Type: recordBuildCompleted, Build: index.build, Files: copyFileSizes(index.snapshot.files), EventLogs: copyCheckpoints(index.eventLogs), EventCount: index.eventCount}
	index.mutex.RUnlock()
	started := &StateJournalRecord{Type: recordBuildStarted, Build: completed.Build}
	content := append(encodeShardHeader(index.codec.version(), categoryIndexState, -1), started.encode()...)
	content = append(content, completed.encode()...)
	temporaryPath := filepath.Join(index.root, index.format.TemporaryIndexStateFile())
	if err := fsutil.WriteAndSync(temporaryPath, content); err != nil {
		os.Remove(temporaryPath)
		return err
	}
	if err := os.Rename(temporaryPath, index.stateJournalPath()); err != nil {
		os.Remove(temporaryPath)
		return err
	}
	if err := fsutil.SyncDirectory(index.root); err != nil {
		return err
	}
	index.journalSize = int64(len(content))
	return nil
}
