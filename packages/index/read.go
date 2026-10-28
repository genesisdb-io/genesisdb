package index

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
)

// ScanOptions control a shard scan.
type ScanOptions struct {
	// StartOffset is a published record boundary of the same build. Lines
	// before it are not read. 0 reads the whole file.
	StartOffset int64
	// LineFilter skips lines without decoding them. nil keeps every line.
	LineFilter LineFilter
}

// scanShard visits the published postings of one index file. Unpublished
// files yield nothing. A published but missing file, an invalid header or
// an invalid record is reported as corruption. Framing is verified for
// every line, also for lines the filter rejects.
func (index *Index) scanShard(snapshot *Snapshot, relativePath string, options ScanOptions, visit func(posting *Posting) error) error {
	_, err := index.scanShardResumable(snapshot, relativePath, options, visit)
	return err
}

// errPauseScan is returned by a visitor to stop a resumable scan before the
// current record. The record is not consumed, so the returned resume offset
// points at it.
var errPauseScan = errors.New("pause scan")

// scanShardResumable is scanShard with a resume offset: the published record
// boundary up to which the file was consumed. A visitor that returns
// errPauseScan stops the scan before its record and the resume offset is
// that record's start. Without a pause the resume offset is the published
// boundary of the snapshot.
func (index *Index) scanShardResumable(snapshot *Snapshot, relativePath string, options ScanOptions, visit func(posting *Posting) error) (int64, error) {
	boundary, isPublished := snapshot.files[relativePath]
	startOffset := options.StartOffset
	if !isPublished || boundary == 0 || startOffset >= boundary {
		return boundary, nil
	}
	if startOffset < 0 {
		startOffset = 0
	}
	category, shard, valid := index.paths.categoryOf(relativePath)
	if !valid {
		return 0, newCorruptionError("invalid index path %q", relativePath)
	}
	file, err := os.Open(index.path(relativePath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, newCorruptionError("published index file %s is missing", relativePath)
		}
		return 0, err
	}
	defer file.Close()
	expectsHeader := startOffset == 0
	if startOffset > 0 {
		if _, err := file.Seek(startOffset, io.SeekStart); err != nil {
			return 0, err
		}
	}
	endOffset := startOffset
	pausedAt := int64(-1)
	err = fsutil.ScanLines(io.LimitReader(file, boundary-startOffset), startOffset, func(line []byte, offset int64, isTerminated bool) error {
		if snapshot.ctx != nil {
			if err := snapshot.ctx.Err(); err != nil {
				return err
			}
		}
		endOffset = offset + int64(len(line))
		if isTerminated {
			endOffset++
		}
		if !isTerminated {
			return newCorruptionError("index file %s ends inside a record at %d", relativePath, offset)
		}
		if expectsHeader {
			expectsHeader = false
			return validateShardHeader(line, index.codec.version(), category, shard)
		}
		if options.LineFilter != nil && !options.LineFilter(line) {
			return nil
		}
		var posting Posting
		if err := index.codec.decode(line, &posting); err != nil {
			return newCorruptionError("invalid posting in %s at %d", relativePath, offset)
		}
		if err := posting.validate(); err != nil {
			return fmt.Errorf("%s at %d: %w", relativePath, offset, err)
		}
		if err := visit(&posting); errors.Is(err, errPauseScan) {
			pausedAt = offset
			return fsutil.ErrStopScan
		} else if err != nil {
			return err
		}
		return nil
	})
	if pausedAt >= 0 {
		return pausedAt, nil
	}
	if err != nil {
		return 0, err
	}
	if endOffset != boundary {
		return 0, newCorruptionError("index file %s is shorter than its published boundary", relativePath)
	}
	return boundary, nil
}

// PostingSet collects postings and deduplicates them by record location.
type PostingSet struct {
	seen     map[core.RecordLocation]struct{}
	postings []*Posting
	ctx      context.Context
	bytes    int64
}

// NewPostingSetContext bounds collection before event payloads are loaded.
func NewPostingSetContext(ctx context.Context) *PostingSet {
	return &PostingSet{seen: map[core.RecordLocation]struct{}{}, ctx: ctx}
}

// NewPostingSet returns an empty set.
func NewPostingSet() *PostingSet {
	return &PostingSet{seen: map[core.RecordLocation]struct{}{}}
}

// Add adds a posting unless its record is already in the set.
func (set *PostingSet) Add(posting *Posting) error {
	location := core.RecordLocation{LogFile: posting.LogFile, Offset: posting.Offset}
	if _, duplicate := set.seen[location]; duplicate {
		return nil
	}
	if set.ctx != nil {
		if err := set.ctx.Err(); err != nil {
			return err
		}
		limits := core.ReadLimitsFromContext(set.ctx)
		if (limits.MaxEvents > 0 && len(set.postings) >= limits.MaxEvents) || (limits.MaxBytes > 0 && set.bytes+posting.Length > limits.MaxBytes) {
			return core.ErrResourceLimit
		}
	}
	set.seen[location] = struct{}{}
	set.postings = append(set.postings, posting)
	set.bytes += posting.Length
	return nil
}

// Len returns the number of distinct records in the set.
func (set *PostingSet) Len() int { return len(set.postings) }

// InLogOrder returns the postings in commit order: by log file, then
// offset. Timestamps do not influence the order.
func (set *PostingSet) InLogOrder() []*Posting {
	sort.Slice(set.postings, func(i, j int) bool { return isBeforeInLogOrder(set.postings[i], set.postings[j]) })
	return set.postings
}

// collectPostingsForKeys scans each required shard of a category once and
// adds postings whose exact key is one of keys.
func (index *Index) collectPostingsForKeys(snapshot *Snapshot, category string, keys []string, set *PostingSet) error {
	keysByShard := map[int]map[string]struct{}{}
	for _, key := range keys {
		shard := shardNumberForKey(key)
		if keysByShard[shard] == nil {
			keysByShard[shard] = map[string]struct{}{}
		}
		keysByShard[shard][key] = struct{}{}
	}
	shards := make([]int, 0, len(keysByShard))
	for shard := range keysByShard {
		shards = append(shards, shard)
	}
	sort.Ints(shards)
	for _, shard := range shards {
		wanted := keysByShard[shard]
		shardKeys := make([]string, 0, len(wanted))
		for key := range wanted {
			shardKeys = append(shardKeys, key)
		}
		if err := index.scanShard(snapshot, index.paths.shard(category, shard), ScanOptions{LineFilter: index.codec.keysFilter(shardKeys...)}, func(posting *Posting) error {
			if _, isWanted := wanted[posting.Key]; isWanted {
				return set.Add(posting)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// collectAllPostings adds every regular event, scanning each exact subject
// shard once.
func (index *Index) collectAllPostings(snapshot *Snapshot, set *PostingSet) error {
	for shard := 0; shard < ShardCount; shard++ {
		if err := index.scanShard(snapshot, index.paths.shard(categoryEventsBySubject, shard), ScanOptions{}, func(posting *Posting) error {
			return set.Add(posting)
		}); err != nil {
			return err
		}
	}
	return nil
}

// Select adds the postings of a selector to set.
func (index *Index) Select(snapshot *Snapshot, selector core.EventSelector, set *PostingSet) error {
	if selector.All {
		return index.collectAllPostings(snapshot, set)
	}
	if err := index.collectPostingsForKeys(snapshot, categoryEventsBySubject, selector.ExactSubjects, set); err != nil {
		return err
	}
	if err := index.collectPostingsForKeys(snapshot, categoryEventsBySubjectWithAncestors, selector.SubjectsWithDescendants, set); err != nil {
		return err
	}
	return index.collectPostingsForKeys(snapshot, categoryEventTypes, selector.EventTypes, set)
}

// SubjectCursor remembers how far a subscription has read the shard of a
// subject. It is bound to one index build: after a rebuild or restore the
// next read starts over, and callers must deduplicate.
type SubjectCursor struct {
	subject string
	build   string
	offset  int64
	// Physical high-water marks survive index rebuilds without retaining IDs.
	logEnds map[string]int64
	// hasMore marks a cursor whose poll stopped at the read budget.
	hasMore bool
}

// PostingsOfSubjectSince returns the postings of a subject and its
// descendants published after cursor, and the cursor for the next call. A
// nil cursor, or one of another build, reads the whole history.
//
// The result is bounded by the read limits of the snapshot context. When the
// budget is reached, the scan pauses at a record boundary and the returned
// cursor resumes there, so a subscriber that falls behind catches up in
// several bounded batches instead of failing on every poll. HasMore reports
// whether the shard holds further matching postings beyond the returned
// batch. A nil cursor with a zero budget means unbounded.
func (index *Index) PostingsOfSubjectSince(snapshot *Snapshot, subject string, cursor *SubjectCursor) ([]*Posting, *SubjectCursor, error) {
	relativePath := index.paths.shard(categoryEventsBySubjectWithAncestors, shardNumberForKey(subject))
	startOffset := int64(0)
	if cursor != nil && cursor.subject == subject && cursor.build == snapshot.build {
		startOffset = cursor.offset
	}
	ctx := snapshot.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	limits := core.ReadLimitsFromContext(ctx)
	set := NewPostingSetContext(ctx)
	logEnds := map[string]int64{}
	if cursor != nil && cursor.subject == subject {
		for name, end := range cursor.logEnds {
			logEnds[name] = end
		}
	}
	var pendingBytes int64
	hasMore := false
	resumeOffset, err := index.scanShardResumable(snapshot, relativePath, ScanOptions{StartOffset: startOffset, LineFilter: index.codec.keysFilter(subject)}, func(posting *Posting) error {
		if posting.Key != subject || posting.Offset < logEnds[posting.LogFile] {
			return nil
		}
		// Pause before the record that would exceed the budget, so the
		// cursor resumes exactly there. A budget that cannot even hold one
		// record is a hard error.
		exceedsEvents := limits.MaxEvents > 0 && len(set.postings) >= limits.MaxEvents
		exceedsBytes := limits.MaxBytes > 0 && pendingBytes+posting.Length > limits.MaxBytes
		if exceedsEvents || exceedsBytes {
			if len(set.postings) == 0 {
				return core.ErrResourceLimit
			}
			hasMore = true
			return errPauseScan
		}
		pendingBytes += posting.Length
		return set.Add(posting)
	})
	if err != nil {
		return nil, nil, err
	}
	postings := set.InLogOrder()
	for _, posting := range postings {
		end := posting.Offset + posting.Length + 1
		if end > logEnds[posting.LogFile] {
			logEnds[posting.LogFile] = end
		}
	}
	next := &SubjectCursor{subject: subject, build: snapshot.build, offset: resumeOffset, logEnds: logEnds, hasMore: hasMore}
	return postings, next, nil
}

// HasMore reports whether the poll that produced the cursor stopped at the
// read budget and the next poll continues immediately.
func (cursor *SubjectCursor) HasMore() bool { return cursor != nil && cursor.hasMore }

// HasSubjectOrDescendant reports whether the subject or a descendant has an
// event, without reading any event.
func (index *Index) HasSubjectOrDescendant(snapshot *Snapshot, subject string) (bool, error) {
	isFound := false
	err := index.scanShard(snapshot, index.paths.shard(categoryEventsBySubjectWithAncestors, shardNumberForKey(subject)), ScanOptions{LineFilter: index.codec.keysFilter(subject)}, func(posting *Posting) error {
		if posting.Key == subject {
			isFound = true
			return fsutil.ErrStopScan
		}
		return nil
	})
	if errors.Is(err, fsutil.ErrStopScan) {
		err = nil
	}
	return isFound, err
}

// FindFirstPostingOfEventID returns the posting of the first physical record
// with the ID, or nil.
func (index *Index) FindFirstPostingOfEventID(snapshot *Snapshot, eventID string) (*Posting, error) {
	var first *Posting
	err := index.scanShard(snapshot, index.paths.shard(categoryEventsByID, shardNumberForKey(eventID)), ScanOptions{LineFilter: index.codec.keysFilter(eventID)}, func(posting *Posting) error {
		if posting.Key != eventID {
			return nil
		}
		if first == nil || posting.LogFile < first.LogFile || (posting.LogFile == first.LogFile && posting.Offset < first.Offset) {
			first = posting
		}
		return nil
	})
	return first, err
}

// FindLatestPostingOfSubject returns the newest posting whose subject equals
// subject, or nil. The exact subject shard holds the same postings a
// separate latest event category would.
func (index *Index) FindLatestPostingOfSubject(snapshot *Snapshot, subject string) (*Posting, error) {
	return index.findLatestPosting(snapshot, index.paths.shard(categoryEventsBySubject, shardNumberForKey(subject)), index.codec.keysFilter(subject), func(posting *Posting) bool {
		return posting.Key == subject
	})
}

// FindLatestPostingOfSubjectAndType returns the newest posting with the
// exact subject and type, or nil.
func (index *Index) FindLatestPostingOfSubjectAndType(snapshot *Snapshot, subject, eventType string) (*Posting, error) {
	return index.findLatestPosting(snapshot, index.paths.shard(categoryLatestEventOfSubjectAndType, shardNumberForSubjectAndType(subject, eventType)), index.codec.pairFilter(subject, eventType), func(posting *Posting) bool {
		return posting.Subject == subject && posting.EventType == eventType
	})
}

func (index *Index) findLatestPosting(snapshot *Snapshot, relativePath string, filter LineFilter, matches func(posting *Posting) bool) (*Posting, error) {
	var latest *Posting
	err := index.scanShard(snapshot, relativePath, ScanOptions{LineFilter: filter}, func(posting *Posting) error {
		if matches(posting) && (latest == nil || isBeforeInLogOrder(latest, posting)) {
			latest = posting
		}
		return nil
	})
	return latest, err
}

// ScanSubjectCatalog visits one posting per indexed event from the catalog.
func (index *Index) ScanSubjectCatalog(snapshot *Snapshot, visit func(posting *Posting) error) error {
	return index.scanShard(snapshot, index.paths.catalog(), ScanOptions{}, visit)
}

// ExactSubjectsWithPrefix returns every indexed subject that lexically starts
// with prefix.
func (index *Index) ExactSubjectsWithPrefix(snapshot *Snapshot, prefix string) ([]string, error) {
	subjects := map[string]struct{}{}
	if err := index.ScanSubjectCatalog(snapshot, func(posting *Posting) error {
		if len(posting.Subject) >= len(prefix) && posting.Subject[:len(prefix)] == prefix {
			subjects[posting.Subject] = struct{}{}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	result := make([]string, 0, len(subjects))
	for subject := range subjects {
		result = append(result, subject)
	}
	sort.Strings(result)
	return result, nil
}

// SelectExactSubjects adds the postings of exact subjects to set.
func (index *Index) SelectExactSubjects(snapshot *Snapshot, subjects []string, set *PostingSet) error {
	return index.collectPostingsForKeys(snapshot, categoryEventsBySubject, subjects, set)
}
