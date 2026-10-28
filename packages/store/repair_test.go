package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// damageIndexFilesInPlace overwrites bytes in the middle of every shard file
// without changing its size, so startup validation (sizes and state journal)
// accepts the index and the damage only surfaces while reading.
func damageIndexFilesInPlace(t *testing.T, directory string, format layout.Format) int {
	t.Helper()
	damaged := 0
	err := filepath.WalkDir(filepath.Join(directory, layout.IndexesDirectory), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if _, isShard := format.ParseShardFileName(entry.Name()); !isShard {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		header := 0
		for header < len(content) && content[header] != '\n' {
			header++
		}
		body := len(content) - header - 1
		if body < 16 {
			return nil
		}
		position := header + 1 + body/2
		copy(content[position:], "\x00garbage\x00")
		damaged++
		return os.WriteFile(path, content, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return damaged
}

func seedRepairStore(t *testing.T, s *Store) {
	t.Helper()
	for position := 0; position < 300; position++ {
		subject := "/customer/" + string(rune('a'+position%7)) + "/" + string(rune('a'+position%13))
		commitTestEvent(t, s, subject, "io.test.type-"+string(rune('a'+position%5)))
	}
}

func TestRepair_DamagedIndexIsRebuiltOnRead(t *testing.T) {
	for _, format := range storageFormats {
		t.Run(format.String(), func(t *testing.T) {
			directory := newTestDirectory(t, format)
			s := openTestStore(t, directory, testStoreOptions{})
			seedRepairStore(t, s)
			want := readEventLogDirectly(t, s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if damageIndexFilesInPlace(t, directory, format) == 0 {
				t.Fatal("no index file damaged")
			}

			s = openTestStore(t, directory, testStoreOptions{})
			defer s.Close()
			ctx := context.Background()
			count, err := s.CountSelectedEvents(ctx, core.EventSelector{All: true})
			if err != nil || count != len(want) {
				t.Fatalf("count after damage: %d, %v, want %d", count, err, len(want))
			}
			all, err := s.Fetch(ctx, "/", FetchOptions{})
			if err != nil {
				t.Fatal(err)
			}
			assertSameIDs(t, "fetch /", all, eventIDs(want))
			byType, err := s.SelectEvents(ctx, core.EventSelector{EventTypes: []string{"io.test.type-c"}})
			if err != nil {
				t.Fatal(err)
			}
			assertSameIDs(t, "by type", byType, idsMatching(want, func(event *core.Event) bool { return event.Type == "io.test.type-c" }))
			assertConsistentWithEventLog(t, s)
		})
	}
}

// A commit holds the writer lock while it checks preconditions, so it
// cannot rebuild synchronously. It must fail without writing anything and
// leave the repair to run after it released the lock.
func TestRepair_DamageFoundByPreconditionIsRepairedAfterCommit(t *testing.T) {
	directory := newTestDirectory(t, layout.FormatCurrent)
	s := openTestStore(t, directory, testStoreOptions{})
	seedRepairStore(t, s)
	want := readEventLogDirectly(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	damageIndexFilesInPlace(t, directory, layout.FormatCurrent)

	s = openTestStore(t, directory, testStoreOptions{})
	defer s.Close()
	ctx := context.Background()
	preconditions := make([]core.Precondition, 0, 91)
	for first := 0; first < 7; first++ {
		for second := 0; second < 13; second++ {
			subject := "/customer/" + string(rune('a'+first)) + "/" + string(rune('a'+second))
			preconditions = append(preconditions, core.Precondition{Type: core.PreconditionIsSubjectExisting, Payload: []byte(`{"subject":"` + subject + `"}`)})
		}
	}
	event := newTestEvent("/order/1", "io.test.order", map[string]any{"n": 1})
	err := s.Commit(ctx, []*core.Event{event}, preconditions...)
	if err != nil && !errors.Is(err, core.ErrIndexCorrupt) {
		t.Fatalf("commit: %v", err)
	}
	if err != nil {
		if got := readEventLogDirectly(t, s); len(got) != len(want) {
			t.Fatalf("failed commit wrote events: %d, want %d", len(got), len(want))
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			err = s.Commit(ctx, []*core.Event{newTestEvent("/order/1", "io.test.order", map[string]any{"n": 1})}, preconditions...)
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("commit after repair: %v", err)
		}
	}
	assertConsistentWithEventLog(t, s)
}

// A repair that cannot rebuild leaves the store unavailable instead of
// serving reads from damaged data.
func TestRepair_ClosedStoreDoesNotRebuild(t *testing.T) {
	directory := newTestDirectory(t, layout.FormatCurrent)
	s := openTestStore(t, directory, testStoreOptions{})
	seedRepairStore(t, s)
	snapshot, err := s.index.AcquireSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.repairIndexAfterCorruption(snapshot.Build(), core.ErrIndexCorrupt) {
		t.Fatal("closed store reported a repaired index")
	}
}

// Concurrent readers that hit the same damage rebuild only once.
func TestRepair_ConcurrentReadersRebuildOnce(t *testing.T) {
	directory := newTestDirectory(t, layout.FormatCurrent)
	s := openTestStore(t, directory, testStoreOptions{})
	seedRepairStore(t, s)
	want := readEventLogDirectly(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	damageIndexFilesInPlace(t, directory, layout.FormatCurrent)
	s = openTestStore(t, directory, testStoreOptions{})
	defer s.Close()
	before, err := s.index.AcquireSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 8)
	builds := make(chan string, 8)
	for reader := 0; reader < 8; reader++ {
		go func() {
			events, err := s.Fetch(context.Background(), "/", FetchOptions{})
			if err == nil && len(events) != len(want) {
				err = errors.New("incomplete fetch")
			}
			snapshot, _ := s.index.AcquireSnapshot()
			builds <- snapshot.Build()
			errs <- err
		}()
	}
	seen := map[string]bool{}
	for reader := 0; reader < 8; reader++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		seen[<-builds] = true
	}
	keys := make([]string, 0, len(seen))
	for build := range seen {
		keys = append(keys, build)
	}
	sort.Strings(keys)
	if len(keys) != 1 || keys[0] == before.Build() {
		t.Fatalf("expected exactly one new build, have %v (before %s)", keys, before.Build())
	}
}
