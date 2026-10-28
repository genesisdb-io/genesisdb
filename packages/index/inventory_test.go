package index

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// The inventory decides what Rebuild deletes. These tests pin down the two
// rules: recognized derived data of earlier layouts and of the other storage
// format is removed, anything else stops the startup and nothing is touched.

func hashedName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func newTestLog(format layout.Format, count int) *memoryEventLog {
	log := &memoryEventLog{format: format}
	for position := 0; position < count; position++ {
		log.events = append(log.events, newMemoryEvent([]string{"/a/1", "/b/2"}[position%2], []string{"x", "y"}[position%2], position))
	}
	return log
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writePreVersion116Layout writes the per-key files and the nested type
// directories of the index layout before 1.1.6.
func writePreVersion116Layout(t *testing.T, root string, log *memoryEventLog) {
	t.Helper()
	legacy := "eventstore\n"
	for _, event := range log.events {
		writeFile(t, filepath.Join(root, categorySubjectCatalog, layout.LegacyIndexFile), legacy+event.Subject+"\n")
		writeFile(t, filepath.Join(root, categoryEventsBySubject, hashedName(event.Subject)+".eventstore"), legacy+event.ID+"\n")
		writeFile(t, filepath.Join(root, categoryEventsBySubjectWithAncestors, hashedName(event.Subject)+".eventstore"), legacy+event.ID+"\n")
		writeFile(t, filepath.Join(root, categoryEventTypes, hashedName(event.Type)+".eventstore"), legacy+event.ID+"\n")
		writeFile(t, filepath.Join(root, categoryRetiredLatestEvent, layout.LegacyIndexFile), legacy+event.ID+"\n")
		writeFile(t, filepath.Join(root, categoryLatestEventOfSubjectAndType, hashedName(event.Type), hashedName(event.Subject), layout.LegacyIndexFile), legacy+event.ID+"\n")
	}
}

func openIndex(t *testing.T, directory string, log *memoryEventLog) (*Index, error) {
	t.Helper()
	index := New(directory, log, Options{Format: log.format})
	return index, index.Open()
}

func assertOnlyActiveLayout(t *testing.T, index *Index) {
	t.Helper()
	inventory, err := index.inspectLayout()
	if err != nil {
		t.Fatal(err)
	}
	if inventory.hasDisposableData() {
		t.Fatalf("disposable data remains: %d files, %d directories", inventory.disposableFiles, inventory.disposableDirectories)
	}
	if _, err := os.Stat(filepath.Join(index.root, categoryRetiredLatestEvent)); !errors.Is(err, os.ErrNotExist) {
		t.Error("retired latest-event directory remains")
	}
	snapshot, err := index.AcquireSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	set := NewPostingSet()
	if err := index.Select(snapshot, core.EventSelector{All: true}, set); err != nil {
		t.Fatal(err)
	}
	if int64(len(set.InLogOrder())) != index.EventCount() {
		t.Fatalf("selected %d postings for %d events", len(set.InLogOrder()), index.EventCount())
	}
}

func TestInventory_PreVersion116LayoutIsRecognizedAndReplaced(t *testing.T) {
	directory := t.TempDir()
	log := newTestLog(layout.FormatLegacy, 6)
	root := filepath.Join(directory, layout.IndexesDirectory)
	writePreVersion116Layout(t, root, log)

	index := New(directory, log, Options{Format: log.format})
	inventory, err := index.inspectLayout()
	if err != nil {
		t.Fatal(err)
	}
	if !inventory.hasDisposableData() || inventory.activeShardFiles != 0 {
		t.Fatalf("inventory %+v", *inventory)
	}
	if err := index.Open(); err != nil {
		t.Fatal(err)
	}
	if index.EventCount() != 6 {
		t.Fatalf("indexed %d events", index.EventCount())
	}
	assertOnlyActiveLayout(t, index)
}

func TestInventory_OtherFormatFilesAreDisposable(t *testing.T) {
	for _, format := range []layout.Format{layout.FormatCurrent, layout.FormatLegacy} {
		t.Run("leftovers of "+otherFormatOf(format).String(), func(t *testing.T) {
			directory := t.TempDir()
			// Build a complete index set in the other format first.
			otherLog := newTestLog(otherFormatOf(format), 4)
			if _, err := openIndex(t, directory, otherLog); err != nil {
				t.Fatal(err)
			}
			log := newTestLog(format, 4)
			index, err := openIndex(t, directory, log)
			if err != nil {
				t.Fatal(err)
			}
			assertOnlyActiveLayout(t, index)
			root := filepath.Join(directory, layout.IndexesDirectory)
			filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() && !strings.HasSuffix(path, format.Extension()) {
					t.Errorf("file of the other format remains: %s", path)
				}
				return nil
			})
		})
	}
}

func TestInventory_RetiredLatestEventCategoryIsRemoved(t *testing.T) {
	directory := t.TempDir()
	log := newTestLog(layout.FormatCurrent, 3)
	root := filepath.Join(directory, layout.IndexesDirectory)
	// Shards of an earlier sharded build that still had the category.
	header := string(encodeShardHeader(jsonRecordVersion, categoryRetiredLatestEvent, 7))
	writeFile(t, filepath.Join(root, categoryRetiredLatestEvent, "07.gdb"), header)
	writeFile(t, filepath.Join(root, categoryRetiredLatestEvent, "08.gdb"), "")
	index, err := openIndex(t, directory, log)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyActiveLayout(t, index)
}

func TestInventory_UnrecognizedEntriesStopTheStartupWithoutChanges(t *testing.T) {
	cases := map[string]func(t *testing.T, root string){
		"unknown file in a category": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, categoryEventTypes, "notes.txt"), "x")
		},
		"unknown file in the root": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, "backup.tar"), "x")
		},
		"unknown directory": func(t *testing.T, root string) {
			os.MkdirAll(filepath.Join(root, "my-index"), 0o755)
		},
		"unknown file in the catalog": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, categorySubjectCatalog, "copy.gdb"), "genesisdb\n")
		},
		"nested directory in a legacy type tree": func(t *testing.T, root string) {
			writeFile(t, filepath.Join(root, categoryLatestEventOfSubjectAndType, hashedName("t"), "readme"), "x")
		},
		"symlink in a category": func(t *testing.T, root string) {
			os.MkdirAll(filepath.Join(root, categoryEventTypes), 0o755)
			if err := os.Symlink(os.TempDir(), filepath.Join(root, categoryEventTypes, "link")); err != nil {
				t.Skip(err)
			}
		},
		"symlink in the root": func(t *testing.T, root string) {
			os.MkdirAll(root, 0o755)
			if err := os.Symlink(os.TempDir(), filepath.Join(root, "link")); err != nil {
				t.Skip(err)
			}
		},
		"state journal is a directory": func(t *testing.T, root string) {
			os.MkdirAll(filepath.Join(root, layout.FormatLegacy.IndexStateFile()), 0o755)
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			log := newTestLog(layout.FormatLegacy, 4)
			root := filepath.Join(directory, layout.IndexesDirectory)
			writePreVersion116Layout(t, root, log)
			arrange(t, root)
			before := snapshotTree(t, root)

			_, err := openIndex(t, directory, log)
			if err == nil {
				t.Fatal("expected the startup to stop")
			}
			if errors.Is(err, core.ErrIndexCorrupt) || errors.Is(err, core.ErrUnsupportedIndexFormat) {
				t.Fatalf("unexpected error class: %v", err)
			}
			if after := snapshotTree(t, root); after != before {
				t.Fatalf("index root was modified\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// A file that carries one of our shard names is derived data of ours, so a
// damaged header is repaired by a rebuild instead of stopping the startup.
func TestInventory_ShardWithDamagedHeaderIsRebuilt(t *testing.T) {
	for name, header := range map[string]string{"foreign": `{"format":"other"}` + "\n", "garbage": "\x00\x01\x02", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			log := newTestLog(layout.FormatCurrent, 4)
			if _, err := openIndex(t, directory, log); err != nil {
				t.Fatal(err)
			}
			shard := filepath.Join(directory, layout.IndexesDirectory, categoryEventsBySubject, layout.FormatCurrent.ShardFileName(shardNumberForKey("/a/1")))
			writeFile(t, shard, header)
			index, err := openIndex(t, directory, log)
			if err != nil {
				t.Fatal(err)
			}
			assertOnlyActiveLayout(t, index)
			snapshot, _ := index.AcquireSnapshot()
			if latest, err := index.FindLatestPostingOfSubject(snapshot, "/a/1"); err != nil || latest == nil {
				t.Fatalf("shard was not rebuilt: %v %v", latest, err)
			}
		})
	}
}

func TestInventory_FutureFormatIsRejectedBeforeAnyChange(t *testing.T) {
	cases := map[string]string{
		"future version":     `{"format":"genesisdb-sharded-index","version":99,"category":"event-types","shard":"00","shardCount":256}` + "\n",
		"future shard count": `{"format":"genesisdb-sharded-index","version":2,"category":"event-types","shard":"00","shardCount":512}` + "\n",
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			log := newTestLog(layout.FormatCurrent, 2)
			root := filepath.Join(directory, layout.IndexesDirectory)
			writeFile(t, filepath.Join(root, categoryEventTypes, "00.gdb"), header)
			// Disposable data next to it must survive as well.
			writeFile(t, filepath.Join(root, categoryRetiredLatestEvent, layout.LegacyIndexFile), "eventstore\n")
			before := snapshotTree(t, root)
			_, err := openIndex(t, directory, log)
			if !errors.Is(err, core.ErrUnsupportedIndexFormat) {
				t.Fatalf("got %v", err)
			}
			if after := snapshotTree(t, root); after != before {
				t.Fatal("index root was modified")
			}
		})
	}
}

func TestInventory_RemovalIsIdempotent(t *testing.T) {
	directory := t.TempDir()
	log := newTestLog(layout.FormatLegacy, 4)
	root := filepath.Join(directory, layout.IndexesDirectory)
	writePreVersion116Layout(t, root, log)
	index := New(directory, log, Options{Format: log.format})
	for round := 0; round < 2; round++ {
		if err := index.removeDisposableData(); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	inventory, err := index.inspectLayout()
	if err != nil {
		t.Fatal(err)
	}
	if inventory.hasDisposableData() {
		t.Fatal("disposable data remains")
	}
	// Category directories are kept, only their legacy content goes.
	for _, category := range []string{categorySubjectCatalog, categoryEventsBySubject, categoryEventsBySubjectWithAncestors, categoryEventTypes, categoryLatestEventOfSubjectAndType} {
		if _, err := os.Stat(filepath.Join(root, category)); err != nil {
			t.Errorf("category directory %s was removed", category)
		}
	}
	if _, err := os.Stat(filepath.Join(root, categoryRetiredLatestEvent)); !errors.Is(err, os.ErrNotExist) {
		t.Error("retired category directory remains")
	}
}

func otherFormatOf(format layout.Format) layout.Format {
	if format == layout.FormatLegacy {
		return layout.FormatCurrent
	}
	return layout.FormatLegacy
}

// snapshotTree lists every path with its size, so a test can prove that a
// directory tree was not modified.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		relative, _ := filepath.Rel(root, path)
		size := int64(-1)
		if info.Mode().IsRegular() {
			size = info.Size()
		}
		lines = append(lines, relative+" "+info.Mode().String()+" "+strconv.FormatInt(size, 10))
		return nil
	})
	return strings.Join(lines, "\n")
}
