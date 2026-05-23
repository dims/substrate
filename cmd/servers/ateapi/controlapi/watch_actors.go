// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controlapi

import (
	"fmt"
	"time"

	"github.com/agent-substrate/substrate/proto/ateapipb"
)

// watchActorsPollInterval is how often the server re-reads the actor
// catalog from the persistence layer to detect changes. Each
// connected client runs its own poll loop, but each poll is one
// `ListActors` round trip to the store -- the catalog is small and
// the read is cheap. Switching to Redis KEYSPACE notifications is a
// future optimization tracked alongside the operator-provisioned
// `notify-keyspace-events` setting.
const watchActorsPollInterval = 1 * time.Second

// WatchActors implements the streaming RPC defined in
// `proto/ateapipb/ateapi.proto`. The contract:
//
//  1. On connect, emit one `Upsert` event for every actor matching
//     the filter. This bootstraps clients without a separate List
//     call.
//  2. Re-read the catalog every `watchActorsPollInterval` and emit
//     `Upsert` for actors whose `version` advanced since the prior
//     snapshot, plus `DeletedActorId` for actors that dropped out
//     of the catalog.
//  3. Run until the client disconnects (`stream.Context().Done()`).
func (s *Service) WatchActors(req *ateapipb.WatchActorsRequest, stream ateapipb.Control_WatchActorsServer) error {
	ctx := stream.Context()
	prev := make(map[string]int64)

	// Initial bootstrap. Send every matching actor as an Upsert.
	actors, err := s.persistence.ListActors(ctx)
	if err != nil {
		return fmt.Errorf("while listing actors for initial WatchActors snapshot: %w", err)
	}
	for _, a := range actors {
		if !matchesWatchFilter(req, a) {
			continue
		}
		if err := stream.Send(&ateapipb.ActorEvent{
			Payload: &ateapipb.ActorEvent_Upsert{Upsert: a},
		}); err != nil {
			return err
		}
		prev[a.ActorId] = a.Version
	}

	// Steady-state diff loop.
	ticker := time.NewTicker(watchActorsPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}

		current, err := s.persistence.ListActors(ctx)
		if err != nil {
			return fmt.Errorf("while polling actors for WatchActors diff: %w", err)
		}

		// Build a working set keyed by actor_id so we can detect
		// removals by what's NOT in this snapshot.
		seen := make(map[string]struct{}, len(current))
		for _, a := range current {
			seen[a.ActorId] = struct{}{}
			if !matchesWatchFilter(req, a) {
				continue
			}
			lastVersion, known := prev[a.ActorId]
			// Emit on insert (not previously seen) OR version bump
			// (any state change advances `version` per the store's
			// optimistic-concurrency contract).
			if !known || a.Version > lastVersion {
				if err := stream.Send(&ateapipb.ActorEvent{
					Payload: &ateapipb.ActorEvent_Upsert{Upsert: a},
				}); err != nil {
					return err
				}
				prev[a.ActorId] = a.Version
			}
		}

		// Anything in `prev` but not in `seen` was deleted between
		// ticks. Walking the prev map is O(actors); fine for the
		// small actor counts Substrate is sized for today.
		for id := range prev {
			if _, stillThere := seen[id]; stillThere {
				continue
			}
			if err := stream.Send(&ateapipb.ActorEvent{
				Payload: &ateapipb.ActorEvent_DeletedActorId{DeletedActorId: id},
			}); err != nil {
				return err
			}
			delete(prev, id)
		}
	}
}

// matchesWatchFilter applies the WatchActorsRequest's filter fields.
// Empty filters mean "match everything".
func matchesWatchFilter(req *ateapipb.WatchActorsRequest, a *ateapipb.Actor) bool {
	if req == nil {
		return true
	}
	if ns := req.GetActorTemplateNamespace(); ns != "" && a.GetActorTemplateNamespace() != ns {
		return false
	}
	if since := req.GetSinceVersion(); since > 0 && a.GetVersion() <= since {
		return false
	}
	return true
}
