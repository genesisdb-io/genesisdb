package store

import (
	"context"
	"time"
)

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
