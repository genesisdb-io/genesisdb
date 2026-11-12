package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// TestGroupCommit_LeadersStepBackForAnnouncedWriters checks the rule that
// keeps maintenance from starving: while another writer is announced, a
// group leader does not take the writer lock, even though it is free.
// Without that rule the leader of the next group retakes the lock before a
// polling waiter sees it free.
func TestGroupCommit_LeadersStepBackForAnnouncedWriters(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	s.waitingWriters.Add(1)
	result := make(chan error, 1)
	go func() {
		result <- s.Commit(context.Background(), []*core.Event{newTestEvent("/w/1", "t", map[string]any{})})
	}()
	select {
	case err := <-result:
		s.waitingWriters.Add(-1)
		t.Fatalf("commit took the writer lock ahead of an announced writer: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	s.waitingWriters.Add(-1)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("commit did not resume after the announced writer left")
	}
}

// TestGroupCommit_OtherWritersProceedUnderWriteLoad runs schema updates and
// a backup while commits arrive without a pause. The deadlines are generous,
// the precise rule is covered by the test above.
func TestGroupCommit_OtherWritersProceedUnderWriteLoad(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var group sync.WaitGroup
	var committed atomic.Int64
	for writer := 0; writer < 32; writer++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for position := 0; ctx.Err() == nil; position++ {
				err := s.Commit(ctx, []*core.Event{newTestEvent(fmt.Sprintf("/m/%d/%d", writer, position), "t", map[string]any{})})
				if err != nil && ctx.Err() == nil {
					t.Error(err)
					return
				}
				committed.Add(1)
			}
		}()
	}
	waitFor(t, "write load", func() bool { return committed.Load() > 50 })
	for attempt := 0; attempt < 8; attempt++ {
		waitCtx, cancelWait := context.WithTimeout(context.Background(), 2*time.Second)
		err := s.UpdateSchemas(waitCtx, func(string) error { return nil })
		cancelWait()
		if err != nil {
			t.Fatalf("schema update %d did not get the writer lock under write load: %v", attempt, err)
		}
	}
	backupCtx, cancelBackup := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := s.Backup(backupCtx)
	cancelBackup()
	if err != nil {
		t.Fatalf("backup under write load: %v", err)
	}
	cancel()
	group.Wait()
	assertConsistentWithEventLog(t, s)
}

// serialModel executes commits with subject preconditions one after another.
// It is the reference the grouped outcome is compared against.
type serialModel struct {
	existing map[string]bool
}

func (model *serialModel) apply(commit *queuedCommit) (isRejected bool) {
	for _, precondition := range commit.preconditions {
		var payload struct {
			Subject string `json:"subject"`
		}
		json.Unmarshal(precondition.Payload, &payload)
		if model.existing[payload.Subject] == (precondition.Type == core.PreconditionIsSubjectNew) {
			return true
		}
	}
	for _, event := range commit.events {
		for _, key := range core.SubjectAndAncestors(event.Subject) {
			model.existing[key] = true
		}
	}
	return false
}

// commitsFromBytes derives a queue of commits from choices. Four bytes
// describe one commit: its subject, an optional second event and up to two
// subject preconditions. Subjects live below prefix, in a small tree, so
// commits of one queue observe each other.
func commitsFromBytes(prefix string, choices []byte) []*queuedCommit {
	subjects := []string{prefix, prefix + "/a", prefix + "/a/1", prefix + "/a/1/x", prefix + "/a/2", prefix + "/b", prefix + "/b/1", prefix + "/c/1/y"}
	kinds := []string{core.PreconditionIsSubjectNew, core.PreconditionIsSubjectExisting}
	var commits []*queuedCommit
	for len(choices) >= 4 && len(commits) < 16 {
		choice := choices[:4]
		choices = choices[4:]
		commit := &queuedCommit{events: []*core.Event{newTestEvent(subjects[1+int(choice[0])%(len(subjects)-1)], "t", map[string]any{})}}
		if choice[1]&1 == 1 {
			commit.events = append(commit.events, newTestEvent(subjects[1+int(choice[1]>>1)%(len(subjects)-1)], "t", map[string]any{}))
		}
		for _, selection := range choice[2:] {
			if selection&1 == 1 {
				commit.preconditions = append(commit.preconditions, subjectPrecondition(kinds[int(selection>>1)&1], subjects[int(selection>>2)%len(subjects)]))
			}
		}
		commits = append(commits, commit)
	}
	return commits
}

// checkSerialEquivalence commits the queue as groups and compares every
// result with the model.
func checkSerialEquivalence(t testing.TB, s *Store, model *serialModel, commits []*queuedCommit) {
	t.Helper()
	commitQueued(t, s, commits)
	for position, commit := range commits {
		wantRejected := model.apply(commit)
		var failed *core.PreconditionFailedError
		if isRejected := errors.As(commit.err, &failed); isRejected != wantRejected || (commit.err != nil && !isRejected) {
			t.Fatalf("commit %d: got %v, serial execution rejects=%v", position, commit.err, wantRejected)
		}
	}
}

func TestGroupCommit_RandomizedSerialEquivalence(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	random := rand.New(rand.NewSource(1))
	model := &serialModel{existing: map[string]bool{}}
	for round := 0; round < 40; round++ {
		choices := make([]byte, 48)
		random.Read(choices)
		// Every fourth round starts a fresh subject tree, so both new and
		// long existing subjects are covered.
		checkSerialEquivalence(t, s, model, commitsFromBytes(fmt.Sprintf("/tree%d", round/4), choices))
	}
	assertConsistentWithEventLog(t, s)
}

// FuzzGroupCommitSerialEquivalence checks the invariant of group commit: a
// queue of concurrent commits with subject preconditions has the outcome of
// executing them one after another in queue order.
//
// One store is shared by every iteration. Each iteration works in its own
// subject tree, so the model of that tree starts empty.
func FuzzGroupCommitSerialEquivalence(f *testing.F) {
	f.Add([]byte{0, 0, 3, 0, 0, 0, 3, 0})
	f.Add([]byte{1, 0, 1, 0, 1, 0, 1, 0, 2, 3, 7, 0})
	f.Add([]byte{0, 1, 5, 9, 4, 3, 13, 1, 2, 0, 0, 0, 6, 255, 255, 255})
	f.Add([]byte{5, 0, 21, 0, 6, 0, 23, 0, 0, 0, 1, 0})
	s, err := tryOpenTestStore(f.TempDir(), testStoreOptions{convertLegacy: true})
	if err != nil {
		f.Fatal(err)
	}
	defer s.Close()
	iteration := 0
	f.Fuzz(func(t *testing.T, choices []byte) {
		iteration++
		commits := commitsFromBytes(fmt.Sprintf("/fuzz%d", iteration), choices)
		if len(commits) == 0 {
			return
		}
		checkSerialEquivalence(t, s, &serialModel{existing: map[string]bool{}}, commits)
		if !s.IsReady() {
			t.Fatal("store unhealthy after a commit queue")
		}
	})
}

// BenchmarkCommitParallel measures single-event commits from concurrent
// writers, with groups of one commit and with the default group size.
func BenchmarkCommitParallel(b *testing.B) {
	for _, maxCommits := range []int{1, DefaultMaxCommitsPerGroup} {
		for _, writers := range []int{1, 4, 16, 64, 256} {
			b.Run(fmt.Sprintf("group=%d/writers=%d", maxCommits, writers), func(b *testing.B) {
				s, err := Open(b.TempDir(), Options{CacheSize: -1, Logger: discardLogger(), MaxCommitsPerGroup: maxCommits})
				if err != nil {
					b.Fatal(err)
				}
				defer s.Close()
				var next atomic.Int64
				var group sync.WaitGroup
				latencies := make([][]time.Duration, writers)
				b.ResetTimer()
				started := time.Now()
				for writer := 0; writer < writers; writer++ {
					group.Add(1)
					go func() {
						defer group.Done()
						for {
							number := next.Add(1)
							if number > int64(b.N) {
								return
							}
							event := newTestEvent(fmt.Sprintf("/benchmark/%d/%d", writer, number), "created", map[string]any{"n": number, "pad": "0123456789012345678901234567890123456789012345678901234567890123456789"})
							begin := time.Now()
							if err := s.Commit(context.Background(), []*core.Event{event}); err != nil {
								b.Error(err)
								return
							}
							latencies[writer] = append(latencies[writer], time.Since(begin))
						}
					}()
				}
				group.Wait()
				elapsed := time.Since(started)
				var all []time.Duration
				for _, part := range latencies {
					all = append(all, part...)
				}
				if len(all) == 0 {
					return
				}
				sort.Slice(all, func(first, second int) bool { return all[first] < all[second] })
				b.ReportMetric(float64(b.N)/elapsed.Seconds(), "commits/s")
				b.ReportMetric(float64(all[len(all)/2].Microseconds())/1000, "p50-ms")
				b.ReportMetric(float64(all[len(all)*99/100].Microseconds())/1000, "p99-ms")
			})
		}
	}
}

// TestGroupCommit_CommitsProceedUnderMaintenanceLoad is the counterpart of
// the test above: writers that are not commits arrive without a pause, so
// one of them is always announced. Commits must still get their turn.
func TestGroupCommit_CommitsProceedUnderMaintenanceLoad(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var group sync.WaitGroup
	var updates atomic.Int64
	for writer := 0; writer < 4; writer++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for ctx.Err() == nil {
				err := s.UpdateSchemas(ctx, func(string) error {
					time.Sleep(time.Millisecond)
					return nil
				})
				if err != nil && ctx.Err() == nil {
					t.Error(err)
					return
				}
				updates.Add(1)
			}
		}()
	}
	waitFor(t, "maintenance load", func() bool { return updates.Load() > 20 })
	for position := 0; position < 20; position++ {
		commitCtx, cancelCommit := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.Commit(commitCtx, []*core.Event{newTestEvent(fmt.Sprintf("/starved/%d", position), "t", map[string]any{})})
		cancelCommit()
		if err != nil {
			t.Fatalf("commit %d did not get the writer lock under maintenance load: %v", position, err)
		}
	}
	cancel()
	group.Wait()
	assertConsistentWithEventLog(t, s)
}
