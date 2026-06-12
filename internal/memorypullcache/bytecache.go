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

// byteCache is a concurrency-safe LRU keyed by digest, bounded by total bytes
// stored rather than by entry count.
type byteCache struct {
	mu       sync.Mutex
	maxBytes int64
	curBytes int64
	// ll orders entries most-recently-used (front) to least (back).
	ll    *list.List
	items map[string]*list.Element
}

type byteCacheEntry struct {
	key  string
	data []byte
}

func newByteCache(maxBytes int64) *byteCache {
	return &byteCache{
		maxBytes: maxBytes,
		ll:       list.New(),
		items:    make(map[string]*list.Element),
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

// add stores data, evicting least-recently-used entries to stay within maxBytes.
// It returns false without storing when data alone exceeds maxBytes.
func (c *byteCache) add(key string, data []byte) bool {
	size := int64(len(data))
	c.mu.Lock()
	defer c.mu.Unlock()

	if size > c.maxBytes {
		return false
	}

	if el, ok := c.items[key]; ok {
		entry := el.Value.(*byteCacheEntry)
		c.curBytes += size - int64(len(entry.data))
		entry.data = data
		c.ll.MoveToFront(el)
	} else {
		el := c.ll.PushFront(&byteCacheEntry{key: key, data: data})
		c.items[key] = el
		c.curBytes += size
	}

	// evict from the back; the just-added front entry survives (size <= maxBytes)
	for c.curBytes > c.maxBytes {
		c.evictOldest()
	}
	return true
}

// evictOldest removes the least-recently-used entry. The caller must hold mu.
func (c *byteCache) evictOldest() {
	el := c.ll.Back()
	if el == nil {
		return
	}
	entry := el.Value.(*byteCacheEntry)
	c.ll.Remove(el)
	delete(c.items, entry.key)
	c.curBytes -= int64(len(entry.data))
}
