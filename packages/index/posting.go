package index

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// Posting is one index record. It references an event record by location and
// carries the exact lookup key, so hash collisions never return unrelated
// events.
//
// The JSON field names and their order are part of the index format: every
// line starts with {"k":, which LineFilter relies on.
type Posting struct {
	// Key is the exact lookup key: subject, ancestor, type or event ID. It
	// is empty in the subject and type pair category and in the catalog.
	Key string `json:"k"`
	// Subject and EventType are the exact values of the event.
	Subject   string `json:"s"`
	EventType string `json:"t"`
	// EventID is the ID of the event.
	EventID string `json:"id"`
	// Timestamp is the event time, UTC, RFC 3339 with nanoseconds. Only
	// records of version 2 carry it, reads do not use it: commit order is
	// log order.
	Timestamp string `json:"ts"`
	// LogFile, Offset and Length locate the event record.
	LogFile string `json:"src"`
	Offset  int64  `json:"off"`
	Length  int64  `json:"len"`
	// EventHash is the persisted event hash, used to verify offset reads.
	EventHash string `json:"h"`
	// CommitGroup identifies the commit or build that wrote the posting.
	// Only records of version 2 carry it.
	CommitGroup string `json:"g"`
}

// Location returns the record location of the posting.
func (posting *Posting) Location() core.RecordLocation {
	return core.RecordLocation{LogFile: posting.LogFile, Offset: posting.Offset, Length: posting.Length}
}

// Matches reports whether a record read from the posting location is the
// event the posting refers to.
//
// Records of version 3 store a prefix of the event hash, which is enough to
// detect a location that points at another record. An empty stored hash
// only matches an event without hash.
func (posting *Posting) Matches(event *core.Event) bool {
	hashMatches := strings.HasPrefix(event.Hash, posting.EventHash) && (posting.EventHash != "" || event.Hash == "")
	return hashMatches && event.ID == posting.EventID && event.Subject == posting.Subject && event.Type == posting.EventType
}

func (posting *Posting) validate() error {
	if posting.LogFile == "" || strings.ContainsAny(posting.LogFile, "/\\") || posting.LogFile == "." || posting.LogFile == ".." {
		return newCorruptionError("invalid posting log file %q", posting.LogFile)
	}
	if posting.Offset < 0 || posting.Length <= 0 || posting.Length >= maxRecordLength {
		return newCorruptionError("invalid posting location %d+%d", posting.Offset, posting.Length)
	}
	return nil
}

// isBeforeInLogOrder orders postings by physical position: log file, then
// offset. This is the commit order and the order of the hash chain. Event
// timestamps are deliberately not consulted, a clock that steps backwards
// must not reorder "created" after "deleted".
func isBeforeInLogOrder(first, second *Posting) bool {
	if first.LogFile != second.LogFile {
		return first.LogFile < second.LogFile
	}
	return first.Offset < second.Offset
}

func encodePosting(posting *Posting) []byte {
	encoded, _ := json.Marshal(posting)
	return append(encoded, '\n')
}

// postingsForEvent generates every posting of a stored event and passes each
// one with its destination path.
func (p paths) postingsForEvent(stored core.StoredEvent, commitGroup string, add func(relativePath string, posting *Posting)) {
	event := stored.Event
	base := Posting{
		Subject:     event.Subject,
		EventType:   event.Type,
		EventID:     event.ID,
		Timestamp:   event.Time.UTC().Format(time.RFC3339Nano),
		LogFile:     stored.Location.LogFile,
		Offset:      stored.Location.Offset,
		Length:      stored.Location.Length,
		EventHash:   event.Hash,
		CommitGroup: commitGroup,
	}
	bySubject := base
	bySubject.Key = event.Subject
	add(p.shard(categoryEventsBySubject, shardNumberForKey(event.Subject)), &bySubject)
	for _, ancestor := range core.SubjectAndAncestors(event.Subject) {
		byAncestor := base
		byAncestor.Key = ancestor
		add(p.shard(categoryEventsBySubjectWithAncestors, shardNumberForKey(ancestor)), &byAncestor)
	}
	byType := base
	byType.Key = event.Type
	add(p.shard(categoryEventTypes, shardNumberForKey(event.Type)), &byType)
	bySubjectAndType := base
	add(p.shard(categoryLatestEventOfSubjectAndType, shardNumberForSubjectAndType(event.Subject, event.Type)), &bySubjectAndType)
	byID := base
	byID.Key = event.ID
	add(p.shard(categoryEventsByID, shardNumberForKey(event.ID)), &byID)
	catalogEntry := base
	add(p.catalog(), &catalogEntry)
}

// LineFilter is a cheap byte check on a raw posting line. Rejected lines are
// skipped without JSON decoding. A filter must never reject a line whose
// posting the caller would accept.
type LineFilter func(line []byte) bool

// matchKeysFilter accepts only lines whose exact key is one of keys. Every
// line starts with {"k":<key>,"s": because encodePosting uses the same
// encoder, so the prefix is byte identical.
func matchKeysFilter(keys ...string) LineFilter {
	prefixes := make([][]byte, 0, len(keys))
	for _, key := range keys {
		prefixes = append(prefixes, postingLinePrefix(key))
	}
	if len(prefixes) == 1 {
		prefix := prefixes[0]
		return func(line []byte) bool { return bytes.HasPrefix(line, prefix) }
	}
	return func(line []byte) bool {
		for _, prefix := range prefixes {
			if bytes.HasPrefix(line, prefix) {
				return true
			}
		}
		return false
	}
}

// matchSubjectAndTypeFilter accepts only pair category lines (empty key) of
// the exact subject and type.
func matchSubjectAndTypeFilter(subject, eventType string) LineFilter {
	prefix := postingLinePrefix("")
	prefix = append(prefix, jsonString(subject)...)
	prefix = append(prefix, `,"t":`...)
	prefix = append(prefix, jsonString(eventType)...)
	prefix = append(prefix, ',')
	return func(line []byte) bool { return bytes.HasPrefix(line, prefix) }
}

func postingLinePrefix(key string) []byte {
	prefix := append([]byte(`{"k":`), jsonString(key)...)
	return append(prefix, `,"s":`...)
}

func jsonString(value string) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}
