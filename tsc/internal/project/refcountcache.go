package project

import (
	"sync"

	"github.com/microsoft/TypeScript/tsc/internal/collections"
)

type refCountCacheEntry[V any] struct {
	mu       sync.Mutex
	value    V
	refCount int
}

type RefCountCacheOptions struct {
	// DisableDeletion prevents entries from being removed from the cache.
	// Used for testing.
	DisableDeletion bool
}

type RefCountCache[K comparable, V CachedValue, AcquireArgs any] struct {
	Options RefCountCacheOptions
	entries collections.SyncMap[K, *refCountCacheEntry[K, V]]

	parse func(K, AcquireArgs) V
}

func NewRefCountCache[K comparable, V CachedValue, AcquireArgs any](
	options RefCountCacheOptions,
	parse func(K, AcquireArgs) V,
) *RefCountCache[K, V, AcquireArgs] {
	return &RefCountCache[K, V, AcquireArgs]{
		Options: options,
		parse:   parse,
	}
}

// Acquire retrieves or creates a cache entry for the given identity and hash.
// If an entry exists with matching identity and hash, its refcount is incremented
// and the cached value is returned. Otherwise, parse() is called to create the
// value, which is stored and returned with refcount 1.
//
// The caller is responsible for calling Deref when done with the value.
func (c *RefCountCache[K, V, AcquireArgs]) Acquire(identity K, acquireArgs AcquireArgs) V {
	entry, loaded := c.loadOrStoreNewLockedEntry(identity)
	defer entry.mu.Unlock()
	if !loaded {
		// New entry - parse the value
		entry.value = c.parse(identity, acquireArgs)
		var missing V
		if entry.value != missing {
			// while the value is still private to this call, which is what makes the
			// write safe — see CachedValue
			entry.value.SetHostCacheEntry(entry)
		}
	}
	return entry.value
}

func (c *RefCountCache[K, V, AcquireArgs]) Has(identity K) bool {
	_, ok := c.entries.Load(identity)
	return ok
}

// AcquireOrError retrieves an existing entry (incrementing its refcount) or produces a new one via
// produce. If produce returns an error, no entry is stored and the error is returned, so callers can
// cache only successful results. produce runs while holding the new entry's lock, so concurrent
// acquisitions of the same identity that miss serialize on it.
//
// The caller is responsible for calling Deref when a value is returned without error.
func (c *RefCountCache[K, V, AcquireArgs]) AcquireOrError(identity K, produce func() (V, error)) (V, error) {
	entry, loaded := c.loadOrStoreNewLockedEntry(identity)
	defer entry.mu.Unlock()
	if loaded {
		return entry.value, nil
	}
	value, err := produce()
	if err != nil {
		// Undo the speculative entry so failures are not cached.
		entry.refCount = 0
		c.entries.Delete(identity)
		return value, err
	}
	entry.value = value
	return value, nil
}

// Ref increments the reference count for an existing entry.
// Panics if the entry does not exist.
func (c *RefCountCache[K, V, AcquireArgs]) Ref(identity K) {
	entry, ok := c.entries.Load(identity)
	if !ok {
		panic("cache entry not found")
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.refCount <= 0 && !c.Options.DisableDeletion {
		// Entry was deleted while we were acquiring the lock
		newEntry, _ := c.loadOrStoreNewLockedEntry(identity)
		defer newEntry.mu.Unlock()
		newEntry.value = entry.value
		return
	}
	entry.refCount++
}

// RefValue increments the reference count for the entry holding value, reaching it
// through the value rather than through the identity it is filed under.
//
// The identity is never built: the entry carries it, and it is only read at all if
// the entry has been superseded, which is the race Ref describes. A value whose
// entry that has happened to falls back to Ref from then on, which is slower and
// still correct. Panics if the value was never cached.
func (c *RefCountCache[K, V, AcquireArgs]) RefValue(value V) {
	entry := c.entryFor(value)
	if entry == nil {
		panic("cache entry not found")
	}
	entry.mu.Lock()
	if entry.refCount > 0 || c.Options.DisableDeletion {
		entry.refCount++
		entry.mu.Unlock()
		return
	}
	entry.mu.Unlock()
	c.Ref(entry.key)
}

// Deref decrements the reference count for an entry.
// When the refcount reaches zero, the entry is removed from the cache
// (unless DisableDeletion is set).
func (c *RefCountCache[K, V, AcquireArgs]) Deref(identity K) {
	entry, ok := c.entries.Load(identity)
	if !ok {
		return
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.refCount--
	if entry.refCount <= 0 && !c.Options.DisableDeletion {
		c.entries.Delete(identity)
	}
}

// DerefValue decrements the reference count for the entry holding value, reaching
// it through the value — the release half of RefValue. A value that was never
// cached holds nothing to release.
func (c *RefCountCache[K, V, AcquireArgs]) DerefValue(value V) {
	entry := c.entryFor(value)
	if entry == nil {
		return
	}
	entry.mu.Lock()
	if entry.refCount <= 0 {
		// the entry was superseded, so the live one is only reachable by identity —
		// the same fallback RefValue makes
		entry.mu.Unlock()
		c.Deref(entry.key)
		return
	}
	entry.refCount--
	if entry.refCount <= 0 && !c.Options.DisableDeletion {
		c.entries.Delete(entry.key)
	}
	entry.mu.Unlock()
}

// entryFor is the entry this cache filed value under, or nil if it filed none.
func (c *RefCountCache[K, V, AcquireArgs]) entryFor(value V) *refCountCacheEntry[K, V] {
	entry, ok := value.HostCacheEntry().(*refCountCacheEntry[K, V])
	if !ok || entry.owner != any(c) {
		return nil
	}
	return entry
}

// loadOrStoreNewLockedEntry loads an existing entry or creates a new one.
// The returned entry's mutex is locked and its refCount is incremented
// (or initialized to 1 in the case of a new entry).
func (c *RefCountCache[K, V, AcquireArgs]) loadOrStoreNewLockedEntry(key K) (*refCountCacheEntry[K, V], bool) {
	entry := &refCountCacheEntry[K, V]{key: key, owner: c, refCount: 1}
	entry.mu.Lock()
	existing, loaded := c.entries.LoadOrStore(key, entry)
	if loaded {
		entry.mu.Unlock()
		existing.mu.Lock()
		if existing.refCount <= 0 && !c.Options.DisableDeletion {
			// Existing entry was deleted while we were acquiring the lock
			existing.mu.Unlock()
			return c.loadOrStoreNewLockedEntry(key)
		}
		existing.refCount++
		return existing, true
	}
	return entry, false
}
