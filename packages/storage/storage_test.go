package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

func newTestEvent(subject string) *core.Event {
	return &core.Event{Source: "test", Subject: subject, Type: "t", Data: json.RawMessage(`{"a":1}`)}
}

// The storage engine works on its own, without indexes or a store.
func TestEngine_StandaloneAppendScanAndRead(t *testing.T) {
	for _, format := range []layout.Format{layout.FormatCurrent, layout.FormatLegacy} {
		t.Run(format.String(), func(t *testing.T) {
			ctx := context.Background()
			directory := t.TempDir()
			engine, err := Open(directory, Options{Format: format})
			if err != nil {
				t.Fatal(err)
			}
			appended, err := engine.Append(ctx, []*core.Event{newTestEvent("/a/1"), newTestEvent("/a/2")})
			if err != nil {
				t.Fatal(err)
			}
			if appended.Events[1].Event.PredecessorHash != appended.Events[0].Event.Hash {
				t.Fatal("hash chain not linked")
			}
			if err := engine.SyncDurable(); err != nil {
				t.Fatalf("SyncDurable: %v", err)
			}
			var scanned []string
			if _, _, err := engine.ScanEventLog(engine.MainEventLogFile(), 0, func(stored core.StoredEvent) error {
				scanned = append(scanned, stored.Event.Subject)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if strings.Join(scanned, ",") != "/a/1,/a/2" {
				t.Fatalf("scanned %v", scanned)
			}
			read, err := engine.ReadEventAt(appended.Events[1].Location)
			if err != nil || read.ID != appended.Events[1].Event.ID {
				t.Fatalf("read by location: %v", err)
			}
			if report, err := engine.VerifyHashChain(); err != nil || !report.IsValid() || report.CheckedEvents != 2 {
				t.Fatalf("%+v %v", report, err)
			}
			content, _ := os.ReadFile(filepath.Join(directory, layout.EventsDirectory, format.MainEventLogFile()))
			if !strings.HasPrefix(string(content), format.LogHeader()+"\n") {
				t.Fatal("wrong log header")
			}

			reopened, err := Open(directory, Options{Format: format})
			if err != nil || reopened.LastEventHash() != appended.Events[1].Event.Hash {
				t.Fatalf("last hash after reopen: %v", err)
			}
		})
	}
}

func TestEngine_ReferenceDataAndSystemEvents(t *testing.T) {
	ctx := context.Background()
	engine, _ := Open(t.TempDir(), Options{})
	event := newTestEvent("/r/1")
	event.Options.StoreDataAsReference = true
	appended, err := engine.Append(ctx, []*core.Event{event})
	if err != nil {
		t.Fatal(err)
	}
	if string(event.Data) != "{}" || event.ReferenceDataKey == "" || string(appended.ReferenceData[0]) != `{"a":1}` {
		t.Fatal("payload not moved to reference data")
	}
	payload, err := engine.LoadReferenceData(event.ReferenceDataKey)
	if err != nil || string(payload) != `{"a":1}` {
		t.Fatalf("%s %v", payload, err)
	}
	replaced, err := engine.ReplaceReferenceData(map[string]json.RawMessage{event.ReferenceDataKey: json.RawMessage(`{"a":""}`), "missing": nil}, nil, nil)
	if err != nil || len(replaced) != 1 {
		t.Fatalf("%v %v", replaced, err)
	}
	if _, err := engine.LoadReferenceData("missing"); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := engine.AppendSystemEvent("audit", "/r/1", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	second, _ := engine.AppendSystemEvent("audit", "/r/1", json.RawMessage(`{}`))
	system, _ := engine.ListSystemEvents()
	if len(system) != 2 || second.PredecessorHash != system[0].Hash {
		t.Fatal("system event chain")
	}
	if isEmpty, _ := engine.IsEmpty(); isEmpty {
		t.Fatal("engine with data reported empty")
	}
}

func TestWriteAheadLog_TornPublishedBatchIsRejected(t *testing.T) {
	directory := t.TempDir()
	wal, _ := OpenWriteAheadLog(directory, nil)
	wal.AppendEntries([]WALEntry{{Event: newTestEvent("/w/1")}})
	path := filepath.Join(directory, layout.WALDirectory, layout.WALFile)
	content, _ := os.ReadFile(path)
	os.WriteFile(path, append(content, []byte(`{"event":{"sub`)...), 0o644)
	entries, err := wal.ReadPendingEntries()
	if err == nil || entries != nil {
		t.Fatalf("damaged published WAL must fail closed: %v %v", entries, err)
	}
	os.WriteFile(path, []byte("{broken}\n"), 0o644)
	if _, err := wal.ReadPendingEntries(); err == nil {
		t.Fatal("corrupt WAL entry accepted")
	}
}

// A WAL write that never completed (no trailing newline) belongs to a commit
// that was never acknowledged. Open discards it and serves the directory. A
// complete but corrupt WAL still refuses to open.
func TestWriteAheadLog_TornWriteIsDiscardedOnOpen(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, Options{Format: layout.FormatCurrent})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Append(context.Background(), []*core.Event{newTestEvent("/w/1")}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, layout.WALDirectory, layout.WALFile)
	os.WriteFile(path, []byte(`{"version":2,"entries":[{"event":{"sub`), 0o644)
	engine, err = Open(directory, Options{Format: layout.FormatCurrent})
	if err != nil {
		t.Fatalf("torn WAL blocked the start: %v", err)
	}
	if engine.wal.HasPendingEntries() {
		t.Fatal("torn WAL not discarded")
	}
	count := 0
	engine.ScanEventLog(engine.MainEventLogFile(), 0, func(core.StoredEvent) error { count++; return nil })
	if count != 1 {
		t.Fatalf("%d events after discarding a torn WAL", count)
	}
	os.WriteFile(path, []byte("{\"version\":2,\"entries\":[],\"checksum\":\"bad\"}\n"), 0o644)
	if _, err := Open(directory, Options{Format: layout.FormatCurrent}); err == nil {
		t.Fatal("complete corrupt WAL did not fail closed")
	}
}

func TestValidateBackup(t *testing.T) {
	system := newTestEvent("/s")
	system.Source = core.SystemEventSource
	orphan := newTestEvent("/o")
	orphan.ReferenceDataKey = "missing"
	orphan.Data = json.RawMessage(`{}`)
	for _, backup := range []*core.Backup{
		{Events: []*core.Event{nil}},
		{Events: []*core.Event{system}},
		{Events: []*core.Event{orphan}},
		{ReferenceData: []*core.ReferenceDataEntry{nil}},
		{SystemEvents: []*core.Event{nil}},
	} {
		if err := ValidateBackup(backup); !errors.Is(err, core.ErrInvalidBackup) {
			t.Errorf("accepted %+v: %v", backup, err)
		}
	}
	if err := ValidateBackup(&core.Backup{Events: []*core.Event{newTestEvent("/ok")}}); err != nil {
		t.Fatal(err)
	}
}

// TestRecordReader_ReadAheadWindow reads records in log order, in reverse,
// one larger than the window, the last record of the log and a location
// with a wrong length. Every read must return exactly the record of its
// location, whether it comes from the window or from a fresh read.
func TestRecordReader_ReadAheadWindow(t *testing.T) {
	// A window of 16 KiB makes every tenth record larger than the window.
	previous := readAheadBytes
	readAheadBytes = 16 << 10
	defer func() { readAheadBytes = previous }()
	ctx := context.Background()
	engine, err := Open(t.TempDir(), Options{Format: layout.FormatCurrent})
	if err != nil {
		t.Fatal(err)
	}
	var events []*core.Event
	for position := 0; position < 400; position++ {
		event := newTestEvent("/w/" + strings.Repeat("x", position%7))
		size := 4000
		if position%10 == 9 {
			size = 20000
		}
		event.Data = json.RawMessage(`{"pad":"` + strings.Repeat("p", size) + `"}`)
		events = append(events, event)
	}
	var locations []core.RecordLocation
	var ids []string
	for start := 0; start < len(events); start += 100 {
		appended, err := engine.Append(ctx, events[start:start+100])
		if err != nil {
			t.Fatal(err)
		}
		for _, stored := range appended.Events {
			locations = append(locations, stored.Location)
			ids = append(ids, stored.Event.ID)
		}
	}
	reader := engine.NewRecordReader()
	defer reader.Close()
	check := func(position int) {
		t.Helper()
		event, err := reader.Read(locations[position])
		if err != nil || event.ID != ids[position] {
			t.Fatalf("record %d: %v", position, err)
		}
	}
	for position := range locations {
		check(position)
	}
	for position := len(locations) - 1; position >= 0; position-- {
		check(position)
	}
	check(len(locations) - 1)
	broken := locations[10]
	broken.Length--
	if _, err := reader.Read(broken); !errors.Is(err, ErrNoRecordAtLocation) {
		t.Fatalf("wrong length accepted: %v", err)
	}
	beyond := locations[len(locations)-1]
	beyond.Offset += beyond.Length + 10
	if _, err := reader.Read(beyond); !errors.Is(err, ErrNoRecordAtLocation) {
		t.Fatalf("location beyond the log accepted: %v", err)
	}
	check(0)
}
