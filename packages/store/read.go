package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/index"
)

type indexSnapshot = *index.Snapshot

// withIndexSnapshot runs read with a consistent published snapshot. If the
// read finds damaged index data, the indexes are rebuilt from the event logs
// and the read runs once more, see repair.go.
func (s *Store) withIndexSnapshot(ctx context.Context, read func(snapshot indexSnapshot) error) error {
	build, err := s.readIndexSnapshot(ctx, read)
	if err == nil || !s.handleIndexCorruption(ctx, build, err) {
		return err
	}
	_, err = s.readIndexSnapshot(ctx, read)
	return err
}

// readIndexSnapshot runs read once and returns the build it read from.
func (s *Store) readIndexSnapshot(ctx context.Context, read func(snapshot indexSnapshot) error) (string, error) {
	if err := acquireContext(ctx, s.maintenanceMutex.TryRLock); err != nil {
		return "", err
	}
	defer s.maintenanceMutex.RUnlock()
	snapshot, err := s.index.AcquireSnapshot()
	if err != nil {
		return "", err
	}
	return snapshot.Build(), read(snapshot)
}

// loadEventsForPostings reads the events of postings by location, verifies
// them against the postings and, with withReferenceData, replaces payload
// placeholders with their reference data.
func (s *Store) loadEventsForPostings(ctx context.Context, postings []*index.Posting, withReferenceData bool) ([]*core.Event, error) {
	limits := core.ReadLimitsFromContext(ctx)
	if limits.MaxEvents > 0 && len(postings) > limits.MaxEvents {
		return nil, core.ErrResourceLimit
	}
	var loadedBytes int64
	charge := func(event *core.Event) error {
		loadedBytes += int64(len(event.Data) + len(event.Subject) + len(event.Type) + len(event.Source) + 512)
		if limits.MaxBytes > 0 && loadedBytes > limits.MaxBytes {
			return core.ErrResourceLimit
		}
		return nil
	}
	events := make([]*core.Event, 0, len(postings))
	reader := s.storage.NewRecordReader()
	defer reader.Close()
	cacheGeneration := s.cache.Generation()
	var withPendingReferenceData []int
	for _, posting := range postings {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if withReferenceData {
			if cached, isCached := s.cache.Get(posting.Location()); isCached {
				if err := charge(cached); err != nil {
					return nil, err
				}
				events = append(events, cached)
				continue
			}
		}
		event, err := reader.Read(posting.Location())
		if err != nil {
			return nil, fmt.Errorf("%w: %v", core.ErrIndexCorrupt, err)
		}
		if !posting.Matches(event) {
			return nil, fmt.Errorf("%w: record %s@%d does not match its index posting", core.ErrIndexCorrupt, posting.LogFile, posting.Offset)
		}
		if err := charge(event); err != nil {
			return nil, err
		}
		if withReferenceData && event.HasReferenceData() {
			withPendingReferenceData = append(withPendingReferenceData, len(events))
		}
		events = append(events, event)
	}
	if !withReferenceData {
		return events, nil
	}
	payloads := map[string]json.RawMessage{}
	for _, position := range withPendingReferenceData {
		payloads[events[position].ReferenceDataKey] = nil
	}
	if err := s.storage.LoadReferenceDataIntoContext(ctx, payloads); err != nil {
		return nil, err
	}
	for _, position := range withPendingReferenceData {
		event := events[position]
		if payload := payloads[event.ReferenceDataKey]; payload != nil {
			event.Data = append(json.RawMessage(nil), payload...)
			continue
		}
		// The payload lives only in the reference data log when the event
		// data is the placeholder. A missing entry is corruption, never an
		// empty payload. Events restored from plain backups keep their
		// payload inline.
		if core.IsReferenceDataPlaceholder(event.Data) {
			return nil, fmt.Errorf("%w: reference data %s of event %s is missing from %s", core.ErrEventLogCorrupt, event.ReferenceDataKey, event.ID, s.format.ReferenceDataLogFile())
		}
	}
	var bytes int64
	for _, event := range events {
		bytes += int64(len(event.Data) + len(event.Subject) + len(event.Type) + len(event.Source) + 512)
	}
	if limits.MaxBytes > 0 && bytes > limits.MaxBytes {
		return nil, core.ErrResourceLimit
	}
	for position, posting := range postings {
		s.cache.PutIfGenerationMatches(cacheGeneration, posting.Location(), events[position])
	}
	return events, nil
}

// selectAndLoadEvents runs a selection under one snapshot and loads the
// events in log order.
func (s *Store) selectAndLoadEvents(ctx context.Context, operation string, withReferenceData bool, collect func(snapshot indexSnapshot, set *index.PostingSet) error) ([]*core.Event, error) {
	started := time.Now()
	var events []*core.Event
	err := s.withIndexSnapshot(ctx, func(snapshot indexSnapshot) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot = snapshot.WithContext(ctx)
		set := index.NewPostingSetContext(ctx)
		if err := collect(snapshot, set); err != nil {
			return err
		}
		var err error
		events, err = s.loadEventsForPostings(ctx, set.InLogOrder(), withReferenceData)
		return err
	})
	s.measure(operation, started, err)
	return events, err
}

// FetchOptions narrow a fetch. The bound and latest event options keep the
// names and semantics of the HTTP API.
type FetchOptions struct {
	// LowerBound is an event ID. Events before it are skipped.
	LowerBound string
	// IncludeLowerBoundEvent returns the lower bound event itself.
	IncludeLowerBoundEvent bool
	// UpperBound is an event ID. Events after it are skipped.
	UpperBound string
	// IncludeUpperBoundEvent returns the upper bound event itself.
	IncludeUpperBoundEvent bool
	// LatestByEventType returns only the newest event of this type below
	// the subject.
	LatestByEventType string
}

// Fetch returns the events of a subject and all of its descendants in time
// order.
func (s *Store) Fetch(ctx context.Context, subject string, options FetchOptions) ([]*core.Event, error) {
	events, err := s.selectAndLoadEvents(ctx, "fetch", true, func(snapshot indexSnapshot, set *index.PostingSet) error {
		return s.index.Select(snapshot, core.EventSelector{SubjectsWithDescendants: []string{subject}}, set)
	})
	if err != nil {
		return nil, err
	}
	if options.LatestByEventType != "" {
		var latest *core.Event
		for _, event := range events {
			if event.Type == options.LatestByEventType {
				latest = event
			}
		}
		events = []*core.Event{}
		if latest != nil {
			events = []*core.Event{latest}
		}
	}
	return ApplyBounds(events, options), nil
}

// ApplyBounds keeps the events between the lower and upper bound of options.
// Without a lower bound the result starts at the first event. When the
// lower bound does not occur, the result is empty. Processing stops at the
// upper bound.
func ApplyBounds(events []*core.Event, options FetchOptions) []*core.Event {
	if options.LowerBound == "" && options.UpperBound == "" {
		return events
	}
	return newBoundsFilter(options).apply(events)
}

// boundsFilter applies FetchOptions bounds across several consecutive
// batches of one ordered sequence, remembering whether the lower bound was
// passed and whether the upper bound ended the sequence.
type boundsFilter struct {
	options           FetchOptions
	isAfterLowerBound bool
	isDone            bool
}

func newBoundsFilter(options FetchOptions) *boundsFilter {
	return &boundsFilter{options: options, isAfterLowerBound: options.LowerBound == ""}
}

func (filter *boundsFilter) apply(events []*core.Event) []*core.Event {
	if filter.options.LowerBound == "" && filter.options.UpperBound == "" {
		return events
	}
	bounded := make([]*core.Event, 0, len(events))
	if filter.isDone {
		return bounded
	}
	for _, event := range events {
		if filter.options.UpperBound != "" && event.ID == filter.options.UpperBound {
			if filter.isAfterLowerBound && filter.options.IncludeUpperBoundEvent {
				bounded = append(bounded, event)
			}
			filter.isDone = true
			break
		}
		if !filter.isAfterLowerBound {
			if event.ID == filter.options.LowerBound {
				filter.isAfterLowerBound = true
				if filter.options.IncludeLowerBoundEvent {
					bounded = append(bounded, event)
				}
			}
			continue
		}
		bounded = append(bounded, event)
	}
	return bounded
}

// FetchPage returns at most limit events of a subject and its descendants
// in commit order, starting after the event lowerBound (empty: from the
// beginning), and the ID of the last returned event when more events
// follow. Only the postings of the subject are read in full, event records
// and payloads are loaded for the returned page alone, so reading a large
// subject page by page needs memory for one page, not for the subject. The
// read limits of ctx bound the loaded page. System events are skipped. A
// lowerBound that does not occur below the subject yields an empty page, as
// in Fetch.
func (s *Store) FetchPage(ctx context.Context, subject, lowerBound string, limit int) ([]*core.Event, string, error) {
	if limit <= 0 {
		return nil, "", fmt.Errorf("page limit must be positive, got %d", limit)
	}
	started := time.Now()
	var page []*core.Event
	next := ""
	err := s.withIndexSnapshot(ctx, func(snapshot indexSnapshot) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The posting set is unbounded on purpose: postings are small, and
		// the read limits apply to the events that are actually loaded.
		set := index.NewPostingSet()
		if err := s.index.Select(snapshot.WithContext(ctx), core.EventSelector{SubjectsWithDescendants: []string{subject}}, set); err != nil {
			return err
		}
		postings := set.InLogOrder()
		start := 0
		if lowerBound != "" {
			start = len(postings)
			for position, posting := range postings {
				if posting.EventID == lowerBound {
					start = position + 1
					break
				}
			}
		}
		// System events are filtered after loading, so a chunk can come
		// back short. Load further chunks until the page is full.
		for start < len(postings) && len(page) < limit {
			end := min(start+limit-len(page), len(postings))
			events, err := s.loadEventsForPostings(ctx, postings[start:end], true)
			if err != nil {
				return err
			}
			for _, event := range events {
				if !event.IsSystemEvent() {
					page = append(page, event)
				}
			}
			start = end
		}
		if start < len(postings) && len(page) > 0 {
			next = page[len(page)-1].ID
		}
		return nil
	})
	s.measure("fetch", started, err)
	return page, next, err
}

// FetchLatestOfType returns the newest event of eventType below subject
// (the subject itself or a descendant), or nil when there is none. Only that
// one event is loaded.
func (s *Store) FetchLatestOfType(ctx context.Context, subject, eventType string) (*core.Event, error) {
	started := time.Now()
	var latest *core.Event
	err := s.withIndexSnapshot(ctx, func(snapshot indexSnapshot) error {
		set := index.NewPostingSet()
		if err := s.index.Select(snapshot.WithContext(ctx), core.EventSelector{SubjectsWithDescendants: []string{subject}}, set); err != nil {
			return err
		}
		postings := set.InLogOrder()
		for position := len(postings) - 1; position >= 0; position-- {
			if postings[position].EventType != eventType {
				continue
			}
			events, err := s.loadEventsForPostings(ctx, postings[position:position+1], true)
			if err != nil {
				return err
			}
			latest = events[0]
			return nil
		}
		return nil
	})
	s.measure("fetch", started, err)
	return latest, err
}

// FetchExactSubjects returns the events whose subject equals one of
// subjects, without descendants.
func (s *Store) FetchExactSubjects(ctx context.Context, subjects ...string) ([]*core.Event, error) {
	return s.selectAndLoadEvents(ctx, "fetch_exact_subjects", true, func(snapshot indexSnapshot, set *index.PostingSet) error {
		return s.index.SelectExactSubjects(snapshot, subjects, set)
	})
}

// FetchBySubjectPrefix returns the events whose subject lexically starts with
// prefix, for example "/us" matches "/user/1" and "/usa".
func (s *Store) FetchBySubjectPrefix(ctx context.Context, prefix string) ([]*core.Event, error) {
	return s.selectAndLoadEvents(ctx, "fetch_by_subject_prefix", true, func(snapshot indexSnapshot, set *index.PostingSet) error {
		subjects, err := s.index.ExactSubjectsWithPrefix(snapshot, prefix)
		if err != nil {
			return err
		}
		return s.index.SelectExactSubjects(snapshot, subjects, set)
	})
}

// FetchByEventType returns every event of a type.
func (s *Store) FetchByEventType(ctx context.Context, eventType string) ([]*core.Event, error) {
	return s.selectAndLoadEvents(ctx, "fetch_by_event_type", true, func(snapshot indexSnapshot, set *index.PostingSet) error {
		return s.index.Select(snapshot, core.EventSelector{EventTypes: []string{eventType}}, set)
	})
}

// FetchAll returns every regular event with its payload.
func (s *Store) FetchAll(ctx context.Context) ([]*core.Event, error) {
	return s.SelectEvents(ctx, core.EventSelector{All: true})
}

// FetchAllWithoutReferenceData returns every regular event exactly as
// persisted: payloads stored as reference data stay placeholders. Used for
// backups.
func (s *Store) FetchAllWithoutReferenceData(ctx context.Context) ([]*core.Event, error) {
	return s.selectAndLoadEvents(ctx, "fetch_all_without_reference_data", false, func(snapshot indexSnapshot, set *index.PostingSet) error {
		return s.index.Select(snapshot, core.EventSelector{All: true}, set)
	})
}

// SelectEvents returns the union of a selector, deduplicated and in time
// order. It implements core.EventSource for the GDBQL engine.
func (s *Store) SelectEvents(ctx context.Context, selector core.EventSelector) ([]*core.Event, error) {
	return s.selectAndLoadEvents(ctx, "select_events", true, func(snapshot indexSnapshot, set *index.PostingSet) error {
		return s.index.Select(snapshot, selector, set)
	})
}

// CountSelectedEvents returns the number of regular events a selector
// selects, from the index alone. No event record is read, so read budgets
// for events and bytes do not apply. It implements core.EventCounter.
func (s *Store) CountSelectedEvents(ctx context.Context, selector core.EventSelector) (int, error) {
	started := time.Now()
	count := 0
	err := s.withIndexSnapshot(ctx, func(snapshot indexSnapshot) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		set := index.NewPostingSet()
		if err := s.index.Select(snapshot.WithContext(ctx), selector, set); err != nil {
			return err
		}
		count = set.Len()
		return nil
	})
	s.measure("count_events", started, err)
	return count, err
}

// FindEventByID returns the first physical event with the ID, or
// core.ErrNotFound.
func (s *Store) FindEventByID(ctx context.Context, eventID string) (*core.Event, error) {
	return s.findOne(ctx, func(snapshot indexSnapshot) (*index.Posting, error) {
		return s.index.FindFirstPostingOfEventID(snapshot, eventID)
	})
}

// FindLatestEvent returns the newest event whose subject equals subject, or
// core.ErrNotFound.
func (s *Store) FindLatestEvent(ctx context.Context, subject string) (*core.Event, error) {
	return s.findOne(ctx, func(snapshot indexSnapshot) (*index.Posting, error) {
		return s.index.FindLatestPostingOfSubject(snapshot, subject)
	})
}

// FindLatestEventOfType returns the newest event with the exact subject and
// type, or core.ErrNotFound.
func (s *Store) FindLatestEventOfType(ctx context.Context, subject, eventType string) (*core.Event, error) {
	return s.findOne(ctx, func(snapshot indexSnapshot) (*index.Posting, error) {
		return s.index.FindLatestPostingOfSubjectAndType(snapshot, subject, eventType)
	})
}

func (s *Store) findOne(ctx context.Context, find func(snapshot indexSnapshot) (*index.Posting, error)) (*core.Event, error) {
	var event *core.Event
	err := s.withIndexSnapshot(ctx, func(snapshot indexSnapshot) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		posting, err := find(snapshot.WithContext(ctx))
		if err != nil {
			return err
		}
		if posting == nil {
			return core.ErrNotFound
		}
		events, err := s.loadEventsForPostings(ctx, []*index.Posting{posting}, true)
		if err != nil {
			return err
		}
		event = events[0]
		return nil
	})
	return event, err
}

// listDistinctCatalogKeys collects sorted distinct values from the catalog.
func (s *Store) listDistinctCatalogKeys(ctx context.Context, key func(posting *index.Posting) string) ([]string, error) {
	distinct := map[string]struct{}{}
	limits := core.ReadLimitsFromContext(ctx)
	var size int64
	err := s.withIndexSnapshot(ctx, func(snapshot indexSnapshot) error {
		return s.index.ScanSubjectCatalog(snapshot.WithContext(ctx), func(posting *index.Posting) error {
			value := key(posting)
			if _, exists := distinct[value]; !exists {
				size += int64(len(value) + 64)
				if (limits.MaxEvents > 0 && len(distinct) >= limits.MaxEvents) || (limits.MaxBytes > 0 && size > limits.MaxBytes) {
					return core.ErrResourceLimit
				}
			}
			distinct[value] = struct{}{}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(distinct))
	for value := range distinct {
		values = append(values, value)
	}
	sort.Strings(values)
	return values, nil
}

// ListSubjects returns every distinct subject, sorted.
func (s *Store) ListSubjects(ctx context.Context) ([]string, error) {
	return s.listDistinctCatalogKeys(ctx, func(posting *index.Posting) string { return posting.Subject })
}

// ListTopLevelSubjects returns every distinct first path segment, for
// example "/user", sorted.
func (s *Store) ListTopLevelSubjects(ctx context.Context) ([]string, error) {
	return s.listDistinctCatalogKeys(ctx, func(posting *index.Posting) string { return core.TopLevelSubject(posting.Subject) })
}

// ListEventTypes returns every distinct event type, sorted.
func (s *Store) ListEventTypes(ctx context.Context) ([]string, error) {
	return s.listDistinctCatalogKeys(ctx, func(posting *index.Posting) string { return posting.EventType })
}
