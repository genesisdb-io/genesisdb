// Package cache is a small, generic, bounded LRU cache with generations.
//
// A generation protects against a classic race: a reader loads a value,
// meanwhile a writer changes the underlying data and clears the cache, then
// the reader stores its stale value. Readers capture Generation before they
// load and store with PutIfGenerationMatches, which ignores values loaded
// before the last Clear.
//
// The package depends on the Go standard library only.
package cache

import (
	"container/list"
	"sync"
)

// DefaultMaxEntries is the capacity used when Options.MaxEntries is not set.
const DefaultMaxEntries = 10000

// Options configure a cache.
type Options[V any] struct {
	// MaxEntries bounds the number of entries. Defaults to DefaultMaxEntries.
	MaxEntries int
	// Clone returns a detached copy of a value. It is applied on Put and on
	// Get, so callers can never modify cached values. nil stores values as
	// they are.
	Clone func(V) V
	// OnHit, OnMiss and OnSizeChange receive signals, for example for
	// metrics. All are optional.
	OnHit        func()
	OnMiss       func()
	OnSizeChange func(entries int)
}

// Cache is a bounded LRU cache safe for concurrent use. A nil *Cache is a
// valid, always empty cache.
type Cache[K comparable, V any] struct {
	mutex      sync.Mutex
	options    Options[V]
	generation uint64
	order      *list.List
	entries    map[K]*list.Element
}

type entry[K comparable, V any] struct {
	key   K
	value V
}

// New returns an empty cache.
func New[K comparable, V any](options Options[V]) *Cache[K, V] {
	if options.MaxEntries <= 0 {
		options.MaxEntries = DefaultMaxEntries
	}
	return &Cache[K, V]{options: options, order: list.New(), entries: map[K]*list.Element{}}
}

// Generation returns the current generation. Capture it before loading a
// value that will be stored with PutIfGenerationMatches.
func (cache *Cache[K, V]) Generation() uint64 {
	if cache == nil {
		return 0
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	return cache.generation
}

// Get returns the value of key and marks it as recently used.
func (cache *Cache[K, V]) Get(key K) (V, bool) {
	var zero V
	if cache == nil {
		return zero, false
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	element, isCached := cache.entries[key]
	if !isCached {
		if cache.options.OnMiss != nil {
			cache.options.OnMiss()
		}
		return zero, false
	}
	cache.order.MoveToFront(element)
	if cache.options.OnHit != nil {
		cache.options.OnHit()
	}
	return cache.clone(element.Value.(*entry[K, V]).value), true
}

// PutIfGenerationMatches stores value unless the cache was cleared after
// generation was captured. The least recently used entry is evicted when
// the cache is full.
func (cache *Cache[K, V]) PutIfGenerationMatches(generation uint64, key K, value V) {
	if cache == nil {
		return
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if generation != cache.generation {
		return
	}
	if element, isCached := cache.entries[key]; isCached {
		element.Value.(*entry[K, V]).value = cache.clone(value)
		cache.order.MoveToFront(element)
		return
	}
	cache.entries[key] = cache.order.PushFront(&entry[K, V]{key: key, value: cache.clone(value)})
	for cache.order.Len() > cache.options.MaxEntries {
		oldest := cache.order.Back()
		cache.order.Remove(oldest)
		delete(cache.entries, oldest.Value.(*entry[K, V]).key)
	}
	cache.reportSize()
}

// Put stores value in the current generation.
func (cache *Cache[K, V]) Put(key K, value V) {
	cache.PutIfGenerationMatches(cache.Generation(), key, value)
}

// Clear drops every entry and starts a new generation atomically.
func (cache *Cache[K, V]) Clear() {
	if cache == nil {
		return
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	cache.generation++
	cache.order.Init()
	cache.entries = map[K]*list.Element{}
	cache.reportSize()
}

// Len returns the number of entries.
func (cache *Cache[K, V]) Len() int {
	if cache == nil {
		return 0
	}
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	return cache.order.Len()
}

func (cache *Cache[K, V]) clone(value V) V {
	if cache.options.Clone == nil {
		return value
	}
	return cache.options.Clone(value)
}

func (cache *Cache[K, V]) reportSize() {
	if cache.options.OnSizeChange != nil {
		cache.options.OnSizeChange(cache.order.Len())
	}
}
