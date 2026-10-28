package index

import (
	"container/list"
	"os"
	"sync"
)

// maxOpenShardHandles bounds the shard files kept open between commits. A
// large batch touches about one shard per event and category, so without
// reuse every commit opens and closes hundreds of files, which costs more
// than writing them. The limit covers every shard of the five categories
// and the catalog, so a warm writer never reopens a file. It
// is lowered to a quarter of the descriptor limit when that is smaller, the
// Go runtime raises the soft limit to the hard limit on start.
var maxOpenShardHandles = shardHandleLimit(MaxIndexFiles)

// shardHandles caches writable shard files by path in least recently used
// order. Only the single writer uses it, the mutex guards against Close and
// rebuilds running from another goroutine.
type shardHandles struct {
	mutex   sync.Mutex
	entries map[string]*list.Element
	order   *list.List
}

type shardHandle struct {
	path string
	file *os.File
}

func newShardHandles() *shardHandles {
	return &shardHandles{entries: map[string]*list.Element{}, order: list.New()}
}

// open returns a writable handle for path, creating the file if needed. The
// caller must not close it. Handles are only evicted by trim, so every
// handle of a batch stays open until the batch is synced.
func (handles *shardHandles) open(path string) (*os.File, error) {
	handles.mutex.Lock()
	defer handles.mutex.Unlock()
	if element, exists := handles.entries[path]; exists {
		handles.order.MoveToFront(element)
		return element.Value.(*shardHandle).file, nil
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	handles.entries[path] = handles.order.PushFront(&shardHandle{path: path, file: file})
	return file, nil
}

// trim closes the least recently used handles above the limit.
func (handles *shardHandles) trim() {
	handles.mutex.Lock()
	defer handles.mutex.Unlock()
	for handles.order.Len() > maxOpenShardHandles {
		oldest := handles.order.Back()
		handles.order.Remove(oldest)
		evicted := oldest.Value.(*shardHandle)
		delete(handles.entries, evicted.path)
		evicted.file.Close()
	}
}

// discard closes the handle of path, for example after a failed write left
// its state unknown.
func (handles *shardHandles) discard(path string) {
	handles.mutex.Lock()
	defer handles.mutex.Unlock()
	if element, exists := handles.entries[path]; exists {
		handles.order.Remove(element)
		delete(handles.entries, path)
		element.Value.(*shardHandle).file.Close()
	}
}

// closeAll closes every cached handle. Files may be removed, renamed or
// truncated by other code afterwards.
func (handles *shardHandles) closeAll() {
	handles.mutex.Lock()
	defer handles.mutex.Unlock()
	for _, element := range handles.entries {
		element.Value.(*shardHandle).file.Close()
	}
	handles.entries = map[string]*list.Element{}
	handles.order.Init()
}
