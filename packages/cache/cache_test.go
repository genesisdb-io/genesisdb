package cache

import (
	"sync"
	"testing"
)

func TestCache_EvictsLeastRecentlyUsed(t *testing.T) {
	cache := New[string, int](Options[int]{MaxEntries: 2})
	cache.Put("a", 1)
	cache.Put("b", 2)
	cache.Get("a")
	cache.Put("c", 3)
	if _, isCached := cache.Get("b"); isCached {
		t.Fatal("least recently used entry not evicted")
	}
	if value, isCached := cache.Get("a"); !isCached || value != 1 {
		t.Fatal("recently used entry evicted")
	}
}

func TestCache_GenerationRejectsStaleValues(t *testing.T) {
	cache := New[string, string](Options[string]{})
	generation := cache.Generation()
	cache.Clear()
	cache.PutIfGenerationMatches(generation, "key", "stale")
	if _, isCached := cache.Get("key"); isCached {
		t.Fatal("value loaded before Clear was stored")
	}
}

func TestCache_CloneDetachesValues(t *testing.T) {
	cache := New[string, []byte](Options[[]byte]{Clone: func(value []byte) []byte { return append([]byte(nil), value...) }})
	original := []byte("abc")
	cache.Put("key", original)
	original[0] = 'x'
	cached, _ := cache.Get("key")
	cached[1] = 'y'
	again, _ := cache.Get("key")
	if string(again) != "abc" {
		t.Fatalf("cached value was shared: %s", again)
	}
}

func TestCache_NilCacheIsEmpty(t *testing.T) {
	var cache *Cache[string, int]
	cache.Put("a", 1)
	cache.Clear()
	if _, isCached := cache.Get("a"); isCached || cache.Len() != 0 {
		t.Fatal("nil cache stored a value")
	}
}

func TestCache_SignalsAndConcurrency(t *testing.T) {
	var mutex sync.Mutex
	hits, misses, size := 0, 0, 0
	cache := New[int, int](Options[int]{
		MaxEntries:   100,
		OnHit:        func() { mutex.Lock(); hits++; mutex.Unlock() },
		OnMiss:       func() { mutex.Lock(); misses++; mutex.Unlock() },
		OnSizeChange: func(entries int) { mutex.Lock(); size = entries; mutex.Unlock() },
	})
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for key := 0; key < 200; key++ {
				cache.Put(key, worker)
				cache.Get(key)
			}
		}(worker)
	}
	group.Wait()
	if cache.Len() != 100 || size != 100 || hits+misses != 8*200 {
		t.Fatalf("len %d size %d hits %d misses %d", cache.Len(), size, hits, misses)
	}
}
