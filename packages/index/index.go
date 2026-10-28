package index

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// Options configure an index.
type Options struct {
	// Format selects the file names of new index files.
	Format layout.Format
	// Logger receives migration and recovery messages.
	Logger *slog.Logger
	// Faults injects I/O failures in tests. Leave nil in production.
	Faults fsutil.FaultInjector
	// NewID returns build and commit group IDs. Defaults to core.NewUUID.
	NewID func() string
}

// Index owns the fixed-shard index set of one data directory.
//
// Publication is serialized by the caller: Publish, Rebuild and MarkRestore
// must not run concurrently. Snapshot and the read methods may run at any
// time.
type Index struct {
	root      string
	format    layout.Format
	paths     paths
	codec     postingCodec
	eventLogs map[string]LogCheckpoint
	reader    core.EventLogReader
	logger    *slog.Logger
	faults    fsutil.FaultInjector
	newID     func() string

	mutex       sync.RWMutex
	snapshot    *Snapshot
	isReady     bool
	failure     error
	build       string
	eventCount  int64
	journalSize int64

	// handles keeps shard files open between commits, see handles.go.
	handles *shardHandles
}

// Snapshot is an immutable set of published byte boundaries. Reads through
// one snapshot never see a partly published batch.
type Snapshot struct {
	build string
	files map[string]int64
	ctx   context.Context
}

// WithContext returns a request-local view sharing immutable boundaries.
func (snapshot *Snapshot) WithContext(ctx context.Context) *Snapshot {
	copy := *snapshot
	copy.ctx = ctx
	return &copy
}

// Build returns the ID of the index build the snapshot belongs to.
func (snapshot *Snapshot) Build() string { return snapshot.build }

// New returns an index over dataDirectory/indexes that reads event logs
// through reader. Call Open before use.
func New(dataDirectory string, reader core.EventLogReader, options Options) *Index {
	index := &Index{
		root:      filepath.Join(dataDirectory, layout.IndexesDirectory),
		format:    options.Format,
		paths:     paths{format: options.Format},
		codec:     codecForFormat(options.Format),
		eventLogs: map[string]LogCheckpoint{},
		reader:    reader,
		logger:    options.Logger,
		faults:    options.Faults,
		newID:     options.NewID,
		snapshot:  &Snapshot{files: map[string]int64{}},
		handles:   newShardHandles(),
	}
	if index.logger == nil {
		index.logger = slog.Default()
	}
	if index.newID == nil {
		index.newID = core.NewUUID
	}
	return index
}

// CheckCompatibility rejects future index formats and unrecognized files
// before anything is modified.
func (index *Index) CheckCompatibility() error {
	if _, err := index.loadStateJournal(); err != nil {
		return err
	}
	_, err := index.inspectLayout()
	return err
}

func (index *Index) path(relativePath string) string {
	return filepath.Join(index.root, filepath.FromSlash(relativePath))
}

// AcquireSnapshot returns the published snapshot or core.ErrNotReady.
func (index *Index) AcquireSnapshot() (*Snapshot, error) {
	index.mutex.RLock()
	defer index.mutex.RUnlock()
	if !index.isReady {
		if index.failure != nil {
			return nil, fmt.Errorf("%w: %v", core.ErrNotReady, index.failure)
		}
		return nil, core.ErrNotReady
	}
	return index.snapshot, nil
}

// IsReady reports whether a published index set is usable.
func (index *Index) IsReady() bool {
	index.mutex.RLock()
	defer index.mutex.RUnlock()
	return index.isReady
}

// MarkFailed marks the index unusable until recovery or rebuild succeeds.
func (index *Index) MarkFailed(err error) {
	index.mutex.Lock()
	index.isReady = false
	index.failure = err
	index.mutex.Unlock()
}

// Close releases the shard files kept open between commits. The index is
// unusable afterwards until Open.
func (index *Index) Close() {
	index.MarkNotReady()
	index.handles.closeAll()
}

// MarkNotReady marks the index unusable without a failure, for example
// during maintenance or after Close.
func (index *Index) MarkNotReady() {
	index.mutex.Lock()
	index.isReady = false
	index.failure = nil
	index.mutex.Unlock()
}

// EventCount returns the number of indexed regular events.
func (index *Index) EventCount() int64 {
	index.mutex.RLock()
	defer index.mutex.RUnlock()
	return index.eventCount
}

// CheckpointForLog returns how much of an event log is indexed.
func (index *Index) CheckpointForLog(logFile string) LogCheckpoint {
	index.mutex.RLock()
	defer index.mutex.RUnlock()
	return index.eventLogs[logFile]
}

// IndexedEventLogs returns the sorted names of every indexed event log.
func (index *Index) IndexedEventLogs() []string {
	index.mutex.RLock()
	defer index.mutex.RUnlock()
	names := make([]string, 0, len(index.eventLogs))
	for name := range index.eventLogs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// PostingBatch groups encoded postings by destination file.
type PostingBatch struct {
	codec postingCodec
	files map[string]*bytes.Buffer
	size  int
}

// newPostingBatch returns an empty batch that encodes with the index codec.
func (index *Index) newPostingBatch() *PostingBatch {
	return &PostingBatch{codec: index.codec, files: map[string]*bytes.Buffer{}}
}

func (batch *PostingBatch) add(relativePath string, posting *Posting) {
	buffer := batch.files[relativePath]
	if buffer == nil {
		buffer = &bytes.Buffer{}
		batch.files[relativePath] = buffer
	}
	line := batch.codec.encode(posting)
	buffer.Write(line)
	batch.size += len(line)
}

// Publish indexes a batch of newly appended events and publishes it
// atomically. mainLogSize is the size of the event log after the append.
func (index *Index) Publish(events []core.StoredEvent, mainLogSize int64) error {
	if len(events) == 0 {
		return nil
	}
	snapshot, err := index.AcquireSnapshot()
	if err != nil {
		return err
	}
	commitGroup := index.newID()
	batch := index.newPostingBatch()
	for _, stored := range events {
		index.paths.postingsForEvent(stored, commitGroup, batch.add)
	}
	files, err := index.writePostingBatch(batch, snapshot.files, true)
	if err != nil {
		return err
	}
	last := events[len(events)-1]
	checkpoint := LogCheckpoint{Size: mainLogSize, LastOffset: last.Location.Offset, LastLength: last.Location.Length, LastHash: last.Event.Hash}
	return index.publishBatch(commitGroup, files, map[string]LogCheckpoint{last.Location.LogFile: checkpoint}, index.EventCount()+int64(len(events)))
}

// writePostingBatch appends every buffered destination once, starting at the
// given boundaries. Bytes beyond a boundary are unpublished and overwritten.
// With syncFiles every touched file is synced once before the function
// returns. It returns the new sizes of the touched files.
//
// Files are written sequentially and synced concurrently: a commit touches
// up to six shards, and the device can overlap their flushes, so the commit
// latency is that of one fsync rather than six.
func (index *Index) writePostingBatch(batch *PostingBatch, boundaries map[string]int64, syncFiles bool) (map[string]int64, error) {
	relativePaths := make([]string, 0, len(batch.files))
	for relativePath := range batch.files {
		relativePaths = append(relativePaths, relativePath)
	}
	sort.Strings(relativePaths)
	sizes := make(map[string]int64, len(relativePaths))
	createdDirectories := map[string]bool{}
	written := make([]*os.File, 0, len(relativePaths))
	// Commits reuse cached handles. A rebuild touches every shard once per
	// flush and opens its files directly.
	openShard := func(path string) (*os.File, error) {
		if syncFiles {
			return index.handles.open(path)
		}
		return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	}
	release := func(failed bool) error {
		var first error
		for position, file := range written {
			switch {
			case syncFiles && failed:
				index.handles.discard(file.Name())
			case !syncFiles:
				if err := file.Close(); err != nil && first == nil {
					first = fmt.Errorf("close index shard %s: %w", relativePaths[position], err)
				}
			}
		}
		if syncFiles {
			index.handles.trim()
		}
		return first
	}
	closeAll := func() { release(true) }
	for _, relativePath := range relativePaths {
		category, shard, valid := index.paths.categoryOf(relativePath)
		if !valid {
			closeAll()
			return nil, fmt.Errorf("invalid index destination %q", relativePath)
		}
		path := index.path(relativePath)
		boundary, isPublished := boundaries[relativePath]
		if err := index.faults.Check("shard.write"); err != nil {
			closeAll()
			return nil, err
		}
		if err := index.faults.CheckCreate(path); err != nil {
			closeAll()
			return nil, err
		}
		file, err := openShard(path)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("open index shard %s: %w", relativePath, err)
		}
		written = append(written, file)
		var content []byte
		if !isPublished || boundary == 0 {
			content = append(encodeShardHeader(index.codec.version(), category, shard), batch.files[relativePath].Bytes()...)
			boundary = 0
			createdDirectories[filepath.Dir(path)] = true
		} else {
			content = batch.files[relativePath].Bytes()
		}
		// Truncate only when an unpublished tail exists: an unconditional
		// truncate costs an extra metadata update per file and commit.
		info, err := file.Stat()
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("stat index shard %s: %w", relativePath, err)
		}
		if info.Size() != boundary {
			if err := file.Truncate(boundary); err != nil {
				closeAll()
				return nil, fmt.Errorf("truncate unpublished tail of %s: %w", relativePath, err)
			}
		}
		if _, err := file.WriteAt(content, boundary); err != nil {
			closeAll()
			return nil, fmt.Errorf("write index shard %s: %w", relativePath, err)
		}
		sizes[relativePath] = boundary + int64(len(content))
	}
	if syncFiles {
		if err := index.faults.Check("shard.sync"); err != nil {
			closeAll()
			return nil, err
		}
		if err := syncConcurrently(written, relativePaths); err != nil {
			closeAll()
			return nil, err
		}
	}
	if err := release(false); err != nil {
		return nil, err
	}
	if syncFiles {
		if err := syncDirectoriesConcurrently(createdDirectories); err != nil {
			return nil, err
		}
	}
	return sizes, nil
}

// syncConcurrently syncs every open file at once and returns the first error.
// Shards are published by the journal record written afterwards, whose full
// sync is the durability barrier, so the shards use the cheaper pre-barrier
// sync (see fsutil.SyncBeforeBarrier).
func syncConcurrently(files []*os.File, names []string) error {
	if len(files) == 1 {
		if err := fsutil.SyncBeforeBarrier(files[0]); err != nil {
			return fmt.Errorf("sync index shard %s: %w", names[0], err)
		}
		return nil
	}
	errs := make([]error, len(files))
	var group sync.WaitGroup
	for position, file := range files {
		group.Add(1)
		go func(position int, file *os.File) {
			defer group.Done()
			if err := fsutil.SyncBeforeBarrier(file); err != nil {
				errs[position] = fmt.Errorf("sync index shard %s: %w", names[position], err)
			}
		}(position, file)
	}
	group.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// syncDirectoriesConcurrently syncs every directory at once.
func syncDirectoriesConcurrently(directories map[string]bool) error {
	if len(directories) == 0 {
		return nil
	}
	errs := make(chan error, len(directories))
	var group sync.WaitGroup
	for directory := range directories {
		group.Add(1)
		go func(directory string) {
			defer group.Done()
			errs <- fsutil.SyncDirectory(directory)
		}(directory)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// publishBatch durably records a commit group and atomically publishes the
// new boundaries.
func (index *Index) publishBatch(commitGroup string, files map[string]int64, eventLogs map[string]LogCheckpoint, eventCount int64) error {
	index.mutex.RLock()
	build := index.build
	index.mutex.RUnlock()
	record := &StateJournalRecord{Type: recordCommit, Build: build, CommitGroup: commitGroup, Files: files, EventLogs: eventLogs, EventCount: eventCount}
	if err := index.appendToStateJournal(record); err != nil {
		return err
	}
	if err := index.faults.Check("publish"); err != nil {
		return err
	}
	index.mutex.Lock()
	nextFiles := make(map[string]int64, len(index.snapshot.files)+len(files))
	for path, size := range index.snapshot.files {
		nextFiles[path] = size
	}
	for path, size := range files {
		nextFiles[path] = size
	}
	index.snapshot = &Snapshot{build: index.build, files: nextFiles}
	for logFile, checkpoint := range eventLogs {
		index.eventLogs[logFile] = checkpoint
	}
	index.eventCount = eventCount
	index.mutex.Unlock()
	return nil
}

// MarkRestoreStarted durably marks the index as restoring before a restore
// writes authoritative data. An interrupted restore is detected on the next
// start.
func (index *Index) MarkRestoreStarted() error {
	if err := index.appendToStateJournal(&StateJournalRecord{Type: recordRestoreStarted, Build: index.newID()}); err != nil {
		return err
	}
	index.MarkNotReady()
	return nil
}

// ErrInterruptedRestore reports a data directory with an unfinished restore.
var ErrInterruptedRestore = errors.New("an interrupted backup restore was detected in this data directory; the restored data is incomplete. Remove the data directory contents and run the restore again")
