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
	"errors"
	"testing"
	"time"

	"go.ate.dev/substrate/internal/actorlock"
	"go.ate.dev/substrate/internal/resources"
)

// fakeWorkloadAgent stands in for *kata.AgentClient: WaitProcess returns what
// is sent on exits, GetOOMEvent returns what is sent on ooms, and closing
// either channel ends the call with an error, as a closed connection would.
type fakeWorkloadAgent struct {
	exits chan int32
	ooms  chan string
}

var errAgentClosed = errors.New("ttrpc: closed")

func (f *fakeWorkloadAgent) WaitProcess(ctx context.Context, _, _ string) (int32, error) {
	select {
	case status, ok := <-f.exits:
		if !ok {
			return 0, errAgentClosed
		}
		return status, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (f *fakeWorkloadAgent) GetOOMEvent(ctx context.Context) (string, error) {
	select {
	case id, ok := <-f.ooms:
		if !ok {
			return "", errAgentClosed
		}
		return id, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestAwaitWorkloadExit(t *testing.T) {
	ctx := context.Background()
	oomNow := make(chan struct{})
	close(oomNow)
	for _, tc := range []struct {
		name    string
		status  int32
		oom     <-chan struct{}
		want    string
		wantErr bool
	}{
		{name: "exit status", status: 1, oom: make(chan struct{}), want: `container "main" exited with status 1`},
		{name: "oom killed", status: 137, oom: oomNow, want: `container "main" was killed for exceeding its memory limit`},
		{name: "no oom tracking", status: 2, oom: nil, want: `container "main" exited with status 2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeWorkloadAgent{exits: make(chan int32, 1)}
			agent.exits <- tc.status
			got, ok := awaitWorkloadExit(ctx, agent, "main", "c_main", tc.oom, 20*time.Millisecond)
			if !ok || got != tc.want {
				t.Fatalf("awaitWorkloadExit() = %q, %v, want %q, true", got, ok, tc.want)
			}
		})
	}
}

// The OOM event can land after the exit does; the grace covers that order.
func TestAwaitWorkloadExitWaitsForALateOOMEvent(t *testing.T) {
	agent := &fakeWorkloadAgent{exits: make(chan int32, 1)}
	agent.exits <- 137
	oom := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(oom)
	}()
	got, ok := awaitWorkloadExit(context.Background(), agent, "main", "c_main", oom, time.Second)
	if want := `container "main" was killed for exceeding its memory limit`; !ok || got != want {
		t.Fatalf("awaitWorkloadExit() = %q, %v, want %q, true", got, ok, want)
	}
}

// A failed wait is the agent connection going away in a teardown, not an exit.
func TestAwaitWorkloadExitEndsWithTheConnection(t *testing.T) {
	agent := &fakeWorkloadAgent{exits: make(chan int32)}
	close(agent.exits)
	if got, ok := awaitWorkloadExit(context.Background(), agent, "main", "c_main", nil, time.Second); ok {
		t.Fatalf("awaitWorkloadExit() = %q, true, want ok false", got)
	}
}

func TestWatchOOMEventsMarksOnlyKnownContainers(t *testing.T) {
	agent := &fakeWorkloadAgent{ooms: make(chan string, 3)}
	agent.ooms <- "c_other"
	agent.ooms <- "c_main"
	agent.ooms <- "c_main" // a repeat must not close the channel twice
	close(agent.ooms)
	oom := newOOMSignals([]string{"c_main", "c_side"})
	watchOOMEvents(context.Background(), agent, oom)
	select {
	case <-oom.of("c_main"):
	default:
		t.Fatal("c_main was not marked OOM-killed")
	}
	select {
	case <-oom.of("c_side"):
		t.Fatal("c_side was marked OOM-killed without an event")
	default:
	}
	if oom.of("c_other") != nil {
		t.Fatal("an unknown container got a channel")
	}
}

// A workload exit on a VM the actor no longer runs is not a crash of the actor.
func TestWatchWorkloadsIgnoresAReplacedVM(t *testing.T) {
	reporter := &fakeCrashReporter{reports: make(chan string, 1)}
	attribution := resources.ActorAttribution{UID: "actor-a"}
	s := &AteWorkerService{
		locks:         actorlock.New(),
		actors:        map[string]*hostedActor{"actor-a": {attribution: attribution, vm: &runningActor{}}},
		crashReporter: reporter,
	}
	agent := &fakeWorkloadAgent{exits: make(chan int32, 1), ooms: make(chan string)}
	agent.exits <- 1
	close(agent.ooms)
	oomGrace = 10 * time.Millisecond
	t.Cleanup(func() { oomGrace = time.Second })
	s.watchWorkloads(attribution, nil, &runningActor{}, agent, []actorContainer{{name: "main", id: "c_main"}})
	select {
	case msg := <-reporter.reports:
		t.Fatalf("reported %q for a VM the actor no longer runs", msg)
	case <-time.After(200 * time.Millisecond):
	}
}
