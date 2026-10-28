//go:build !windows

package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// TestPermissions_NewFilesAreOwnerOnly walks a freshly written data directory
// and checks that every file and directory GenesisDB created is readable by
// its owner only. The data directory holds personal data, so group and world
// bits are never set, regardless of the umask being more permissive.
func TestPermissions_NewFilesAreOwnerOnly(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "data")
	s := openTestStore(t, directory, testStoreOptions{convertLegacy: true})
	defer s.Close()
	event := newTestEvent("/p/1", "created", map[string]any{"email": "private@example.invalid"})
	event.Options.StoreDataAsReference = true
	if err := s.Commit(ctx, []*core.Event{event}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplaceReferenceData(ctx, map[string]json.RawMessage{event.ReferenceDataKey: core.EmptyEventData()}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendSystemEvent("audit", "/", core.EmptyEventData()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RebuildIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	err := filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s is group or world accessible: %o", strings.TrimPrefix(path, directory), perm)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
