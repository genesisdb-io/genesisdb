package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// memoryEventLog is a core.EventLogReader without files, which shows that
// the index engine works over any event log implementation.
type memoryEventLog struct {
	format layout.Format
	events []*core.Event
}

func (log *memoryEventLog) lines() []string {
	lines := []string{log.format.LogHeader()}
	for _, event := range log.events {
		encoded, _ := json.Marshal(map[string]any{"event": event})
		lines = append(lines, string(encoded))
	}
	return lines
}

func (log *memoryEventLog) ListEventLogs() ([]string, error) {
	return []string{log.format.MainEventLogFile()}, nil
}

func (log *memoryEventLog) EventLogSize(string) (int64, error) {
	return int64(len(strings.Join(log.lines(), "\n")) + 1), nil
}

func (log *memoryEventLog) ScanEventLog(logFile string, startOffset int64, visit func(core.StoredEvent) error) (int64, *core.StoredEvent, error) {
	offset := int64(0)
	var last *core.StoredEvent
	for position, line := range log.lines() {
		lineOffset := offset
		offset += int64(len(line)) + 1
		if position == 0 || lineOffset < startOffset {
			continue
		}
		stored := core.StoredEvent{Event: log.events[position-1], Location: core.RecordLocation{LogFile: logFile, Offset: lineOffset, Length: int64(len(line))}}
		last = &stored
		if err := visit(stored); err != nil {
			return 0, nil, err
		}
	}
	return offset, last, nil
}

func (log *memoryEventLog) ReadEventAt(location core.RecordLocation) (*core.Event, error) {
	offset := int64(0)
	for position, line := range log.lines() {
		if offset == location.Offset && position > 0 {
			return log.events[position-1], nil
		}
		offset += int64(len(line)) + 1
	}
	return nil, errors.New("no record")
}

func newMemoryEvent(subject, eventType string, position int) *core.Event {
	return &core.Event{Subject: subject, Type: eventType, ID: fmt.Sprintf("id-%d", position), Hash: fmt.Sprintf("hash-%d", position), Time: time.Date(2026, 1, 1, 0, 0, position, 0, time.UTC)}
}

func TestIndex_StandaloneOverAnyEventLog(t *testing.T) {
	for _, format := range []layout.Format{layout.FormatCurrent, layout.FormatLegacy} {
		t.Run(format.String(), func(t *testing.T) {
			log := &memoryEventLog{format: format}
			for position, subject := range []string{"/a/1", "/a/2", "/b/1", "/a/1"} {
				log.events = append(log.events, newMemoryEvent(subject, []string{"x", "y"}[position%2], position))
			}
			directory := t.TempDir()
			index := New(directory, log, Options{Format: format})
			if err := index.Open(); err != nil {
				t.Fatal(err)
			}
			snapshot, err := index.AcquireSnapshot()
			if err != nil {
				t.Fatal(err)
			}
			set := NewPostingSet()
			index.Select(snapshot, core.EventSelector{SubjectsWithDescendants: []string{"/a"}}, set)
			var ids []string
			for _, posting := range set.InLogOrder() {
				ids = append(ids, posting.EventID)
			}
			if !reflect.DeepEqual(ids, []string{"id-0", "id-1", "id-3"}) {
				t.Fatalf("selected %v", ids)
			}
			latest, _ := index.FindLatestPostingOfSubject(snapshot, "/a/1")
			if latest.EventID != "id-3" {
				t.Fatal("latest posting")
			}
			if exists, _ := index.HasSubjectOrDescendant(snapshot, "/b"); !exists {
				t.Fatal("HasSubjectOrDescendant")
			}
			filepath.Walk(filepath.Join(directory, layout.IndexesDirectory), func(path string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() && !strings.HasSuffix(path, format.Extension()) {
					t.Errorf("index file with the wrong extension: %s", path)
				}
				return nil
			})
		})
	}
}

func TestRouting_IsStable(t *testing.T) {
	for _, key := range []string{"", "/", "/user/123", "üml/äöü", "日本/語", "a\x00b"} {
		sum := sha256.Sum256([]byte(key))
		if shardNumberForKey(key) != int(sum[0]) {
			t.Errorf("shardNumberForKey(%q)", key)
		}
	}
	if hex.EncodeToString(subjectAndTypeRoutingKey("a", "bc")) == hex.EncodeToString(subjectAndTypeRoutingKey("ab", "c")) {
		t.Error("ambiguous pair encoding")
	}
	if !reflect.DeepEqual(subjectAndTypeRoutingKey("a", "bc"), []byte{0, 0, 0, 1, 'a', 0, 0, 0, 2, 'b', 'c'}) {
		t.Error("pair encoding changed")
	}
}

// The posting and header encodings are part of the index format of 1.1.6.
func TestEncoding_MatchesVersion116(t *testing.T) {
	posting := encodePosting(&Posting{Key: "k", EventID: "i", Timestamp: "2026-01-01T00:00:00Z", LogFile: "events.eventstore", Offset: 11, Length: 5})
	if string(posting) != `{"k":"k","s":"","t":"","id":"i","ts":"2026-01-01T00:00:00Z","src":"events.eventstore","off":11,"len":5,"h":"","g":""}`+"\n" {
		t.Errorf("posting format changed: %s", posting)
	}
	header := encodeShardHeader(jsonRecordVersion, categoryEventTypes, 10)
	if string(header) != `{"format":"genesisdb-sharded-index","version":2,"category":"event-types","shard":"0a","shardCount":256}`+"\n" {
		t.Errorf("header format changed: %s", header)
	}
	if err := validateShardHeader(header[:len(header)-1], jsonRecordVersion, categoryEventTypes, 11); err == nil {
		t.Error("wrong shard accepted")
	}
	if err := validateShardHeader(header[:len(header)-1], compactRecordVersion, categoryEventTypes, 10); !errors.Is(err, errOtherRecordVersion) {
		t.Errorf("version 2 records read as version 3: %v", err)
	}
	future := []byte(`{"format":"genesisdb-sharded-index","version":4,"category":"event-types","shard":"0a","shardCount":256}`)
	if err := validateShardHeader(future, compactRecordVersion, categoryEventTypes, 10); !errors.Is(err, core.ErrUnsupportedIndexFormat) {
		t.Errorf("future version: %v", err)
	}
	parsed := Posting{LogFile: "../x", Length: 1, Timestamp: "2026-01-01T00:00:00Z"}
	if parsed.validate() == nil {
		t.Error("path escape accepted")
	}
}

func TestLineFilters_MatchTheEncoder(t *testing.T) {
	keys := []string{"/a", "/a\"b", "ü", "<tag>", "a\\b", ""}
	for _, key := range keys {
		line := encodePosting(&Posting{Key: key, Subject: "s"})
		if !matchKeysFilter(key)(line) {
			t.Errorf("filter rejects its own key %q", key)
		}
		for _, other := range keys {
			if other != key && matchKeysFilter(other)(line) {
				t.Errorf("filter for %q accepts %q", other, key)
			}
		}
	}
	line := encodePosting(&Posting{Subject: "/s", EventType: "t"})
	if !matchSubjectAndTypeFilter("/s", "t")(line) || matchSubjectAndTypeFilter("/s", "u")(line) {
		t.Error("pair filter")
	}
}
