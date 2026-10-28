package store

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// decodeExact decodes JSON with numbers kept as text, so values outside
// the float64 range compare correctly.
func decodeExact(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	err := decoder.Decode(&value)
	return value, err
}

// FuzzCommitRoundTrip checks the invariant that an accepted commit is fully
// recoverable: the event is found by its exact subject, by its ID, carries
// the submitted payload byte for byte, and the hash chain verifies. Invalid
// UTF-8 in a subject or type would break all three, so it is rejected.
//
// One store is shared by every iteration, so the chain grows and every
// verification covers the predecessors as well.
func FuzzCommitRoundTrip(f *testing.F) {
	f.Add("/user/1", "registered", "app", `{"name":"Bruce"}`)
	f.Add("/kunde/Müller", "angelegt", "crm", `{"stadt":"Zürich"}`)
	f.Add("/", "root", "app", `{}`)
	f.Add("/a//b", "double-slash", "app", `{"n":1}`)
	f.Add("/u/\xff", "t", "app", `{}`)
	f.Add("/u/1", "t\xfe", "app", `{}`)
	f.Add("/u/1", "t", "app", "{\"a\":\"\xff\"}")
	f.Add("/u/1", "t", "app", `[1,2]`)
	f.Add("/u/1", "t", "app", `"text"`)
	f.Add("/u/1", "t", "app", `{"a":1}{"b":2}`)
	f.Add("/u/1", "t", "app", `{"\u0000":"\u2028"}`)
	f.Add(" /u/1", "t", "app", `{}`)
	f.Add("/u/1\n", "t", "app", `{}`)
	f.Add("/u/{\"k\":", "t", "app", `{}`)
	f.Add("/u/1", core.SystemEventSource, "app", `{}`)
	f.Add("/u/1", "t", "app", `"&<>"`)
	f.Add("/u/1", "t", "app", `{ "a" : [ 1 , 2 ] }`)
	f.Add("/u/1", "t", "app", `{"a":1,"a":2}`)
	f.Add("/u/1", "t", "app", `1e400`)
	f.Add("/u/1", "t", "app", `{"n":12345678901234567890123}`)

	ctx := context.Background()
	s, err := tryOpenTestStore(f.TempDir(), testStoreOptions{convertLegacy: true})
	if err != nil {
		f.Fatal(err)
	}
	defer s.Close()

	f.Fuzz(func(t *testing.T, subject, eventType, source, data string) {
		if len(subject)+len(eventType)+len(source)+len(data) > 60*1024 {
			return
		}
		event := &core.Event{Source: source, Subject: subject, Type: eventType, Data: json.RawMessage(data)}
		if err := s.Commit(ctx, []*core.Event{event}); err != nil {
			if !s.IsReady() {
				t.Fatalf("rejected commit left the store unhealthy: %v", err)
			}
			return
		}
		if event.ID == "" || event.Hash == "" {
			t.Fatalf("accepted event without ID or hash: %+v", event)
		}
		found, err := s.FindEventByID(ctx, event.ID)
		if err != nil {
			t.Fatalf("accepted event %s not found by ID: %v", event.ID, err)
		}
		if found.Subject != subject || found.Type != eventType || found.Source != source {
			t.Fatalf("stored metadata differs from the request: %q/%q/%q vs %q/%q/%q", found.Subject, found.Type, found.Source, subject, eventType, source)
		}
		// Payloads are stored in the canonical form of encoding/json: compact,
		// with <, > and & escaped. The value is identical, the bytes may not be.
		submitted, err := decodeExact([]byte(data))
		if err != nil {
			t.Fatalf("accepted payload is not JSON: %v", err)
		}
		stored, err := decodeExact(found.Data)
		if err != nil {
			t.Fatalf("stored payload is not JSON: %v", err)
		}
		if !reflect.DeepEqual(submitted, stored) {
			t.Fatalf("stored payload differs from the request: %s vs %s", found.Data, data)
		}
		exact, err := s.FetchExactSubjects(ctx, subject)
		if err != nil {
			t.Fatalf("exact fetch failed: %v", err)
		}
		isListed := false
		for _, candidate := range exact {
			if candidate.ID == event.ID {
				isListed = true
			}
		}
		if !isListed {
			t.Fatalf("accepted event %s is not found under its exact subject %q", event.ID, subject)
		}
		report, err := s.VerifyHashChain(ctx)
		if err != nil {
			t.Fatalf("hash chain verification failed: %v", err)
		}
		if !report.IsValid() {
			t.Fatalf("hash chain invalid after accepted commit of %q/%q: %s", subject, eventType, report.Problem)
		}
	})
}
