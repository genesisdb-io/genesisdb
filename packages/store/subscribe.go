package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/index"
)

// Subscription interval bounds.
const (
	DefaultPollInterval = time.Second
	MinimumPollInterval = 100 * time.Millisecond
	MaximumPollInterval = 24 * time.Hour
)

// maxConsecutivePollFailures is the number of failed polls in a row after
// which a subscription gives up and reports the error to the subscriber. A
// subscriber must never hang silently on a persistent failure.
const maxConsecutivePollFailures = 5

// ErrSubscriptionFailed wraps the last poll error of an ended subscription.
var ErrSubscriptionFailed = errors.New("subscription ended after repeated poll failures")

// SubscribeOptions configure a subscription.
type SubscribeOptions struct {
	// Bounds of the initial history, see FetchOptions. LatestByEventType
	// is not supported for subscriptions and is ignored.
	FetchOptions
	// PollInterval between two checks for new events. It is clamped to
	// MinimumPollInterval and MaximumPollInterval. 0 uses
	// DefaultPollInterval.
	PollInterval time.Duration
}

// ClampPollInterval applies the default and the bounds to an interval.
func ClampPollInterval(interval time.Duration) time.Duration {
	switch {
	case interval <= 0:
		return DefaultPollInterval
	case interval < MinimumPollInterval:
		return MinimumPollInterval
	case interval > MaximumPollInterval:
		return MaximumPollInterval
	}
	return interval
}

// SubscriptionBatch is one delivery of a subscription.
type SubscriptionBatch struct {
	// Events are new events in commit order. A batch without events and
	// without an error is a heartbeat that tells the subscriber the
	// subscription is alive.
	Events []*core.Event
	// IsHistory marks the first batch, which holds the existing events.
	IsHistory bool
	// HasMore reports that the batch was cut at the read budget and the
	// next batch follows immediately, without waiting for the interval.
	HasMore bool
	// Err is set on the final batch when the subscription ends because of
	// repeated failures. The channel is closed afterwards.
	Err error
}

// Subscription delivers the events of a subject and its descendants: first
// the existing history, then new events as they are committed.
//
// Polls are bounded by the read limits of the context. When more events were
// committed than one batch may hold, they are delivered as several
// consecutive batches. A poll that fails repeatedly ends the subscription
// with an error batch instead of retrying silently forever.
type Subscription struct {
	batches chan SubscriptionBatch
}

// Batches returns the delivery channel. It is closed when the context of
// the subscription ends or after a final error batch.
func (subscription *Subscription) Batches() <-chan SubscriptionBatch { return subscription.batches }

// Subscribe starts a subscription. The initial history is read before
// Subscribe returns, so an error there is returned directly. Each poll only
// reads index data published since the previous poll. After a rebuild or a
// restore the history is read again and events that were already delivered
// are skipped.
//
// The history is delivered in one or more batches bounded by the read
// limits of ctx. A history that exceeds the budget is therefore not an
// error, it is delivered incrementally with HasMore set.
func (s *Store) Subscribe(ctx context.Context, subject string, options SubscribeOptions) (*Subscription, error) {
	poller := &subscriptionPoller{store: s, subject: subject, limits: core.ReadLimitsFromContext(ctx)}
	history, err := poller.poll(ctx)
	if err != nil {
		return nil, err
	}
	// The cursor tracks physical offsets, including bounded-out history.
	// The bounds filter keeps its state across split history batches.
	bounds := newBoundsFilter(options.FetchOptions)
	subscription := &Subscription{batches: make(chan SubscriptionBatch, 1)}
	subscription.batches <- SubscriptionBatch{Events: bounds.apply(history.Events), IsHistory: true, HasMore: history.HasMore}

	go func() {
		defer close(subscription.batches)
		interval := ClampPollInterval(options.PollInterval)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		isHistory := history.HasMore
		failures := 0
		for {
			// A cut batch is followed up immediately, the subscriber is
			// behind and waiting would only widen the gap.
			if !poller.cursor.HasMore() {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			} else if ctx.Err() != nil {
				return
			}
			batch, err := poller.poll(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				failures++
				s.logger.Warn("subscription poll failed", "subject", subject, "attempt", failures, "error", err)
				if failures < maxConsecutivePollFailures {
					continue
				}
				final := SubscriptionBatch{Err: fmt.Errorf("%w: %w", ErrSubscriptionFailed, err)}
				select {
				case subscription.batches <- final:
				case <-ctx.Done():
				}
				return
			}
			failures = 0
			// Remaining history after the first batch still belongs to the
			// history, so bounds apply to it as well.
			if isHistory {
				batch.Events = bounds.apply(batch.Events)
				batch.IsHistory = true
				isHistory = batch.HasMore
			}
			select {
			case subscription.batches <- batch:
			case <-ctx.Done():
				return
			}
		}
	}()
	return subscription, nil
}

// subscriptionPoller holds the cursor and the adaptive batch size of one
// subscription.
type subscriptionPoller struct {
	store   *Store
	subject string
	cursor  *index.SubjectCursor
	// limits are the configured read limits. maxEvents is the current batch
	// size, which shrinks when resolved payloads exceed the byte budget and
	// grows back on success, so a run of large reference payloads cannot
	// stall the subscription.
	limits    core.ReadLimits
	maxEvents int
}

// poll returns the next batch and advances the cursor on success.
func (poller *subscriptionPoller) poll(ctx context.Context) (SubscriptionBatch, error) {
	if poller.maxEvents <= 0 {
		poller.maxEvents = poller.limits.MaxEvents
	}
	for {
		limits := poller.limits
		limits.MaxEvents = poller.maxEvents
		events, next, err := poller.store.fetchSinceCursor(core.WithReadLimits(ctx, limits), poller.subject, poller.cursor)
		if err == nil {
			poller.cursor = next
			if poller.maxEvents > 0 && poller.maxEvents < poller.limits.MaxEvents {
				poller.maxEvents = min(poller.maxEvents*2, poller.limits.MaxEvents)
			}
			return SubscriptionBatch{Events: events, HasMore: next.HasMore()}, nil
		}
		if !errors.Is(err, core.ErrResourceLimit) {
			return SubscriptionBatch{}, err
		}
		// The postings fit, but the loaded payloads did not. Halve the
		// batch and try again. One event that alone exceeds the byte
		// budget is a genuine error.
		switch {
		case poller.maxEvents <= 1:
			return SubscriptionBatch{}, err
		case poller.maxEvents <= 0:
			poller.maxEvents = 1024
		default:
			poller.maxEvents /= 2
		}
	}
}

// fetchSinceCursor returns the events of a subject and its descendants that
// were published after cursor, and the cursor for the next call. The read
// limits of ctx bound the batch, see index.PostingsOfSubjectSince.
func (s *Store) fetchSinceCursor(ctx context.Context, subject string, cursor *index.SubjectCursor) ([]*core.Event, *index.SubjectCursor, error) {
	started := time.Now()
	var events []*core.Event
	var next *index.SubjectCursor
	err := s.withIndexSnapshot(ctx, func(snapshot indexSnapshot) error {
		postings, nextCursor, err := s.index.PostingsOfSubjectSince(snapshot.WithContext(ctx), subject, cursor)
		if err != nil {
			return err
		}
		next = nextCursor
		events, err = s.loadEventsForPostings(ctx, postings, true)
		return err
	})
	s.measure("subscribe", started, err)
	return events, next, err
}
