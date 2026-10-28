package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// TestFault_CommitPoints fails a batch commit at every durability point,
// restarts and verifies: no acknowledged event is lost, the failed batch is
// fully present or fully absent, the indexes are consistent and further
// commits work.
func TestFault_CommitPoints(t *testing.T) {
	for _, format := range storageFormats {
		referenceLog := format.ReferenceDataLogFile()
		points := []string{
			"wal.write", "wal.sync",
			"append:" + referenceLog, "sync:" + referenceLog,
			"events.write", "events.sync",
			"shard.write", "shard.sync",
			"journal.append", "journal.sync",
			"publish", "wal.clear",
		}
		for _, point := range points {
			for _, hit := range []int{1, 3} {
				t.Run(fmt.Sprintf("%s/%s#%d", format, point, hit), func(t *testing.T) {
					ctx := context.Background()
					directory := newTestDirectory(t, format)
					faults := &switchableFaults{}
					s := openTestStore(t, directory, testStoreOptions{faults: faults.injector()})
					acknowledged := []*core.Event{commitTestEvent(t, s, "/f/0", "t"), commitTestEvent(t, s, "/f/1", "u")}
					batch := []*core.Event{
						newTestEvent("/f/2", "t", map[string]any{"a": 1}),
						newTestEvent("/f/2/x", "u", map[string]any{"a": 2}),
						newTestEvent("/g/3", "v", map[string]any{"a": 3}),
					}
					batch[1].Options.StoreDataAsReference = true
					var commitErr error
					faults.during(failAtNthCall(point, hit), func() { commitErr = s.Commit(ctx, batch) })
					if commitErr == nil {
						acknowledged = append(acknowledged, batch...)
					} else if !s.IsReady() {
						if err := s.Commit(ctx, []*core.Event{newTestEvent("/z", "t", map[string]any{})}); !errors.Is(err, core.ErrUnhealthy) {
							t.Errorf("unhealthy store accepted a write: %v", err)
						}
					} else if !strings.HasPrefix(point, "wal.") {
						t.Errorf("store still ready after a failure at %s", point)
					}
					s.Close()

					s = openTestStore(t, directory, testStoreOptions{})
					defer s.Close()
					raw := assertConsistentWithEventLog(t, s)
					isPresent := map[string]bool{}
					for _, event := range raw {
						isPresent[event.ID] = true
					}
					for _, event := range acknowledged {
						if !isPresent[event.ID] {
							t.Fatalf("acknowledged event %s lost", event.ID)
						}
					}
					presentInBatch := 0
					for _, event := range batch {
						if isPresent[event.ID] {
							presentInBatch++
						}
					}
					if presentInBatch != 0 && presentInBatch != len(batch) {
						t.Fatalf("partial batch visible: %d of %d", presentInBatch, len(batch))
					}
					if presentInBatch == len(batch) {
						found, err := s.FindEventByID(ctx, batch[1].ID)
						if err != nil || !strings.Contains(string(found.Data), `"a":2`) {
							t.Fatalf("reference data not durable with its event: %v", err)
						}
					}
					commitTestEvent(t, s, "/after", "t")
					assertConsistentWithEventLog(t, s)
				})
			}
		}
	}
}

// TestFault_RebuildPoints fails a rebuild and verifies that nothing partial
// is served and that a restart rebuilds a complete index set.
func TestFault_RebuildPoints(t *testing.T) {
	for _, point := range []string{"journal.reset", "build.afterLegacyDelete", "shard.write", "build.beforeComplete", "journal.append"} {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			directory := t.TempDir()
			faults := &switchableFaults{}
			s := openTestStore(t, directory, testStoreOptions{faults: faults.injector()})
			for position := 0; position < 15; position++ {
				commitTestEvent(t, s, fmt.Sprintf("/r/%d/x", position), "t")
			}
			var err error
			faults.during(failAtNthCall(point, 1), func() { _, err = s.RebuildIndexes(ctx) })
			if err == nil {
				t.Fatal("expected failure")
			}
			if _, readErr := s.FetchAll(ctx); !errors.Is(readErr, core.ErrNotReady) {
				t.Fatalf("partly rebuilt index served: %v", readErr)
			}
			s.Close()
			s = openTestStore(t, directory, testStoreOptions{})
			defer s.Close()
			if raw := assertConsistentWithEventLog(t, s); len(raw) != 15 {
				t.Fatal(len(raw))
			}
		})
	}
}

func hashedName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// writePreVersion116Indexes replaces the indexes of a data directory with
// the per-key layout of releases before 1.1.6.
func writePreVersion116Indexes(t *testing.T, directory string, events []*core.Event) {
	t.Helper()
	root := filepath.Join(directory, layout.IndexesDirectory)
	os.RemoveAll(root)
	write := func(relativePath, line string) {
		path := filepath.Join(root, relativePath)
		os.MkdirAll(filepath.Dir(path), 0o755)
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if info, _ := file.Stat(); info.Size() == 0 {
			file.WriteString("eventstore\n")
		}
		file.WriteString(line + "\n")
		file.Close()
	}
	for _, event := range events {
		write("subjects/index.eventstore", event.Subject)
		write("events-by-subject/"+hashedName(event.Subject)+".eventstore", event.ID)
		write("events-by-subject-recursive/"+hashedName(event.Subject)+".eventstore", event.ID)
		write("event-types/"+hashedName(event.Type)+".eventstore", event.ID)
		write("latest-event/index.eventstore", event.ID)
		write("latest-event-of-type/"+hashedName(event.Type)+"/"+hashedName(event.Subject)+"/index.eventstore", event.ID)
	}
}

func newPreVersion116Directory(t *testing.T, eventCount int) (string, []byte) {
	t.Helper()
	directory := newTestDirectory(t, layout.FormatLegacy)
	s := openTestStore(t, directory, testStoreOptions{})
	for position := 0; position < eventCount; position++ {
		commitTestEvent(t, s, fmt.Sprintf("/user/%d", position), fmt.Sprintf("type-%d", position%3))
	}
	events := readEventLogDirectly(t, s)
	s.Close()
	writePreVersion116Indexes(t, directory, events)
	return directory, mustReadFile(t, filepath.Join(directory, layout.EventsDirectory, "events.eventstore"))
}

func countFiles(t *testing.T, root string) (files, directories int) {
	t.Helper()
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			directories++
		} else {
			files++
		}
		return nil
	})
	return files, directories
}

func TestMigration_PreVersion116IndexesAreReplaced(t *testing.T) {
	directory, logBefore := newPreVersion116Directory(t, 40)
	root := filepath.Join(directory, layout.IndexesDirectory)
	rootBefore, _ := os.Stat(root)
	s := openTestStore(t, directory, testStoreOptions{})
	defer s.Close()
	rootAfter, _ := os.Stat(root)
	if !os.SameFile(rootBefore, rootAfter) {
		t.Error("index root was replaced")
	}
	files, directories := countFiles(t, root)
	if files > 5*256+2 || directories != 7 {
		t.Errorf("files %d directories %d", files, directories)
	}
	if string(mustReadFile(t, filepath.Join(directory, layout.EventsDirectory, "events.eventstore"))) != string(logBefore) {
		t.Error("migration changed the event log")
	}
	if raw := assertConsistentWithEventLog(t, s); len(raw) != 40 {
		t.Fatal(len(raw))
	}
}

func TestMigration_FailuresAreRetriedOnNextStart(t *testing.T) {
	for _, point := range []string{"journal.reset", "build.afterLegacyDelete", "shard.write", "build.beforeComplete", "journal.append"} {
		t.Run(point, func(t *testing.T) {
			directory, logBefore := newPreVersion116Directory(t, 12)
			if _, err := tryOpenTestStore(directory, testStoreOptions{faults: failAtNthCall(point, 1)}); err == nil {
				t.Fatal("expected migration failure")
			}
			s := openTestStore(t, directory, testStoreOptions{})
			defer s.Close()
			if raw := assertConsistentWithEventLog(t, s); len(raw) != 12 {
				t.Fatal(len(raw))
			}
			if string(mustReadFile(t, filepath.Join(directory, layout.EventsDirectory, "events.eventstore"))) != string(logBefore) {
				t.Error("event log changed")
			}
		})
	}
}

func TestMigration_RejectsUnknownFilesSymlinksAndFutureFormats(t *testing.T) {
	t.Run("unknown file", func(t *testing.T) {
		directory, _ := newPreVersion116Directory(t, 2)
		os.WriteFile(filepath.Join(directory, layout.IndexesDirectory, "event-types", "notes.txt"), []byte("x"), 0o644)
		if _, err := tryOpenTestStore(directory, testStoreOptions{}); err == nil {
			t.Fatal("expected error")
		}
		if _, err := os.Stat(filepath.Join(directory, layout.IndexesDirectory, "latest-event", "index.eventstore")); err != nil {
			t.Error("cleanup ran before validation")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		directory, _ := newPreVersion116Directory(t, 2)
		os.Symlink(os.TempDir(), filepath.Join(directory, layout.IndexesDirectory, "event-types", "link"))
		if _, err := tryOpenTestStore(directory, testStoreOptions{}); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("future index format", func(t *testing.T) {
		directory := t.TempDir()
		s := openTestStore(t, directory, testStoreOptions{})
		commitTestEvent(t, s, "/a", "t")
		s.Close()
		statePath := filepath.Join(directory, layout.IndexesDirectory, layout.FormatCurrent.IndexStateFile())
		os.WriteFile(statePath, []byte(`{"format":"genesisdb-sharded-index","version":9,"category":"index-state","shardCount":256}`+"\n"), 0o644)
		if _, err := tryOpenTestStore(directory, testStoreOptions{}); !errors.Is(err, core.ErrUnsupportedIndexFormat) {
			t.Fatalf("got %v", err)
		}
		if !strings.Contains(string(mustReadFile(t, statePath)), `"version":9`) {
			t.Error("future state was modified")
		}
	})
	t.Run("future storage format", func(t *testing.T) {
		directory := t.TempDir()
		os.WriteFile(filepath.Join(directory, layout.FormatMarkerFile), []byte(`{"format":"genesisdb","version":99}`), 0o644)
		if _, err := tryOpenTestStore(directory, testStoreOptions{}); !errors.Is(err, core.ErrUnsupportedStorageFormat) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("logs of both formats", func(t *testing.T) {
		directory := newTestDirectory(t, layout.FormatLegacy)
		os.WriteFile(filepath.Join(directory, layout.EventsDirectory, "events.gdb"), []byte("genesisdb\n"), 0o644)
		if _, err := tryOpenTestStore(directory, testStoreOptions{}); !errors.Is(err, core.ErrUnsupportedStorageFormat) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestRecovery_UnpublishedShardTailIsDiscarded(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	s := openTestStore(t, directory, testStoreOptions{})
	commitTestEvent(t, s, "/a/1", "t")
	s.Close()
	eventTypes := filepath.Join(directory, layout.IndexesDirectory, "event-types")
	entries, _ := os.ReadDir(eventTypes)
	fsutil.AppendAndSync(filepath.Join(eventTypes, entries[0].Name()), []byte(`{"k":"t","partial`), nil)
	s = openTestStore(t, directory, testStoreOptions{})
	defer s.Close()
	if events, err := s.FetchByEventType(ctx, "t"); err != nil || len(events) != 1 {
		t.Fatalf("%d %v", len(events), err)
	}
}

// writeLegacyDirectory returns a legacy directory with reference data and a
// system event, plus the expected events.
func writeLegacyDirectory(t *testing.T) (string, []*core.Event) {
	t.Helper()
	ctx := context.Background()
	directory := newTestDirectory(t, layout.FormatLegacy)
	s := openTestStore(t, directory, testStoreOptions{})
	for position := 0; position < 5; position++ {
		event := newTestEvent(fmt.Sprintf("/c/%d", position), "t", map[string]any{"position": position})
		event.Options.StoreDataAsReference = position%2 == 0
		if err := s.Commit(ctx, []*core.Event{event}); err != nil {
			t.Fatal(err)
		}
	}
	s.AppendSystemEvent("audit", "/c", nil)
	events, _ := s.FetchAll(ctx)
	s.Close()
	return directory, events
}

func assertConvertedDirectory(t *testing.T, directory string, expected []*core.Event, format layout.Format) {
	t.Helper()
	s := openTestStore(t, directory, testStoreOptions{})
	defer s.Close()
	if s.StorageFormat() != format {
		t.Fatalf("format %s, want %s", s.StorageFormat(), format)
	}
	fetched, err := s.FetchAll(context.Background())
	if err != nil || len(fetched) != len(expected) {
		t.Fatalf("%d events, %v", len(fetched), err)
	}
	for position := range expected {
		if fetched[position].ID != expected[position].ID || string(fetched[position].Data) != string(expected[position].Data) {
			t.Fatalf("event %d differs after conversion", position)
		}
	}
	if system, _ := s.ListSystemEvents(); len(system) != 1 {
		t.Fatal("system event lost")
	}
	assertConsistentWithEventLog(t, s)
	other := layout.FormatLegacy
	if format == layout.FormatLegacy {
		other = layout.FormatCurrent
	}
	filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(path, other.Extension()) && filepath.Base(path) != layout.FormatMarkerFile {
			t.Errorf("file of the old format left: %s", path)
		}
		return nil
	})
}

func TestConvert_MigratesLegacyDirectoryOnce(t *testing.T) {
	directory, events := writeLegacyDirectory(t)
	report, err := Convert(directory, layout.FormatCurrent, true, Options{Logger: discardLogger()})
	if err != nil || len(report.ConvertedFiles) != 3 {
		t.Fatalf("dry run: %+v %v", report, err)
	}
	if _, err := os.Stat(filepath.Join(directory, layout.EventsDirectory, "events.eventstore")); err != nil {
		t.Fatal("dry run changed the directory")
	}
	if _, err := Convert(directory, layout.FormatCurrent, false, Options{Logger: discardLogger()}); err != nil {
		t.Fatal(err)
	}
	assertConvertedDirectory(t, directory, events, layout.FormatCurrent)
	if report, err := Convert(directory, layout.FormatCurrent, false, Options{Logger: discardLogger()}); err != nil || report.From != report.To {
		t.Fatalf("second conversion must be a no-op: %+v %v", report, err)
	}
	// GenesisDB 2 data is never converted back to the 1.x format.
	if _, err := Convert(directory, layout.FormatLegacy, false, Options{Logger: discardLogger()}); !errors.Is(err, core.ErrUnsupportedStorageFormat) {
		t.Fatalf("downgrade must be refused: %v", err)
	}
	assertConvertedDirectory(t, directory, events, layout.FormatCurrent)
}

// TestConvert_ResumesAfterACrashAtEveryStep interrupts the conversion at
// every file operation and verifies that the next start finishes it with an
// identical result.
func TestConvert_ResumesAfterACrashAtEveryStep(t *testing.T) {
	points := []string{"create-marker", "copy:refs", "rename:refs", "remove:refs", "copy:system", "rename:events", "remove:system", "finalize-marker"}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			directory, events := writeLegacyDirectory(t)
			crashConversionAt(t, directory, point)
			s := openTestStore(t, directory, testStoreOptions{})
			s.Close()
			assertConvertedDirectory(t, directory, events, layout.FormatCurrent)
		})
	}
}

// crashConversionAt replays the conversion steps by hand up to point, which
// leaves the directory exactly as a crash at that point would.
func crashConversionAt(t *testing.T, directory, point string) {
	t.Helper()
	eventsDirectory := filepath.Join(directory, layout.EventsDirectory)
	marker := layout.NewFormatMarker()
	marker.Conversion = layout.ConversionInProgress
	marker.Target = layout.FormatCurrent.String()
	if err := writeFormatMarker(directory, marker); err != nil {
		t.Fatal(err)
	}
	if point == "create-marker" {
		return
	}
	for _, base := range []string{"refs", "system", "events"} {
		source := filepath.Join(eventsDirectory, base+".eventstore")
		target := filepath.Join(eventsDirectory, base+".gdb")
		if err := copyLogWithHeader(source, target+layout.TemporaryFileSuffix, layout.FormatCurrent); err != nil {
			t.Fatal(err)
		}
		if point == "copy:"+base {
			return
		}
		if err := os.Rename(target+layout.TemporaryFileSuffix, target); err != nil {
			t.Fatal(err)
		}
		if point == "rename:"+base {
			return
		}
		os.Remove(source)
		if point == "remove:"+base {
			return
		}
	}
}

func TestFormat_NewDirectoriesUseTheCurrentFormat(t *testing.T) {
	directory := t.TempDir()
	s := openTestStore(t, directory, testStoreOptions{})
	commitTestEvent(t, s, "/n/1", "t")
	s.Close()
	marker, err := layout.ReadFormatMarker(directory)
	if err != nil || marker.Version != layout.CurrentFormatVersion || marker.IsConverting() {
		t.Fatalf("marker %+v %v", marker, err)
	}
	filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err == nil && strings.Contains(path, ".eventstore") {
			t.Errorf("legacy file in a new directory: %s", path)
		}
		if err == nil && !info.IsDir() && strings.HasSuffix(path, ".gdb") && filepath.Dir(path) == filepath.Join(directory, layout.EventsDirectory) {
			if !strings.HasPrefix(string(mustReadFile(t, path)), "genesisdb\n") {
				t.Errorf("%s has no genesisdb header", path)
			}
		}
		return nil
	})
}

// Opening a legacy directory with default options migrates it to the current
// format. That is the production path for every 1.x data directory. Events,
// reference payloads, system events and the hash chain survive, and legacy
// hash versions stay verifiable.
func TestOpen_DefaultOptionsMigrateLegacyDirectory(t *testing.T) {
	directory, events := writeLegacyDirectory(t)
	s, err := Open(directory, Options{Logger: discardLogger(), EvaluateGDBQLPrecondition: countPrecondition})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.StorageFormat() != layout.FormatCurrent {
		t.Fatalf("format %s", s.StorageFormat())
	}
	if _, err := os.Stat(filepath.Join(directory, layout.EventsDirectory, "events.eventstore")); !os.IsNotExist(err) {
		t.Fatal("legacy log left behind")
	}
	fetched, err := s.FetchAll(context.Background())
	if err != nil || len(fetched) != len(events) {
		t.Fatalf("%d events after migration, %v", len(fetched), err)
	}
	for position := range events {
		if fetched[position].ID != events[position].ID || string(fetched[position].Data) != string(events[position].Data) {
			t.Fatalf("event %d changed by migration", position)
		}
	}
	system, err := s.ListSystemEvents()
	if err != nil || len(system) != 1 {
		t.Fatalf("system events after migration: %d %v", len(system), err)
	}
	report, err := s.VerifyHashChain(context.Background())
	if err != nil || !report.IsValid() || report.LegacyEvents != len(events) {
		t.Fatalf("hash chain after migration: %+v %v", report, err)
	}
	// New events in the migrated directory get the stronger hash.
	commitTestEvent(t, s, "/after", "t")
	report, _ = s.VerifyHashChain(context.Background())
	if !report.IsValid() || report.CheckedEvents != len(events)+1 || report.LegacyEvents != len(events) {
		t.Fatalf("chain after a commit into the migrated directory: %+v", report)
	}
}

// A legacy directory whose 1.x process left a pending WAL is migrated with
// the WAL replayed first, so the last 1.x commit is not lost.
func TestOpen_DefaultOptionsReplayLegacyWriteAheadLogBeforeMigrating(t *testing.T) {
	directory, events := writeLegacyDirectory(t)
	last := events[len(events)-1]
	pending := newTestEvent("/c/pending", "t", map[string]any{"position": 99})
	pending.SpecVersion, pending.DataContentType, pending.ID = core.EventSpecVersion, core.EventDataContentType, core.NewUUID()
	pending.Time = last.Time.Add(time.Second)
	pending.PredecessorHash = last.Hash
	pending.Hash = core.HashEvent(pending)
	line, _ := json.Marshal(map[string]any{"event": pending})
	walPath := filepath.Join(directory, layout.WALDirectory, layout.WALFile)
	if err := os.WriteFile(walPath, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(directory, Options{Logger: discardLogger(), EvaluateGDBQLPrecondition: countPrecondition})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	found, err := s.FindEventByID(context.Background(), pending.ID)
	if err != nil || string(found.Data) != string(pending.Data) {
		t.Fatalf("1.x WAL entry lost in migration: %v", err)
	}
	if report, err := s.VerifyHashChain(context.Background()); err != nil || !report.IsValid() || report.CheckedEvents != len(events)+1 {
		t.Fatalf("%+v %v", report, err)
	}
}

// A directory from a release before 1.1.6 (per-key index layout) is migrated
// with default options in one start: logs converted, old indexes replaced.
// An index failure during that start is retried on the next start.
func TestOpen_DefaultOptionsMigratePreVersion116Directory(t *testing.T) {
	directory, _ := newPreVersion116Directory(t, 12)
	if _, err := tryOpenTestStore(directory, testStoreOptions{convertLegacy: true, faults: failAtNthCall("shard.write", 1)}); err == nil {
		t.Fatal("expected migration failure")
	}
	s := openTestStore(t, directory, testStoreOptions{convertLegacy: true})
	defer s.Close()
	if s.StorageFormat() != layout.FormatCurrent {
		t.Fatalf("format %s", s.StorageFormat())
	}
	if raw := assertConsistentWithEventLog(t, s); len(raw) != 12 {
		t.Fatal(len(raw))
	}
	if _, err := os.Stat(filepath.Join(directory, layout.EventsDirectory, "events.eventstore")); !os.IsNotExist(err) {
		t.Fatal("legacy log left behind")
	}
}
