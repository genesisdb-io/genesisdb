package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
)

func subjectPrecondition(kind, subject string) core.Precondition {
	payload, _ := json.Marshal(map[string]string{"subject": subject})
	return core.Precondition{Type: kind, Payload: payload}
}

func queryPrecondition(text string) core.Precondition {
	payload, _ := json.Marshal(map[string]string{"query": text})
	return core.Precondition{Type: core.PreconditionIsQueryResultTrue, Payload: payload}
}

// queuedCommit is one commit of a deterministic group test.
type queuedCommit struct {
	ctx           context.Context
	events        []*core.Event
	validate      func(*core.Event) error
	preconditions []core.Precondition
	err           error
}

func (s *Store) queuedCommitCount() int {
	s.commitQueue.mutex.Lock()
	defer s.commitQueue.mutex.Unlock()
	return len(s.commitQueue.members)
}

func waitFor(t testing.TB, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// queueCommits starts the commits in order while the caller holds the
// writer lock, so they wait in the queue in exactly that order. The returned
// function waits for every result.
func queueCommits(t testing.TB, s *Store, commits []*queuedCommit) (wait func()) {
	t.Helper()
	var group sync.WaitGroup
	for position, commit := range commits {
		if commit.ctx == nil {
			commit.ctx = context.Background()
		}
		group.Add(1)
		go func() {
			defer group.Done()
			commit.err = s.CommitValidated(commit.ctx, commit.events, commit.validate, commit.preconditions...)
		}()
		waitFor(t, "queued commit", func() bool { return s.queuedCommitCount() == position+1 })
	}
	return group.Wait
}

// commitQueued queues the commits in order behind the writer lock, releases
// the lock and waits for every result.
func commitQueued(t testing.TB, s *Store, commits []*queuedCommit) {
	t.Helper()
	s.writeMutex.Lock()
	wait := queueCommits(t, s, commits)
	s.writeMutex.Unlock()
	wait()
}

// pointCounter counts the hits of fault points without failing any. One
// hit of "journal.append" is one written group.
type pointCounter struct {
	mutex sync.Mutex
	hits  map[string]int
}

func (counter *pointCounter) injector() fsutil.FaultInjector {
	return func(point string) error {
		counter.mutex.Lock()
		if counter.hits == nil {
			counter.hits = map[string]int{}
		}
		counter.hits[point]++
		counter.mutex.Unlock()
		return nil
	}
}

func (counter *pointCounter) count(point string) int {
	counter.mutex.Lock()
	defer counter.mutex.Unlock()
	return counter.hits[point]
}

func TestGroupCommit_ConcurrentCommitsShareOneWrite(t *testing.T) {
	for _, format := range storageFormats {
		t.Run(format.String(), func(t *testing.T) {
			counter := &pointCounter{}
			s := openTestStore(t, newTestDirectory(t, format), testStoreOptions{faults: counter.injector()})
			defer s.Close()
			var commits []*queuedCommit
			for position := 0; position < 20; position++ {
				events := []*core.Event{
					newTestEvent(fmt.Sprintf("/g/%d", position), "t", map[string]any{"n": position}),
					newTestEvent(fmt.Sprintf("/g/%d/x", position), "u", map[string]any{"secret": position}),
				}
				events[1].Options.StoreDataAsReference = true
				commits = append(commits, &queuedCommit{events: events})
			}
			before := counter.count("journal.append")
			commitQueued(t, s, commits)
			if groups := counter.count("journal.append") - before; groups != 1 {
				t.Fatalf("20 queued commits used %d groups", groups)
			}
			var want []string
			for position, commit := range commits {
				if commit.err != nil {
					t.Fatal(commit.err)
				}
				if !strings.Contains(string(commit.events[1].Data), fmt.Sprintf(`"secret":%d`, position)) {
					t.Fatalf("commit %d got a foreign reference payload: %s", position, commit.events[1].Data)
				}
				want = append(want, commit.events[0].ID, commit.events[1].ID)
			}
			raw := assertConsistentWithEventLog(t, s)
			assertSameIDs(t, "commit order equals queue order", raw, want)
		})
	}
}

func TestGroupCommit_PreconditionsMatchSerialExecution(t *testing.T) {
	counter := &pointCounter{}
	s := openTestStore(t, t.TempDir(), testStoreOptions{faults: counter.injector()})
	defer s.Close()
	commitTestEvent(t, s, "/old/1", "t")
	event := func(subject string) []*core.Event {
		return []*core.Event{newTestEvent(subject, "t", map[string]any{})}
	}
	commits := []*queuedCommit{
		0: {events: event("/a/1"), preconditions: []core.Precondition{subjectPrecondition(core.PreconditionIsSubjectNew, "/a/1")}},
		// Published subject: decided without the unpublished events, joins.
		1: {events: event("/old/1"), preconditions: []core.Precondition{subjectPrecondition(core.PreconditionIsSubjectExisting, "/old/1")}},
		2: {events: event("/old/1/x"), preconditions: []core.Precondition{subjectPrecondition(core.PreconditionIsSubjectNew, "/old")}},
		// Unrelated subject, joins.
		3: {events: event("/b/1"), preconditions: []core.Precondition{subjectPrecondition(core.PreconditionIsSubjectNew, "/b/1")}},
		// Observes the unpublished /a/1: starts group 2 and fails.
		4: {events: event("/a/1"), preconditions: []core.Precondition{subjectPrecondition(core.PreconditionIsSubjectNew, "/a/1")}},
		// Ancestor of /b/1, which is published by now.
		5: {events: event("/c/1"), preconditions: []core.Precondition{subjectPrecondition(core.PreconditionIsSubjectExisting, "/b")}},
		6: {events: event("/d/1"), preconditions: []core.Precondition{subjectPrecondition(core.PreconditionIsSubjectExisting, "/missing")}},
		// Observes the unpublished /c/1 through its ancestor: starts group 3.
		7: {events: event("/e/1"), preconditions: []core.Precondition{subjectPrecondition(core.PreconditionIsSubjectNew, "/c")}},
		// A query precondition never joins: group 4, sees /a/1 once.
		8:  {events: event("/q/1"), preconditions: []core.Precondition{queryPrecondition("COUNT /a/1 == 1")}},
		9:  {events: event("/q/2"), preconditions: []core.Precondition{queryPrecondition("COUNT /q/1 == 1")}},
		10: {preconditions: []core.Precondition{subjectPrecondition(core.PreconditionIsSubjectExisting, "/q/2")}},
	}
	before := counter.count("journal.append")
	commitQueued(t, s, commits)
	wantFailed := map[int]bool{2: true, 4: true, 6: true, 7: true}
	for position, commit := range commits {
		var failed *core.PreconditionFailedError
		if isFailed := errors.As(commit.err, &failed); isFailed != wantFailed[position] || (commit.err != nil && !isFailed) {
			t.Errorf("commit %d: %v", position, commit.err)
		}
	}
	// Groups: [0..3] [4,5,6] [7] [8] [9] [10]. The groups of 7 and 10 write
	// nothing.
	if groups := counter.count("journal.append") - before; groups != 4 {
		t.Errorf("%d writing groups, want 4", groups)
	}
	// The earlier event plus the commits 0, 1, 3, 5, 8 and 9.
	if raw := assertConsistentWithEventLog(t, s); len(raw) != 7 {
		t.Fatalf("%d events", len(raw))
	}
}

func TestGroupCommit_LimitsSplitWithoutRejecting(t *testing.T) {
	counter := &pointCounter{}
	s, err := Open(t.TempDir(), Options{CacheSize: -1, Logger: discardLogger(), Faults: counter.injector(), MaxCommitsPerGroup: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var commits []*queuedCommit
	for position := 0; position < 7; position++ {
		commits = append(commits, &queuedCommit{events: []*core.Event{newTestEvent(fmt.Sprintf("/l/%d", position), "t", map[string]any{})}})
	}
	large := &queuedCommit{}
	for position := 0; position < DefaultMaxEventsPerGroup+5; position++ {
		large.events = append(large.events, newTestEvent(fmt.Sprintf("/large/%d", position), "t", map[string]any{}))
	}
	commits = append(commits, large, &queuedCommit{events: []*core.Event{newTestEvent("/l/after", "t", map[string]any{})}})
	before := counter.count("journal.append")
	commitQueued(t, s, commits)
	for position, commit := range commits {
		if commit.err != nil {
			t.Fatalf("commit %d: %v", position, commit.err)
		}
	}
	// [0,1,2] [3,4,5] [6] [large] [after]
	if groups := counter.count("journal.append") - before; groups != 5 {
		t.Errorf("%d groups, want 5", groups)
	}
	if raw := assertConsistentWithEventLog(t, s); len(raw) != 7+DefaultMaxEventsPerGroup+5+1 {
		t.Fatalf("%d events", len(raw))
	}
}

func TestGroupCommit_ConfiguredEventAndByteLimitsSplitGroups(t *testing.T) {
	for name, options := range map[string]Options{
		"events": {MaxEventsPerGroup: 4},
		"bytes":  {MaxBytesPerGroup: 2 * 1100},
	} {
		t.Run(name, func(t *testing.T) {
			counter := &pointCounter{}
			options.CacheSize = -1
			options.Logger = discardLogger()
			options.Faults = counter.injector()
			s, err := Open(t.TempDir(), options)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			var commits []*queuedCommit
			for position := 0; position < 6; position++ {
				commits = append(commits, &queuedCommit{events: []*core.Event{
					newTestEvent(fmt.Sprintf("/s/%d", position), "t", map[string]any{"pad": strings.Repeat("x", 500)}),
					newTestEvent(fmt.Sprintf("/s/%d/x", position), "t", map[string]any{"pad": strings.Repeat("x", 500)}),
				}})
			}
			before := counter.count("journal.append")
			commitQueued(t, s, commits)
			for position, commit := range commits {
				if commit.err != nil {
					t.Fatalf("commit %d: %v", position, commit.err)
				}
			}
			// Two commits of two events fit into one group.
			if groups := counter.count("journal.append") - before; groups != 3 {
				t.Errorf("%d groups, want 3", groups)
			}
			if raw := assertConsistentWithEventLog(t, s); len(raw) != 12 {
				t.Fatalf("%d events", len(raw))
			}
		})
	}
}

// operationRecorder counts completed operations by name.
type operationRecorder struct {
	core.NoopObserver
	mutex      sync.Mutex
	operations map[string]int
}

func (recorder *operationRecorder) OperationCompleted(operation string, _ time.Duration) {
	recorder.mutex.Lock()
	if recorder.operations == nil {
		recorder.operations = map[string]int{}
	}
	recorder.operations[operation]++
	recorder.mutex.Unlock()
}

func TestGroupCommit_ObserverSeesEveryCommitAndEveryGroup(t *testing.T) {
	recorder := &operationRecorder{}
	s, err := Open(t.TempDir(), Options{CacheSize: -1, Logger: discardLogger(), Observer: recorder})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var commits []*queuedCommit
	for position := 0; position < 5; position++ {
		commits = append(commits, &queuedCommit{events: []*core.Event{newTestEvent(fmt.Sprintf("/o/%d", position), "t", map[string]any{})}})
	}
	commitQueued(t, s, commits)
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	if recorder.operations["commit"] != 5 || recorder.operations["commit_group"] != 1 {
		t.Fatalf("operations: %v", recorder.operations)
	}
}

func TestGroupCommit_SingleCommitGroupsWriteEveryCommitAlone(t *testing.T) {
	counter := &pointCounter{}
	s, err := Open(t.TempDir(), Options{CacheSize: -1, Logger: discardLogger(), Faults: counter.injector(), MaxCommitsPerGroup: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var commits []*queuedCommit
	for position := 0; position < 5; position++ {
		commits = append(commits, &queuedCommit{events: []*core.Event{newTestEvent(fmt.Sprintf("/s/%d", position), "t", map[string]any{})}})
	}
	before := counter.count("journal.append")
	commitQueued(t, s, commits)
	if groups := counter.count("journal.append") - before; groups != 5 {
		t.Errorf("%d groups, want 5", groups)
	}
	if raw := assertConsistentWithEventLog(t, s); len(raw) != 5 {
		t.Fatalf("%d events", len(raw))
	}
}

func TestGroupCommit_RejectedMemberDoesNotAffectOthers(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	rejected := errors.New("schema rejected")
	cancelled, cancel := context.WithCancel(context.Background())
	commits := []*queuedCommit{
		{events: []*core.Event{newTestEvent("/r/0", "t", map[string]any{})}},
		{events: []*core.Event{newTestEvent("/r/1", "t", map[string]any{})}, validate: func(*core.Event) error { return rejected }},
		{events: []*core.Event{newTestEvent("/r/2", "t", map[string]any{})}, ctx: cancelled},
		{events: []*core.Event{newTestEvent("/r/3", "t", map[string]any{})}},
	}
	s.writeMutex.Lock()
	wait := queueCommits(t, s, commits)
	cancel()
	waitFor(t, "withdrawal", func() bool { return s.queuedCommitCount() == 3 })
	s.writeMutex.Unlock()
	wait()
	if commits[0].err != nil || commits[3].err != nil {
		t.Fatal(commits[0].err, commits[3].err)
	}
	if !errors.Is(commits[1].err, core.ErrInvalidEvent) || !errors.Is(commits[2].err, context.Canceled) {
		t.Fatal(commits[1].err, commits[2].err)
	}
	if commits[2].events[0].ID != "" {
		t.Fatal("withdrawn commit was modified")
	}
	if raw := assertConsistentWithEventLog(t, s); len(raw) != 2 {
		t.Fatalf("%d events", len(raw))
	}
}

// TestGroupCommit_LeaderHandsOverWhenItsContextEnds lets the leader give up
// while it waits behind another writer. The commit behind it must take over
// and complete.
func TestGroupCommit_LeaderHandsOverWhenItsContextEnds(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	impatient, cancel := context.WithCancel(context.Background())
	defer cancel()
	commits := []*queuedCommit{
		{events: []*core.Event{newTestEvent("/h/0", "t", map[string]any{})}, ctx: impatient},
		{events: []*core.Event{newTestEvent("/h/1", "t", map[string]any{})}},
	}
	s.writeMutex.Lock()
	wait := queueCommits(t, s, commits)
	cancel()
	waitFor(t, "withdrawal of the leader", func() bool { return s.queuedCommitCount() == 1 })
	s.writeMutex.Unlock()
	wait()
	if !errors.Is(commits[0].err, context.Canceled) || commits[1].err != nil {
		t.Fatal(commits[0].err, commits[1].err)
	}
	raw := assertConsistentWithEventLog(t, s)
	assertSameIDs(t, "only the patient commit", raw, []string{commits[1].events[0].ID})
}

// TestGroupCommit_ClaimedCommitOutlivesItsContext ends the context of a
// commit while its group is being written. The write is not aborted and the
// commit reports its real outcome.
func TestGroupCommit_ClaimedCommitOutlivesItsContext(t *testing.T) {
	faults := &switchableFaults{}
	s := openTestStore(t, t.TempDir(), testStoreOptions{faults: faults.injector()})
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	commits := []*queuedCommit{
		{events: []*core.Event{newTestEvent("/o/0", "t", map[string]any{})}, ctx: ctx},
		{events: []*core.Event{newTestEvent("/o/1", "t", map[string]any{})}},
	}
	cancelDuringWrite := func(point string) error {
		if point == "events.write" {
			cancel()
		}
		return nil
	}
	faults.during(cancelDuringWrite, func() { commitQueued(t, s, commits) })
	if ctx.Err() == nil {
		t.Fatal("the context was not cancelled during the write")
	}
	if commits[0].err != nil || commits[1].err != nil {
		t.Fatal(commits[0].err, commits[1].err)
	}
	if raw := assertConsistentWithEventLog(t, s); len(raw) != 2 {
		t.Fatalf("%d events", len(raw))
	}
}

func TestGroupCommit_ValidationPanicReachesItsCaller(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	panicked := make(chan any, 1)
	results := make(chan error, 2)
	commit := func(subject string) {
		results <- s.Commit(context.Background(), []*core.Event{newTestEvent(subject, "t", map[string]any{})})
	}
	s.writeMutex.Lock()
	go commit("/p/0")
	waitFor(t, "first", func() bool { return s.queuedCommitCount() == 1 })
	go func() {
		defer func() { panicked <- recover() }()
		s.CommitValidated(context.Background(), []*core.Event{newTestEvent("/p/1", "t", map[string]any{})}, func(*core.Event) error { panic("boom") })
	}()
	waitFor(t, "second", func() bool { return s.queuedCommitCount() == 2 })
	go commit("/p/2")
	waitFor(t, "third", func() bool { return s.queuedCommitCount() == 3 })
	s.writeMutex.Unlock()
	if value := <-panicked; value != "boom" {
		t.Fatalf("panic value %v", value)
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	commitTestEvent(t, s, "/p/after", "t")
	if raw := assertConsistentWithEventLog(t, s); len(raw) != 3 {
		t.Fatalf("%d events", len(raw))
	}
}

// TestGroupCommit_FaultPoints fails a group of several commits at every
// durability point: no member is acknowledged, the store stops writing, and
// after a restart the group is completely present or completely absent.
func TestGroupCommit_FaultPoints(t *testing.T) {
	for _, format := range storageFormats {
		referenceLog := format.ReferenceDataLogFile()
		points := []string{
			"wal.write", "wal.sync",
			"append:" + referenceLog, "sync:" + referenceLog,
			"events.write", "events.sync",
			"shard.write", "shard.sync",
			"journal.append", "journal.sync",
			"publish",
		}
		for _, point := range points {
			t.Run(fmt.Sprintf("%s/%s", format, point), func(t *testing.T) {
				directory := newTestDirectory(t, format)
				faults := &switchableFaults{}
				s := openTestStore(t, directory, testStoreOptions{faults: faults.injector()})
				acknowledged := commitTestEvent(t, s, "/f/0", "t")
				var commits []*queuedCommit
				for position := 0; position < 6; position++ {
					events := []*core.Event{newTestEvent(fmt.Sprintf("/f/%d", position+1), "t", map[string]any{"a": position})}
					events[0].Options.StoreDataAsReference = position%2 == 0
					commits = append(commits, &queuedCommit{events: events})
				}
				faults.during(failAtNthCall(point, 1), func() { commitQueued(t, s, commits) })
				for position, commit := range commits {
					if commit.err == nil {
						t.Errorf("commit %d acknowledged although the group failed at %s", position, point)
					}
				}
				// Every point lies after the write-ahead log write started,
				// so the group may be durable and the store must stop writing.
				if s.IsReady() {
					t.Errorf("store still ready after a failure at %s", point)
				}
				s.Close()

				s = openTestStore(t, directory, testStoreOptions{})
				defer s.Close()
				raw := assertConsistentWithEventLog(t, s)
				isPresent := map[string]bool{}
				for _, event := range raw {
					isPresent[event.ID] = true
				}
				if !isPresent[acknowledged.ID] {
					t.Fatal("acknowledged event lost")
				}
				present := 0
				for _, commit := range commits {
					if isPresent[commit.events[0].ID] {
						present++
					}
				}
				if present != 0 && present != len(commits) {
					t.Fatalf("partial group visible: %d of %d", present, len(commits))
				}
				commitTestEvent(t, s, "/after", "t")
				assertConsistentWithEventLog(t, s)
			})
		}
	}
}

func TestGroupCommit_UnhealthyStoreRejectsEveryQueuedCommit(t *testing.T) {
	faults := &switchableFaults{}
	s := openTestStore(t, t.TempDir(), testStoreOptions{faults: faults.injector()})
	defer s.Close()
	faults.during(failAtNthCall("events.sync", 1), func() {
		if err := s.Commit(context.Background(), []*core.Event{newTestEvent("/u/0", "t", map[string]any{})}); err == nil {
			t.Fatal("expected failure")
		}
	})
	var commits []*queuedCommit
	for position := 0; position < 5; position++ {
		commits = append(commits, &queuedCommit{events: []*core.Event{newTestEvent(fmt.Sprintf("/u/%d", position+1), "t", map[string]any{})}})
	}
	commitQueued(t, s, commits)
	for _, commit := range commits {
		if !errors.Is(commit.err, core.ErrUnhealthy) {
			t.Fatal(commit.err)
		}
	}
}

func TestGroupCommit_CloseEndsQueuedCommits(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	var commits []*queuedCommit
	for position := 0; position < 8; position++ {
		commits = append(commits, &queuedCommit{events: []*core.Event{newTestEvent(fmt.Sprintf("/x/%d", position), "t", map[string]any{})}})
	}
	s.writeMutex.Lock()
	wait := queueCommits(t, s, commits)
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	time.Sleep(20 * time.Millisecond)
	s.writeMutex.Unlock()
	wait()
	for _, commit := range commits {
		if commit.err != nil && !errors.Is(commit.err, core.ErrClosed) {
			t.Fatal(commit.err)
		}
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

func TestGroupCommit_SignedGroupVerifies(t *testing.T) {
	directory := t.TempDir()
	s := openSigningStore(t, directory, newTestEventSigner(t, 1))
	// More events than parallelSigningThreshold, so the group is signed on
	// several cores.
	var commits []*queuedCommit
	for position := 0; position < 48; position++ {
		commits = append(commits, &queuedCommit{events: []*core.Event{newTestEvent(fmt.Sprintf("/signed/%d", position), "t", map[string]any{"n": position})}})
	}
	commitQueued(t, s, commits)
	for position, commit := range commits {
		if commit.err != nil || commit.events[0].Signature == "" {
			t.Fatalf("commit %d: signature %q, error %v", position, commit.events[0].Signature, commit.err)
		}
	}
	if counts := verifyChain(t, s); counts.Problem != "" || counts.Signed != 48 || counts.Unsigned != 0 {
		t.Fatalf("%+v", counts)
	}
	s.Close()
	s = openSigningStore(t, directory, newTestEventSigner(t, 1))
	defer s.Close()
	if counts := verifyChain(t, s); counts.Problem != "" || counts.Signed != 48 {
		t.Fatalf("after restart: %+v", counts)
	}
}
