package core

import (
	"encoding/json"
	"testing"
)

func TestVersionedHashBindsPredecessorAndRejectsFallback(t *testing.T) {
	event := &Event{Source: "test", Subject: "/a/1", Type: "created", Data: json.RawMessage(`{}`), HashVersion: CurrentHashVersion, PredecessorHash: ZeroHash}
	event.Hash = HashEvent(event)
	if !HasValidHash(event) {
		t.Fatal("valid hash rejected")
	}
	event.PredecessorHash = "changed"
	if HasValidHash(event) {
		t.Fatal("predecessor was not authenticated")
	}
	event.Hash = HashData(event.Data)
	if HasValidHash(event) {
		t.Fatal("tagged event accepted a legacy fallback")
	}
	event.HashVersion = 99
	if HasValidHash(event) {
		t.Fatal("unknown algorithm accepted")
	}
}

func FuzzDecodeBackup(f *testing.F) {
	for _, seed := range []string{`[]`, `{"events":[],"schemas":{}}`, `null`, `{"events":[null]}`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 1024*1024 {
			return
		}
		_, _ = DecodeBackup([]byte(text))
	})
}
