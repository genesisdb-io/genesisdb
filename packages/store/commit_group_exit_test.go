package store

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

func TestGroupCommit_LeaderExitFinishesFollowers(t *testing.T) {
	s := openTestStore(t, t.TempDir(), testStoreOptions{})
	defer s.Close()
	ctx := context.Background()
	events := func(subject string) []*core.Event {
		return []*core.Event{newTestEvent(subject, "t", map[string]any{})}
	}
	// Queue followers directly so the third caller must lead. Its own
	// request is still queued when the second validation stops the leader.
	first := newPendingCommit(ctx, events("/first"), nil, nil, time.Now())
	second := newPendingCommit(ctx, events("/second"), func(*core.Event) error {
		runtime.Goexit()
		return nil
	}, nil, time.Now())
	s.commitQueue.enqueue(first)
	s.commitQueue.enqueue(second)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		s.Commit(ctx, events("/leader"))
	}()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("leader did not exit")
	}
	for _, member := range []*pendingCommit{first, second} {
		select {
		case <-member.done:
			if member.result() == nil {
				t.Fatal("unfinished commit reported success")
			}
		default:
			t.Fatal("leader exited without releasing follower")
		}
	}
	if s.queuedCommitCount() != 0 {
		t.Fatal("leader left its own request in the queue")
	}
	commitTestEvent(t, s, "/after", "t")
	if raw := assertConsistentWithEventLog(t, s); len(raw) != 1 {
		t.Fatalf("expected only the subsequent event, got %d events", len(raw))
	}
}
