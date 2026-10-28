package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

var errInjected = errors.New("injected fault")

// storageFormats runs a test in both storage formats. Legacy directories
// are created by writing the legacy log headers before the first open.
var storageFormats = []layout.Format{layout.FormatCurrent, layout.FormatLegacy}

// newTestDirectory returns an empty data directory in the given format.
func newTestDirectory(t *testing.T, format layout.Format) string {
	t.Helper()
	directory := t.TempDir()
	if format == layout.FormatLegacy {
		events := filepath.Join(directory, layout.EventsDirectory)
		if err := os.MkdirAll(events, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(events, format.MainEventLogFile()), format.LogHeaderLine(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

type testStoreOptions struct {
	isCacheEnabled bool
	faults         fsutil.FaultInjector
	// convertLegacy opens with the production default, which migrates a
	// legacy directory. The store tests otherwise keep the legacy format, so
	// both on-disk formats stay covered.
	convertLegacy bool
}

func openTestStore(t *testing.T, directory string, options testStoreOptions) *Store {
	t.Helper()
	s, err := tryOpenTestStore(directory, options)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func tryOpenTestStore(directory string, options testStoreOptions) (*Store, error) {
	cacheSize := -1
	if options.isCacheEnabled {
		cacheSize = 0
	}
	policy := KeepExistingFormat
	if options.convertLegacy {
		policy = ConvertToCurrentFormat
	}
	return Open(directory, Options{CacheSize: cacheSize, FormatPolicy: policy, Logger: discardLogger(), Faults: options.faults, EvaluateGDBQLPrecondition: countPrecondition})
}

// countPrecondition evaluates "COUNT <subject> == <n>" without the GDBQL
// engine, which keeps the store module independent of package query.
func countPrecondition(ctx context.Context, source core.EventSource, text string) (bool, error) {
	var subject string
	var expected int
	if _, err := fmt.Sscanf(text, "COUNT %s == %d", &subject, &expected); err != nil {
		return false, err
	}
	events, err := source.SelectEvents(ctx, core.EventSelector{ExactSubjects: []string{subject}})
	return len(events) == expected, err
}

func newTestEvent(subject, eventType string, data map[string]any) *core.Event {
	encoded, _ := json.Marshal(data)
	return &core.Event{Source: "test", Subject: subject, Type: eventType, Data: encoded}
}

func commitTestEvent(t *testing.T, s *Store, subject, eventType string) *core.Event {
	t.Helper()
	event := newTestEvent(subject, eventType, map[string]any{"subject": subject})
	if err := s.Commit(context.Background(), []*core.Event{event}); err != nil {
		t.Fatalf("commit %s: %v", subject, err)
	}
	return event
}

// readEventLogDirectly returns every regular event of the main log in
// commit (log) order, read without indexes. It is the reference
// implementation reads are compared against.
func readEventLogDirectly(t *testing.T, s *Store) []*core.Event {
	t.Helper()
	var events []*core.Event
	if _, _, err := s.storage.ScanEventLog(s.storage.MainEventLogFile(), 0, func(stored core.StoredEvent) error {
		if !stored.Event.IsSystemEvent() {
			events = append(events, stored.Event)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return events
}

func eventIDs(events []*core.Event) []string {
	ids := make([]string, len(events))
	for position, event := range events {
		ids[position] = event.ID
	}
	return ids
}

func idsMatching(events []*core.Event, matches func(*core.Event) bool) []string {
	ids := []string{}
	for _, event := range events {
		if matches(event) {
			ids = append(ids, event.ID)
		}
	}
	return ids
}

func assertSameIDs(t *testing.T, label string, got []*core.Event, want []string) {
	t.Helper()
	gotIDs := eventIDs(got)
	if len(gotIDs) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Errorf("%s: got %v want %v", label, gotIDs, want)
	}
}

// failAtNthCall fails the n-th hit of a point and every later hit of any
// point, which simulates a crash that stops all further durable progress.
func failAtNthCall(point string, n int) fsutil.FaultInjector {
	var hits int32
	var hasCrashed atomic.Bool
	return func(candidate string) error {
		if hasCrashed.Load() {
			return errInjected
		}
		if candidate == point && int(atomic.AddInt32(&hits, 1)) >= n {
			hasCrashed.Store(true)
			return errInjected
		}
		return nil
	}
}

// switchableFaults lets a test turn fault injection on for one operation.
type switchableFaults struct {
	active atomic.Pointer[fsutil.FaultInjector]
}

func (switchable *switchableFaults) injector() fsutil.FaultInjector {
	return func(point string) error {
		if inject := switchable.active.Load(); inject != nil {
			return (*inject)(point)
		}
		return nil
	}
}

func (switchable *switchableFaults) during(inject fsutil.FaultInjector, operation func()) {
	switchable.active.Store(&inject)
	defer switchable.active.Store(nil)
	operation()
}

// assertConsistentWithEventLog checks every read path against the raw log
// and returns the raw events.
func assertConsistentWithEventLog(t *testing.T, s *Store) []*core.Event {
	t.Helper()
	ctx := context.Background()
	raw := readEventLogDirectly(t, s)
	all, err := s.FetchAll(ctx)
	if err != nil {
		t.Fatalf("read after recovery: %v", err)
	}
	assertSameIDs(t, "all after recovery", all, eventIDs(raw))
	if count, _ := s.CountEvents(); int(count) != len(raw) {
		t.Fatalf("count %d, raw %d", count, len(raw))
	}
	for _, event := range raw {
		if found, err := s.FindEventByID(ctx, event.ID); err != nil || found == nil {
			t.Fatalf("event %s not indexed after recovery: %v", event.ID, err)
		}
	}
	report, err := s.VerifyHashChain(ctx)
	if err != nil || !report.IsValid() {
		t.Fatalf("hash chain broken after recovery: %+v %v", report, err)
	}
	return raw
}

func logPath(s *Store, name string) string {
	return filepath.Join(s.storage.EventsDirectory(), name)
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
