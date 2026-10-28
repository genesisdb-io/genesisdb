package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

func TestReferenceFlagsPreserveOriginalPayload(t *testing.T) {
	engine, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	event := newTestEvent("/r/1")
	event.Options.StoreDataAsReference = true
	event.ReferenceDataKey = core.ReferenceDataPending
	if _, err := engine.Append(context.Background(), []*core.Event{event}); err != nil {
		t.Fatal(err)
	}
	data, err := engine.LoadReferenceData(event.ReferenceDataKey)
	if err != nil || string(data) != `{"a":1}` {
		t.Fatalf("payload lost: %s %v", data, err)
	}
}

func TestWALRejectsEveryTruncatedBatchPrefix(t *testing.T) {
	directory := t.TempDir()
	engine, err := Open(directory, Options{})
	if err != nil {
		t.Fatal(err)
	}
	appended, err := engine.Append(context.Background(), []*core.Event{newTestEvent("/a/1"), newTestEvent("/a/2")})
	if err != nil {
		t.Fatal(err)
	}
	wal, err := OpenWriteAheadLog(directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries := []WALEntry{{Event: appended.Events[0].Event}, {Event: appended.Events[1].Event}}
	if err := wal.AppendEntries(entries); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, layout.WALDirectory, layout.WALFile)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for cut := 0; cut < len(content); cut++ {
		if err := os.WriteFile(path, content[:cut], 0o600); err != nil {
			t.Fatal(err)
		}
		pending, err := wal.ReadPendingEntries()
		if len(pending) > 0 || (cut > 0 && err == nil) {
			t.Fatalf("damaged published WAL did not fail closed at %d: %v", cut, err)
		}
	}
	// A complete but modified batch must not be replayed.
	corrupt := bytes.Replace(content, []byte("/a/1"), []byte("/b/1"), 1)
	os.WriteFile(path, corrupt, 0o600)
	if _, err := wal.ReadPendingEntries(); err == nil {
		t.Fatal("corrupt checksum accepted")
	}
	os.WriteFile(path, content, 0o600)
	pending, err := wal.ReadPendingEntries()
	if err != nil || len(pending) != 2 {
		t.Fatalf("complete batch rejected: %d %v", len(pending), err)
	}
}

func TestBackupRejectsUnsafeSchemaPaths(t *testing.T) {
	for _, name := range []string{"../outside.json", "a/b.json", `a\b.json`, "not-json"} {
		err := ValidateBackup(&core.Backup{Schemas: map[string]json.RawMessage{name: json.RawMessage(`{}`)}})
		if err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}
