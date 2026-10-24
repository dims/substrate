//go:build linux

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

package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/resources"
)

// workloadAgent is the slice of *kata.AgentClient the workload watchers use.
type workloadAgent interface {
	WaitProcess(ctx context.Context, containerID, execID string) (int32, error)
	GetOOMEvent(ctx context.Context) (string, error)
}

// oomGrace is how long a watcher waits, once a workload has exited, for the
// agent's OOM event about it: the kernel's kill and the cgroup's event reach
// the agent on separate paths, so the exit can be seen first. A var so tests
// can shorten it.
var oomGrace = time.Second

// oomSignals records which containers the agent reported OOM-killed, one
// channel per container, closed at most once.
type oomSignals struct {
	mu     sync.Mutex
	killed map[string]chan struct{}
	closed map[string]bool
}

func newOOMSignals(ids []string) *oomSignals {
	o := &oomSignals{killed: make(map[string]chan struct{}, len(ids)), closed: make(map[string]bool, len(ids))}
	for _, id := range ids {
		o.killed[id] = make(chan struct{})
	}
	return o
}

// of returns the channel closed when id is OOM-killed, or nil for an id the
// actor does not run.
func (o *oomSignals) of(id string) <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.killed[id]
}

// mark records that id was OOM-killed. Unknown and repeated ids are ignored.
func (o *oomSignals) mark(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ch, ok := o.killed[id]; ok && !o.closed[id] {
		o.closed[id] = true
		close(ch)
	}
}

// watchOOMEvents relays the agent's OOM events into oom until the connection
// fails, which is teardownActor closing it.
func watchOOMEvents(ctx context.Context, agent workloadAgent, oom *oomSignals) {
	for {
		id, err := agent.GetOOMEvent(ctx)
		if err != nil {
			return
		}
		oom.mark(id)
	}
}

// awaitWorkloadExit waits for the container's init process to exit and says
// why. ok is false when the wait itself failed, which is the agent connection
// going away in a teardown, not a process exiting.
func awaitWorkloadExit(ctx context.Context, agent workloadAgent, name, id string, oom <-chan struct{}, grace time.Duration) (message string, ok bool) {
	status, err := agent.WaitProcess(ctx, id, id)
	if err != nil {
		return "", false
	}
	select {
	case <-oom:
		return fmt.Sprintf("container %q was killed for exceeding its memory limit", name), true
	case <-time.After(grace):
	}
	return fmt.Sprintf("container %q exited with status %d", name, status), true
}

// watchWorkloads reports the actor crashed when one of its containers exits
// while nothing is tearing the actor down: the VM and its agent outlive the
// workload, so watchVM alone would leave the actor RUNNING with nothing in it.
// The goroutines read over agent for the actor's lifetime and end when
// teardownActor closes it, like the stdio forwarding.
func (s *AteWorkerService) watchWorkloads(attribution resources.ActorAttribution, actorDirs *ateworkerpb.ActorDirs, ra *runningActor, agent workloadAgent, ctrs []actorContainer) {
	ctx := context.Background()
	oom := newOOMSignals(workloadIDs(ctrs))
	go watchOOMEvents(ctx, agent, oom)
	for _, c := range ctrs {
		go func(c actorContainer) {
			message, ok := awaitWorkloadExit(ctx, agent, c.name, c.id, oom.of(c.id), oomGrace)
			if !ok {
				return
			}
			s.crashActor(attribution, actorDirs, ra, message)
		}(c)
	}
}
