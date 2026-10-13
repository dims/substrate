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
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/agent-substrate/substrate/internal/apierror"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
)

// Every RPC rejects a request without ActorDirs before touching any state.
func TestRPCsRejectMissingActorDirs(t *testing.T) {
	s := &AteomService{}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"RunWorkload": func() error {
			_, err := s.RunWorkload(ctx, &ateompb.RunWorkloadRequest{})
			return err
		},
		"RestoreWorkload": func() error {
			_, err := s.RestoreWorkload(ctx, &ateompb.RestoreWorkloadRequest{})
			return err
		},
		"CheckpointWorkload": func() error {
			_, err := s.CheckpointWorkload(ctx, &ateompb.CheckpointWorkloadRequest{})
			return err
		},
		"TerminateWorkload": func() error {
			_, err := s.TerminateWorkload(ctx, &ateompb.TerminateWorkloadRequest{})
			return err
		},
	} {
		if got := apierror.Code(call()); got != codes.InvalidArgument {
			t.Errorf("%s() code = %v, want %v", name, got, codes.InvalidArgument)
		}
	}
}

func TestRPCsRejectUntrustedRunscPath(t *testing.T) {
	s := &AteomService{}
	ctx := context.Background()
	dirs := &ateompb.ActorDirs{
		RootDir:                   "/node/actors/actor-a",
		OciBundleDir:              "/node/actors/actor-a/bundle",
		CheckpointDir:             "/node/actors/actor-a/checkpoint-state",
		RestoreDir:                "/node/actors/actor-a/restore",
		DurableDirVolumeMountsDir: "/node/actors/actor-a/durable-dirs",
		SystemInfoVolumeRootsDir:  "/node/actors/actor-a/system-info",
		VolumesDir:                "/node/actors/actor-a/volumes",
	}
	for name, call := range map[string]func() error{
		"RunWorkload": func() error {
			_, err := s.RunWorkload(ctx, &ateompb.RunWorkloadRequest{ActorDirs: dirs, RunscPath: "/bin/sh"})
			return err
		},
		"RestoreWorkload": func() error {
			_, err := s.RestoreWorkload(ctx, &ateompb.RestoreWorkloadRequest{ActorDirs: dirs, RunscPath: "/bin/sh"})
			return err
		},
		"CheckpointWorkload": func() error {
			_, err := s.CheckpointWorkload(ctx, &ateompb.CheckpointWorkloadRequest{ActorDirs: dirs, RunscPath: "/bin/sh"})
			return err
		},
		"TerminateWorkload": func() error {
			_, err := s.TerminateWorkload(ctx, &ateompb.TerminateWorkloadRequest{ActorDirs: dirs, RunscPath: "/bin/sh"})
			return err
		},
	} {
		if got := apierror.Code(call()); got != codes.InvalidArgument {
			t.Errorf("%s() code = %v, want %v", name, got, codes.InvalidArgument)
		}
	}
}

func TestPreviousCheckpointReusable(t *testing.T) {
	running := func(context.Context) (string, error) { return "running", nil }
	stopped := func(context.Context) (string, error) { return "stopped", nil }
	gone := func(context.Context) (string, error) { return "", errors.New("container does not exist") }
	withImage := func(size int) string {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	tests := []struct {
		name   string
		dir    string
		status func(context.Context) (string, error)
		want   bool
	}{
		{"no image", t.TempDir(), gone, false},
		{"empty image", withImage(0), gone, false},
		{"image, container gone", withImage(1), gone, true},
		{"image, container stopped", withImage(1), stopped, true},
		{"image, container running", withImage(1), running, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := previousCheckpointReusable(context.Background(), tt.status, tt.dir); got != tt.want {
				t.Errorf("previousCheckpointReusable() = %v, want %v", got, tt.want)
			}
		})
	}
}
