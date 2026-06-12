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
	"bytes"
	"fmt"
	"sync"
	"testing"
)

func blob(n int) []byte { return bytes.Repeat([]byte{'x'}, n) }

func TestByteCacheGetHitAndMiss(t *testing.T) {
	c := newByteCache(100)
	if _, ok := c.get("absent"); ok {
		t.Fatal("get on empty cache reported a hit")
	}
	if stored := c.add("a", blob(10)); !stored {
		t.Fatal("add within budget returned false")
	}
	got, ok := c.get("a")
	if !ok {
		t.Fatal("expected hit after add")
	}
	if len(got) != 10 {
		t.Fatalf("got %d bytes, want 10", len(got))
	}
}

func TestByteCacheEvictsByBytesNotCount(t *testing.T) {
	// Budget holds two 40-byte entries but not three.
	c := newByteCache(100)
	c.add("a", blob(40))
	c.add("b", blob(40))
	c.add("c", blob(40)) // total would be 120 > 100; "a" (LRU) must go.

	if _, ok := c.get("a"); ok {
		t.Error("expected LRU entry \"a\" to be evicted")
	}
	if _, ok := c.get("b"); !ok {
		t.Error("expected \"b\" to remain")
	}
	if _, ok := c.get("c"); !ok {
		t.Error("expected \"c\" to remain")
	}
	if c.curBytes != 80 {
		t.Errorf("curBytes = %d, want 80", c.curBytes)
	}
}

func TestByteCacheGetRefreshesLRUOrder(t *testing.T) {
	c := newByteCache(100)
	c.add("a", blob(40))
	c.add("b", blob(40))
	// Touch "a" so it becomes most-recently-used; "b" is now the LRU.
	if _, ok := c.get("a"); !ok {
		t.Fatal("expected \"a\" present")
	}
	c.add("c", blob(40)) // evicts the LRU, which is now "b".

	if _, ok := c.get("b"); ok {
		t.Error("expected \"b\" to be evicted after \"a\" was refreshed")
	}
	if _, ok := c.get("a"); !ok {
		t.Error("expected refreshed \"a\" to survive")
	}
}

func TestByteCacheRejectsOversizedEntry(t *testing.T) {
	c := newByteCache(100)
	c.add("keep", blob(50))
	if stored := c.add("huge", blob(101)); stored {
		t.Error("add of entry larger than maxBytes returned true")
	}
	if _, ok := c.get("huge"); ok {
		t.Error("oversized entry should not be stored")
	}
	if _, ok := c.get("keep"); !ok {
		t.Error("oversized add must not evict existing entries")
	}
	if c.curBytes != 50 {
		t.Errorf("curBytes = %d, want 50 (unchanged by rejected add)", c.curBytes)
	}
}

func TestByteCacheEntryEqualToBudgetEvictsRest(t *testing.T) {
	c := newByteCache(100)
	c.add("a", blob(40))
	c.add("b", blob(40))
	c.add("full", blob(100)) // exactly the budget: everything else must go.

	if _, ok := c.get("full"); !ok {
		t.Error("entry equal to maxBytes should be cached")
	}
	if _, ok := c.get("a"); ok {
		t.Error("expected \"a\" evicted to fit the budget entry")
	}
	if _, ok := c.get("b"); ok {
		t.Error("expected \"b\" evicted to fit the budget entry")
	}
	if c.curBytes != 100 {
		t.Errorf("curBytes = %d, want 100", c.curBytes)
	}
}

func TestByteCacheReplaceUpdatesBytes(t *testing.T) {
	c := newByteCache(100)
	c.add("a", blob(20))
	c.add("a", blob(50)) // same key, larger value.

	got, ok := c.get("a")
	if !ok || len(got) != 50 {
		t.Fatalf("get(a) = (%d bytes, %v), want (50, true)", len(got), ok)
	}
	if c.curBytes != 50 {
		t.Errorf("curBytes = %d, want 50 after replace", c.curBytes)
	}
}

func TestByteCacheConcurrentAccess(t *testing.T) {
	// Exercises the mutex under -race; correctness is just "no panic / no race".
	c := newByteCache(1 << 20)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i%4)
			for j := 0; j < 200; j++ {
				c.add(key, blob(1024))
				c.get(key)
			}
		}(i)
	}
	wg.Wait()
	if c.curBytes > c.maxBytes {
		t.Errorf("curBytes %d exceeded maxBytes %d", c.curBytes, c.maxBytes)
	}
}
