package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

func TestCommit_AssignsMetadataAndHashChain(t *testing.T) {
	for _, format := range storageFormats {
		t.Run(format.String(), func(t *testing.T) {
			s := openTestStore(t, newTestDirectory(t, format), testStoreOptions{})
			defer s.Close()
			first := commitTestEvent(t, s, "/user/1", "created")
			second := commitTestEvent(t, s, "/user/1", "updated")
			if first.ID == "" || first.SpecVersion != "1.0" || first.DataContentType != "application/json" {
				t.Fatalf("metadata missing: %+v", first)
			}
			if first.PredecessorHash != core.ZeroHash || second.PredecessorHash != first.Hash {
				t.Fatal("hash chain not linked")
			}
			if first.Hash != core.HashEvent(first) {
				t.Fatal("hash mismatch")
			}
			if s.StorageFormat() != format {
				t.Fatalf("format %s", s.StorageFormat())
			}
			header := mustReadFile(t, logPath(s, format.MainEventLogFile()))
			if !strings.HasPrefix(string(header), format.LogHeader()+"\n") {
				t.Fatalf("log header %q", header[:12])
			}
		})
	}
}

func TestCommit_SystemEventsAreRejected(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	event := newTestEvent("/x", "t", map[string]any{})
	event.Source = core.SystemEventSource
	if err := s.Commit(context.Background(), []*core.Event{event}); !errors.Is(err, core.ErrSystemEventNotAllowed) {
		t.Fatalf("got %v", err)
	}
	if err := s.AppendSystemEvent("audit", "/x", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	all, _ := s.FetchAll(context.Background())
	if len(all) != 0 {
		t.Fatal("system events must never be returned")
	}
	system, _ := s.ListSystemEvents()
	if len(system) != 1 || system[0].PredecessorHash != core.ZeroHash {
		t.Fatalf("system events %+v", system)
	}
}

func TestSelectors_MatchEventLog(t *testing.T) {
	for _, format := range storageFormats {
		for _, isCacheEnabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cache=%v", format, isCacheEnabled), func(t *testing.T) {
				ctx := context.Background()
				s := openTestStore(t, newTestDirectory(t, format), testStoreOptions{isCacheEnabled: isCacheEnabled})
				defer s.Close()
				subjects := []string{"/org", "/org/acme", "/org/acme/user/1", "/org/acme/user/2", "/org/acmex/user/1",
					"/org/acme/", "/org//x", "/user/äö", "/a", "/ab", "/a/b/c/d/e/f/g/h"}
				eventTypes := []string{"created", "updated", "deleted"}
				var batch []*core.Event
				for position, subject := range subjects {
					batch = append(batch, newTestEvent(subject, eventTypes[position%3], map[string]any{"position": position}))
				}
				if err := s.Commit(ctx, batch); err != nil {
					t.Fatal(err)
				}
				for position, subject := range subjects {
					commitTestEvent(t, s, subject, eventTypes[(position+1)%3])
				}
				raw := readEventLogDirectly(t, s)

				all, err := s.FetchAll(ctx)
				if err != nil {
					t.Fatal(err)
				}
				assertSameIDs(t, "all", all, eventIDs(raw))

				for _, subject := range append(subjects, "/", "/org/ac", "/nothing", "") {
					fetched, err := s.Fetch(ctx, subject, FetchOptions{})
					if err != nil {
						t.Fatal(err)
					}
					assertSameIDs(t, "subtree "+subject, fetched, idsMatching(raw, func(event *core.Event) bool {
						switch subject {
						case "":
							return false
						case "/":
							return strings.HasPrefix(event.Subject, "/")
						}
						return event.Subject == subject || strings.HasPrefix(event.Subject, subject+"/")
					}))
					exact, err := s.FetchExactSubjects(ctx, subject)
					if err != nil {
						t.Fatal(err)
					}
					assertSameIDs(t, "exact "+subject, exact, idsMatching(raw, func(event *core.Event) bool { return event.Subject == subject }))
					prefixed, err := s.FetchBySubjectPrefix(ctx, subject)
					if err != nil {
						t.Fatal(err)
					}
					assertSameIDs(t, "prefix "+subject, prefixed, idsMatching(raw, func(event *core.Event) bool { return strings.HasPrefix(event.Subject, subject) }))
				}
				for _, eventType := range append(eventTypes, "none") {
					fetched, err := s.FetchByEventType(ctx, eventType)
					if err != nil {
						t.Fatal(err)
					}
					assertSameIDs(t, "type "+eventType, fetched, idsMatching(raw, func(event *core.Event) bool { return event.Type == eventType }))
				}
				for _, event := range raw {
					found, err := s.FindEventByID(ctx, event.ID)
					if err != nil || found.Hash != event.Hash {
						t.Fatalf("by id %s: %v", event.ID, err)
					}
				}
				if _, err := s.FindEventByID(ctx, "missing"); !errors.Is(err, core.ErrNotFound) {
					t.Error("missing ID must be ErrNotFound")
				}
				for _, subject := range subjects {
					latest, err := s.FindLatestEvent(ctx, subject)
					if err != nil {
						t.Fatal(err)
					}
					want := idsMatching(raw, func(event *core.Event) bool { return event.Subject == subject })
					if latest.ID != want[len(want)-1] {
						t.Errorf("latest %s", subject)
					}
					for _, eventType := range eventTypes {
						latestOfType, err := s.FindLatestEventOfType(ctx, subject, eventType)
						wantOfType := idsMatching(raw, func(event *core.Event) bool { return event.Subject == subject && event.Type == eventType })
						if len(wantOfType) == 0 {
							if !errors.Is(err, core.ErrNotFound) {
								t.Errorf("latest of type %s %s: %v", subject, eventType, err)
							}
							continue
						}
						if err != nil || latestOfType.ID != wantOfType[len(wantOfType)-1] {
							t.Errorf("latest of type %s %s", subject, eventType)
						}
					}
				}
				listedSubjects, _ := s.ListSubjects(ctx)
				wantSubjects := append([]string{}, subjects...)
				sort.Strings(wantSubjects)
				if !reflect.DeepEqual(listedSubjects, wantSubjects) {
					t.Errorf("subjects %v", listedSubjects)
				}
				topLevel, _ := s.ListTopLevelSubjects(ctx)
				if !reflect.DeepEqual(topLevel, []string{"/a", "/ab", "/org", "/user"}) {
					t.Errorf("top level %v", topLevel)
				}
				listedTypes, _ := s.ListEventTypes(ctx)
				if !reflect.DeepEqual(listedTypes, []string{"created", "deleted", "updated"}) {
					t.Errorf("types %v", listedTypes)
				}
			})
		}
	}
}

func TestFetch_BoundsAndLatestByEventType(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	var events []*core.Event
	for position := 0; position < 5; position++ {
		events = append(events, commitTestEvent(t, s, fmt.Sprintf("/b/%d", position), []string{"x", "y"}[position%2]))
	}
	id := func(position int) string { return events[position].ID }
	cases := []struct {
		options FetchOptions
		want    []string
	}{
		{FetchOptions{LowerBound: id(1)}, []string{id(2), id(3), id(4)}},
		{FetchOptions{LowerBound: id(1), IncludeLowerBoundEvent: true}, []string{id(1), id(2), id(3), id(4)}},
		{FetchOptions{UpperBound: id(2)}, []string{id(0), id(1)}},
		{FetchOptions{UpperBound: id(2), IncludeUpperBoundEvent: true}, []string{id(0), id(1), id(2)}},
		{FetchOptions{LowerBound: id(1), UpperBound: id(3), IncludeLowerBoundEvent: true}, []string{id(1), id(2)}},
		{FetchOptions{LowerBound: "missing"}, []string{}},
		{FetchOptions{LatestByEventType: "y"}, []string{id(3)}},
		{FetchOptions{LatestByEventType: "none"}, []string{}},
	}
	for _, testCase := range cases {
		fetched, err := s.Fetch(ctx, "/b", testCase.options)
		if err != nil {
			t.Fatal(err)
		}
		assertSameIDs(t, fmt.Sprintf("%+v", testCase.options), fetched, testCase.want)
	}
}

func TestPreconditions_SubjectsAreHierarchical(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	commitTestEvent(t, s, "/user/1", "t")
	payload := func(subject string) json.RawMessage {
		encoded, _ := json.Marshal(map[string]string{"subject": subject})
		return encoded
	}
	err := s.Commit(ctx, []*core.Event{newTestEvent("/user", "t", map[string]any{})}, core.Precondition{Type: core.PreconditionIsSubjectNew, Payload: payload("/user")})
	var failed *core.PreconditionFailedError
	if !errors.As(err, &failed) || failed.Message != "subject /user already has events" {
		t.Fatalf("/user must be taken once /user/1 exists, got %v", err)
	}
	if err := s.Commit(ctx, []*core.Event{newTestEvent("/user", "t", map[string]any{})}, core.Precondition{Type: core.PreconditionIsSubjectExisting, Payload: payload("/user")}); err != nil {
		t.Fatalf("/user must count as existing: %v", err)
	}
	if err := s.Commit(ctx, []*core.Event{newTestEvent("/use", "t", map[string]any{})}, core.Precondition{Type: core.PreconditionIsSubjectNew, Payload: payload("/use")}); err != nil {
		t.Fatalf("/use is new: %v", err)
	}
	if err := s.Commit(ctx, nil, core.Precondition{Type: "unknown"}); err == nil {
		t.Fatal("unknown precondition accepted")
	}
}

func TestPreconditions_ConcurrentIsSubjectNewOnlyOneWins(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	payload, _ := json.Marshal(map[string]string{"subject": "/race/1"})
	var wins, losses int32
	var mutex sync.Mutex
	var group sync.WaitGroup
	for worker := 0; worker < 20; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			err := s.Commit(ctx, []*core.Event{newTestEvent("/race/1", "t", map[string]any{})}, core.Precondition{Type: core.PreconditionIsSubjectNew, Payload: payload})
			mutex.Lock()
			defer mutex.Unlock()
			if err == nil {
				wins++
			} else {
				losses++
			}
		}()
	}
	group.Wait()
	if wins != 1 || losses != 19 {
		t.Fatalf("wins %d losses %d", wins, losses)
	}
}

func TestPreconditions_GDBQLEvaluatorRunsWithoutDeadlock(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	commitTestEvent(t, s, "/q/1", "t")
	query, _ := json.Marshal(map[string]string{"query": "COUNT /q/1 == 1"})
	done := make(chan error, 1)
	go func() {
		done <- s.Commit(ctx, []*core.Event{newTestEvent("/q/2", "t", map[string]any{})}, core.Precondition{Type: core.PreconditionIsQueryResultTrue, Payload: query})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("precondition deadlocked")
	}
}

func TestReferenceData_RoundTripAndErasure(t *testing.T) {
	for _, format := range storageFormats {
		t.Run(format.String(), func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t, newTestDirectory(t, format), testStoreOptions{isCacheEnabled: true})
			defer s.Close()
			event := newTestEvent("/person/1", "registered", map[string]any{"name": "Bruce", "address": map[string]any{"city": "Gotham"}, "tags": []any{"a", 1}})
			event.Options.StoreDataAsReference = true
			if err := s.Commit(ctx, []*core.Event{event}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(event.Data), "Bruce") {
				t.Fatal("commit must return the payload")
			}
			persisted := readEventLogDirectly(t, s)[0]
			if string(persisted.Data) != "{}" || persisted.ReferenceDataKey == "" {
				t.Fatalf("payload must live in the reference data log: %s", persisted.Data)
			}
			fetched, _ := s.Fetch(ctx, "/person/1", FetchOptions{})
			fetched[0].Data = json.RawMessage(`{"mutated":true}`)
			fetchedAgain, _ := s.Fetch(ctx, "/person/1", FetchOptions{})
			if !strings.Contains(string(fetchedAgain[0].Data), "Bruce") {
				t.Fatal("cache returned a shared mutable value")
			}
			report, err := s.Erase(ctx, "/person")
			if err != nil || report.ErasedEntries != 1 {
				t.Fatalf("erase: %+v %v", report, err)
			}
			erased, _ := s.Fetch(ctx, "/person/1", FetchOptions{})
			if string(erased[0].Data) != `{}` {
				t.Fatalf("erased payload %s", erased[0].Data)
			}
			system, _ := s.ListSystemEvents()
			if len(system) != 2 || system[1].Type != ErasureSystemEventType {
				t.Fatal("erasure must write a system event")
			}
			if hashReport, _ := s.VerifyHashChain(ctx); !hashReport.IsValid() {
				t.Fatal("erasure broke the hash chain")
			}
			if _, err := s.Erase(ctx, "/nobody"); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestReferenceData_AtomicRewriteAndCorruption(t *testing.T) {
	ctx := context.Background()
	faults := &switchableFaults{}
	s := openTestStore(t, t.TempDir(), testStoreOptions{faults: faults.injector()})
	defer s.Close()
	event := newTestEvent("/r/1", "x", map[string]any{"name": "Bruce"})
	event.Options.StoreDataAsReference = true
	if err := s.Commit(ctx, []*core.Event{event}); err != nil {
		t.Fatal(err)
	}
	referenceLog := logPath(s, s.StorageFormat().ReferenceDataLogFile())
	before := mustReadFile(t, referenceLog)
	var err error
	faults.during(failAtNthCall("replace.rename:"+filepath.Base(referenceLog), 1), func() {
		_, err = s.ReplaceReferenceData(ctx, map[string]json.RawMessage{event.ReferenceDataKey: json.RawMessage(`{"name":""}`)})
	})
	if err == nil {
		t.Fatal("expected injected failure")
	}
	if string(mustReadFile(t, referenceLog)) != string(before) {
		t.Fatal("reference data changed although the rewrite failed")
	}
	if _, err := os.Stat(referenceLog + ".tmp"); !os.IsNotExist(err) {
		t.Error("temporary file left behind")
	}
	os.WriteFile(referenceLog, []byte(string(s.StorageFormat().LogHeaderLine())+"{not json\n"), 0o644)
	if _, err := s.Fetch(ctx, "/r/1", FetchOptions{}); !errors.Is(err, core.ErrEventLogCorrupt) {
		t.Fatalf("corrupt reference data must fail, got %v", err)
	}
	os.WriteFile(referenceLog, s.StorageFormat().LogHeaderLine(), 0o644)
	if _, err := s.Fetch(ctx, "/r/1", FetchOptions{}); !errors.Is(err, core.ErrEventLogCorrupt) {
		t.Fatalf("missing reference data must fail, got %v", err)
	}
}

func TestBackupAndRestore_RoundTrip(t *testing.T) {
	for _, format := range storageFormats {
		t.Run(format.String(), func(t *testing.T) {
			ctx := context.Background()
			source := openTestStore(t, newTestDirectory(t, format), testStoreOptions{})
			defer source.Close()
			secret := newTestEvent("/p/1", "x", map[string]any{"secret": "v"})
			secret.Options.StoreDataAsReference = true
			if err := source.Commit(ctx, []*core.Event{secret}); err != nil {
				t.Fatal(err)
			}
			commitTestEvent(t, source, "/p/2", "y")
			backup, err := source.Backup(ctx)
			if err != nil || !backup.Extended || string(backup.Events[0].Data) != "{}" {
				t.Fatalf("backup: %+v %v", backup, err)
			}

			target := openTestStore(t, t.TempDir(), testStoreOptions{})
			defer target.Close()
			if err := target.Restore(ctx, backup); err != nil {
				t.Fatal(err)
			}
			restored, _ := target.FetchAll(ctx)
			if len(restored) != 2 || restored[0].ID != backup.Events[0].ID || !strings.Contains(string(restored[0].Data), "secret") {
				t.Fatalf("restored %+v", restored)
			}
			if err := target.Restore(ctx, backup); !errors.Is(err, core.ErrNotEmpty) {
				t.Fatalf("second restore: %v", err)
			}
			commitTestEvent(t, target, "/p/3", "z")
			if report, _ := target.VerifyHashChain(ctx); !report.IsValid() {
				t.Fatal(report.Problem)
			}
		})
	}
}

func TestRestore_RejectsExistingDataAndInvalidBackups(t *testing.T) {
	ctx := context.Background()
	backup := &core.Backup{Events: []*core.Event{newTestEvent("/b/1", "t", map[string]any{})}}
	withSystemEvent := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer withSystemEvent.Close()
	withSystemEvent.AppendSystemEvent("s", "/sys", json.RawMessage(`{}`))
	if err := withSystemEvent.Restore(ctx, backup); !errors.Is(err, core.ErrNotEmpty) {
		t.Fatalf("got %v", err)
	}

	empty := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer empty.Close()
	systemEvent := newTestEvent("/a", "t", map[string]any{})
	systemEvent.Source = core.SystemEventSource
	for _, invalid := range []*core.Backup{
		{Events: []*core.Event{nil}},
		{Events: []*core.Event{systemEvent}},
		{Events: []*core.Event{{Subject: "/a", Type: "t", ReferenceDataKey: "missing", Data: json.RawMessage(`{}`)}}},
	} {
		if err := empty.Restore(ctx, invalid); !errors.Is(err, core.ErrInvalidBackup) {
			t.Fatalf("got %v", err)
		}
	}
	commitTestEvent(t, empty, "/still/works", "t")
}

func TestSubscribe_DeliversHistoryThenNewEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	first := commitTestEvent(t, s, "/o/1", "t")
	excluded := commitTestEvent(t, s, "/o/2", "t")
	subscription, err := s.Subscribe(ctx, "/o", SubscribeOptions{PollInterval: MinimumPollInterval, FetchOptions: FetchOptions{UpperBound: excluded.ID}})
	if err != nil {
		t.Fatal(err)
	}
	history := <-subscription.Batches()
	if !history.IsHistory || len(history.Events) != 1 || history.Events[0].ID != first.ID {
		t.Fatalf("history %+v", history)
	}
	fresh := commitTestEvent(t, s, "/o/3", "t")
	deadline := time.After(5 * time.Second)
	for {
		select {
		case batch := <-subscription.Batches():
			for _, event := range batch.Events {
				if event.ID == excluded.ID {
					t.Fatal("history excluded by the bounds was delivered later")
				}
				if event.ID == fresh.ID {
					return
				}
			}
		case <-deadline:
			t.Fatal("new event not delivered")
		}
	}
}

func TestSubscribe_IncrementalAcrossRebuild(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	commitTestEvent(t, s, "/i/1", "t")
	_, cursor, err := s.fetchSinceCursor(ctx, "/i", nil)
	if err != nil {
		t.Fatal(err)
	}
	commitTestEvent(t, s, "/i/2", "t")
	commitTestEvent(t, s, "/other", "t")
	events, cursor, _ := s.fetchSinceCursor(ctx, "/i", cursor)
	if len(events) != 1 || events[0].Subject != "/i/2" {
		t.Fatalf("incremental poll %+v", events)
	}
	if events, _, _ := s.fetchSinceCursor(ctx, "/i", cursor); len(events) != 0 {
		t.Fatal("poll without commits returned events")
	}
	if _, err := s.RebuildIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	if events, _, _ := s.fetchSinceCursor(ctx, "/i", cursor); len(events) != 0 {
		t.Fatal("physical cursor must not redeliver history after a rebuild")
	}
}

func TestOwnership_SecondOpenIsRejected(t *testing.T) {
	directory := t.TempDir()
	s := openTestStore(t, directory, testStoreOptions{})
	defer s.Close()
	if _, err := tryOpenTestStore(directory, testStoreOptions{}); err == nil {
		t.Fatal("second owner accepted")
	}
}

func TestRebuildIndexes_InPlace(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	for position := 0; position < 20; position++ {
		commitTestEvent(t, s, fmt.Sprintf("/r/%d", position), "t")
	}
	root := filepath.Join(s.DataDirectory(), layout.IndexesDirectory)
	before, _ := os.Stat(root)
	count, err := s.RebuildIndexes(ctx)
	if err != nil || count != 20 {
		t.Fatalf("%d %v", count, err)
	}
	after, _ := os.Stat(root)
	if !os.SameFile(before, after) {
		t.Error("index root replaced")
	}
	commitTestEvent(t, s, "/r/x", "t")
	if events, _ := s.FetchByEventType(ctx, "t"); len(events) != 21 {
		t.Error(len(events))
	}
}

func TestConcurrency_ReadersWritersSubscribersAndRebuild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openTestStore(t, t.TempDir(), testStoreOptions{isCacheEnabled: true})
	defer s.Close()
	var group sync.WaitGroup
	for writer := 0; writer < 4; writer++ {
		group.Add(1)
		go func(writer int) {
			defer group.Done()
			for position := 0; position < 25; position++ {
				if err := s.Commit(ctx, []*core.Event{newTestEvent(fmt.Sprintf("/c/%d/%d", writer, position), "t", map[string]any{})}); err != nil {
					t.Error(err)
					return
				}
			}
		}(writer)
	}
	for reader := 0; reader < 4; reader++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for attempt := 0; attempt < 25; attempt++ {
				if _, err := s.Fetch(ctx, "/c", FetchOptions{}); err != nil && !errors.Is(err, core.ErrNotReady) {
					t.Error(err)
					return
				}
			}
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		if _, err := s.RebuildIndexes(ctx); err != nil {
			t.Error(err)
		}
	}()
	group.Wait()
	if raw := assertConsistentWithEventLog(t, s); len(raw) != 100 {
		t.Fatalf("%d events", len(raw))
	}
}

func TestRecovery_WriteAheadLogReplayIsIdempotent(t *testing.T) {
	for _, format := range storageFormats {
		t.Run(format.String(), func(t *testing.T) {
			directory := newTestDirectory(t, format)
			faults := &switchableFaults{}
			s := openTestStore(t, directory, testStoreOptions{faults: faults.injector()})
			commitTestEvent(t, s, "/a/1", "t")
			batch := []*core.Event{newTestEvent("/a/2", "t", map[string]any{})}
			faults.during(failAtNthCall("wal.clear", 1), func() {
				s.Commit(context.Background(), batch)
			})
			s.Close()
			s = openTestStore(t, directory, testStoreOptions{})
			raw := assertConsistentWithEventLog(t, s)
			s.Close()
			if len(raw) != 2 {
				t.Fatalf("%d events", len(raw))
			}
			s = openTestStore(t, directory, testStoreOptions{})
			defer s.Close()
			if count, _ := s.CountEvents(); count != 2 {
				t.Fatalf("replay not idempotent: %d", count)
			}
		})
	}
}

func TestRecovery_InterruptedFinalRecordIsRepaired(t *testing.T) {
	directory := t.TempDir()
	s := openTestStore(t, directory, testStoreOptions{})
	commitTestEvent(t, s, "/a/1", "t")
	mainLog := logPath(s, s.StorageFormat().MainEventLogFile())
	s.Close()
	fsutil.AppendAndSync(mainLog, []byte(`{"event":{"subj`), nil)
	s = openTestStore(t, directory, testStoreOptions{})
	defer s.Close()
	if count, _ := s.CountEvents(); count != 1 {
		t.Fatal(count)
	}
}

func TestRecovery_InteriorCorruptionIsFatal(t *testing.T) {
	directory := t.TempDir()
	s := openTestStore(t, directory, testStoreOptions{})
	commitTestEvent(t, s, "/a/1", "t")
	commitTestEvent(t, s, "/a/2", "t")
	mainLog := logPath(s, s.StorageFormat().MainEventLogFile())
	s.Close()
	os.RemoveAll(filepath.Join(directory, layout.IndexesDirectory))
	lines := strings.SplitN(string(mustReadFile(t, mainLog)), "\n", 3)
	os.WriteFile(mainLog, []byte(lines[0]+"\n{broken\n"+lines[2]), 0o644)
	if _, err := tryOpenTestStore(directory, testStoreOptions{}); !errors.Is(err, core.ErrEventLogCorrupt) {
		t.Fatalf("expected event log corruption, got %v", err)
	}
}

// Every header is accepted in every file, independent of the extension.
func TestFormat_MismatchedHeadersAreRead(t *testing.T) {
	for _, format := range storageFormats {
		t.Run(format.String(), func(t *testing.T) {
			directory := newTestDirectory(t, format)
			s := openTestStore(t, directory, testStoreOptions{})
			commitTestEvent(t, s, "/h/1", "t")
			s.Close()
			other := layout.FormatLegacy
			if format == layout.FormatLegacy {
				other = layout.FormatCurrent
			}
			for _, name := range format.LogFiles() {
				path := filepath.Join(directory, layout.EventsDirectory, name)
				content := mustReadFile(t, path)
				body := content[len(format.LogHeaderLine()):]
				os.WriteFile(path, append(other.LogHeaderLine(), body...), 0o644)
			}
			os.RemoveAll(filepath.Join(directory, layout.IndexesDirectory))
			s = openTestStore(t, directory, testStoreOptions{})
			defer s.Close()
			assertConsistentWithEventLog(t, s)
		})
	}
}

// TestFetchPage_PagesMatchFetch walks a subject page by page and compares the
// concatenation with a full Fetch, also for a page size that does not divide
// the event count.
func TestFetchPage_PagesMatchFetch(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	for position := 0; position < 23; position++ {
		commitTestEvent(t, s, fmt.Sprintf("/p/%d", position%4), "t")
		commitTestEvent(t, s, fmt.Sprintf("/other/%d", position), "t")
	}
	if err := s.AppendSystemEvent("audit", "/p", nil); err != nil {
		t.Fatal(err)
	}
	all, err := s.Fetch(ctx, "/p", FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, event := range all {
		if !event.IsSystemEvent() {
			want = append(want, event.ID)
		}
	}
	for _, size := range []int{1, 5, 23, 100} {
		var got []*core.Event
		lowerBound, pages := "", 0
		for {
			page, next, err := s.FetchPage(ctx, "/p", lowerBound, size)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) > size {
				t.Fatalf("size %d: page of %d", size, len(page))
			}
			got = append(got, page...)
			pages++
			if next == "" {
				break
			}
			lowerBound = next
		}
		assertSameIDs(t, fmt.Sprintf("size %d", size), got, want)
		if wantPages := (len(want) + size - 1) / size; pages != wantPages {
			t.Errorf("size %d: %d pages, want %d", size, pages, wantPages)
		}
	}
	if page, next, _ := s.FetchPage(ctx, "/p", "missing", 10); len(page) != 0 || next != "" {
		t.Error("unknown lower bound must yield an empty page")
	}
	if _, _, err := s.FetchPage(ctx, "/p", "", 0); err == nil {
		t.Error("zero limit accepted")
	}
}

// TestFetchPage_LoadsOnlyThePage proves the point of FetchPage: a read
// budget far below the subject size still serves every page.
func TestFetchPage_LoadsOnlyThePage(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	for position := 0; position < 60; position++ {
		commitTestEvent(t, s, fmt.Sprintf("/big/%d", position), "t")
	}
	ctx := core.WithReadLimits(context.Background(), core.ReadLimits{MaxEvents: 10})
	if _, err := s.Fetch(ctx, "/big", FetchOptions{}); !errors.Is(err, core.ErrResourceLimit) {
		t.Fatalf("full fetch under budget: %v", err)
	}
	seen, lowerBound := 0, ""
	for {
		page, next, err := s.FetchPage(ctx, "/big", lowerBound, 10)
		if err != nil {
			t.Fatalf("page under budget: %v", err)
		}
		seen += len(page)
		if next == "" {
			break
		}
		lowerBound = next
	}
	if seen != 60 {
		t.Fatalf("paged %d events, want 60", seen)
	}
}

func TestFetchLatestOfType_LoadsOneEvent(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	var last *core.Event
	for position := 0; position < 30; position++ {
		event := commitTestEvent(t, s, fmt.Sprintf("/l/%d", position), []string{"x", "y"}[position%2])
		if position%2 == 0 {
			last = event
		}
	}
	ctx := core.WithReadLimits(context.Background(), core.ReadLimits{MaxEvents: 1})
	latest, err := s.FetchLatestOfType(ctx, "/l", "x")
	if err != nil || latest == nil || latest.ID != last.ID {
		t.Fatalf("latest %v %v, want %s", latest, err, last.ID)
	}
	if none, err := s.FetchLatestOfType(ctx, "/l", "missing"); err != nil || none != nil {
		t.Fatalf("missing type: %v %v", none, err)
	}
}

// TestCountSelectedEvents_MatchesSelectEvents compares the index count with
// the loaded selection for every selector kind, with system events in the
// store, so a count from the index can never differ from a full evaluation.
func TestCountSelectedEvents_MatchesSelectEvents(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	for position := 0; position < 30; position++ {
		commitTestEvent(t, s, fmt.Sprintf("/c/%d/x", position%5), []string{"a", "b", "c"}[position%3])
	}
	if err := s.AppendSystemEvent("audit", "/c/1/x", nil); err != nil {
		t.Fatal(err)
	}
	selectors := []core.EventSelector{
		{All: true},
		{ExactSubjects: []string{"/c/1/x"}},
		{ExactSubjects: []string{"/c/1/x", "/c/2/x", "/missing"}},
		{EventTypes: []string{"a"}},
		{EventTypes: []string{"a", "c", "none"}},
		{SubjectsWithDescendants: []string{"/c"}},
		{SubjectsWithDescendants: []string{"/"}},
	}
	for _, selector := range selectors {
		events, err := s.SelectEvents(ctx, selector)
		if err != nil {
			t.Fatal(err)
		}
		count, err := s.CountSelectedEvents(ctx, selector)
		if err != nil || count != len(events) {
			t.Errorf("%+v: counted %d, selected %d (%v)", selector, count, len(events), err)
		}
	}
	limited := core.WithReadLimits(ctx, core.ReadLimits{MaxEvents: 1})
	if count, err := s.CountSelectedEvents(limited, core.EventSelector{All: true}); err != nil || count != 30 {
		t.Fatalf("a count loads no events and is not bound by the event budget: %d %v", count, err)
	}
}
