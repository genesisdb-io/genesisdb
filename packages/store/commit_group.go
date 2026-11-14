package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/storage"
)

// Group commit. Commits that wait at the same moment are written as one
// storage batch and one index publication, so they share every device flush.
// A group is written exactly like one large client batch: the storage
// engine, the index engine, the write-ahead log and recovery see no
// difference.
//
// One of the waiting callers leads a group, there is no background
// goroutine. Callers queue in arrival order. The caller that gets the leader
// baton takes writeMutex, claims a group from the head of the queue, writes
// it and gives every member its own result.
//
// Preconditions are always evaluated against published, durable state. A
// commit joins a group only if none of its preconditions can observe the
// unpublished events of the members before it, otherwise it starts the next
// group. The outcome therefore equals running the commits one after another
// in queue order.

const (
	// DefaultMaxCommitsPerGroup is the default of Options.MaxCommitsPerGroup.
	DefaultMaxCommitsPerGroup = 256
	// DefaultMaxEventsPerGroup and DefaultMaxBytesPerGroup are the defaults
	// of Options.MaxEventsPerGroup and Options.MaxBytesPerGroup. The limits
	// only split groups: the first member is always admitted, whatever its
	// size.
	DefaultMaxEventsPerGroup = 1024
	DefaultMaxBytesPerGroup  = 8 << 20
)

// pendingCommit is one Commit call waiting in the queue.
type pendingCommit struct {
	ctx           context.Context
	events        []*core.Event
	validate      func(*core.Event) error
	preconditions []core.Precondition
	started       time.Time

	// Admission data, computed once before the commit is queued.
	observedSubjects   []string
	observesEverything bool
	sizeBytes          int

	// isClaimed is guarded by commitQueue.mutex. A claimed commit can no
	// longer be withdrawn: its events are modified by the leader.
	isClaimed bool

	// The fields below are written by the leader before done is closed.
	batchOffset int
	isFinished  bool
	err         error
	panicValue  any
	done        chan struct{}
}

func newPendingCommit(ctx context.Context, events []*core.Event, validate func(*core.Event) error, preconditions []core.Precondition, started time.Time) *pendingCommit {
	member := &pendingCommit{ctx: ctx, events: events, validate: validate, preconditions: preconditions, started: started, done: make(chan struct{})}
	for _, event := range events {
		member.sizeBytes += len(event.Source) + len(event.Subject) + len(event.Type) + len(event.Data)
	}
	for _, precondition := range preconditions {
		switch precondition.Type {
		case core.PreconditionIsSubjectNew, core.PreconditionIsSubjectExisting:
			var payload struct {
				Subject string `json:"subject"`
			}
			// An invalid payload is rejected during evaluation and reads
			// nothing.
			if json.Unmarshal(precondition.Payload, &payload) == nil {
				member.observedSubjects = append(member.observedSubjects, payload.Subject)
			}
		case core.PreconditionIsQueryResultTrue:
			member.observesEverything = true
		}
	}
	return member
}

// finish publishes the result of the commit to its caller.
func (member *pendingCommit) finish(err error) {
	if member.isFinished {
		return
	}
	member.isFinished = true
	member.err = err
	close(member.done)
}

// result returns the outcome after done is closed. A panic of the caller's
// validation callback surfaces in the caller's goroutine, as without groups.
func (member *pendingCommit) result() error {
	if member.panicValue != nil {
		panic(member.panicValue)
	}
	return member.err
}

// commitQueue holds waiting commits in arrival order. Its mutex is a leaf
// lock and is never held during I/O.
type commitQueue struct {
	mutex   sync.Mutex
	members []*pendingCommit
}

func (queue *commitQueue) enqueue(member *pendingCommit) {
	queue.mutex.Lock()
	queue.members = append(queue.members, member)
	queue.mutex.Unlock()
}

// withdraw removes a commit that no leader has claimed. It reports false
// for a claimed commit, whose outcome must be awaited.
func (queue *commitQueue) withdraw(member *pendingCommit) bool {
	queue.mutex.Lock()
	defer queue.mutex.Unlock()
	if member.isClaimed {
		return false
	}
	for position, candidate := range queue.members {
		if candidate == member {
			queue.members = slices.Delete(queue.members, position, position+1)
			break
		}
	}
	return true
}

// head returns the oldest waiting commit without claiming it, or nil.
func (queue *commitQueue) head() *pendingCommit {
	queue.mutex.Lock()
	defer queue.mutex.Unlock()
	if len(queue.members) == 0 {
		return nil
	}
	return queue.members[0]
}

// claim removes member from the head of the queue. It reports false when
// the caller withdrew the commit in the meantime.
func (queue *commitQueue) claim(member *pendingCommit) bool {
	queue.mutex.Lock()
	defer queue.mutex.Unlock()
	if len(queue.members) == 0 || queue.members[0] != member {
		return false
	}
	queue.members[0] = nil
	queue.members = queue.members[1:]
	member.isClaimed = true
	return true
}

// commitGroup is the state of one group while its leader holds writeMutex.
type commitGroup struct {
	claimed  []*pendingCommit
	accepted []*pendingCommit
	batch    []*core.Event
	// sizeBytes is the payload size of the batch.
	sizeBytes int
	// unpublishedKeys holds every key under which the events of the batch
	// will be indexed in the recursive category: each subject and each of
	// its ancestors.
	unpublishedKeys map[string]struct{}
	// publishedSubjects caches subject existence in the published state,
	// which cannot change while the leader holds writeMutex.
	publishedSubjects map[string]bool
}

// subjectExistsWhileLocked reports whether the subject or a descendant has a
// published event. The caller holds writeMutex.
func (s *Store) subjectExistsWhileLocked(ctx context.Context, group *commitGroup, subject string) (bool, error) {
	if exists, isKnown := group.publishedSubjects[subject]; isKnown {
		return exists, nil
	}
	exists, err := s.subjectExistsContext(ctx, subject)
	if err == nil {
		group.publishedSubjects[subject] = exists
	}
	return exists, err
}

// canJoinGroupWhileLocked reports whether member can be written with the
// commits already accepted into group. The first commit always can.
//
// A later commit must not observe the unpublished events before it. A query
// precondition can observe anything. A subject precondition observes them
// only if an unpublished event is indexed under its subject and the subject
// does not exist in the published state: existence never ends, so a subject
// that exists already decides the precondition on its own.
func (s *Store) canJoinGroupWhileLocked(group *commitGroup, member *pendingCommit) bool {
	if len(group.claimed) == 0 {
		return true
	}
	if len(group.claimed) >= s.maxCommitsPerGroup || len(group.batch)+len(member.events) > s.maxEventsPerGroup || group.sizeBytes+member.sizeBytes > s.maxBytesPerGroup {
		return false
	}
	if member.observesEverything {
		return false
	}
	for _, subject := range member.observedSubjects {
		if _, isUnpublished := group.unpublishedKeys[subject]; !isUnpublished {
			continue
		}
		exists, err := s.subjectExistsWhileLocked(withWriteLockHeld(member.ctx), group, subject)
		if err != nil || !exists {
			return false
		}
	}
	return true
}

// acquireWriteLock takes writeMutex for a writer that is not a commit:
// maintenance, schema updates and reference data replacement. It announces
// the waiter, so commit leaders step back until it has the lock. Without
// the announcement the leader of the next group retakes the lock before the
// waiter sees it free under sustained write load.
//
// The priority is bounded in both directions. A writer that releases the
// lock through releaseWriteLock while commits are queued hands the next
// turn to them: other announced writers then wait for one commit group.
// Commits and other writers therefore alternate under load, and neither
// side can starve the other.
func (s *Store) acquireWriteLock(ctx context.Context) error {
	s.waitingWriters.Add(1)
	err := acquireContextNotified(ctx, s.tryLockForOtherWriter, &s.writeLockReleases)
	s.waitingWriters.Add(-1)
	if err != nil {
		// A leader that stepped back for this writer may proceed.
		s.writeLockReleases.notify()
	}
	return err
}

// releaseWriteLock releases writeMutex taken with acquireWriteLock. Queued
// commits get the next turn.
func (s *Store) releaseWriteLock() {
	if s.commitQueue.head() != nil {
		s.isCommitTurn.Store(true)
	}
	s.writeMutex.Unlock()
	s.writeLockReleases.notify()
}

// tryLockForOtherWriter takes writeMutex for a writer that is not a commit,
// unless the turn belongs to queued commits. A turn whose commits were all
// withdrawn does not block.
func (s *Store) tryLockForOtherWriter() bool {
	if s.isCommitTurn.Load() && s.commitQueue.head() != nil {
		return false
	}
	return s.writeMutex.TryLock()
}

// tryLockForCommitGroup takes writeMutex for a group leader. It steps back
// while another writer is announced through acquireWriteLock, unless the
// turn belongs to the commits. The turn ends with the group.
func (s *Store) tryLockForCommitGroup() bool {
	if s.waitingWriters.Load() != 0 && !s.isCommitTurn.Load() {
		return false
	}
	if !s.writeMutex.TryLock() {
		return false
	}
	s.isCommitTurn.Store(false)
	return true
}

// awaitCommit queues the commit and waits for its result. While waiting it
// may lead groups, also groups ahead of its own commit. That is work the
// caller would otherwise wait for: every group it leads is in front of its
// commit in the queue, and it never leads once its own commit is finished.
func (s *Store) awaitCommit(ctx context.Context, member *pendingCommit) error {
	s.commitQueue.enqueue(member)
	for {
		select {
		case <-member.done:
			return member.result()
		default:
		}
		if err := ctx.Err(); err != nil {
			if s.commitQueue.withdraw(member) {
				return err
			}
			<-member.done
			return member.result()
		}
		select {
		case <-member.done:
			return member.result()
		case s.commitLeader <- struct{}{}:
			s.leadCommitGroup(ctx, member)
		case <-ctx.Done():
		}
	}
}

// leadCommitGroup writes one group. The caller holds the leader baton,
// which is released on return.
func (s *Store) leadCommitGroup(ctx context.Context, own *pendingCommit) {
	defer func() { <-s.commitLeader }()
	select {
	case <-own.done:
		return
	default:
	}
	if err := acquireContextNotified(ctx, s.tryLockForCommitGroup, &s.writeLockReleases); err != nil {
		return
	}
	defer func() {
		s.writeMutex.Unlock()
		s.writeLockReleases.notify()
	}()
	started := time.Now()
	group := &commitGroup{unpublishedKeys: map[string]struct{}{}, publishedSubjects: map[string]bool{}}
	hasAppendStarted := false
	defer func() {
		problem := recover()
		if problem == nil {
			return
		}
		// Followers must never wait for a leader that is gone.
		err := fmt.Errorf("commit group failed: %v", problem)
		if hasAppendStarted {
			s.index.MarkFailed(err)
		}
		for _, member := range group.claimed {
			member.finish(err)
		}
		panic(problem)
	}()
	writableErr := s.checkWritable()
	for {
		member := s.commitQueue.head()
		if member == nil || !s.canJoinGroupWhileLocked(group, member) {
			break
		}
		if !s.commitQueue.claim(member) {
			continue
		}
		group.claimed = append(group.claimed, member)
		if writableErr != nil {
			member.finish(writableErr)
			continue
		}
		if err := s.evaluateCommitWhileLocked(group, member); err != nil {
			member.finish(err)
			continue
		}
		if len(member.events) == 0 {
			member.finish(nil)
			continue
		}
		member.batchOffset = len(group.batch)
		group.batch = append(group.batch, member.events...)
		group.sizeBytes += member.sizeBytes
		group.accepted = append(group.accepted, member)
		for _, event := range member.events {
			for _, key := range core.SubjectAndAncestors(event.Subject) {
				group.unpublishedKeys[key] = struct{}{}
			}
		}
	}
	if len(group.batch) == 0 {
		return
	}
	hasAppendStarted = true
	// The write is detached from the leader's cancellation: once commits of
	// other callers are claimed, one caller going away must not fail them.
	err := s.writeCommitGroupWhileLocked(context.WithoutCancel(ctx), group)
	for _, member := range group.accepted {
		if err != nil {
			s.observer.ErrorOccurred("commit")
		} else {
			s.measure("commit", member.started, nil)
		}
		member.finish(err)
	}
	if err == nil {
		s.measure("commit_group", started, nil)
	}
}

// evaluateCommitWhileLocked validates one claimed commit and checks its
// preconditions against the published state. The caller holds writeMutex.
func (s *Store) evaluateCommitWhileLocked(group *commitGroup, member *pendingCommit) (err error) {
	defer func() {
		if problem := recover(); problem != nil {
			member.panicValue = problem
			err = fmt.Errorf("commit failed: %v", problem)
		}
	}()
	if err := member.ctx.Err(); err != nil {
		return err
	}
	if member.validate != nil {
		for _, event := range member.events {
			if err := member.validate(event); err != nil {
				return fmt.Errorf("%w: %v", core.ErrInvalidEvent, err)
			}
		}
	}
	return s.checkPreconditionsWhileLocked(withWriteLockHeld(member.ctx), group, member.preconditions)
}

// writeCommitGroupWhileLocked appends and publishes the events of every
// accepted commit as one batch. The caller holds writeMutex.
func (s *Store) writeCommitGroupWhileLocked(ctx context.Context, group *commitGroup) error {
	appended, err := s.storage.Append(ctx, group.batch)
	if err != nil {
		if !errors.Is(err, storage.ErrNothingWritten) {
			// The batch may be durable already. Reject further writes until
			// a restart has recovered it.
			s.index.MarkFailed(err)
			return fmt.Errorf("store marked unhealthy, the batch is recovered on restart: %w", err)
		}
		return err
	}
	if err := s.index.Publish(appended.Events, appended.MainEventLogSize); err != nil {
		s.index.MarkFailed(err)
		return fmt.Errorf("events are durable but indexing failed; store marked unhealthy until restart recovery: %w", err)
	}
	for _, member := range group.accepted {
		for position, event := range member.events {
			s.observer.EventCommitted(event.Subject, event.Type)
			if data, hasReferenceData := appended.ReferenceData[member.batchOffset+position]; hasReferenceData {
				event.Data = data
			}
		}
	}
	return nil
}
