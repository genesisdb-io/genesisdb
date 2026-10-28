package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

func TestCachedAndRawReadsRespectByteBudgets(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	commitTestEvent(t, s, "/a/1", "created")
	if _, err := s.FetchAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := core.WithReadLimits(context.Background(), core.ReadLimits{MaxBytes: 1})
	for name, read := range map[string]func() error{
		"cached": func() error {
			_, err := s.FetchAll(ctx)
			return err
		},
		"raw": func() error {
			_, err := s.FetchAllWithoutReferenceData(ctx)
			return err
		},
		"backup": func() error {
			_, err := s.Backup(ctx)
			return err
		},
		"catalog": func() error {
			_, err := s.ListSubjects(ctx)
			return err
		},
	} {
		if err := read(); !errors.Is(err, core.ErrResourceLimit) {
			t.Fatalf("%s ignored byte budget: %v", name, err)
		}
	}
}

func TestWriterWaitObservesCancellation(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	s.writeMutex.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.Commit(ctx, []*core.Event{newTestEvent("/a/1", "created", map[string]any{})})
	}()
	select {
	case err := <-done:
		s.writeMutex.Unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		s.writeMutex.Unlock()
		<-done
		t.Fatal("writer lock ignored deadline")
	}
}

func TestValidationSerializedWithSchemaUpdates(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	ctx := context.Background()
	if err := s.UpdateSchemas(ctx, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	rejected := errors.New("schema rejected event")
	err := s.CommitValidated(ctx, []*core.Event{newTestEvent("/a/1", "created", map[string]any{})}, func(*core.Event) error {
		if s.writeMutex.TryLock() {
			s.writeMutex.Unlock()
			t.Error("validation was not under the writer lock")
		}
		return rejected
	})
	if !errors.Is(err, core.ErrInvalidEvent) {
		t.Fatal(err)
	}
	events, err := s.FetchAll(ctx)
	if err != nil || len(events) != 0 {
		t.Fatal("invalid event was published")
	}
}
