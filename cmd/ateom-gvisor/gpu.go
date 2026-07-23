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
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/agent-substrate/substrate/internal/ateompath"
)

const (
	gpuSentinelDev = "/dev/nvidia0"
	toolkitDir     = "/usr/local/nvidia/toolkit"
	cdiOutputDir   = "/run/ate-cdi"
	procMountsPath = "/proc/mounts"
)

// cdiSpec is the minimal CDI spec shape we need. nvidia-ctk writes JSON with
// --format=json so no YAML library is required.
type cdiSpec struct {
	Devices []struct {
		Name           string   `json:"name"`
		ContainerEdits cdiEdits `json:"containerEdits"`
	} `json:"devices"`
	ContainerEdits cdiEdits `json:"containerEdits"`
}

type cdiEdits struct {
	Env         []string   `json:"env,omitempty"`
	DeviceNodes []cdiDev   `json:"deviceNodes,omitempty"`
	Hooks       []cdiHook  `json:"hooks,omitempty"`
	Mounts      []cdiMount `json:"mounts,omitempty"`
}

type cdiDev struct {
	Path     string       `json:"path"`
	Type     string       `json:"type,omitempty"`
	Major    int64        `json:"major,omitempty"`
	Minor    int64        `json:"minor,omitempty"`
	FileMode *os.FileMode `json:"fileMode,omitempty"`
	UID      *uint32      `json:"uid,omitempty"`
	GID      *uint32      `json:"gid,omitempty"`
}

type cdiHook struct {
	HookName string   `json:"hookName"`
	Path     string   `json:"path"`
	Args     []string `json:"args,omitempty"`
	Env      []string `json:"env,omitempty"`
	Timeout  *int     `json:"timeout,omitempty"`
}

type cdiMount struct {
	HostPath      string   `json:"hostPath"`
	ContainerPath string   `json:"containerPath"`
	Type          string   `json:"type,omitempty"`
	Options       []string `json:"options,omitempty"`
}

// gpuPresent reports whether a GPU is available to this worker pod.
func gpuPresent(sentinelPath string) bool {
	_, err := os.Stat(sentinelPath)
	return err == nil
}

// enforceCDIMode fails fast when the cluster injected the GPU via the legacy NVIDIA
// runtime rather than CDI. The legacy runtime overmounts /proc/driver/nvidia, which
// trips the kernel's mount_too_revealing() when runsc runs the CDI update-ldcache
// hook in the deprivileged gofer; CDI mode never creates that overmount.
func enforceCDIMode(procMountsFile string) error {
	data, err := os.ReadFile(procMountsFile)
	if err != nil {
		return fmt.Errorf("reading %s: %w", procMountsFile, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "/proc/driver/nvidia" {
			return fmt.Errorf("GPU injection requires the cluster in CDI mode; detected legacy /proc/driver/nvidia overmount")
		}
	}
	return nil
}

var (
	generateOnce sync.Once
	generateErr  error
)

// generateCDISpec runs nvidia-ctk (from the mounted host toolkit) to produce a CDI
// spec scoped to this pod's assigned GPU. Runs under reapLock like every other
// subprocess in this process (a child reaper is running).
func generateCDISpec(ctx context.Context, ctkPath, hookPath, outDir string) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating CDI output dir %s: %w", outDir, err)
	}
	reapLock.RLock()
	defer reapLock.RUnlock()
	cmd := exec.CommandContext(ctx, ctkPath, "cdi", "generate",
		"--format=json",
		"--nvidia-cdi-hook-path="+hookPath,
		"--output="+filepath.Join(outDir, "nvidia.json"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nvidia-ctk cdi generate failed: %w: %s", err, out)
	}
	return nil
}

// ensureCDISpec generates the per-pod CDI spec exactly once for this process.
func ensureCDISpec(ctx context.Context) error {
	generateOnce.Do(func() {
		generateErr = generateCDISpec(ctx,
			filepath.Join(toolkitDir, "nvidia-ctk"),
			filepath.Join(toolkitDir, "nvidia-cdi-hook"),
			cdiOutputDir)
	})
	return generateErr
}

// maybeInjectGPU is a no-op unless the worker pod has a GPU. When it does,
// it enforces CDI mode, generates the per-pod CDI spec once, and injects the
// GPU into the actor container's OCI bundle before runsc create.
func maybeInjectGPU(ctx context.Context, actorUID, containerName string) error {
	if !gpuPresent(gpuSentinelDev) {
		return nil
	}
	slog.InfoContext(ctx, "Injecting GPU into actor container", slog.String("container", containerName))
	if err := enforceCDIMode(procMountsPath); err != nil {
		return err
	}
	if err := ensureCDISpec(ctx); err != nil {
		return err
	}
	bundleDir := ateompath.OCIBundlePath(actorUID, containerName)
	if err := injectGPUIntoBundle(bundleDir, filepath.Join(cdiOutputDir, "nvidia.json")); err != nil {
		return fmt.Errorf("injecting GPU into %q bundle: %w", containerName, err)
	}
	return nil
}

// injectGPUIntoBundle reads the JSON CDI spec at cdiSpecPath and merges its
// device nodes, mounts, hooks, and env vars into the OCI config.json in bundleDir.
// The CDI spec format is JSON (nvidia-ctk --format=json) so encoding/json is
// sufficient; no CDI library is needed.
func injectGPUIntoBundle(bundleDir, cdiSpecPath string) error {
	cdiData, err := os.ReadFile(cdiSpecPath)
	if err != nil {
		return fmt.Errorf("reading CDI spec %s: %w", cdiSpecPath, err)
	}
	var cdi cdiSpec
	if err := json.Unmarshal(cdiData, &cdi); err != nil {
		return fmt.Errorf("parsing CDI spec: %w", err)
	}

	// Collect all edits: spec-level (common to all nvidia devices) + per-device.
	var edits cdiEdits
	edits.Env = append(edits.Env, cdi.ContainerEdits.Env...)
	edits.DeviceNodes = append(edits.DeviceNodes, cdi.ContainerEdits.DeviceNodes...)
	edits.Hooks = append(edits.Hooks, cdi.ContainerEdits.Hooks...)
	edits.Mounts = append(edits.Mounts, cdi.ContainerEdits.Mounts...)
	for _, d := range cdi.Devices {
		edits.Env = append(edits.Env, d.ContainerEdits.Env...)
		edits.DeviceNodes = append(edits.DeviceNodes, d.ContainerEdits.DeviceNodes...)
		edits.Hooks = append(edits.Hooks, d.ContainerEdits.Hooks...)
		edits.Mounts = append(edits.Mounts, d.ContainerEdits.Mounts...)
	}
	if len(edits.DeviceNodes) == 0 && len(edits.Mounts) == 0 && len(edits.Hooks) == 0 {
		return fmt.Errorf("CDI spec %s resolved no devices", cdiSpecPath)
	}

	cfgPath := filepath.Join(bundleDir, "config.json")
	specData, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", cfgPath, err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(specData, &spec); err != nil {
		return fmt.Errorf("parsing OCI spec: %w", err)
	}

	if spec.Process != nil {
		spec.Process.Env = append(spec.Process.Env, edits.Env...)
	}

	if spec.Linux == nil {
		spec.Linux = &specs.Linux{}
	}
	if spec.Linux.Resources == nil {
		spec.Linux.Resources = &specs.LinuxResources{}
	}
	for _, dn := range edits.DeviceNodes {
		major, minor := dn.Major, dn.Minor
		spec.Linux.Devices = append(spec.Linux.Devices, specs.LinuxDevice{
			Path: dn.Path, Type: dn.Type, Major: major, Minor: minor,
			FileMode: dn.FileMode, UID: dn.UID, GID: dn.GID,
		})
		spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, specs.LinuxDeviceCgroup{
			Allow: true, Type: dn.Type, Major: &major, Minor: &minor, Access: "rwm",
		})
	}

	for _, m := range edits.Mounts {
		spec.Mounts = append(spec.Mounts, specs.Mount{
			Source: m.HostPath, Destination: m.ContainerPath,
			Type: m.Type, Options: m.Options,
		})
	}

	if spec.Hooks == nil {
		spec.Hooks = &specs.Hooks{}
	}
	for _, h := range edits.Hooks {
		hook := specs.Hook{Path: h.Path, Args: h.Args, Env: h.Env, Timeout: h.Timeout}
		switch h.HookName {
		case "createContainer":
			spec.Hooks.CreateContainer = append(spec.Hooks.CreateContainer, hook)
		case "startContainer":
			spec.Hooks.StartContainer = append(spec.Hooks.StartContainer, hook)
		}
	}

	out, err := json.Marshal(&spec)
	if err != nil {
		return fmt.Errorf("serializing OCI spec: %w", err)
	}
	return os.WriteFile(cfgPath, out, 0o644)
}
