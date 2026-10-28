package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/storage"
)

// Commit appends events atomically after checking the preconditions under
// the writer lock. Nothing is written when a precondition fails, which is
// reported as *core.PreconditionFailedError.
//
// Commit fills in ID, time, hashes and reference data keys of the events. On
// return, events whose payload went into the reference data log carry their
// payload in Data again.
//
// If an error is returned after the events reached the event log, they are
// durable: the store is marked unhealthy and finishes indexing on restart.
// Do not retry blindly in that case.
func (s *Store) Commit(ctx context.Context, events []*core.Event, preconditions ...core.Precondition) error {
	return s.CommitValidated(ctx, events, nil, preconditions...)
}

// CommitValidated also validates payloads under the writer lock, serialized
// with UpdateSchemas. validate must not call Store methods or mutate events.
func (s *Store) CommitValidated(ctx context.Context, events []*core.Event, validate func(*core.Event) error, preconditions ...core.Precondition) error {
	if len(events) == 0 && len(preconditions) == 0 {
		return nil
	}
	started := time.Now()
	for _, event := range events {
		if err := core.ValidateNewEvent(event); err != nil {
			return err
		}
	}

	if err := acquireContext(ctx, s.writeMutex.TryLock); err != nil {
		return err
	}
	defer s.writeMutex.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.checkWritable(); err != nil {
		return err
	}
	if validate != nil {
		for _, event := range events {
			if err := validate(event); err != nil {
				return fmt.Errorf("%w: %v", core.ErrInvalidEvent, err)
			}
		}
	}
	if err := s.checkPreconditionsWhileLocked(withWriteLockHeld(ctx), preconditions); err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}

	appended, err := s.storage.Append(ctx, events)
	if err != nil {
		s.observer.ErrorOccurred("commit")
		if !errors.Is(err, storage.ErrNothingWritten) {
			// The batch may be durable already. Reject further writes until
			// a restart has recovered it.
			s.index.MarkFailed(err)
			return fmt.Errorf("store marked unhealthy, the batch is recovered on restart: %w", err)
		}
		return err
	}
	if err := s.index.Publish(appended.Events, appended.MainEventLogSize); err != nil {
		s.observer.ErrorOccurred("commit")
		s.index.MarkFailed(err)
		return fmt.Errorf("events are durable but indexing failed; store marked unhealthy until restart recovery: %w", err)
	}
	for position, stored := range appended.Events {
		s.observer.EventCommitted(stored.Event.Subject, stored.Event.Type)
		if data, hasReferenceData := appended.ReferenceData[position]; hasReferenceData {
			stored.Event.Data = data
		}
	}
	s.measure("commit", started, nil)
	return nil
}

// checkPreconditionsWhileLocked evaluates every precondition. The caller
// holds writeMutex. Reads used here only take the shared maintenance lock.
func (s *Store) checkPreconditionsWhileLocked(ctx context.Context, preconditions []core.Precondition) error {
	for _, precondition := range preconditions {
		switch precondition.Type {
		case core.PreconditionIsSubjectNew, core.PreconditionIsSubjectExisting:
			var payload struct {
				Subject string `json:"subject"`
			}
			if err := json.Unmarshal(precondition.Payload, &payload); err != nil {
				return fmt.Errorf("invalid %s payload: %w", precondition.Type, err)
			}
			exists, err := s.subjectExistsContext(ctx, payload.Subject)
			if err != nil {
				return fmt.Errorf("failed to check subject: %w", err)
			}
			if precondition.Type == core.PreconditionIsSubjectNew && exists {
				return &core.PreconditionFailedError{Type: precondition.Type, Message: fmt.Sprintf("subject %s already has events", payload.Subject)}
			}
			if precondition.Type == core.PreconditionIsSubjectExisting && !exists {
				return &core.PreconditionFailedError{Type: precondition.Type, Message: fmt.Sprintf("subject %s has no events", payload.Subject)}
			}
		case core.PreconditionIsQueryResultTrue:
			var payload struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(precondition.Payload, &payload); err != nil {
				return fmt.Errorf("invalid isQueryResultTrue payload: %w", err)
			}
			if s.evaluateGDBQLPrecondition == nil {
				return fmt.Errorf("isQueryResultTrue preconditions are not enabled for this store")
			}
			isTrue, err := s.evaluateGDBQLPrecondition(ctx, s, payload.Query)
			if err != nil {
				return fmt.Errorf("failed to execute query precondition: %w", err)
			}
			if !isTrue {
				return &core.PreconditionFailedError{Type: precondition.Type, Message: fmt.Sprintf("query precondition failed: %s", payload.Query)}
			}
		default:
			return fmt.Errorf("unknown precondition type: %s", precondition.Type)
		}
	}
	return nil
}

// subjectExists reports whether the subject or one of its descendants has an
// event.
func (s *Store) subjectExistsContext(ctx context.Context, subject string) (bool, error) {
	var exists bool
	err := s.withIndexSnapshot(ctx, func(snapshot indexSnapshot) error {
		var err error
		exists, err = s.index.HasSubjectOrDescendant(snapshot.WithContext(ctx), subject)
		return err
	})
	return exists, err
}

// AppendSystemEvent appends an internal system event, for example the audit
// record of an erasure. System events are never returned by reads.
func (s *Store) AppendSystemEvent(eventType, subject string, data json.RawMessage) error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	if s.isClosed {
		return core.ErrClosed
	}
	if _, err := s.storage.AppendSystemEvent(eventType, subject, data); err != nil {
		return err
	}
	s.logger.Info("system event written", "type", eventType, "subject", subject)
	return nil
}

// ListSystemEvents returns every system event. Used for backups.
func (s *Store) ListSystemEvents() ([]*core.Event, error) {
	s.maintenanceMutex.RLock()
	defer s.maintenanceMutex.RUnlock()
	return s.storage.ListSystemEvents()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
