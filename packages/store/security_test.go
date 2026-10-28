package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

func TestBackupIncludesSchemasAndFailsOnCorruptAuxiliaryLogs(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	event := newTestEvent("/r/1", "created", map[string]any{"email": "private@example.invalid"})
	event.Options.StoreDataAsReference = true
	if err := s.Commit(ctx, []*core.Event{event}); err != nil {
		t.Fatal(err)
	}
	definition := []byte(`{"properties":{"email":{"type":"string"}}}`)
	if err := s.UpdateSchemas(ctx, func(directory string) error {
		return os.WriteFile(filepath.Join(directory, "created.json"), definition, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	backup, err := s.Backup(ctx)
	if err != nil || len(backup.Schemas) != 1 {
		t.Fatalf("schema backup: %v %v", backup, err)
	}
	target := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer target.Close()
	if err := target.Restore(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(target.SchemasDirectory(), "created.json")); err != nil || string(got) != string(definition) {
		t.Fatalf("schema restore: %s %v", got, err)
	}
	referencePath := filepath.Join(s.storage.EventsDirectory(), s.format.ReferenceDataLogFile())
	if err := os.WriteFile(referencePath, []byte("invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Backup(ctx); err == nil {
		t.Fatal("corrupt reference log exported successfully")
	}
}

func TestReadBudgetAndCancellation(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	commitTestEvent(t, s, "/a/1", "created")
	commitTestEvent(t, s, "/a/2", "created")
	ctx := core.WithReadLimits(context.Background(), core.ReadLimits{MaxEvents: 1, MaxBytes: 1024 * 1024})
	if _, err := s.FetchAll(ctx); !errors.Is(err, core.ErrResourceLimit) {
		t.Fatalf("unbounded read: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.FetchAll(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored cancellation: %v", err)
	}
	if err := s.Commit(context.Background(), []*core.Event{nil}); !errors.Is(err, core.ErrInvalidEvent) {
		t.Fatalf("nil event: %v", err)
	}
}

func TestConcurrentBackupAndReferenceCommitsRemainRestorable(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	var group sync.WaitGroup
	group.Add(1)
	errorsChannel := make(chan error, 1)
	go func() {
		defer group.Done()
		for position := 0; position < 8; position++ {
			event := newTestEvent("/a/1", "created", map[string]any{"n": position})
			event.Options.StoreDataAsReference = true
			if err := s.Commit(ctx, []*core.Event{event}); err != nil {
				errorsChannel <- err
				return
			}
		}
	}()
	for position := 0; position < 8; position++ {
		backup, err := s.Backup(ctx)
		if err != nil {
			t.Fatal(err)
		}
		refs := map[string]bool{}
		for _, entry := range backup.ReferenceData {
			refs[entry.Key] = true
		}
		for _, event := range backup.Events {
			if event.HasReferenceData() && !refs[event.ReferenceDataKey] {
				t.Fatal("inconsistent backup")
			}
		}
	}
	group.Wait()
	select {
	case err := <-errorsChannel:
		t.Fatal(err)
	default:
	}
}

func TestEraseRemovesPersonalKeysAndReportsFailures(t *testing.T) {
	faults := &switchableFaults{}
	s := openTestStore(t, t.TempDir(), testStoreOptions{faults: faults.injector()})
	defer s.Close()
	event := newTestEvent("/a/1", "created", map[string]any{"private@example.invalid": "secret"})
	event.Options.StoreDataAsReference = true
	if err := s.Commit(context.Background(), []*core.Event{event}); err != nil {
		t.Fatal(err)
	}
	var eraseErr error
	faults.during(failAtNthCall("replace.rename:"+s.format.ReferenceDataLogFile(), 1), func() { _, eraseErr = s.Erase(context.Background(), "/a") })
	if eraseErr == nil {
		t.Fatal("erasure failure hidden")
	}
	report, err := s.Erase(context.Background(), "/a")
	if err != nil || report.ErasedEntries != 1 {
		t.Fatalf("erase: %+v %v", report, err)
	}
	payload, err := s.LoadReferenceData(event.ReferenceDataKey)
	if err != nil || string(payload) != `{}` {
		t.Fatalf("personal key remains: %s %v", payload, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil || len(decoded) != 0 {
		t.Fatal("not empty")
	}
}

func BenchmarkCommitBatch(b *testing.B) {
	s, err := Open(b.TempDir(), Options{Logger: discardLogger()})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	b.ReportAllocs()
	for b.Loop() {
		events := make([]*core.Event, 10)
		for position := range events {
			events[position] = newTestEvent("/benchmark/1", "created", map[string]any{"n": position})
		}
		if err := s.Commit(context.Background(), events); err != nil {
			b.Fatal(err)
		}
	}
}
