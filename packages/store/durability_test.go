package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
	"github.com/genesisdb-io/genesisdb/v2/packages/storage"
)

// TestStorageFailure_RebuildDoesNotResurrectTheStore checks that a storage
// failure after WAL publication is not cleared by an index rebuild: only a
// restart that replays the WAL makes the store writable again, so the
// in-memory log position and predecessor hash stay trustworthy.
func TestStorageFailure_RebuildDoesNotResurrectTheStore(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	faults := &switchableFaults{}
	s := openTestStore(t, directory, testStoreOptions{faults: faults.injector()})

	var commitErr error
	faults.during(failAtNthCall("events.sync", 1), func() {
		commitErr = s.Commit(ctx, []*core.Event{newTestEvent("/a/1", "t", map[string]any{})})
	})
	if commitErr == nil {
		t.Fatal("injected failure not reported")
	}
	if s.IsReady() {
		t.Fatal("store ready after a storage failure")
	}
	if _, err := s.RebuildIndexes(ctx); !errors.Is(err, core.ErrUnhealthy) {
		t.Fatalf("rebuild must refuse to clear a storage failure, got %v", err)
	}
	if s.IsReady() {
		t.Fatal("rebuild resurrected a store with an untrusted event log")
	}
	for name, attempt := range map[string]func() error{
		"commit": func() error { return s.Commit(ctx, []*core.Event{newTestEvent("/a/2", "t", map[string]any{})}) },
		"erase":  func() error { _, err := s.Erase(ctx, "/a"); return err },
		"restore": func() error {
			return s.Restore(ctx, &core.Backup{})
		},
		"replace reference data": func() error {
			_, err := s.ReplaceReferenceData(ctx, map[string]json.RawMessage{"x": core.EmptyEventData()})
			return err
		},
		"system event": func() error { return s.AppendSystemEvent("audit", "/", nil) },
	} {
		if err := attempt(); !errors.Is(err, core.ErrUnhealthy) {
			t.Errorf("%s accepted on an unhealthy store: %v", name, err)
		}
	}
	s.Close()

	// Restart recovers through WAL replay and the chain is intact.
	s = openTestStore(t, directory, testStoreOptions{})
	defer s.Close()
	if !s.IsReady() {
		t.Fatal("restart did not recover")
	}
	commitTestEvent(t, s, "/a/2", "t")
	report, err := s.VerifyHashChain(ctx)
	if err != nil || !report.IsValid() {
		t.Fatalf("hash chain after recovery: %+v %v", report, err)
	}
	assertConsistentWithEventLog(t, s)
}

// TestStorageFailure_IsReadyIsSafeConcurrently reads the readiness of the
// store (as GET /status does, without any lock) while a commit fails and
// marks the storage engine unhealthy. The race detector guards the sticky
// failure flag.
func TestStorageFailure_IsReadyIsSafeConcurrently(t *testing.T) {
	ctx := context.Background()
	faults := &switchableFaults{}
	s := openTestStore(t, t.TempDir(), testStoreOptions{faults: faults.injector()})
	defer s.Close()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				s.IsReady()
			}
		}
	}()
	var commitErr error
	faults.during(failAtNthCall("events.sync", 1), func() {
		commitErr = s.Commit(ctx, []*core.Event{newTestEvent("/a/1", "t", map[string]any{})})
	})
	close(stop)
	<-done
	if commitErr == nil {
		t.Fatal("injected failure not reported")
	}
	if s.IsReady() {
		t.Fatal("store ready after a storage failure")
	}
}

// TestStorageFailure_DoesNotLeakIntoAppendSystemEvent checks that the
// storage engine itself refuses writes after a failure, independent of the
// store's checks.
func TestStorageFailure_EngineRefusesWrites(t *testing.T) {
	ctx := context.Background()
	faults := &switchableFaults{}
	engine, err := storage.Open(t.TempDir(), storage.Options{Format: layout.FormatCurrent, Logger: discardLogger(), Faults: faults.injector()})
	if err != nil {
		t.Fatal(err)
	}
	faults.during(failAtNthCall("events.write", 1), func() {
		_, err = engine.Append(ctx, []*core.Event{newTestEvent("/x", "t", map[string]any{})})
	})
	if err == nil || engine.Failure() == nil {
		t.Fatal("failure not recorded")
	}
	if _, err := engine.Append(ctx, []*core.Event{newTestEvent("/y", "t", map[string]any{})}); !errors.Is(err, core.ErrUnhealthy) || !errors.Is(err, storage.ErrNothingWritten) {
		t.Fatalf("append after failure: %v", err)
	}
	if _, err := engine.AppendSystemEvent("audit", "/", nil); !errors.Is(err, core.ErrUnhealthy) {
		t.Fatalf("system event after failure: %v", err)
	}
}

// TestOrder_FollowsTheLogNotTheClock commits with a clock that steps
// backwards and verifies that reads, subscriptions and restores keep the
// commit order.
func TestOrder_FollowsTheLogNotTheClock(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	engine, err := storage.Open(directory, storage.Options{Format: layout.FormatCurrent, Logger: discardLogger(), Now: func() time.Time {
		now = now.Add(-time.Second)
		return now
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"created", "updated", "deleted"} {
		if _, err := engine.Append(ctx, []*core.Event{{Source: "test", Subject: "/a/1", Type: kind, Data: json.RawMessage(`{}`)}}); err != nil {
			t.Fatal(err)
		}
	}
	s := openTestStore(t, directory, testStoreOptions{})
	defer s.Close()
	types := func(events []*core.Event) string {
		names := make([]string, len(events))
		for position, event := range events {
			names[position] = event.Type
		}
		return strings.Join(names, ",")
	}
	fetched, err := s.Fetch(ctx, "/a", FetchOptions{})
	if err != nil || types(fetched) != "created,updated,deleted" {
		t.Fatalf("fetch order %s %v", types(fetched), err)
	}
	all, _ := s.FetchAll(ctx)
	if types(all) != "created,updated,deleted" {
		t.Fatalf("fetch all order %s", types(all))
	}
	latest, err := s.FindLatestEvent(ctx, "/a/1")
	if err != nil || latest.Type != "deleted" {
		t.Fatalf("latest %v %v", latest, err)
	}
	history, _, err := s.fetchSinceCursor(ctx, "/a", nil)
	if err != nil || types(history) != "created,updated,deleted" {
		t.Fatalf("subscription order %s %v", types(history), err)
	}

	backup, err := s.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer target.Close()
	if err := target.Restore(ctx, backup); err != nil {
		t.Fatal(err)
	}
	restored, _ := target.Fetch(ctx, "/a", FetchOptions{})
	if types(restored) != "created,updated,deleted" {
		t.Fatalf("restore reordered by clock: %s", types(restored))
	}
	if report, _ := target.VerifyHashChain(ctx); !report.IsValid() {
		t.Fatal("restored chain invalid")
	}
}

// TestSubscribe_CatchesUpInBoundedBatches checks that a commit burst larger
// than the read budget is delivered in several consecutive batches.
func TestSubscribe_CatchesUpInBoundedBatches(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	ctx, cancel := context.WithCancel(core.WithReadLimits(context.Background(), core.ReadLimits{MaxEvents: 3}))
	defer cancel()

	// History larger than the budget is delivered in several batches.
	for position := 0; position < 7; position++ {
		commitTestEvent(t, s, fmt.Sprintf("/b/%d", position), "t")
	}
	subscription, err := s.Subscribe(ctx, "/b", SubscribeOptions{PollInterval: MinimumPollInterval})
	if err != nil {
		t.Fatalf("history exceeding the budget must not fail the subscription: %v", err)
	}
	received := []string{}
	deadline := time.After(5 * time.Second)
	for len(received) < 7 {
		select {
		case batch := <-subscription.Batches():
			if batch.Err != nil {
				t.Fatal(batch.Err)
			}
			if !batch.IsHistory && len(batch.Events) > 0 {
				t.Fatal("history delivered as live events")
			}
			if len(batch.Events) > 3 {
				t.Fatalf("batch of %d exceeds the budget", len(batch.Events))
			}
			for _, event := range batch.Events {
				received = append(received, event.Subject)
			}
		case <-deadline:
			t.Fatalf("history stalled after %d events", len(received))
		}
	}
	for position := 0; position < 7; position++ {
		if received[position] != fmt.Sprintf("/b/%d", position) {
			t.Fatalf("history order %v", received)
		}
	}

	// A burst of live commits is delivered completely and in order.
	batch := make([]*core.Event, 0, 10)
	for position := 7; position < 17; position++ {
		batch = append(batch, newTestEvent(fmt.Sprintf("/b/%d", position), "t", map[string]any{}))
	}
	if err := s.Commit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	live := []string{}
	deadline = time.After(5 * time.Second)
	for len(live) < 10 {
		select {
		case batch := <-subscription.Batches():
			if batch.Err != nil {
				t.Fatal(batch.Err)
			}
			if batch.IsHistory {
				t.Fatal("live events marked as history")
			}
			for _, event := range batch.Events {
				live = append(live, event.Subject)
			}
		case <-deadline:
			t.Fatalf("subscription stalled after %d live events", len(live))
		}
	}
	for position := 0; position < 10; position++ {
		if live[position] != fmt.Sprintf("/b/%d", position+7) {
			t.Fatalf("live order %v", live)
		}
	}
}

// TestSubscribe_BoundsSurviveSplitHistory checks that lower/upper bounds
// apply across several history batches.
func TestSubscribe_BoundsSurviveSplitHistory(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	var events []*core.Event
	for position := 0; position < 9; position++ {
		events = append(events, commitTestEvent(t, s, fmt.Sprintf("/c/%d", position), "t"))
	}
	ctx, cancel := context.WithCancel(core.WithReadLimits(context.Background(), core.ReadLimits{MaxEvents: 2}))
	defer cancel()
	subscription, err := s.Subscribe(ctx, "/c", SubscribeOptions{PollInterval: MinimumPollInterval, FetchOptions: FetchOptions{LowerBound: events[2].ID, UpperBound: events[6].ID}})
	if err != nil {
		t.Fatal(err)
	}
	received := []string{}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case batch := <-subscription.Batches():
			if batch.Err != nil {
				t.Fatal(batch.Err)
			}
			for _, event := range batch.Events {
				received = append(received, event.Subject)
			}
			if batch.IsHistory && !batch.HasMore {
				if strings.Join(received, ",") != "/c/3,/c/4,/c/5" {
					t.Fatalf("bounded history %v", received)
				}
				return
			}
		case <-deadline:
			t.Fatalf("history incomplete: %v", received)
		}
	}
}

// TestSubscribe_ReportsPersistentFailure checks that a subscription whose
// polls keep failing ends with an error batch instead of hanging silently.
func TestSubscribe_ReportsPersistentFailure(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	commitTestEvent(t, s, "/d/1", "t")
	// A byte budget no single event fits in makes every poll with new
	// events fail.
	ctx, cancel := context.WithCancel(core.WithReadLimits(context.Background(), core.ReadLimits{MaxBytes: 1}))
	defer cancel()
	if _, err := s.Subscribe(ctx, "/d", SubscribeOptions{PollInterval: MinimumPollInterval}); !errors.Is(err, core.ErrResourceLimit) {
		t.Fatalf("history that cannot be delivered at all must fail directly: %v", err)
	}
	ctx, cancel = context.WithCancel(core.WithReadLimits(context.Background(), core.ReadLimits{MaxBytes: 1}))
	defer cancel()
	subscription, err := s.Subscribe(ctx, "/e", SubscribeOptions{PollInterval: MinimumPollInterval})
	if err != nil {
		t.Fatal(err)
	}
	<-subscription.Batches()
	commitTestEvent(t, s, "/e/1", "t")
	deadline := time.After(10 * time.Second)
	for {
		select {
		case batch, isOpen := <-subscription.Batches():
			if !isOpen {
				t.Fatal("channel closed without an error batch")
			}
			if batch.Err != nil {
				if !errors.Is(batch.Err, ErrSubscriptionFailed) || !errors.Is(batch.Err, core.ErrResourceLimit) {
					t.Fatalf("error batch %v", batch.Err)
				}
				if _, isOpen := <-subscription.Batches(); isOpen {
					t.Fatal("channel not closed after the error batch")
				}
				return
			}
		case <-deadline:
			t.Fatal("subscription hung silently on a persistent failure")
		}
	}
}

// TestErase_IgnoresTheSelectionBudget checks that erasure of a subject with
// more events than the HTTP read budget allows still erases everything and
// never loads payloads through the budgeted path.
func TestErase_IgnoresTheSelectionBudget(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	batch := make([]*core.Event, 0, 50)
	for position := 0; position < 50; position++ {
		event := newTestEvent(fmt.Sprintf("/p/%d", position), "t", map[string]any{"secret": position})
		event.Options.StoreDataAsReference = position%2 == 0
		batch = append(batch, event)
	}
	if err := s.Commit(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	ctx := core.WithReadLimits(context.Background(), core.ReadLimits{MaxEvents: 5, MaxBytes: 1024})
	if _, err := s.Fetch(ctx, "/p", FetchOptions{}); !errors.Is(err, core.ErrResourceLimit) {
		t.Fatalf("precondition: fetch must exceed the budget, got %v", err)
	}
	report, err := s.Erase(ctx, "/p")
	if err != nil || report.ErasedEntries != 25 {
		t.Fatalf("erase under a budget: %+v %v", report, err)
	}
	for _, event := range batch {
		if !event.HasReferenceData() {
			continue
		}
		payload, err := s.LoadReferenceData(event.ReferenceDataKey)
		if err != nil || string(payload) != "{}" {
			t.Fatalf("payload of %s not erased: %s %v", event.Subject, payload, err)
		}
	}
	if system, _ := s.ListSystemEvents(); len(system) != 2 || system[1].Type != ErasureSystemEventType {
		t.Fatal("audit events missing")
	}
}

// TestErase_ObservesCancellation checks that a cancelled context stops the
// erasure before any audit record is written.
func TestErase_ObservesCancellation(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	event := newTestEvent("/q/1", "t", map[string]any{"secret": true})
	event.Options.StoreDataAsReference = true
	if err := s.Commit(context.Background(), []*core.Event{event}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Erase(ctx, "/q"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if system, _ := s.ListSystemEvents(); len(system) != 0 {
		t.Fatal("cancelled erasure wrote audit records")
	}
	if payload, _ := s.LoadReferenceData(event.ReferenceDataKey); string(payload) == "{}" {
		t.Fatal("cancelled erasure replaced the payload")
	}
}
