// Package cache provides a bounded, in-memory exact response cache.
package cache

import (
	"container/list"
	"crypto/sha256"
	"errors"
	"sync"
	"time"
)

const maxEntryBytes = 1 << 20

// Entry is the response metadata retained for one cacheable chat response.
type Entry struct {
	Body             []byte
	Provider         string
	Model            string
	Route            string
	PromptTokens     int64
	CompletionTokens int64
	UsageSource      string
}

type item struct {
	key       [sha256.Size]byte
	entry     Entry
	expiresAt time.Time
	bytes     int
}

// Cache stores responses in process memory and evicts least-recently-used entries.
type Cache struct {
	mu       sync.Mutex
	entries  map[[sha256.Size]byte]*list.Element
	lru      list.List
	maxBytes int
	used     int
	ttl      time.Duration
	now      func() time.Time
}

// New returns a cache bounded by maxBytes and ttl.
func New(maxBytes int, ttl time.Duration, now func() time.Time) (*Cache, error) {
	if maxBytes < 1 {
		return nil, errInvalidBounds
	}
	if ttl < time.Second || ttl > 24*time.Hour {
		return nil, errInvalidBounds
	}
	if now == nil {
		now = time.Now
	}
	return &Cache{entries: make(map[[sha256.Size]byte]*list.Element), maxBytes: maxBytes, ttl: ttl, now: now}, nil
}

var errInvalidBounds = errors.New("cache: size must be positive and TTL from 1s to 24h")

// Get returns a copy of a live entry and promotes it to most recently used.
func (c *Cache) Get(app string, body []byte) (Entry, bool) {
	key := cacheKey(app, body)
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return Entry{}, false
	}
	value := element.Value.(*item)
	if !c.now().Before(value.expiresAt) {
		c.remove(element)
		return Entry{}, false
	}
	c.lru.MoveToFront(element)
	return cloneEntry(value.entry), true
}

// Set retains a response when its body is within the per-entry and total bounds.
func (c *Cache) Set(app string, body []byte, entry Entry) {
	if len(entry.Body) > maxEntryBytes {
		return
	}
	weight := len(entry.Body) + sha256.Size + 32 + len(entry.Provider) + len(entry.Model) + len(entry.Route) + len(entry.UsageSource)
	if weight > c.maxBytes {
		return
	}
	entry = cloneEntry(entry)
	key := cacheKey(app, body)
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[key]; ok {
		c.remove(existing)
	}
	for c.used+weight > c.maxBytes {
		c.remove(c.lru.Back())
	}
	value := &item{key: key, entry: entry, expiresAt: c.now().Add(c.ttl), bytes: weight}
	c.entries[key] = c.lru.PushFront(value)
	c.used += value.bytes
}

func (c *Cache) remove(element *list.Element) {
	if element == nil {
		return
	}
	value := element.Value.(*item)
	delete(c.entries, value.key)
	c.used -= value.bytes
	c.lru.Remove(element)
}

func cacheKey(app string, body []byte) [sha256.Size]byte {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(app))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write(body)
	var key [sha256.Size]byte
	copy(key[:], hasher.Sum(nil))
	return key
}

func cloneEntry(entry Entry) Entry {
	entry.Body = append([]byte(nil), entry.Body...)
	return entry
}
