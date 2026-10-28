package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

// Every sync variant must succeed on a regular file and keep its content.
// The ordering and durability guarantees themselves are device behaviour
// that a unit test cannot observe, the crash tests of the store cover the
// recovery path that depends on them.
func TestSyncVariants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("header\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendAndSync(path, []byte("a\n"), nil); err != nil {
		t.Fatal(err)
	}
	if err := AppendAndSyncBeforeBarrier(path, []byte("b\n"), nil); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for name, sync := range map[string]func(*os.File) error{"ordered": SyncOrdered, "before barrier": SyncBeforeBarrier} {
		if err := sync(file); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	content, _ := os.ReadFile(path)
	if string(content) != "header\na\nb\n" {
		t.Fatalf("content %q", content)
	}
	file.Close()
	if err := SyncOrdered(file); err == nil {
		t.Fatal("sync of a closed file succeeded")
	}
}
