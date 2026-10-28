package index

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

var awkwardValues = []string{"/a", "/a\tb", "/a\nb", "/a\rb", "/a\\b", "/a\\tb", "\\", "\t", "/ü/ä", `/"q"`, "/a b", strings.Repeat("/x", 500)}

func samplePosting(key, subject, eventType string) Posting {
	return Posting{Key: key, Subject: subject, EventType: eventType, EventID: "0e1f6a3c-9d4b-4c7e-8a51-3b2d1f0c9e8a", LogFile: "events.gdb", Offset: 1234567, Length: 321, EventHash: strings.Repeat("ab", 32)}
}

// Every record round trips exactly, for keys equal to and different from
// subject, type and ID, with every character the escaping handles.
func TestCompactCodec_RoundTrip(t *testing.T) {
	codec := compactCodec{}
	for _, value := range awkwardValues {
		for _, posting := range []Posting{
			samplePosting(value, value, "t"),
			samplePosting(value, "/s", value),
			samplePosting("", value, "t"),
			samplePosting(value, "/s", "t"),
		} {
			line := codec.encode(&posting)
			if line[len(line)-1] != '\n' || strings.Count(string(line), "\n") != 1 {
				t.Fatalf("record is not one line: %q", line)
			}
			var decoded Posting
			if err := codec.decode(line[:len(line)-1], &decoded); err != nil {
				t.Fatalf("%q: %v", line, err)
			}
			if !reflect.DeepEqual(decoded, withHashPrefix(posting)) {
				t.Fatalf("round trip\n got %+v\nwant %+v", decoded, posting)
			}
		}
	}
}

func withHashPrefix(posting Posting) Posting {
	if len(posting.EventHash) > hashPrefixLength {
		posting.EventHash = posting.EventHash[:hashPrefixLength]
	}
	return posting
}

// A posting with a hash prefix matches its event and rejects another event
// whose hash differs, an empty hash only matches an event without hash.
func TestPosting_MatchesHashPrefix(t *testing.T) {
	full := strings.Repeat("ab", 32)
	event := &core.Event{ID: "i", Subject: "/s", Type: "t", Hash: full}
	posting := Posting{EventID: "i", Subject: "/s", EventType: "t", EventHash: full[:hashPrefixLength]}
	if !posting.Matches(event) {
		t.Fatal("prefix does not match its event")
	}
	other := *event
	other.Hash = "ff" + full[2:]
	if posting.Matches(&other) {
		t.Fatal("prefix matches another hash")
	}
	posting.EventHash = ""
	if posting.Matches(event) {
		t.Fatal("empty hash matches a hashed event")
	}
	event.Hash = ""
	if !posting.Matches(event) {
		t.Fatal("empty hash does not match an event without hash")
	}
}

// A key filter accepts exactly the records of its key, so a prefix of one
// key or an escaped look-alike never matches another key.
func TestCompactCodec_FiltersAreExact(t *testing.T) {
	codec := compactCodec{}
	keys := append([]string{"/a/b", "/a/bc", "/a"}, awkwardValues...)
	for _, key := range keys {
		posting := samplePosting(key, "/s", "t")
		line := codec.encode(&posting)
		line = line[:len(line)-1]
		if !codec.keysFilter(key)(line) {
			t.Errorf("filter rejects its own key %q", key)
		}
		for _, other := range keys {
			if other != key && codec.keysFilter(other)(line) {
				t.Errorf("filter for %q accepts key %q", other, key)
			}
		}
	}
	for _, subject := range awkwardValues {
		posting := samplePosting("", subject, "type\tx")
		line := codec.encode(&posting)
		line = line[:len(line)-1]
		if !codec.pairFilter(subject, "type\tx")(line) || codec.pairFilter(subject, "type")(line) || codec.pairFilter(subject+"x", "type\tx")(line) {
			t.Errorf("pair filter for %q", subject)
		}
	}
}

func TestCompactCodec_RejectsMalformedRecords(t *testing.T) {
	codec := compactCodec{}
	for _, line := range []string{
		"",
		"k\ts\tt\tid\tlog\t1\t2",
		"k\ts\tt\tid\tlog\t1\t2\th\textra",
		"k\ts\tt\tid\tlog\t-1\t2\th",
		"k\ts\tt\tid\tlog\t1\tx\th",
		"k\ts\tt\tid\tlog\t\t2\th",
		"k\\x\ts\tt\tid\tlog\t1\t2\th",
		"k\\\ts\tt\tid\tlog\t1\t2\th",
		"\t\tt\tid\tlog\t1\t2\th",
		"k\ts\tt\tid\tlog\t99999999999999999999\t2\th",
	} {
		var posting Posting
		if err := codec.decode([]byte(line), &posting); !errors.Is(err, errInvalidRecord) {
			t.Errorf("%q accepted: %v", line, err)
		}
	}
}

// The compact records are much smaller than the JSON records of 1.1.6.
func TestCompactCodec_IsSmallerThanJSON(t *testing.T) {
	posting := samplePosting("/customer/42", "/customer/42", "io.genesisdb.shop.customer-registered")
	posting.Timestamp = "2026-10-26T08:00:00.123456789Z"
	posting.CommitGroup = "4f6c2a1e-8d3b-4b9a-9e7f-2c1d0a3b5e6f"
	compact, json := len(compactCodec{}.encode(&posting)), len(jsonCodec{}.encode(&posting))
	if compact*2 > json {
		t.Fatalf("compact record %d bytes, JSON %d bytes", compact, json)
	}
}

func TestCodecForFormat(t *testing.T) {
	if codecForFormat(layout.FormatLegacy).version() != jsonRecordVersion {
		t.Error("the legacy storage format must keep the 1.1.6 records, 1.x reads them")
	}
	if codecForFormat(layout.FormatCurrent).version() != compactRecordVersion {
		t.Error("the current storage format writes compact records")
	}
}

func FuzzCompactCodec(f *testing.F) {
	for _, value := range awkwardValues {
		f.Add(value, "/s", "t", int64(10), int64(20))
	}
	f.Fuzz(func(t *testing.T, key, subject, eventType string, offset, length int64) {
		if subject == "" || eventType == "" || offset < 0 || length < 0 {
			return
		}
		codec := compactCodec{}
		posting := samplePosting(key, subject, eventType)
		posting.Offset, posting.Length = offset, length
		line := codec.encode(&posting)
		if strings.Count(string(line), "\n") != 1 {
			t.Fatalf("record is not one line: %q", line)
		}
		var decoded Posting
		if err := codec.decode(line[:len(line)-1], &decoded); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if !reflect.DeepEqual(decoded, withHashPrefix(posting)) {
			t.Fatalf("round trip\n got %+v\nwant %+v", decoded, posting)
		}
		if !codec.keysFilter(key)(line[:len(line)-1]) {
			t.Fatal("filter rejects its own key")
		}
		var junk Posting
		_ = codec.decode([]byte(key), &junk)
	})
}

// An index written by GenesisDB 2.0.0 (current storage format, records of
// version 2) is rebuilt with records of version 3 on the next open, and
// every lookup answers as before.
func TestIndex_Version2RecordsAreRebuiltAsVersion3(t *testing.T) {
	directory := t.TempDir()
	log := newTestLog(layout.FormatCurrent, 40)
	old := New(directory, log, Options{Format: layout.FormatCurrent})
	old.codec = jsonCodec{}
	if err := old.Open(); err != nil {
		t.Fatal(err)
	}
	before, _ := old.AcquireSnapshot()
	selectors := []core.EventSelector{{All: true}, {EventTypes: []string{"x"}}, {SubjectsWithDescendants: []string{"/a"}}, {ExactSubjects: []string{"/b/2"}}}
	counts := make([]int, len(selectors))
	for position, selector := range selectors {
		set := NewPostingSet()
		if err := old.Select(before, selector, set); err != nil || set.Len() == 0 {
			t.Fatalf("version 2 index %+v: %d %v", selector, set.Len(), err)
		}
		counts[position] = set.Len()
	}
	header, _ := os.ReadFile(filepath.Join(directory, layout.IndexesDirectory, categoryEventTypes, layout.FormatCurrent.ShardFileName(shardNumberForKey("x"))))
	if !strings.Contains(string(header), `"version":2`) {
		t.Fatalf("setup did not write version 2: %.120s", header)
	}

	upgraded, err := openIndex(t, directory, log)
	if err != nil {
		t.Fatal(err)
	}
	header, _ = os.ReadFile(filepath.Join(directory, layout.IndexesDirectory, categoryEventTypes, layout.FormatCurrent.ShardFileName(shardNumberForKey("x"))))
	if !strings.Contains(string(header), `"version":3`) || strings.Contains(string(header), `{"k":`) {
		t.Fatalf("not rebuilt as version 3: %.200s", header)
	}
	assertOnlyActiveLayout(t, upgraded)
	snapshot, _ := upgraded.AcquireSnapshot()
	for position, selector := range selectors {
		set := NewPostingSet()
		if err := upgraded.Select(snapshot, selector, set); err != nil {
			t.Fatal(err)
		}
		if set.Len() != counts[position] {
			t.Errorf("%+v: %d before, %d after", selector, counts[position], set.Len())
		}
	}
	if latest, err := upgraded.FindLatestPostingOfSubjectAndType(snapshot, "/a/1", "x"); err != nil || latest == nil {
		t.Fatalf("pair lookup after upgrade: %v %v", latest, err)
	}
}
