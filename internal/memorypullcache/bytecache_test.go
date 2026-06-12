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

// putResident inserts data as a resident entry through the production
// reserve/commit path, reporting whether it was admitted.
func putResident(c *byteCache, key string, data []byte) bool {
	n := int64(len(data))
	if !c.reserve(n) {
		return false
	}
	c.commit(key, data, n)
	return true
}

func TestByteCacheGetHitAndMiss(t *testing.T) {
	c := newByteCache(100)
	if _, ok := c.get("absent"); ok {
		t.Fatal("get on empty cache reported a hit")
	}
	if stored := putResident(c, "a", blob(10)); !stored {
		t.Fatal("admission within budget returned false")
	}
	got, ok := c.get("a")
	if !ok {
		t.Fatal("expected hit after admission")
	}
	if len(got) != 10 {
		t.Fatalf("got %d bytes, want 10", len(got))
	}
}

func TestByteCacheEvictsByBytesNotCount(t *testing.T) {
	// Budget holds two 40-byte entries but not three.
	c := newByteCache(100)
	putResident(c, "a", blob(40))
	putResident(c, "b", blob(40))
	putResident(c, "c", blob(40)) // total would be 120 > 100; "a" (LRU) must go.

	if _, ok := c.get("a"); ok {
		t.Error("expected LRU entry \"a\" to be evicted")
	}
	if _, ok := c.get("b"); !ok {
		t.Error("expected \"b\" to remain")
	}
	if _, ok := c.get("c"); !ok {
		t.Error("expected \"c\" to remain")
	}
	if c.usedBytes != 80 {
		t.Errorf("usedBytes = %d, want 80", c.usedBytes)
	}
}

func TestByteCacheGetRefreshesLRUOrder(t *testing.T) {
	c := newByteCache(100)
	putResident(c, "a", blob(40))
	putResident(c, "b", blob(40))
	// Touch "a" so it becomes most-recently-used; "b" is now the LRU.
	if _, ok := c.get("a"); !ok {
		t.Fatal("expected \"a\" present")
	}
	putResident(c, "c", blob(40)) // evicts the LRU, which is now "b".

	if _, ok := c.get("b"); ok {
		t.Error("expected \"b\" to be evicted after \"a\" was refreshed")
	}
	if _, ok := c.get("a"); !ok {
		t.Error("expected refreshed \"a\" to survive")
	}
}

func TestByteCacheRejectsOversizedEntry(t *testing.T) {
	c := newByteCache(100)
	putResident(c, "keep", blob(50))
	if stored := putResident(c, "huge", blob(101)); stored {
		t.Error("entry larger than maxBytes was admitted")
	}
	if _, ok := c.get("huge"); ok {
		t.Error("oversized entry should not be stored")
	}
	if _, ok := c.get("keep"); !ok {
		t.Error("oversized admission must not evict existing entries")
	}
	if c.usedBytes != 50 {
		t.Errorf("usedBytes = %d, want 50 (unchanged by rejected admission)", c.usedBytes)
	}
}

func TestByteCacheReplaceUpdatesBytes(t *testing.T) {
	c := newByteCache(100)
	putResident(c, "a", blob(20))
	putResident(c, "a", blob(50)) // same key, larger value.

	got, ok := c.get("a")
	if !ok || len(got) != 50 {
		t.Fatalf("get(a) = (%d bytes, %v), want (50, true)", len(got), ok)
	}
	if c.usedBytes != 50 {
		t.Errorf("usedBytes = %d, want 50 after replace", c.usedBytes)
	}
}

func TestByteCacheReserveRejectsBadSizes(t *testing.T) {
	c := newByteCache(100)
	for _, n := range []int64{0, -1, 101} {
		if c.reserve(n) {
			t.Errorf("reserve(%d) = true, want false", n)
		}
		if c.usedBytes != 0 {
			t.Errorf("usedBytes = %d after rejected reserve(%d), want 0", c.usedBytes, n)
		}
	}
}

func TestByteCacheReserveEvictsResidentToFit(t *testing.T) {
	c := newByteCache(100)
	putResident(c, "resident", blob(80))
	if !c.reserve(25) { // needs usedBytes <= 75; must evict the 80-byte entry.
		t.Fatal("reserve(25) = false, want true after eviction")
	}
	if _, ok := c.get("resident"); ok {
		t.Error("expected resident entry evicted to make room for the reservation")
	}
	if c.usedBytes != 25 {
		t.Errorf("usedBytes = %d, want 25 (the reservation)", c.usedBytes)
	}
}

func TestByteCacheResidentAndReservationCoexistWithinBudget(t *testing.T) {
	c := newByteCache(100)
	putResident(c, "resident", blob(50))
	if !c.reserve(25) { // 50 + 25 = 75 <= 100, no eviction needed.
		t.Fatal("reserve(25) = false, want true")
	}
	if _, ok := c.get("resident"); !ok {
		t.Error("resident entry should coexist with the reservation")
	}
	if c.usedBytes != 75 {
		t.Errorf("usedBytes = %d, want 75 (50 resident + 25 reserved)", c.usedBytes)
	}
}

func TestByteCacheReserveFailsWhenBudgetCommittedToReservations(t *testing.T) {
	c := newByteCache(100)
	if !c.reserve(40) {
		t.Fatal("first reserve(40) should succeed")
	}
	if !c.reserve(40) {
		t.Fatal("second reserve(40) should succeed")
	}
	// 80 reserved; a third needs to evict, but reservations are not evictable.
	if c.reserve(40) {
		t.Error("reserve(40) = true, want false when budget is held by reservations")
	}
	if c.usedBytes != 80 {
		t.Errorf("usedBytes = %d, want 80", c.usedBytes)
	}
}

func TestByteCacheReleaseFreesReservation(t *testing.T) {
	c := newByteCache(100)
	c.reserve(60)
	c.release(60)
	if c.usedBytes != 0 {
		t.Fatalf("usedBytes = %d after release, want 0", c.usedBytes)
	}
	if !c.reserve(60) {
		t.Error("reserve(60) should succeed again after release")
	}
}

func TestByteCacheCommitConvertsReservationToEntry(t *testing.T) {
	c := newByteCache(100)
	if !c.reserve(25) {
		t.Fatal("reserve(25) failed")
	}
	c.commit("k", blob(10), 25) // actual data (10) is smaller than reserved (25).

	got, ok := c.get("k")
	if !ok || len(got) != 10 {
		t.Fatalf("get(k) = (%d bytes, %v), want (10, true)", len(got), ok)
	}
	// usedBytes should reflect the 10 resident bytes, not the 25 reserved.
	if c.usedBytes != 10 {
		t.Errorf("usedBytes = %d, want 10 (reservation released, entry committed)", c.usedBytes)
	}
}

func TestByteCacheConcurrentReserveCommitRelease(t *testing.T) {
	// Exercises the reservation paths under -race; the invariant is that
	// usedBytes never exceeds maxBytes and the cache stays consistent.
	const maxBytes = 1 << 16
	c := newByteCache(maxBytes)
	per := c.perEntryBytes // maxBytes/4

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i%4)
			for j := 0; j < 300; j++ {
				if !c.reserve(per) {
					continue
				}
				if j%2 == 0 {
					c.commit(key, blob(int(per)/2), per)
				} else {
					c.release(per)
				}
				c.get(key)
				if got := c.usedBytesSnapshot(); got > maxBytes {
					t.Errorf("usedBytes %d exceeded maxBytes %d", got, maxBytes)
				}
			}
		}(i)
	}
	wg.Wait()
	if c.usedBytes > maxBytes {
		t.Errorf("final usedBytes %d exceeded maxBytes %d", c.usedBytes, maxBytes)
	}
}

// usedBytesSnapshot reads usedBytes under the lock for race-free assertions.
func (c *byteCache) usedBytesSnapshot() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usedBytes
}
