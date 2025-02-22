// Package agecache is largely inspired by https://github.com/golang/groupcache
package agecache

import (
	"container/list"
	"errors"
	"math/rand"
	"sync"
	"time"
)

// Stats hold cache statistics.
//
// The struct supports stats package tags, example:
//
// 		prev := cache.Stats()
// 		s := cache.Stats().Delta(prev)
// 		stats.WithPrefix("mycache").Observe(s)
//
type Stats struct {
	Capacity  int64 `metric:"capacity" type:"gauge"`    // Gauge, maximum capacity for the cache
	Count     int64 `metric:"count" type:"gauge"`       // Gauge, number of items in the cache
	Sets      int64 `metric:"sets" type:"counter"`      // Counter, number of sets
	Gets      int64 `metric:"gets" type:"counter"`      // Counter, number of gets
	Hits      int64 `metric:"hits" type:"counter"`      // Counter, number of cache hits from Get operations
	Misses    int64 `metric:"misses" type:"counter"`    // Counter, number of cache misses from Get operations
	Evictions int64 `metric:"evictions" type:"counter"` // Counter, number of evictions
}

// Delta returns a Stats object such that all counters are calculated as the
// difference since the previous.
func (stats Stats) Delta(previous Stats) Stats {
	return Stats{
		Capacity:  stats.Capacity,
		Count:     stats.Count,
		Sets:      stats.Sets - previous.Sets,
		Gets:      stats.Gets - previous.Gets,
		Hits:      stats.Hits - previous.Hits,
		Misses:    stats.Misses - previous.Misses,
		Evictions: stats.Evictions - previous.Evictions,
	}
}

// RandGenerator represents a random number generator.
type RandGenerator interface {
	Int63n(n int64) int64
}

// ExpirationType enumerates expiration types.
type ExpirationType int

const (
	// PassiveExpration expires items passively by checking
	// the item expiry when `.Get()` is called, if the item was
	// expired, it is deleted and nil is returned.
	PassiveExpration ExpirationType = iota

	// ActiveExpiration expires items by managing
	// a goroutine to actively GC expired items in the background.
	ActiveExpiration
)

// Config configures the cache.
type Config[K comparable, V any] struct {
	// Maximum number of items in the cache
	Capacity int
	// Optional max duration before an item expires. Must be greater than or
	// equal to MinAge. If zero, expiration is disabled.
	MaxAge time.Duration
	// Optional min duration before an item expires. Must be less than or equal
	// to MaxAge. When less than MaxAge, uniformly distributed random jitter is
	// added to the expiration time. If equal or zero, jitter is disabled.
	MinAge time.Duration
	// Type of key expiration: Passive or Active
	ExpirationType ExpirationType
	// For active expiration, how often to iterate over the keyspace. Defaults
	// to the MaxAge
	ExpirationInterval time.Duration
	// Optional callback invoked when an item is evicted due to the LRU policy
	OnEviction func(key K, value V)
	// Optional callback invoked when an item expired
	OnExpiration func(key K, value V)
}

// Entry pointed to by each list.Element
type cacheEntry[K comparable, V any] struct {
	key       K
	value     V
	timestamp time.Time
}

// Cache implements a thread-safe fixed-capacity LRU cache.
type Cache[K comparable, V any] struct {
	// Fields defined by configuration
	capacity           int
	minAge             time.Duration
	maxAge             time.Duration
	expirationType     ExpirationType
	expirationInterval time.Duration
	onEviction         func(key K, value V)
	onExpiration       func(key K, value V)

	// Cache statistics
	sets      int64
	gets      int64
	hits      int64
	misses    int64
	evictions int64

	items        map[K]*list.Element
	evictionList *list.List
	mutex        sync.RWMutex
	rand         RandGenerator
}

// New constructs an LRU Cache with the given Config object. config.Capacity
// must be a positive int, and config.MaxAge a zero or positive duration. A
// duration of zero disables item expiration. Panics given an invalid
// config.Capacity or config.MaxAge.
func New[K comparable, V any](config Config[K, V]) *Cache[K, V] {
	if config.Capacity <= 0 {
		panic("Must supply a positive config.Capacity")
	}

	if config.MaxAge < 0 {
		panic("Must supply a zero or positive config.MaxAge")
	}

	if config.MinAge < 0 {
		panic("Must supply a zero or positive config.MinAge")
	}

	if config.MinAge > 0 && config.MinAge > config.MaxAge {
		panic("config.MinAge must be less than or equal to config.MaxAge")
	}

	minAge := config.MinAge
	if minAge == 0 {
		minAge = config.MaxAge
	}

	interval := config.ExpirationInterval
	if interval <= 0 {
		interval = config.MaxAge
	}

	seed := rand.NewSource(time.Now().UnixNano())

	cache := &Cache[K, V]{
		capacity:           config.Capacity,
		maxAge:             config.MaxAge,
		minAge:             minAge,
		expirationType:     config.ExpirationType,
		expirationInterval: interval,
		onEviction:         config.OnEviction,
		onExpiration:       config.OnExpiration,
		items:              make(map[K]*list.Element),
		evictionList:       list.New(),
		rand:               rand.New(seed),
	}

	if config.ExpirationType == ActiveExpiration && interval > 0 {
		go func() {
			for range time.Tick(interval) {
				cache.deleteExpired()
			}
		}()
	}

	return cache
}

// Set updates a key:value pair in the cache. Returns true if an eviction
// occurred, and subsequently invokes the OnEviction callback.
func (cache *Cache[K, V]) Set(key K, value V) bool {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	cache.sets++
	timestamp := cache.getTimestamp()

	if element, ok := cache.items[key]; ok {
		cache.evictionList.MoveToFront(element)
		entry := element.Value.(*cacheEntry[K, V])
		entry.value = value
		entry.timestamp = timestamp
		return false
	}

	entry := &cacheEntry[K, V]{key, value, timestamp}
	element := cache.evictionList.PushFront(entry)
	cache.items[key] = element

	evict := cache.evictionList.Len() > cache.capacity
	if evict {
		cache.evictOldest()
	}
	return evict
}

// Get returns the value stored at `key`. The boolean value reports whether
//  the value was found. The OnExpiration callback is invoked if the value
// had expired on access
func (cache *Cache[K, V]) Get(key K) (value V, found bool) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	cache.gets++

	if element, ok := cache.items[key]; ok {
		entry := element.Value.(*cacheEntry[K, V])
		if cache.maxAge == 0 || time.Since(entry.timestamp) <= cache.maxAge {
			cache.evictionList.MoveToFront(element)
			cache.hits++
			return entry.value, true
		}

		// Entry expired
		cache.deleteElement(element)
		cache.misses++
		if cache.onExpiration != nil {
			cache.onExpiration(entry.key, entry.value)
		}
		return value, false
	}

	cache.misses++
	return value, false
}

// Has returns whether the `key` is in the cache without updating
// how recently it was accessed or deleting it for having expired.
func (cache *Cache[K, V]) Has(key K) bool {
	cache.mutex.RLock()
	defer cache.mutex.RUnlock()

	_, ok := cache.items[key]
	return ok
}

// Peek returns the value at the specified key and a boolean specifying whether
//  it was found, without updating how recently it was accessed or
// deleting it for having expired.
func (cache *Cache[K, V]) Peek(key K) (value V, found bool) {
	cache.mutex.RLock()
	defer cache.mutex.RUnlock()

	if element, ok := cache.items[key]; ok {
		return element.Value.(*cacheEntry[K, V]).value, true
	}

	return value, false
}

// Remove removes the provided key from the cache, returning a bool indicating
// whether it existed.
func (cache *Cache[K, V]) Remove(key K) bool {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	if element, ok := cache.items[key]; ok {
		cache.deleteElement(element)
		return true
	}

	return false
}

// EvictOldest removes the oldest item from the cache, while also invoking any
// eviction callback. A bool is returned indicating whether or not an item was
// removed
func (cache *Cache[K, V]) EvictOldest() bool {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	return cache.evictOldest()
}

// Len returns the number of items in the cache.
func (cache *Cache[K, V]) Len() int {
	cache.mutex.RLock()
	defer cache.mutex.RUnlock()

	return cache.evictionList.Len()
}

// Clear empties the cache.
func (cache *Cache[K, V]) Clear() {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	for _, val := range cache.items {
		cache.deleteElement(val)
	}
	cache.evictionList.Init()
}

// Keys returns all keys in the cache.
func (cache *Cache[K, V]) Keys() []K {
	cache.mutex.RLock()
	defer cache.mutex.RUnlock()

	keys := make([]K, len(cache.items))
	i := 0

	for key := range cache.items {
		keys[i] = key
		i++
	}

	return keys
}

// OrderedKeys returns all keys in the cache, ordered from oldest to newest.
func (cache *Cache[K, V]) OrderedKeys() []K {
	cache.mutex.RLock()
	defer cache.mutex.RUnlock()

	keys := make([]K, len(cache.items))
	i := 0

	for element := cache.evictionList.Back(); element != nil; element = element.Prev() {
		keys[i] = element.Value.(*cacheEntry[K, V]).key
		i++
	}

	return keys
}

// SetMaxAge updates the max age for items in the cache. A duration of zero
// disables expiration. A negative duration, or one that is less than minAge,
// results in an error.
func (cache *Cache[K, V]) SetMaxAge(maxAge time.Duration) error {
	if maxAge < 0 {
		return errors.New("Must supply a zero or positive maxAge")
	} else if maxAge < cache.minAge {
		return errors.New("Must supply a maxAge greater than or equal to minAge")
	}

	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	cache.maxAge = maxAge

	return nil
}

// SetMinAge updates the min age for items in the cache. A duration of zero
// or equal to maxAge disables jitter. A negative duration, or one that is
// greater than maxAge, results in an error.
func (cache *Cache[K, V]) SetMinAge(minAge time.Duration) error {
	if minAge < 0 {
		return errors.New("Must supply a zero or positive minAge")
	} else if minAge > cache.maxAge {
		return errors.New("Must supply a minAge lesser than or equal to maxAge")
	}

	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	if minAge == 0 {
		cache.minAge = cache.maxAge
	} else {
		cache.minAge = minAge
	}

	return nil
}

// OnEviction sets the eviction callback.
func (cache *Cache[K, V]) OnEviction(callback func(key K, value V)) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	cache.onEviction = callback
}

// OnExpiration sets the expiration callback.
func (cache *Cache[K, V]) OnExpiration(callback func(key K, value V)) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	cache.onExpiration = callback
}

// Stats returns cache stats.
func (cache *Cache[K, V]) Stats() Stats {
	cache.mutex.RLock()
	defer cache.mutex.RUnlock()

	return Stats{
		Capacity:  int64(cache.capacity),
		Count:     int64(cache.evictionList.Len()),
		Sets:      cache.sets,
		Gets:      cache.gets,
		Hits:      cache.hits,
		Misses:    cache.misses,
		Evictions: cache.evictions,
	}
}

// Resize the cache to hold at most n entries. If n is smaller than the current
// size, entries are evicted to fit the new size. It errors if n <= 0.
func (cache *Cache[K, V]) Resize(n int) error {
	if n <= 0 {
		return errors.New("must supply a positive capacity to Resize")
	}

	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	c := cache.capacity
	cache.capacity = n

	for i := 0; i < c-n; i++ {
		successful := cache.evictOldest()
		if !successful {
			break
		}
	}

	return nil
}

func (cache *Cache[K, V]) deleteExpired() {
	keys := cache.Keys()

	for i := range keys {
		cache.mutex.Lock()

		if element, ok := cache.items[keys[i]]; ok {
			entry := element.Value.(*cacheEntry[K, V])
			if cache.maxAge > 0 && time.Since(entry.timestamp) > cache.maxAge {
				cache.deleteElement(element)
				if cache.onExpiration != nil {
					cache.onExpiration(entry.key, entry.value)
				}
			}
		}

		cache.mutex.Unlock()
	}
}

func (cache *Cache[K, V]) evictOldest() bool {
	element := cache.evictionList.Back()
	if element == nil {
		return false
	}

	cache.evictions++
	entry := cache.deleteElement(element)
	if cache.onEviction != nil {
		cache.onEviction(entry.key, entry.value)
	}
	return true
}

func (cache *Cache[K, V]) deleteElement(element *list.Element) *cacheEntry[K, V] {
	cache.evictionList.Remove(element)
	entry := element.Value.(*cacheEntry[K, V])
	delete(cache.items, entry.key)
	return entry
}

func (cache *Cache[K, V]) getTimestamp() time.Time {
	timestamp := time.Now()
	if cache.minAge == cache.maxAge {
		return timestamp
	}

	jitter := cache.maxAge - cache.minAge
	randVal := cache.rand.Int63n(jitter.Nanoseconds())

	return timestamp.Add(time.Duration(-randVal))
}
