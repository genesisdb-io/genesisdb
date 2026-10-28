package core

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func validNewEvent() *Event {
	return &Event{Source: "app", Subject: "/user/1", Type: "registered", Data: json.RawMessage(`{"name":"Bruce"}`)}
}

func TestValidateNewEvent(t *testing.T) {
	cases := map[string]func(event *Event){
		"nil data":                 func(event *Event) { event.Data = nil },
		"invalid json":             func(event *Event) { event.Data = json.RawMessage(`{`) },
		"empty source":             func(event *Event) { event.Source = " " },
		"empty type":               func(event *Event) { event.Type = "" },
		"subject without slash":    func(event *Event) { event.Subject = "user/1" },
		"subject with whitespace":  func(event *Event) { event.Subject = "/user/1 " },
		"system source":            func(event *Event) { event.Source = SystemEventSource },
		"assigned reference key":   func(event *Event) { event.ReferenceDataKey = "abc" },
		"invalid utf-8 in subject": func(event *Event) { event.Subject = "/user/\xff" },
		"invalid utf-8 in type":    func(event *Event) { event.Type = "t\xfe" },
		"invalid utf-8 in source":  func(event *Event) { event.Source = "\xc3" },
		"invalid utf-8 in data":    func(event *Event) { event.Data = json.RawMessage("{\"a\":\"\xff\"}") },
		"oversized":                func(event *Event) { event.Data = json.RawMessage(`"` + string(make([]byte, 64*1024)) + `"`) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			event := validNewEvent()
			mutate(event)
			err := ValidateNewEvent(event)
			if err == nil {
				t.Fatal("accepted")
			}
			if !errors.Is(err, ErrInvalidEvent) && !errors.Is(err, ErrSystemEventNotAllowed) {
				t.Fatalf("unexpected error class: %v", err)
			}
		})
	}
	if err := ValidateNewEvent(nil); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("nil event: %v", err)
	}
	valid := validNewEvent()
	valid.Subject = "/kunde/Müller/ü"
	valid.Data = json.RawMessage(`{"name":"Grüße 日本"}`)
	valid.ReferenceDataKey = ReferenceDataPending
	if err := ValidateNewEvent(valid); err != nil {
		t.Fatalf("valid UTF-8 event rejected: %v", err)
	}
}

// TestHashEvent_InvalidUTF8DoesNotSurviveEncoding documents why
// ValidateNewEvent rejects invalid UTF-8: encoding replaces it, so the
// persisted record would carry a hash that never verifies.
func TestHashEvent_InvalidUTF8DoesNotSurviveEncoding(t *testing.T) {
	event := &Event{Source: "s", Subject: "/u/\xff", Type: "t", SpecVersion: EventSpecVersion, ID: "x", Time: time.Now(), DataContentType: EventDataContentType, Data: json.RawMessage(`{}`), PredecessorHash: ZeroHash, HashVersion: CurrentHashVersion}
	event.Hash = HashEvent(event)
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Event
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if HasValidHash(&decoded) {
		t.Fatal("expected the round trip to break the hash, the validation guard would be unnecessary")
	}
}
