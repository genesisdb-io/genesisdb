package core

import (
	"encoding/json"
	"reflect"
	"regexp"
	"testing"
	"time"
)

// The JSON encoding of persisted records is part of the data format. These
// golden values come from records written by GenesisDB 1.1.6.
func TestEvent_EncodesLikeVersion116(t *testing.T) {
	line := `{"source":"io.genesisdb.app","subject":"/customer/3","type":"io.genesisdb.app.customer-added","specversion":"1.0","id":"98153155-96ae-49ed-bc71-2a9467b3a4e5","time":"2026-10-23T19:02:23.316487+02:00","datacontenttype":"application/json","predecessorhash":"70f679fc02b0199de93bb82c6b5750b0af0f5d5d046a802f16bfe50d3cdac7a5","data":{},"hash":"b25bbca0f8320f7df526d787bd0be4852edb6c3c46f1ee923d5849084823979c","ref":"7926cb47b8585281ed1b9afc937ff8f4c94e4c4440e34f3cf3dd55bf5bfaa377","options":{"storeDataAsReference":true}}`
	var event Event
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(event)
	if string(encoded) != line {
		t.Fatalf("event encoding changed\n got: %s\nwant: %s", encoded, line)
	}
	plain := `{"source":"s","subject":"/a","type":"t","specversion":"1.0","id":"i","time":"2026-01-01T00:00:00Z","datacontenttype":"application/json","predecessorhash":"p","data":{"a":1},"hash":"h","options":{}}`
	var plainEvent Event
	json.Unmarshal([]byte(plain), &plainEvent)
	if encoded, _ := json.Marshal(plainEvent); string(encoded) != plain {
		t.Fatalf("plain event encoding changed: %s", encoded)
	}
	entry := `{"eventHash":"k","refData":{"a":1},"timestamp":"2026-01-01T00:00:00Z"}`
	var reference ReferenceDataEntry
	json.Unmarshal([]byte(entry), &reference)
	if encoded, _ := json.Marshal(reference); string(encoded) != entry {
		t.Fatalf("reference data encoding changed: %s", encoded)
	}
}

// The golden hash was computed by GenesisDB 1.1.6 for the same event.
func TestHashEvent_MatchesVersion116(t *testing.T) {
	event := &Event{Source: "s", Subject: "/a", Type: "t", SpecVersion: "1.0", ID: "id", Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), DataContentType: "application/json", Data: json.RawMessage(`{"a":1}`)}
	if got := HashEvent(event); got != "b16147507104f3f052078341208bfff627f856e9681b0983bbf3ca7efdbbe9c0" {
		t.Fatalf("hash %s differs from 1.1.6", got)
	}
	event.Hash = HashEventV1(event)
	if !HasValidHash(event) {
		t.Error("first hash algorithm not accepted")
	}
	event.Hash = HashData(event.Data)
	if !HasValidHash(event) {
		t.Error("oldest hash algorithm not accepted")
	}
	event.Hash = "wrong"
	if HasValidHash(event) {
		t.Error("wrong hash accepted")
	}
}

func TestSubjectHierarchy(t *testing.T) {
	cases := map[string][]string{
		"/org/acme/user/123": {"/", "/org", "/org/acme", "/org/acme/user", "/org/acme/user/123"},
		"/a":                 {"/", "/a"},
		"/":                  {"/"},
		"/a/":                {"/", "/a", "/a/"},
		"/a//b":              {"/", "/a", "/a/", "/a//b"},
		"x/y":                {"x", "x/y"},
	}
	for subject, want := range cases {
		if got := SubjectAndAncestors(subject); !reflect.DeepEqual(got, want) {
			t.Errorf("SubjectAndAncestors(%q) = %v want %v", subject, got, want)
		}
	}
	if !IsDirectChild("/user/1", "/user") || IsDirectChild("/user/1/x", "/user") || IsDirectChild("/user", "/user") {
		t.Error("IsDirectChild")
	}
	if !IsDirectChild("/a", "/") || IsDirectChild("/a/b", "/") {
		t.Error("IsDirectChild of root")
	}
	if !IsStrictDescendant("/user/1/x", "/user") || IsStrictDescendant("/user", "/user") || IsStrictDescendant("/users", "/user") {
		t.Error("IsStrictDescendant")
	}
	if TopLevelSubject("/user/123") != "/user" {
		t.Error("TopLevelSubject")
	}
}

func TestBackup_EncodesBothForms(t *testing.T) {
	plain := &Backup{}
	var buffer jsonBuffer
	plain.EncodeJSON(&buffer)
	if buffer.String() != "[]\n" {
		t.Fatalf("empty plain backup %q", buffer.String())
	}
	decoded, err := DecodeBackup([]byte(`{"events":[],"refs":[{"eventHash":"k","refData":{},"timestamp":"2026-01-01T00:00:00Z"}]}`))
	if err != nil || !decoded.Extended || len(decoded.ReferenceData) != 1 {
		t.Fatalf("%+v %v", decoded, err)
	}
	decoded, err = DecodeBackup([]byte(` [{"subject":"/a"}]`))
	if err != nil || decoded.Extended || len(decoded.Events) != 1 {
		t.Fatalf("%+v %v", decoded, err)
	}
}

type jsonBuffer struct{ data []byte }

func (buffer *jsonBuffer) Write(p []byte) (int, error) {
	buffer.data = append(buffer.data, p...)
	return len(p), nil
}

func (buffer *jsonBuffer) String() string { return string(buffer.data) }

func TestNewUUID_IsVersion4(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for attempt := 0; attempt < 1000; attempt++ {
		id := NewUUID()
		if !pattern.MatchString(id) || seen[id] {
			t.Fatalf("invalid or duplicate UUID %s", id)
		}
		seen[id] = true
	}
}
