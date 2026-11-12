package store

import (
	"context"
	"sync"
	"time"
)

// releaseBroadcast wakes every waiter of a lock when it is released, so a
// handover does not wait for the next poll. The zero value is ready to use.
type releaseBroadcast struct {
	mutex   sync.Mutex
	channel chan struct{}
}

// next returns a channel that is closed by the next notify. A waiter takes
// it before trying the lock, so a release in between is never missed.
func (broadcast *releaseBroadcast) next() <-chan struct{} {
	broadcast.mutex.Lock()
	defer broadcast.mutex.Unlock()
	if broadcast.channel == nil {
		broadcast.channel = make(chan struct{})
	}
	return broadcast.channel
}

// notify wakes the current waiters. Without waiters it does nothing.
func (broadcast *releaseBroadcast) notify() {
	broadcast.mutex.Lock()
	if broadcast.channel != nil {
		close(broadcast.channel)
		broadcast.channel = nil
	}
	broadcast.mutex.Unlock()
}

// acquireContextNotified is acquireContext for a lock whose releases are
// announced through broadcast. The poll stays as a fallback for releases
// that are not announced and for conditions of tryLock that change without
// a release.
func acquireContextNotified(ctx context.Context, tryLock func() bool, broadcast *releaseBroadcast) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tryLock() {
		return nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		released := broadcast.next()
		if tryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-released:
		case <-ticker.C:
		}
	}
}

// acquireContext makes lock contention cancellable without a waiter goroutine.
// Uncontended acquisitions do not allocate a timer.
func acquireContext(ctx context.Context, tryLock func() bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tryLock() {
		return nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if tryLock() {
				return nil
			}
		}
	}
}
