package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// The disk-full tests run against a real, small volume instead of injected
// faults, so the actual ENOSPC paths of the operating system are exercised:
// partial writes, failing fsync, failing directory sync and a start on a
// volume that is still full. They are opt-in because they need a dedicated
// volume (a few MiB is enough). See CONTRIBUTING.md for the setup.
func fullVolumeRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("GENESISDB_FULL_VOLUME")
	if root == "" {
		t.Skip("set GENESISDB_FULL_VOLUME to a small, dedicated volume to run disk-full tests")
	}
	// A reserve file is created first and removed to make room again. A
	// completely full HFS or ext4 volume can refuse even unlink.
	for _, name := range []string{"reserve", "filler", "data"} {
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "reserve"), []byte(strings.Repeat("r", 512*1024)), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// fillVolume writes a filler file until the volume refuses the last byte.
func fillVolume(t *testing.T, root string) {
	t.Helper()
	filler, err := os.Create(filepath.Join(root, "filler"))
	if err != nil {
		t.Fatal(err)
	}
	defer filler.Close()
	for _, chunk := range [][]byte{[]byte(strings.Repeat("x", 64*1024)), []byte(strings.Repeat("x", 512)), []byte("x")} {
		for {
			if _, err := filler.Write(chunk); err != nil {
				break
			}
		}
	}
}

// freeVolume removes the filler and the reserve, so a restart has room for
// WAL replay.
func freeVolume(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{"filler", "reserve"} {
		if err := os.Remove(filepath.Join(root, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

// TestDiskFull_CommitsUntilFull commits until the volume is exhausted and
// verifies: the failing commit marks the store unhealthy, a restart on the
// still full volume refuses the directory rather than serving it half
// recovered, and after space is freed no acknowledged event is lost, at most
// the one in-flight batch appears additionally, and the hash chain is intact.
func TestDiskFull_CommitsUntilFull(t *testing.T) {
	root := fullVolumeRoot(t)
	ctx := context.Background()
	directory := filepath.Join(root, "data")
	s := openTestStore(t, directory, testStoreOptions{convertLegacy: true})
	payload := map[string]any{"blob": strings.Repeat("x", 30*1024)}
	var acknowledged []string
	var failure error
	for i := 0; i < 100000 && failure == nil; i++ {
		event := newTestEvent("/d/1", "t", payload)
		if err := s.Commit(ctx, []*core.Event{event}); err != nil {
			failure = err
			break
		}
		acknowledged = append(acknowledged, event.ID)
	}
	if failure == nil {
		t.Fatal("the volume did not fill up, use a smaller one")
	}
	if !strings.Contains(failure.Error(), "no space left") {
		t.Fatalf("unexpected failure: %v", failure)
	}
	if s.IsReady() {
		t.Fatal("store ready after a failed append")
	}
	if err := s.Commit(ctx, []*core.Event{newTestEvent("/d/2", "t", map[string]any{})}); !errors.Is(err, core.ErrUnhealthy) {
		t.Fatalf("write accepted after disk-full failure: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Where ENOSPC struck decides what a restart on the still full volume
	// does. A complete WAL batch that needs replay cannot be replayed without
	// space, so Open must refuse with ENOSPC rather than serve a directory
	// that hides a recoverable batch. A torn or already cleared WAL needs no
	// space and the store must open and serve every acknowledged event.
	if reopened, err := tryOpenTestStore(directory, testStoreOptions{convertLegacy: true}); err != nil {
		if !strings.Contains(err.Error(), "no space left") {
			t.Fatalf("unexpected open error on a full volume: %v", err)
		}
	} else {
		all, err := reopened.Fetch(ctx, "/d", FetchOptions{})
		if err != nil {
			t.Fatalf("fetch on a full volume: %v", err)
		}
		if len(all) < len(acknowledged) {
			t.Fatalf("%d events on a full volume, %d acknowledged", len(all), len(acknowledged))
		}
		reopened.Close()
	}

	freeVolume(t, root)
	s = openTestStore(t, directory, testStoreOptions{convertLegacy: true})
	defer s.Close()
	all, err := s.Fetch(ctx, "/d", FetchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	present := map[string]bool{}
	for _, event := range all {
		present[event.ID] = true
	}
	for _, id := range acknowledged {
		if !present[id] {
			t.Errorf("acknowledged event %s lost", id)
		}
	}
	if len(all) > len(acknowledged)+1 {
		t.Errorf("%d events after restart, expected at most %d", len(all), len(acknowledged)+1)
	}
	report, err := s.VerifyHashChain(ctx)
	if err != nil || !report.IsValid() {
		t.Fatalf("hash chain after disk-full recovery: %+v %v", report, err)
	}
	commitTestEvent(t, s, "/d/3", "t")
	assertConsistentWithEventLog(t, s)
}

// TestDiskFull_RestartOnFullVolume fills the volume completely, fails one
// commit at the WAL step (so the WAL is torn or empty, nothing to replay),
// and restarts while the volume is still full. The store must open and serve
// the committed events, recovery needs no space in this case.
func TestDiskFull_RestartOnFullVolume(t *testing.T) {
	root := fullVolumeRoot(t)
	ctx := context.Background()
	directory := filepath.Join(root, "data")
	s := openTestStore(t, directory, testStoreOptions{convertLegacy: true})
	for i := 0; i < 5; i++ {
		commitTestEvent(t, s, "/d/1", "t")
	}
	fillVolume(t, root)
	err := s.Commit(ctx, []*core.Event{newTestEvent("/d/1", "t", map[string]any{"blob": strings.Repeat("y", 20*1024)})})
	if err == nil {
		t.Fatal("commit succeeded on a full volume")
	}
	if s.IsReady() {
		t.Fatal("store ready after a failed commit")
	}
	s.Close()

	s, err = tryOpenTestStore(directory, testStoreOptions{convertLegacy: true})
	if err != nil {
		t.Fatalf("reopen on a full volume: %v", err)
	}
	all, err := s.Fetch(ctx, "/d", FetchOptions{})
	if err != nil || len(all) != 5 {
		t.Fatalf("events on a full volume: %d %v", len(all), err)
	}
	s.Close()

	freeVolume(t, root)
	s = openTestStore(t, directory, testStoreOptions{convertLegacy: true})
	defer s.Close()
	all, err = s.Fetch(ctx, "/d", FetchOptions{})
	if err != nil || len(all) != 5 {
		t.Fatalf("events after freeing space: %d %v", len(all), err)
	}
	report, err := s.VerifyHashChain(ctx)
	if err != nil || !report.IsValid() {
		t.Fatalf("hash chain: %+v %v", report, err)
	}
	commitTestEvent(t, s, "/d/2", "t")
	assertConsistentWithEventLog(t, s)
}
