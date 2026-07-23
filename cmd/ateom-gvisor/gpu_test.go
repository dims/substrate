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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestMaybeInjectGPU_NoGPUIsNoop(t *testing.T) {
	if _, err := os.Stat(gpuSentinelDev); err == nil {
		t.Skip("host has a real GPU; skipping no-op assertion")
	}
	if err := maybeInjectGPU(context.Background(), "actor_uid", "c1"); err != nil {
		t.Fatalf("expected no-op nil on non-GPU host, got %v", err)
	}
}

func TestGPUPresent(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "nvidia0")
	if gpuPresent(sentinel) {
		t.Fatal("expected absent before creation")
	}
	if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !gpuPresent(sentinel) {
		t.Fatal("expected present after creation")
	}
}

func TestEnforceCDIMode_LegacyOvermountFails(t *testing.T) {
	dir := t.TempDir()
	mounts := filepath.Join(dir, "mounts")
	os.WriteFile(mounts, []byte("proc /proc proc rw 0 0\ntmpfs /proc/driver/nvidia tmpfs rw 0 0\n"), 0o644)
	err := enforceCDIMode(mounts)
	if err == nil || !strings.Contains(err.Error(), "CDI mode") {
		t.Fatalf("expected CDI-mode error, got %v", err)
	}
}

func TestEnforceCDIMode_OK(t *testing.T) {
	dir := t.TempDir()
	mounts := filepath.Join(dir, "mounts")
	os.WriteFile(mounts, []byte("proc /proc proc rw 0 0\ntmpfs /dev tmpfs rw 0 0\n"), 0o644)
	if err := enforceCDIMode(mounts); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestGenerateCDISpec_InvokesCtk(t *testing.T) {
	dir := t.TempDir()
	ctk := filepath.Join(dir, "nvidia-ctk")
	// fake nvidia-ctk that writes a minimal JSON spec to the path given by --output=.
	const script = `#!/bin/sh
out=""
for a in "$@"; do
	case "$a" in --output=*) out="${a#--output=}" ;; esac
done
printf '{"cdiVersion":"0.6.0","kind":"nvidia.com/gpu","devices":[{"name":"all","containerEdits":{"deviceNodes":[{"path":"/dev/nvidia0","type":"c","major":195,"minor":0}]}}]}' > "$out"
`
	if err := os.WriteFile(ctk, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "cdi")
	if err := generateCDISpec(context.Background(), ctk, filepath.Join(dir, "nvidia-cdi-hook"), out); err != nil {
		t.Fatalf("generate: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "nvidia.json"))
	if err != nil || !strings.Contains(string(data), "nvidia.com/gpu") {
		t.Fatalf("spec not written correctly: %q err=%v", data, err)
	}
}

func TestGenerateCDISpec_NonZeroFails(t *testing.T) {
	dir := t.TempDir()
	ctk := filepath.Join(dir, "nvidia-ctk")
	os.WriteFile(ctk, []byte("#!/bin/sh\nexit 3\n"), 0o755)
	err := generateCDISpec(context.Background(), ctk, "hook", filepath.Join(dir, "cdi"))
	if err == nil {
		t.Fatal("expected error on non-zero exit")
	}
}

func TestInjectGPUIntoBundle(t *testing.T) {
	dir := t.TempDir()

	// Minimal JSON CDI spec with one device node and an env var.
	specJSON := `{
  "cdiVersion": "0.6.0",
  "kind": "nvidia.com/gpu",
  "devices": [
    {
      "name": "all",
      "containerEdits": {
        "deviceNodes": [{"path": "/dev/nvidia0", "type": "c", "major": 195, "minor": 0}],
        "env": ["NVIDIA_TEST=1"]
      }
    }
  ]
}`
	cdiSpecPath := filepath.Join(dir, "nvidia.json")
	os.WriteFile(cdiSpecPath, []byte(specJSON), 0o644)

	bundle := filepath.Join(dir, "bundle")
	os.MkdirAll(bundle, 0o755)
	base := &specs.Spec{Version: "1.0.0", Process: &specs.Process{Args: []string{"true"}}}
	data, _ := json.Marshal(base)
	os.WriteFile(filepath.Join(bundle, "config.json"), data, 0o644)

	if err := injectGPUIntoBundle(bundle, cdiSpecPath); err != nil {
		t.Fatalf("inject: %v", err)
	}

	out, _ := os.ReadFile(filepath.Join(bundle, "config.json"))
	var got specs.Spec
	json.Unmarshal(out, &got)

	var hasDev bool
	for _, d := range got.Linux.Devices {
		if d.Path == "/dev/nvidia0" {
			hasDev = true
		}
	}
	if !hasDev {
		t.Fatalf("expected /dev/nvidia0 injected, spec=%s", out)
	}
	var hasEnv bool
	for _, e := range got.Process.Env {
		if e == "NVIDIA_TEST=1" {
			hasEnv = true
		}
	}
	if !hasEnv {
		t.Fatalf("expected NVIDIA_TEST env injected, spec=%s", out)
	}
}

func TestInjectGPUIntoBundle_MissingSpecFails(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "bundle")
	os.MkdirAll(bundle, 0o755)
	base := &specs.Spec{Version: "1.0.0", Process: &specs.Process{}}
	data, _ := json.Marshal(base)
	os.WriteFile(filepath.Join(bundle, "config.json"), data, 0o644)

	if err := injectGPUIntoBundle(bundle, filepath.Join(dir, "nonexistent.json")); err == nil {
		t.Fatal("expected error for missing CDI spec")
	}
}
