// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package memorypullcache

import (
	"container/list"
	"sync"
)

// DefaultMaxCacheBytes is the default total size of the in-memory pull cache.
const DefaultMaxCacheBytes int64 = 4 << 30 // 4 GiB

// maxEntryDenominator caps each entry, and each in-flight buffer, at a quarter
// of maxBytes so several pulls fit the budget at once; larger images stream.
const maxEntryDenominator = 4

// byteCache is a concurrency-safe LRU keyed by digest, bounded by total bytes
// rather than entry count. usedBytes also counts in-flight reservations, so a
// caller reserves space, then commits or releases it; reservations aren't evicted.
type byteCache struct {
	mu sync.Mutex

	maxBytes int64
	// perEntryBytes is the largest cacheable entry and the size of one reservation.
	perEntryBytes int64
	// usedBytes is resident entry bytes plus outstanding reservations.
	usedBytes int64

	// ll orders resident entries most-recently-used (front) to least (back).
	ll    *list.List
	items map[string]*list.Element
}

type byteCacheEntry struct {
	key  string
	data []byte
}

func newByteCache(maxBytes int64) *byteCache {
	return &byteCache{
		maxBytes:      maxBytes,
		perEntryBytes: maxBytes / maxEntryDenominator,
		ll:            list.New(),
		items:         make(map[string]*list.Element),
	}
}

// get returns the cached value for key, refreshing its recency on a hit.
func (c *byteCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*byteCacheEntry).data, true
}

// reserve sets aside n bytes for an in-flight admission, evicting LRU entries to
// fit. It returns false if n is out of range or the budget is held by reservations.
func (c *byteCache) reserve(n int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if n <= 0 || n > c.maxBytes {
		return false
	}
	for c.usedBytes > c.maxBytes-n {
		if !c.evictOldest() {
			// only reservations left; can't fit
			return false
		}
	}
	c.usedBytes += n
	return true
}

// release returns n reserved bytes to the budget.
func (c *byteCache) release(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.usedBytes -= n
}

// commit turns a reservation of reserved bytes into a resident entry holding data.
func (c *byteCache) commit(key string, data []byte, reserved int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.usedBytes -= reserved
	c.addLocked(key, data)
}

// addLocked is the body of commit; the caller must hold mu.
func (c *byteCache) addLocked(key string, data []byte) bool {
	size := int64(len(data))
	if size > c.maxBytes {
		return false
	}

	// replace any existing entry
	if el, ok := c.items[key]; ok {
		c.removeElement(el)
	}

	// evict until there is room, then insert
	for c.usedBytes > c.maxBytes-size {
		if !c.evictOldest() {
			break
		}
	}

	el := c.ll.PushFront(&byteCacheEntry{key: key, data: data})
	c.items[key] = el
	c.usedBytes += size
	return true
}

// evictOldest drops the LRU entry, reporting whether one existed. Caller holds mu.
func (c *byteCache) evictOldest() bool {
	el := c.ll.Back()
	if el == nil {
		return false
	}
	c.removeElement(el)
	return true
}

// removeElement unlinks a resident entry and updates accounting. Caller holds mu.
func (c *byteCache) removeElement(el *list.Element) {
	entry := el.Value.(*byteCacheEntry)
	c.ll.Remove(el)
	delete(c.items, entry.key)
	c.usedBytes -= int64(len(entry.data))
}
