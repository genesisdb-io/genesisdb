package index

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

func countOpenHandles(index *Index) int {
	index.handles.mutex.Lock()
	defer index.handles.mutex.Unlock()
	return index.handles.order.Len()
}

func publishMemoryEvents(t *testing.T, index *Index, log *memoryEventLog, events ...*core.Event) {
	t.Helper()
	start := len(log.events)
	log.events = append(log.events, events...)
	var stored []core.StoredEvent
	if _, _, err := log.ScanEventLog(log.format.MainEventLogFile(), 0, func(event core.StoredEvent) error {
		stored = append(stored, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	size, _ := log.EventLogSize(log.format.MainEventLogFile())
	if err := index.Publish(stored[start:], size); err != nil {
		t.Fatal(err)
	}
}

func selectSubjectIDs(t *testing.T, index *Index, subject string) []string {
	t.Helper()
	snapshot, err := index.AcquireSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	set := NewPostingSet()
	if err := index.Select(snapshot, core.EventSelector{SubjectsWithDescendants: []string{subject}}, set); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, posting := range set.InLogOrder() {
		ids = append(ids, posting.EventID)
	}
	return ids
}

// Shard handles stay open between commits and are bounded by the limit.
// Evicted and reused handles must write at the published boundary, never
// at a stale offset.
func TestHandles_ReuseAndEvictionKeepShardsConsistent(t *testing.T) {
	previous := maxOpenShardHandles
	maxOpenShardHandles = 4
	defer func() { maxOpenShardHandles = previous }()

	log := &memoryEventLog{format: layout.FormatCurrent}
	index := New(t.TempDir(), log, Options{Format: layout.FormatCurrent})
	if err := index.Open(); err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	want := map[string][]string{}
	for position := 0; position < 60; position++ {
		subject := "/s/" + string(rune('a'+position%9))
		event := newMemoryEvent(subject, "t", position)
		publishMemoryEvents(t, index, log, event)
		want[subject] = append(want[subject], event.ID)
		if open := countOpenHandles(index); open > maxOpenShardHandles {
			t.Fatalf("%d handles open, limit %d", open, maxOpenShardHandles)
		}
	}
	for subject, ids := range want {
		if got := selectSubjectIDs(t, index, subject); !reflect.DeepEqual(got, ids) {
			t.Fatalf("%s: got %v want %v", subject, got, ids)
		}
	}
}

// A rebuild deletes and rewrites shard files. Handles cached before it must
// not write into the unlinked files afterwards.
func TestHandles_RebuildAndCloseReleaseHandles(t *testing.T) {
	log := &memoryEventLog{format: layout.FormatCurrent}
	directory := t.TempDir()
	index := New(directory, log, Options{Format: layout.FormatCurrent})
	if err := index.Open(); err != nil {
		t.Fatal(err)
	}
	publishMemoryEvents(t, index, log, newMemoryEvent("/a/1", "t", 0))
	if countOpenHandles(index) == 0 {
		t.Fatal("commit did not keep shard handles")
	}
	if err := index.Rebuild(); err != nil {
		t.Fatal(err)
	}
	if open := countOpenHandles(index); open != 0 {
		t.Fatalf("%d handles survived the rebuild", open)
	}
	publishMemoryEvents(t, index, log, newMemoryEvent("/a/1", "t", 1))
	if got := selectSubjectIDs(t, index, "/a"); !reflect.DeepEqual(got, []string{"id-0", "id-1"}) {
		t.Fatalf("after rebuild: %v", got)
	}
	index.Close()
	if open := countOpenHandles(index); open != 0 {
		t.Fatalf("%d handles survived Close", open)
	}

	reopened := New(directory, log, Options{Format: layout.FormatCurrent})
	if err := reopened.Open(); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := selectSubjectIDs(t, reopened, "/a"); !reflect.DeepEqual(got, []string{"id-0", "id-1"}) {
		t.Fatalf("after reopen: %v", got)
	}
}

// A failed shard write discards the handles of the batch, the next commit
// reopens the files and overwrites the unpublished tail.
func TestHandles_FailedWriteDiscardsHandles(t *testing.T) {
	log := &memoryEventLog{format: layout.FormatCurrent}
	failSync := false
	index := New(t.TempDir(), log, Options{Format: layout.FormatCurrent, Faults: func(point string) error {
		if failSync && point == "shard.sync" {
			return os.ErrDeadlineExceeded
		}
		return nil
	}})
	if err := index.Open(); err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	publishMemoryEvents(t, index, log, newMemoryEvent("/a/1", "t", 0))
	before := countOpenHandles(index)
	failSync = true
	log.events = append(log.events, newMemoryEvent("/a/1", "t", 1))
	stored := core.StoredEvent{Event: log.events[1], Location: core.RecordLocation{LogFile: log.format.MainEventLogFile(), Offset: 1, Length: 1}}
	if err := index.Publish([]core.StoredEvent{stored}, 0); err == nil {
		t.Fatal("injected sync failure was ignored")
	}
	// The batch touches every shard of the first commit except the subject
	// catalog, which only changes for new subjects.
	if open := countOpenHandles(index); open != 1 || before < 2 {
		t.Fatalf("%d of %d handles kept after a failed write, want only the untouched catalog", open, before)
	}
	log.events = log.events[:1]
	failSync = false
	publishMemoryEvents(t, index, log, newMemoryEvent("/a/1", "t", 2))
	if got := selectSubjectIDs(t, index, "/a"); !reflect.DeepEqual(got, []string{"id-0", "id-2"}) {
		t.Fatalf("after failed write: %v", got)
	}
	matches, _ := filepath.Glob(filepath.Join(index.root, "*", "*"+layout.FormatCurrent.Extension()))
	if len(matches) == 0 {
		t.Fatal("no shard files")
	}
}
