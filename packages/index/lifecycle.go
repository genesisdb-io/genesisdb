package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
)

// rebuildFlushBytes bounds the postings buffered in memory during a rebuild.
const rebuildFlushBytes = 16 * 1024 * 1024

// journalCompactionBytes is the journal size above which Open compacts it.
const journalCompactionBytes = 64 * 1024 * 1024

// Open runs the startup lifecycle: inspect the index root, then either
// recover the published index set or rebuild it from the event logs. Future
// formats are rejected before anything is modified.
func (index *Index) Open() error {
	index.handles.closeAll()
	state, err := index.loadStateJournal()
	if err != nil {
		return err
	}
	inventory, err := index.inspectLayout()
	if err != nil {
		return err
	}
	if state.isRestoring {
		return ErrInterruptedRestore
	}
	rebuildReason := ""
	switch {
	case !state.exists && inventory.hasDisposableData():
		rebuildReason = "legacy or foreign index files detected"
	case !state.exists:
		rebuildReason = "no index state found"
	case state.corruption != nil:
		rebuildReason = fmt.Sprintf("index state invalid: %v", state.corruption)
	case !state.isReady:
		rebuildReason = "index build was interrupted"
	}
	if rebuildReason == "" {
		if err := index.recoverLastPublishedState(state, inventory); err != nil {
			if errors.Is(err, core.ErrUnsupportedIndexFormat) {
				return err
			}
			rebuildReason = fmt.Sprintf("index validation failed: %v", err)
		}
	}
	if rebuildReason != "" {
		index.logger.Info("rebuilding indexes", "reason", rebuildReason)
		if err := index.Rebuild(); err != nil {
			index.MarkFailed(err)
			return fmt.Errorf("index rebuild failed: %w", err)
		}
	}
	if index.journalSize > journalCompactionBytes {
		if err := index.compactStateJournal(); err != nil {
			index.MarkFailed(err)
			return fmt.Errorf("index state compaction failed: %w", err)
		}
	}
	return nil
}

// Rebuild rebuilds every index file in place from the event logs. The caller
// must exclude readers and writers for the duration.
func (index *Index) Rebuild() error {
	return index.RebuildContext(context.Background())
}

// RebuildContext observes cancellation between records and flushes. Once a
// rebuild starts, failure leaves the index unavailable until restart recovery.
func (index *Index) RebuildContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	started := time.Now()
	index.MarkNotReady()
	// A rebuild removes and truncates files, cached handles must not
	// outlive the files they were opened for.
	index.handles.closeAll()
	if err := index.removeDisposableData(); err != nil {
		return fmt.Errorf("remove legacy index files: %w", err)
	}
	if err := index.faults.Check("build.afterLegacyDelete"); err != nil {
		return err
	}
	if err := index.ensureCategoryDirectories(); err != nil {
		return err
	}
	build := index.newID()
	// Durably mark the index set incomplete before touching any shard.
	if err := index.resetStateJournal(&StateJournalRecord{Type: recordBuildStarted, Build: build}); err != nil {
		return err
	}
	if err := index.truncateUnpublishedWrites(nil); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(index.root, index.format.TemporaryIndexStateFile()))

	eventLogs, err := index.reader.ListEventLogs()
	if err != nil {
		return err
	}
	sizes := map[string]int64{}
	checkpoints := map[string]LogCheckpoint{}
	var eventCount int64
	batch := index.newPostingBatch()
	flush := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if batch.size == 0 {
			return nil
		}
		written, err := index.writePostingBatch(batch, sizes, false)
		if err != nil {
			return err
		}
		for path, size := range written {
			sizes[path] = size
		}
		batch = index.newPostingBatch()
		return nil
	}
	for _, logFile := range eventLogs {
		size, err := index.reader.EventLogSize(logFile)
		if err != nil {
			return err
		}
		if size == 0 {
			checkpoints[logFile] = LogCheckpoint{}
			continue
		}
		endOffset, last, err := index.reader.ScanEventLog(logFile, 0, func(stored core.StoredEvent) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if stored.Event.IsSystemEvent() {
				return nil
			}
			index.paths.postingsForEvent(stored, build, batch.add)
			eventCount++
			if batch.size >= rebuildFlushBytes {
				return flush()
			}
			return nil
		})
		if err != nil {
			return err
		}
		checkpoints[logFile] = newLogCheckpoint(endOffset, last)
	}
	if err := flush(); err != nil {
		return err
	}
	if err := index.syncFiles(sizes); err != nil {
		return err
	}
	if err := index.faults.Check("build.beforeComplete"); err != nil {
		return err
	}
	completed := &StateJournalRecord{Type: recordBuildCompleted, Build: build, Files: sizes, EventLogs: checkpoints, EventCount: eventCount}
	if err := index.appendToStateJournal(completed); err != nil {
		return err
	}
	if err := index.verifyDirectoryLayout(); err != nil {
		return err
	}
	index.mutex.Lock()
	index.build = build
	index.snapshot = &Snapshot{build: build, files: copyFileSizes(sizes)}
	index.eventLogs = copyCheckpoints(checkpoints)
	index.eventCount = eventCount
	index.isReady = true
	index.failure = nil
	index.mutex.Unlock()
	index.logger.Info("indexes rebuilt", "events", eventCount, "eventLogs", len(eventLogs), "seconds", time.Since(started).Seconds())
	return nil
}

// syncFiles syncs every written file once and then the category directories.
func (index *Index) syncFiles(sizes map[string]int64) error {
	relativePaths := make([]string, 0, len(sizes))
	for relativePath := range sizes {
		relativePaths = append(relativePaths, relativePath)
	}
	sort.Strings(relativePaths)
	for _, relativePath := range relativePaths {
		file, err := os.OpenFile(index.path(relativePath), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		err = file.Sync()
		file.Close()
		if err != nil {
			return fmt.Errorf("sync %s: %w", relativePath, err)
		}
	}
	for _, category := range categoryDirectories {
		if err := fsutil.SyncDirectory(filepath.Join(index.root, category)); err != nil {
			return err
		}
	}
	return nil
}

// verifyDirectoryLayout checks that no disposable data remains and that the
// file budget holds.
func (index *Index) verifyDirectoryLayout() error {
	inventory, err := index.inspectLayout()
	if err != nil {
		return err
	}
	if inventory.hasDisposableData() {
		return fmt.Errorf("legacy index artifacts remain after cleanup (%d files, %d directories)", inventory.disposableFiles, inventory.disposableDirectories)
	}
	if inventory.activeShardFiles+2 > MaxIndexFiles {
		return fmt.Errorf("index file count %d exceeds budget %d", inventory.activeShardFiles+2, MaxIndexFiles)
	}
	return nil
}

// verifyLogCheckpoint checks that the indexed prefix of a log is unchanged
// and returns the current log size.
func (index *Index) verifyLogCheckpoint(logFile string, checkpoint LogCheckpoint) (int64, error) {
	size, err := index.reader.EventLogSize(logFile)
	if err != nil {
		return 0, err
	}
	if size < checkpoint.Size {
		return 0, fmt.Errorf("event log %s is shorter than its index checkpoint", logFile)
	}
	if checkpoint.LastLength > 0 {
		event, err := index.reader.ReadEventAt(core.RecordLocation{LogFile: logFile, Offset: checkpoint.LastOffset, Length: checkpoint.LastLength})
		if err != nil {
			return 0, err
		}
		if event.Hash != checkpoint.LastHash {
			return 0, fmt.Errorf("event log %s does not match its index checkpoint", logFile)
		}
		if checkpoint.LastOffset+checkpoint.LastLength+1 != checkpoint.Size {
			return 0, fmt.Errorf("event log %s checkpoint is inconsistent", logFile)
		}
	}
	return size, nil
}

// verifyPublishedShardSizes checks that every published file exists, has a
// valid header and is at least as long as its boundary.
func (index *Index) verifyPublishedShardSizes(files map[string]int64) error {
	for relativePath, boundary := range files {
		category, shard, valid := index.paths.categoryOf(relativePath)
		if !valid {
			return newCorruptionError("state references invalid path %q", relativePath)
		}
		path := index.path(relativePath)
		info, err := os.Lstat(path)
		if err != nil {
			return newCorruptionError("published index file %s: %v", relativePath, err)
		}
		if !info.Mode().IsRegular() || info.Size() < boundary {
			return newCorruptionError("published index file %s is truncated", relativePath)
		}
		if boundary == 0 {
			continue
		}
		line, err := fsutil.ReadFirstLine(path)
		if err != nil {
			return err
		}
		if err := validateShardHeader(line, index.codec.version(), category, shard); err != nil {
			return err
		}
	}
	return nil
}

// recoverLastPublishedState validates a completed index set, discards
// verified unpublished tails, removes disposable leftovers and indexes
// verified append-only log tails as one recovery batch.
func (index *Index) recoverLastPublishedState(state *journalState, inventory *layoutInventory) error {
	if inventory.hasDisposableData() {
		index.logger.Info("removing leftover legacy index files", "files", inventory.disposableFiles)
		if err := index.removeDisposableData(); err != nil {
			return err
		}
	}
	if inventory.hasTemporaryState {
		if err := os.Remove(filepath.Join(index.root, index.format.TemporaryIndexStateFile())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := index.ensureCategoryDirectories(); err != nil {
		return err
	}
	if err := index.verifyPublishedShardSizes(state.files); err != nil {
		return err
	}
	eventLogs, err := index.reader.ListEventLogs()
	if err != nil {
		return err
	}
	isOnDisk := map[string]bool{}
	for _, logFile := range eventLogs {
		isOnDisk[logFile] = true
	}
	for logFile := range state.eventLogs {
		if !isOnDisk[logFile] {
			return fmt.Errorf("indexed event log %s is missing", logFile)
		}
	}
	type unindexedTail struct {
		logFile     string
		startOffset int64
	}
	var tails []unindexedTail
	for _, logFile := range eventLogs {
		checkpoint, isKnown := state.eventLogs[logFile]
		size, err := index.verifyLogCheckpoint(logFile, checkpoint)
		if err != nil {
			return err
		}
		if !isKnown || size > checkpoint.Size {
			tails = append(tails, unindexedTail{logFile, checkpoint.Size})
		}
	}
	if err := index.truncateUnpublishedWrites(state.files); err != nil {
		return err
	}
	if state.size > state.endOfLastValidLine {
		if err := fsutil.TruncateAndSync(index.stateJournalPath(), state.endOfLastValidLine); err != nil {
			return err
		}
	}
	index.mutex.Lock()
	index.build = state.build
	index.snapshot = &Snapshot{build: state.build, files: copyFileSizes(state.files)}
	index.eventLogs = copyCheckpoints(state.eventLogs)
	index.eventCount = state.eventCount
	index.journalSize = state.endOfLastValidLine
	index.isReady = true
	index.failure = nil
	index.mutex.Unlock()

	if len(tails) == 0 {
		return nil
	}
	batch := index.newPostingBatch()
	changedCheckpoints := map[string]LogCheckpoint{}
	eventCount := state.eventCount
	commitGroup := "recovery-" + index.newID()
	for _, tail := range tails {
		previous := state.eventLogs[tail.logFile]
		endOffset, last, err := index.reader.ScanEventLog(tail.logFile, tail.startOffset, func(stored core.StoredEvent) error {
			if stored.Event.IsSystemEvent() {
				return nil
			}
			index.paths.postingsForEvent(stored, commitGroup, batch.add)
			eventCount++
			return nil
		})
		if err != nil {
			index.MarkNotReady()
			return err
		}
		checkpoint := newLogCheckpoint(endOffset, last)
		if last == nil {
			checkpoint.LastOffset, checkpoint.LastLength, checkpoint.LastHash = previous.LastOffset, previous.LastLength, previous.LastHash
		}
		changedCheckpoints[tail.logFile] = checkpoint
	}
	index.logger.Info("indexing unindexed events from event log tails", "events", eventCount-state.eventCount)
	files, err := index.writePostingBatch(batch, state.files, true)
	if err != nil {
		index.MarkNotReady()
		return err
	}
	if err := index.publishBatch(commitGroup, files, changedCheckpoints, eventCount); err != nil {
		index.MarkNotReady()
		return err
	}
	return nil
}
