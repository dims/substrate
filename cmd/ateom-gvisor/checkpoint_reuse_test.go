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
)

func TestPreviousCheckpointReusable(t *testing.T) {
	t.Parallel()
	running := func(context.Context) (string, error) { return "running", nil }
	stopped := func(context.Context) (string, error) { return "stopped", nil }
	gone := func(context.Context) (string, error) { return "", errors.New("container does not exist") }

	withImage := func(t *testing.T, size int) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, checkpointImageFile), make([]byte, size), 0o600); err != nil {
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
		{name: "no image, container gone", dir: t.TempDir(), status: gone, want: false},
		{name: "empty image, container gone", dir: withImage(t, 0), status: gone, want: false},
		{name: "image, container gone", dir: withImage(t, 1), status: gone, want: true},
		{name: "image, container stopped", dir: withImage(t, 1), status: stopped, want: true},
		{name: "image, container running", dir: withImage(t, 1), status: running, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := previousCheckpointReusable(context.Background(), tt.status, tt.dir); got != tt.want {
				t.Errorf("previousCheckpointReusable() = %v, want %v", got, tt.want)
			}
		})
	}
}
