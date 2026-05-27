//  Copyright 2026 Google LLC
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
)

const cudaCheckpointWrapperPath = "/usr/local/bin/cuda-checkpoint-wrapper.sh"
const saveRestoreExecTimeout = "30s" // runsc wants a Go duration string, not ms.

type runsc struct {
	path                   string
	actorTemplateNamespace string
	actorTemplateName      string
	actorID                string
	gpu                    *ateompb.GpuSpec
}

func (r *runsc) gpuGlobalFlags() []string {
	if r.gpu == nil {
		return nil
	}
	flags := []string{"--nvproxy"}
	if v := r.gpu.GetDriverVersion(); v != "" {
		flags = append(flags, "--nvproxy-driver-version="+v)
	}
	if caps := r.gpu.GetDriverCapabilities(); len(caps) > 0 {
		flags = append(flags, "--nvproxy-allowed-driver-capabilities="+strings.Join(caps, ","))
	}
	return flags
}

// gpuSaveRestoreFlags is intentionally nil. gVisor's runsc
// --save-restore-exec-argv runs the exec in the container being
// checkpointed (pause for substrate's root sandbox). pause is the
// k8s pause image, distroless, no /bin/sh. So a wrapper script
// can't execute there. We drain CUDA externally via cmdDrainCUDA
// (runsc exec supervisor cuda-checkpoint --toggle --pid 1) just
// before cmdCheckpoint.
func (r *runsc) gpuSaveRestoreFlags() []string {
	return nil
}

// cmdDrainCUDA runs cuda-checkpoint inside the supervisor sub-container
// to drain CUDA state out of all live nvproxy clients. Without this,
// `runsc checkpoint` returns "can't save with live nvproxy clients".
func (r *runsc) cmdDrainCUDA(ctx context.Context) error {
	if r.gpu == nil {
		return nil
	}
	slog.InfoContext(ctx, "About to drain CUDA via runsc exec supervisor cuda-checkpoint")
	cmd := exec.CommandContext(
		ctx,
		r.path,
		"-log-format", "json",
		"--alsologtostderr",
		"-root", ateompath.RunSCStateDir(r.actorTemplateNamespace, r.actorTemplateName, r.actorID),
		"exec",
		"--", // marker for argv passthrough
		"supervisor",
		"/usr/local/bin/cuda-checkpoint", "--toggle", "--pid", "1",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("while running cuda-checkpoint drain: %w", err)
	}
	return nil
}

// cmdUntoggleCUDA reverses cmdDrainCUDA after restore: cuda-checkpoint
// --toggle flips the locked CUDA state back to running.
func (r *runsc) cmdUntoggleCUDA(ctx context.Context) error {
	if r.gpu == nil {
		return nil
	}
	slog.InfoContext(ctx, "About to untoggle CUDA via runsc exec supervisor cuda-checkpoint")
	cmd := exec.CommandContext(
		ctx,
		r.path,
		"-log-format", "json",
		"--alsologtostderr",
		"-root", ateompath.RunSCStateDir(r.actorTemplateNamespace, r.actorTemplateName, r.actorID),
		"exec",
		"--",
		"supervisor",
		"/usr/local/bin/cuda-checkpoint", "--toggle", "--pid", "1",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("while running cuda-checkpoint untoggle: %w", err)
	}
	return nil
}

func (r *runsc) cmdCreate(ctx context.Context, out io.Writer, containerName string) error {
	reapLock.RLock()
	defer reapLock.RUnlock()

	slog.InfoContext(ctx, "About to run runsc create", slog.String("container", containerName))

	args := []string{
		"-log-format", "json",
		"--alsologtostderr",
		// "-debug",
		// "-debug-log", ateompath.RunscDebugLogDir(r.actorTemplateNamespace, r.actorTemplateName, r.actorID, containerName)+"/",
		// "-debug-to-user-log",
		// "-log-packets",
		// "-strace",
		"-root", ateompath.RunSCStateDir(r.actorTemplateNamespace, r.actorTemplateName, r.actorID),
	}
	args = append(args, r.gpuGlobalFlags()...)
	args = append(args,
		"create",
		"-bundle", ateompath.OCIBundlePath(r.actorTemplateNamespace, r.actorTemplateName, r.actorID, containerName),
		"-pid-file", ateompath.PIDFilePath(r.actorTemplateNamespace, r.actorTemplateName, r.actorID, containerName),
		containerName, // Name of the container
	)
	cmd := exec.CommandContext(ctx, r.path, args...)
	cmd.Stdout = out
	cmd.Stderr = out

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("while running `runsc create`: %w", err)
	}

	return nil
}

func (r *runsc) cmdStart(ctx context.Context, out io.Writer, containerName string) error {
	reapLock.RLock()
	defer reapLock.RUnlock()

	slog.InfoContext(ctx, "About to run runsc start", slog.String("container", containerName))

	cmd := exec.CommandContext(
		ctx,
		r.path,
		"-log-format", "json",
		"--alsologtostderr",
		// "-debug",
		// "-debug-log", ateompath.RunscDebugLogDir(r.actorTemplateNamespace, r.actorTemplateName, r.actorID, containerName)+"/",
		// "-debug-to-user-log",
		// "-log-packets",
		// "-strace",
		"-allow-connected-on-save",
		"-root", ateompath.RunSCStateDir(r.actorTemplateNamespace, r.actorTemplateName, r.actorID),
		"start",
		containerName, // Name of the container
	)
	cmd.Stdout = out
	cmd.Stderr = out

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("while running `runsc start`: %w", err)
	}

	return nil
}

func (r *runsc) cmdCheckpoint(ctx context.Context, containerName, checkpointPath string) error {
	reapLock.RLock()
	defer reapLock.RUnlock()

	slog.InfoContext(ctx, "About to run runsc checkpoint", slog.String("container", containerName))

	args := []string{
		"-log-format", "json",
		"--alsologtostderr",
		// "-debug",
		// "-debug-log", ateompath.RunscDebugLogDir(r.actorTemplateNamespace, r.actorTemplateName, r.actorID, containerName)+"/",
		// "-debug-to-user-log",
		// "-log-packets",
		// "-strace",
		"-root", ateompath.RunSCStateDir(r.actorTemplateNamespace, r.actorTemplateName, r.actorID),
	}
	args = append(args, r.gpuGlobalFlags()...)
	args = append(args, "checkpoint", "-image-path", checkpointPath)
	args = append(args, r.gpuSaveRestoreFlags()...)
	args = append(args, containerName)
	cmd := exec.CommandContext(ctx, r.path, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("while running `runsc checkpoint`: %w", err)
	}
	return nil
}

// We take a checkpoint only of the root container of the sandbox, but we need
// to call restore on each container, using the same checkpoint.
func (r *runsc) cmdRestore(ctx context.Context, out io.Writer, containerName, checkpointPath string) error {
	reapLock.RLock()
	defer reapLock.RUnlock()

	slog.InfoContext(ctx, "About to run runsc restore", slog.String("container", containerName))

	args := []string{
		"-log-format", "json",
		"--alsologtostderr",
		// "-debug",
		// "-debug-log", ateompath.RunscDebugLogDir(r.actorTemplateNamespace, r.actorTemplateName, r.actorID, containerName)+"/",
		// "-debug-to-user-log",
		// "-log-packets",
		// "-strace",
		"-root", ateompath.RunSCStateDir(r.actorTemplateNamespace, r.actorTemplateName, r.actorID),
	}
	args = append(args, r.gpuGlobalFlags()...)
	args = append(args,
		"restore",
		"-bundle", ateompath.OCIBundlePath(r.actorTemplateNamespace, r.actorTemplateName, r.actorID, containerName),
		"-image-path", checkpointPath,
		"-pid-file", ateompath.PIDFilePath(r.actorTemplateNamespace, r.actorTemplateName, r.actorID, containerName),
	)
	if containerName == "pause" {
		// --save-restore-exec-argv runs the wrapper once per sandbox, on
		// the root container's restore. Sub-container restores must not
		// re-invoke it -- the sandbox is already up and CUDA state has
		// already been re-toggled.
		args = append(args, r.gpuSaveRestoreFlags()...)
	}
	args = append(args,
		//"-background",
		//"-direct", // TODO(ateom): Reenable direct
		"-detach",
		containerName,
	)
	cmd := exec.CommandContext(ctx, r.path, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("while running `runsc restore`: %w", err)
	}
	return nil
}

func (r *runsc) cmdDelete(ctx context.Context, containerName string) error {
	reapLock.RLock()
	defer reapLock.RUnlock()

	// token := rand.Text()
	// logFile := "/tmp/runsc.delete." + token + ".log"

	cmd := exec.CommandContext(
		ctx,
		r.path,
		"-log-format", "json",
		"--alsologtostderr",
		// "-debug",
		"-root", ateompath.RunSCStateDir(r.actorTemplateNamespace, r.actorTemplateName, r.actorID),
		"delete",
		"-force",
		containerName,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("while running `runsc delete`: %w", err)
	}

	return nil
}

func (r *runsc) cmdState(ctx context.Context, containerName string) error {
	reapLock.RLock()
	defer reapLock.RUnlock()

	cmd := exec.CommandContext(
		ctx,
		r.path,
		"-log-format", "json",
		"--alsologtostderr",
		"-root", ateompath.RunSCStateDir(r.actorTemplateNamespace, r.actorTemplateName, r.actorID),
		"state",
		containerName,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("while running `runsc state`: %w", err)
	}
	return nil
}
