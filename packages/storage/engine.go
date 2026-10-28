package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// Options configure a storage engine.
type Options struct {
	// Format selects the file names and log headers of new files.
	Format layout.Format
	// Logger receives recovery messages. Defaults to slog.Default().
	Logger *slog.Logger
	// Faults injects I/O failures in tests. Leave nil in production.
	Faults fsutil.FaultInjector
	// Now returns the commit time of new events. Defaults to time.Now.
	Now func() time.Time
	// NewEventID returns the ID of new events. Defaults to core.NewUUID.
	NewEventID func() string
}

// Engine owns the event logs of one data directory. It does not lock the
// directory: callers that share a directory between processes must use
// fsutil.LockDirectory first.
type Engine struct {
	eventsDirectory string
	format          layout.Format
	wal             *WriteAheadLog
	logger          *slog.Logger
	faults          fsutil.FaultInjector
	now             func() time.Time
	newEventID      func() string

	// lastEventHash and mainEventLogSize are written by the single writer
	// only.
	lastEventHash    string
	mainEventLogSize int64

	// failure is set after an append failed once anything may be durable. The
	// in-memory log state and the retained WAL are then untrustworthy, so
	// every further write is refused until Open recovers the directory. It is
	// written by the single writer and read by unsynchronized status checks,
	// hence atomic.
	failure atomic.Pointer[error]
}

// Failure returns the sticky write failure, or nil while the engine is
// healthy. Only a restart (Open) clears it. Safe for concurrent use.
func (engine *Engine) Failure() error {
	if failure := engine.failure.Load(); failure != nil {
		return *failure
	}
	return nil
}

// checkHealthy refuses writes after a sticky failure.
func (engine *Engine) checkHealthy() error {
	if failure := engine.Failure(); failure != nil {
		return fmt.Errorf("%w: %v", core.ErrUnhealthy, failure)
	}
	return nil
}

// markFailed records a write failure that may have left durable data. It
// keeps the first failure.
func (engine *Engine) markFailed(err error) error {
	engine.failure.CompareAndSwap(nil, &err)
	return err
}

// Open prepares the event logs of a data directory: it creates missing
// directories and logs, repairs interrupted final records, and replays a
// pending write-ahead log batch.
func Open(dataDirectory string, options Options) (*Engine, error) {
	engine := &Engine{
		eventsDirectory: filepath.Join(dataDirectory, layout.EventsDirectory),
		format:          options.Format,
		logger:          options.Logger,
		faults:          options.Faults,
		now:             options.Now,
		newEventID:      options.NewEventID,
	}
	if engine.logger == nil {
		engine.logger = slog.Default()
	}
	if engine.now == nil {
		engine.now = time.Now
	}
	if engine.newEventID == nil {
		engine.newEventID = core.NewUUID
	}
	if err := os.MkdirAll(engine.eventsDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create %s: %w", engine.eventsDirectory, err)
	}
	wal, err := OpenWriteAheadLog(dataDirectory, options.Faults)
	if err != nil {
		return nil, err
	}
	engine.wal = wal
	for _, name := range engine.format.LogFiles() {
		if err := engine.ensureLogFileExists(name); err != nil {
			return nil, err
		}
	}
	if err := engine.repairIncompleteLogTails(); err != nil {
		return nil, err
	}
	if err := engine.replayWriteAheadLog(); err != nil {
		return nil, fmt.Errorf("WAL recovery failed: %w", err)
	}
	if err := engine.loadMainEventLogState(); err != nil {
		return nil, err
	}
	return engine, nil
}

// Format returns the storage format of the engine.
func (engine *Engine) Format() layout.Format { return engine.format }

// EventsDirectory returns the directory that holds the event logs.
func (engine *Engine) EventsDirectory() string { return engine.eventsDirectory }

// MainEventLogFile returns the file name of the event log new events go to.
func (engine *Engine) MainEventLogFile() string { return engine.format.MainEventLogFile() }

// LastEventHash returns the hash of the newest event of the main log, or
// core.ZeroHash for an empty log.
func (engine *Engine) LastEventHash() string { return engine.lastEventHash }

func (engine *Engine) logPath(name string) string {
	return filepath.Join(engine.eventsDirectory, name)
}

func (engine *Engine) mainEventLogPath() string { return engine.logPath(engine.MainEventLogFile()) }

func (engine *Engine) referenceDataLogPath() string {
	return engine.logPath(engine.format.ReferenceDataLogFile())
}

func (engine *Engine) systemEventLogPath() string {
	return engine.logPath(engine.format.SystemEventLogFile())
}

// ensureLogFileExists creates a log with its header if it does not exist.
func (engine *Engine) ensureLogFileExists(name string) error {
	path := engine.logPath(name)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := fsutil.CreateAndSync(path, engine.format.LogHeaderLine()); err != nil {
		return fmt.Errorf("failed to create %s: %w", name, err)
	}
	return nil
}

// repairIncompleteLogTails truncates verified interrupted final records of
// every log. Interior corruption is reported later by scans.
func (engine *Engine) repairIncompleteLogTails() error {
	return fsutil.VisitDirectory(engine.eventsDirectory, func(name string, mode os.FileMode) error {
		if !engine.format.IsLogFile(name) || !mode.IsRegular() {
			return nil
		}
		return repairIncompleteLogTail(engine.logPath(name), engine.logger)
	})
}

// repairIncompleteLogTail removes the bytes after the last newline. Only an
// unterminated final line is removed. Complete but invalid records are never
// truncated here.
func repairIncompleteLogTail(path string, logger *slog.Logger) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size == 0 {
		return nil
	}
	lastByte := make([]byte, 1)
	if _, err := file.ReadAt(lastByte, size-1); err != nil {
		return err
	}
	if lastByte[0] == '\n' {
		return nil
	}
	cut, err := offsetAfterLastNewline(file, size)
	if err != nil {
		return err
	}
	if cut == 0 {
		head := make([]byte, size)
		if _, err := file.ReadAt(head, 0); err != nil && err != io.EOF {
			return err
		}
		if !layout.IsLogHeaderPrefix(head) {
			return newCorruptionError("%s has an invalid header", path)
		}
	}
	logger.Warn("repairing interrupted final record", "file", path, "truncatedBytes", size-cut)
	if err := file.Truncate(cut); err != nil {
		return err
	}
	return file.Sync()
}

// offsetAfterLastNewline returns the offset right after the last newline
// before size, or 0 if there is none.
func offsetAfterLastNewline(file *os.File, size int64) (int64, error) {
	const chunkSize = 64 * 1024
	buffer := make([]byte, chunkSize)
	position := size
	for position > 0 {
		length := int64(chunkSize)
		if position < length {
			length = position
		}
		position -= length
		if _, err := file.ReadAt(buffer[:length], position); err != nil && err != io.EOF {
			return 0, err
		}
		if index := bytes.LastIndexByte(buffer[:length], '\n'); index >= 0 {
			return position + int64(index) + 1, nil
		}
	}
	return 0, nil
}

// loadMainEventLogState reads the size and the newest hash of the main log.
func (engine *Engine) loadMainEventLogState() error {
	file, err := os.Open(engine.mainEventLogPath())
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	engine.mainEventLogSize = info.Size()
	engine.lastEventHash = core.ZeroHash
	if info.Size() == 0 {
		return nil
	}
	lineEnd := info.Size() - 1
	lineStart, err := offsetAfterLastNewline(file, lineEnd)
	if err != nil {
		return err
	}
	if lineStart == 0 {
		return nil // only the header
	}
	line := make([]byte, lineEnd-lineStart)
	if _, err := file.ReadAt(line, lineStart); err != nil {
		return err
	}
	event, err := DecodeEventLine(line)
	if err != nil {
		return newCorruptionError("%s: invalid final record: %v", engine.MainEventLogFile(), err)
	}
	engine.lastEventHash = event.Hash
	return nil
}

// replayWriteAheadLog idempotently appends prepared records that are missing
// from the logs. Records are identified by ID and hash, so historical
// duplicate IDs are never collapsed. Indexes pick replayed records up as an
// unindexed log tail.
func (engine *Engine) replayWriteAheadLog() error {
	if !engine.wal.HasPendingEntries() {
		return engine.wal.Truncate()
	}
	entries, err := engine.wal.ReadPendingEntries()
	if IsTorn(err) {
		// The WAL write itself never completed, so the batch was never
		// acknowledged and nothing was appended to the logs. Discard it.
		engine.logger.Warn("discarding torn write-ahead log of an unacknowledged commit")
		return engine.wal.Truncate()
	}
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return engine.wal.Truncate()
	}
	engine.logger.Info("replaying write-ahead log", "pendingEvents", len(entries))

	identity := func(event *core.Event) string { return event.ID + "\x00" + event.Hash }
	isPersisted := make(map[string]bool, len(entries))
	isReferenceDataPersisted := map[string]bool{}
	for _, entry := range entries {
		isPersisted[identity(entry.Event)] = false
		if len(entry.ReferenceData) > 0 {
			isReferenceDataPersisted[entry.Event.ReferenceDataKey] = false
		}
	}
	if _, _, err := engine.ScanEventLog(engine.MainEventLogFile(), 0, func(stored core.StoredEvent) error {
		if _, wanted := isPersisted[identity(stored.Event)]; wanted {
			isPersisted[identity(stored.Event)] = true
		}
		return nil
	}); err != nil {
		return err
	}
	if len(isReferenceDataPersisted) > 0 {
		if err := engine.ScanReferenceData(func(entry *core.ReferenceDataEntry) bool {
			if _, wanted := isReferenceDataPersisted[entry.Key]; wanted {
				isReferenceDataPersisted[entry.Key] = true
			}
			return true
		}); err != nil {
			return err
		}
	}

	var referenceDataBuffer, eventBuffer []byte
	replayedEvents := 0
	for _, entry := range entries {
		if isPersisted[identity(entry.Event)] {
			continue
		}
		if len(entry.ReferenceData) > 0 && !isReferenceDataPersisted[entry.Event.ReferenceDataKey] {
			encoded, err := json.Marshal(core.ReferenceDataEntry{Key: entry.Event.ReferenceDataKey, Data: entry.ReferenceData, Timestamp: engine.now()})
			if err != nil {
				return err
			}
			referenceDataBuffer = append(append(referenceDataBuffer, encoded...), '\n')
			isReferenceDataPersisted[entry.Event.ReferenceDataKey] = true
		}
		line, err := EncodeEventLine(entry.Event)
		if err != nil {
			return err
		}
		eventBuffer = append(append(eventBuffer, line...), '\n')
		replayedEvents++
	}
	if len(referenceDataBuffer) > 0 {
		if err := fsutil.AppendAndSync(engine.referenceDataLogPath(), referenceDataBuffer, engine.faults); err != nil {
			return err
		}
	}
	if len(eventBuffer) > 0 {
		if err := fsutil.AppendAndSync(engine.mainEventLogPath(), eventBuffer, engine.faults); err != nil {
			return err
		}
	}
	if err := engine.wal.Truncate(); err != nil {
		return err
	}
	engine.logger.Info("write-ahead log replayed", "replayedEvents", replayedEvents)
	return nil
}

// ListEventLogs returns the sorted names of all regular event logs.
func (engine *Engine) ListEventLogs() ([]string, error) {
	var names []string
	err := fsutil.VisitDirectory(engine.eventsDirectory, func(name string, mode os.FileMode) error {
		if !engine.format.IsRegularEventLogFile(name) {
			return nil
		}
		if mode&os.ModeSymlink != 0 || !mode.IsRegular() {
			return fmt.Errorf("event log %s is not a regular file", engine.logPath(name))
		}
		names = append(names, name)
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// EventLogSize returns the size of an event log in bytes.
func (engine *Engine) EventLogSize(logFile string) (int64, error) {
	info, err := os.Stat(engine.logPath(logFile))
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// ScanEventLog visits every record of a log from startOffset on. Offset 0
// expects the log header first. Every record must be complete and valid.
func (engine *Engine) ScanEventLog(logFile string, startOffset int64, visit func(core.StoredEvent) error) (int64, *core.StoredEvent, error) {
	file, err := os.Open(engine.logPath(logFile))
	if err != nil {
		return 0, nil, err
	}
	defer file.Close()
	if _, err := file.Seek(startOffset, io.SeekStart); err != nil {
		return 0, nil, err
	}
	endOffset := startOffset
	expectsHeader := startOffset == 0
	var last *core.StoredEvent
	err = fsutil.ScanLines(file, startOffset, func(line []byte, offset int64, isTerminated bool) error {
		if !isTerminated {
			return newCorruptionError("%s: incomplete record at offset %d", logFile, offset)
		}
		endOffset = offset + int64(len(line)) + 1
		if expectsHeader {
			expectsHeader = false
			if !layout.IsLogHeader(line) {
				return newCorruptionError("%s: invalid header", logFile)
			}
			return nil
		}
		event, err := DecodeEventLine(line)
		if err != nil {
			return newCorruptionError("%s: invalid record at offset %d: %v", logFile, offset, err)
		}
		stored := core.StoredEvent{Event: event, Location: core.RecordLocation{LogFile: logFile, Offset: offset, Length: int64(len(line))}}
		last = &stored
		if visit != nil {
			return visit(stored)
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return endOffset, last, nil
}

// ReadEventAt reads one record by its location.
func (engine *Engine) ReadEventAt(location core.RecordLocation) (*core.Event, error) {
	reader := engine.NewRecordReader()
	defer reader.Close()
	return reader.Read(location)
}

// NewRecordReader returns a reader that keeps every opened log open until
// Close, which makes many reads from the same logs cheap.
func (engine *Engine) NewRecordReader() *RecordReader {
	return &RecordReader{eventsDirectory: engine.eventsDirectory, openFiles: map[string]*os.File{}}
}

// readAheadBytes is the window a RecordReader reads at once. Reads in log
// order (selections are sorted that way) are then served from memory
// instead of one system call per record. A record larger than the window,
// possible only for records written before the 64 KiB event limit, gets a
// window of its own size. It is a variable so tests can shrink it.
var readAheadBytes = 1 << 20

// RecordReader reads records by location. It is not safe for concurrent use.
type RecordReader struct {
	eventsDirectory string
	openFiles       map[string]*os.File
	// window holds the bytes of windowFile starting at windowOffset.
	window       []byte
	windowFile   string
	windowOffset int64
}

// Read returns the event stored at location.
func (reader *RecordReader) Read(location core.RecordLocation) (*core.Event, error) {
	line, err := reader.readRecord(location)
	if err != nil {
		return nil, err
	}
	event, err := DecodeEventLine(line)
	if err != nil {
		return nil, fmt.Errorf("%w: %s@%d is not a valid event: %v", ErrNoRecordAtLocation, location.LogFile, location.Offset, err)
	}
	return event, nil
}

// readRecord returns the record line at location without the newline. The
// slice is valid until the next call.
func (reader *RecordReader) readRecord(location core.RecordLocation) ([]byte, error) {
	end := location.Offset + location.Length + 1
	inWindow := reader.windowFile == location.LogFile && location.Offset >= reader.windowOffset && end <= reader.windowOffset+int64(len(reader.window))
	if !inWindow {
		if err := reader.fill(location); err != nil {
			return nil, err
		}
	}
	start := location.Offset - reader.windowOffset
	record := reader.window[start : start+location.Length+1]
	if record[location.Length] != '\n' {
		return nil, fmt.Errorf("%w: %s@%d has an unexpected length", ErrNoRecordAtLocation, location.LogFile, location.Offset)
	}
	return record[:location.Length], nil
}

// fill loads the window that starts at location and holds at least the
// whole record. Near the end of the log the window is shorter.
func (reader *RecordReader) fill(location core.RecordLocation) error {
	file := reader.openFiles[location.LogFile]
	if file == nil {
		var err error
		file, err = os.Open(filepath.Join(reader.eventsDirectory, location.LogFile))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: event log %s is missing", ErrNoRecordAtLocation, location.LogFile)
			}
			return err
		}
		reader.openFiles[location.LogFile] = file
	}
	size := max(int64(readAheadBytes), location.Length+1)
	if int64(cap(reader.window)) < size {
		reader.window = make([]byte, size)
	}
	buffer := reader.window[:size]
	read, err := file.ReadAt(buffer, location.Offset)
	if err != nil && err != io.EOF {
		return err
	}
	if int64(read) < location.Length+1 {
		return fmt.Errorf("%w: %s@%d is beyond the end of the event log", ErrNoRecordAtLocation, location.LogFile, location.Offset)
	}
	reader.window = buffer[:read]
	reader.windowFile = location.LogFile
	reader.windowOffset = location.Offset
	return nil
}

// Close closes every opened log.
func (reader *RecordReader) Close() {
	for _, file := range reader.openFiles {
		file.Close()
	}
	reader.openFiles = map[string]*os.File{}
}

// IsEmpty reports whether no log holds any record: no events, no reference
// data and no system events.
func (engine *Engine) IsEmpty() (bool, error) {
	isEmpty := true
	err := fsutil.VisitDirectory(engine.eventsDirectory, func(name string, mode os.FileMode) error {
		if !engine.format.IsLogFile(name) {
			return nil
		}
		path := engine.logPath(name)
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		header, err := fsutil.ReadFirstLine(path)
		if err != nil {
			return err
		}
		if info.Size() > int64(len(header)) {
			isEmpty = false
			return fsutil.ErrStopScan
		}
		return nil
	})
	if err != nil && !errors.Is(err, fsutil.ErrStopScan) {
		return false, err
	}
	return isEmpty, nil
}

// TotalEventLogSize returns the summed size of all regular event logs.
func (engine *Engine) TotalEventLogSize() (int64, error) {
	names, err := engine.ListEventLogs()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, name := range names {
		size, err := engine.EventLogSize(name)
		if err != nil {
			return 0, err
		}
		total += size
	}
	return total, nil
}
