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
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/memorypullcache"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/opencontainers/runtime-spec/specs-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sys/unix"
)

func prepareOCIDirectory(ctx context.Context, pullCache *memorypullcache.MemoryPullCache, actorTemplateNamespace, actorTemplateName, actorID, containerName, ref string, args []string, env []string, annotations map[string]string, netns string, gpu *ateletpb.GpuSpec) error {
	tracer := otel.Tracer("prepareOCIDirectory")

	ctx, span := tracer.Start(ctx, "prepareOCIDirectory")
	span.SetAttributes(attribute.String("image", ref))
	defer span.End()

	bundlePath := ateompath.OCIBundlePath(actorTemplateNamespace, actorTemplateName, actorID, containerName)
	rootPath := path.Join(bundlePath, "rootfs")

	if err := os.RemoveAll(rootPath); err != nil {
		return fmt.Errorf("while clearing rootfs %q: %w", rootPath, err)
	}

	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		return fmt.Errorf("in os.MkdirAll for container bundle dir: %w", err)
	}

	tarData, err := pullCache.Fetch(ctx, ref)
	if err != nil {
		return fmt.Errorf("in pullCache.Fetch: %w", err)
	}
	defer tarData.Close()

	if err := untar(ctx, tarData, rootPath); err != nil {
		return fmt.Errorf("in untar: %w", err)
	}

	envVars := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	envVars = append(envVars, env...)

	ociSpec := &specs.Spec{
		Process: &specs.Process{
			User: specs.User{
				UID: 0,
				GID: 0,
			},
			Args: args,
			Env:  envVars,
			Cwd:  "/",
			Capabilities: &specs.LinuxCapabilities{
				Bounding: []string{
					"CAP_AUDIT_WRITE",
					"CAP_KILL",
					"CAP_NET_BIND_SERVICE",
				},
				Effective: []string{
					"CAP_AUDIT_WRITE",
					"CAP_KILL",
					"CAP_NET_BIND_SERVICE",
				},
				Inheritable: []string{
					"CAP_AUDIT_WRITE",
					"CAP_KILL",
					"CAP_NET_BIND_SERVICE",
				},
				Permitted: []string{
					"CAP_AUDIT_WRITE",
					"CAP_KILL",
					"CAP_NET_BIND_SERVICE",
				},
				// TODO(gvisor.dev/issue/3166): support ambient capabilities
			},
			Rlimits: []specs.POSIXRlimit{
				{
					Type: "RLIMIT_NOFILE",
					Hard: 1024,
					Soft: 1024,
				},
			},
		},
		Root: &specs.Root{
			Path:     "rootfs",
			Readonly: false,
		},
		Hostname: "runsc",
		Mounts: []specs.Mount{
			{
				Destination: "/proc",
				Type:        "proc",
				Source:      "proc",
			},
			{
				Destination: "/dev",
				Type:        "tmpfs",
				Source:      "tmpfs",
			},
			{
				Destination: "/sys",
				Type:        "sysfs",
				Source:      "sysfs",
				Options: []string{
					"nosuid",
					"noexec",
					"nodev",
					"ro",
				},
			},
			{
				Destination: "/etc/resolv.conf",
				Type:        "bind",
				Source:      "/etc/resolv.conf",
				Options:     []string{"ro"},
			},
		},
		Linux: &specs.Linux{
			Namespaces: []specs.LinuxNamespace{
				{
					Type: "pid",
				},
				{
					Type: "network",
					Path: netns, // Will be created by ateom
				},
				{
					Type: "ipc",
				},
				{
					Type: "uts",
				},
				{
					Type: "mount",
				},
			},
		},
		Annotations: annotations,
	}

	if gpu != nil {
		if err := addGPUToOCISpec(ociSpec); err != nil {
			return fmt.Errorf("while adding GPU passthrough to OCI spec: %w", err)
		}
		if err := injectNVIDIAAssetsIntoRootfs(ctx, rootPath); err != nil {
			return fmt.Errorf("while injecting NVIDIA driver assets into rootfs: %w", err)
		}
	}
	ociSpecBytes, err := json.MarshalIndent(ociSpec, "", "  ")
	if err != nil {
		return fmt.Errorf("while marshaling OCI spec: %w", err)
	}
	specPath := path.Join(bundlePath, "config.json")
	if err := os.WriteFile(specPath, ociSpecBytes, 0o600); err != nil {
		return fmt.Errorf("while writing OCI spec: %w", err)
	}

	return nil
}

func untar(ctx context.Context, tarData io.Reader, rootPath string) error {
	tracer := otel.Tracer("ateom-gvisor")
	ctx, span := tracer.Start(ctx, "untar")
	defer span.End()

	tarReader := tar.NewReader(tarData)
	for {
		hdr, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("in tarReader.Next: %w", err)
		}

		switch hdr.Typeflag {
		case tar.TypeReg: // Regular file
			target := filepath.Join(rootPath, hdr.Name)

			// Stream directly from tarReader to target file to avoid buffering in memory.
			outFile, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, hdr.FileInfo().Mode())
			if err != nil {
				return fmt.Errorf("while creating file %q: %w", target, err)
			}

			// TODO: Use a constrained fs so that paths containing `..` cannot
			// end up outside the root, and symlinks / hardlinks cannot point
			// outside the root.
			_, err = io.Copy(outFile, tarReader)
			closeErr := outFile.Close()

			if err != nil {
				return fmt.Errorf("while writing contents of %q from tar stream: %w", hdr.Name, err)
			}
			if closeErr != nil {
				return fmt.Errorf("while closing file %q: %w", target, closeErr)
			}

		case tar.TypeDir:
			if hdr.Name == "." {
				// Huh?  I guess this is for setting mode, etc on the root
				// folder.  Ignore for now.
				continue
			}
			target := filepath.Join(rootPath, hdr.Name)
			err := os.Mkdir(target, hdr.FileInfo().Mode())
			if errors.Is(err, os.ErrExist) {
				// Ignore --- real images produced by ko seem to have directory entries placed multiple times?
			} else if err != nil {
				return fmt.Errorf("while creating directory=%q, mode=%v: %w", target, hdr.FileInfo().Mode(), err)
			}

		case tar.TypeSymlink:
			// TODO: Make sure no tricky people are trying to create a symlink pointing out of the rootfs.
			source := filepath.Join(rootPath, hdr.Name)
			// OCI image layers may re-define the same path across layers (e.g.
			// an earlier layer creates /var/run as a directory and a later
			// layer re-declares it as a symlink to /run). Standard tar-extract
			// semantics are "later entry wins": replace any existing entry.
			if existing, err := os.Lstat(source); err == nil {
				// If it's already the same symlink, skip the unlink+symlink pair.
				if existing.Mode()&os.ModeSymlink != 0 {
					if cur, rerr := os.Readlink(source); rerr == nil && cur == hdr.Linkname {
						continue
					}
				}
				// os.RemoveAll removes the symlink entry itself; it does NOT
				// traverse and remove the directory the symlink points to.
				// That's the desired semantic here — replace this path's
				// entry without touching whatever the prior symlink targeted.
				if err := os.RemoveAll(source); err != nil {
					return fmt.Errorf("while replacing existing path at %q before symlink: %w", source, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("while checking existing path at %q before symlink: %w", source, err)
			}
			if err := os.Symlink(hdr.Linkname, source); err != nil {
				return fmt.Errorf("while creating symlink src=%q target=%q: %w", source, hdr.Linkname, err)
			}

		case tar.TypeLink:
			// TODO: Make sure no tricky people are trying to create a hardlink pointing out of the rootfs.
			source := filepath.Join(rootPath, hdr.Linkname)
			target := filepath.Join(rootPath, hdr.Name)
			// Same "later entry wins" handling as TypeSymlink: replace existing entry.
			if _, err := os.Lstat(target); err == nil {
				if err := os.RemoveAll(target); err != nil {
					return fmt.Errorf("while replacing existing path at %q before hardlink: %w", target, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("while checking existing path at %q before hardlink: %w", target, err)
			}
			if err := os.Link(source, target); err != nil {
				return fmt.Errorf("while creating hardlink src=%q target=%q: %w", source, target, err)
			}

		default:
			tfStr := string([]byte{hdr.Typeflag})
			slog.ErrorContext(ctx, "Unhandled tar entry typeflag", slog.String("typeflag", tfStr), slog.Any("hdr", hdr))
			return fmt.Errorf("unhandled tar entry typeflag %q", tfStr)
		}

	}

	return nil
}

var gpuDevicePaths = []string{
	"/dev/nvidiactl",
	"/dev/nvidia-uvm",
	"/dev/nvidia-uvm-tools",
	"/dev/nvidia-modeset",
}

func addGPUToOCISpec(spec *specs.Spec) error {
	paths := append([]string{}, gpuDevicePaths...)
	entries, err := os.ReadDir("/dev")
	if err != nil {
		return fmt.Errorf("reading /dev: %w", err)
	}
	for _, e := range entries {
		n := e.Name()
		if len(n) > 6 && n[:6] == "nvidia" && n[6] >= '0' && n[6] <= '9' {
			paths = append(paths, "/dev/"+n)
		}
	}
	for _, p := range paths {
		var st unix.Stat_t
		if err := unix.Stat(p, &st); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("stat %s: %w", p, err)
		}
		major := int64(unix.Major(uint64(st.Rdev))) //nolint:gosec
		minor := int64(unix.Minor(uint64(st.Rdev))) //nolint:gosec
		mode := os.FileMode(st.Mode & 0o777)        //nolint:gosec
		uid := st.Uid
		gid := st.Gid
		spec.Linux.Devices = append(spec.Linux.Devices, specs.LinuxDevice{
			Path: p, Type: "c", Major: major, Minor: minor,
			FileMode: &mode, UID: &uid, GID: &gid,
		})
		if spec.Linux.Resources == nil {
			spec.Linux.Resources = &specs.LinuxResources{}
		}
		allow := true
		access := "rwm"
		spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices,
			specs.LinuxDeviceCgroup{Allow: allow, Type: "c", Major: &major, Minor: &minor, Access: access},
		)
	}
	for _, name := range []string{"cuda-checkpoint", "cuda-checkpoint-wrapper.sh"} {
		dest := "/usr/local/bin/" + name
		// Source paths: prefer the shared /run/ateom-gvisor/static-files
		// drop (visible inside both atelet and kind-node), fall back to
		// /usr/local/bin if the operator installed it system-wide.
		candidates := []string{"/run/ateom-gvisor/static-files/" + name, dest}
		for _, src := range candidates {
			if _, err := os.Stat(src); err != nil {
				continue
			}
			spec.Mounts = append(spec.Mounts, specs.Mount{
				Destination: dest, Type: "bind", Source: src,
				Options: []string{"ro", "bind"},
			})
			break
		}
	}
	return nil
}

// nvidiaLibsStagingDir is where setup-host.sh stages the host's NVIDIA
// driver libs (libcuda.so.<v>, libnvidia-ml.so.<v>, …) plus their
// SONAME / dev symlinks. atelet copies them into each actor's rootfs at
// sandbox-create time so the workload image doesn't have to bake them in.
//
// This is the substrate-side equivalent of what
// `nvidia-container-cli configure --compute --utility --device=all` does
// in the standard docker+nvidia-container-runtime flow. We replicate the
// effect in Go rather than exec'ing nvidia-container-cli because atelet
// runs on `distroless/static-debian13` and has no dynamic linker for the
// `nvidia-container-cli` binary's libnvidia-container.so.1 dep.
const nvidiaLibsStagingDir = "/run/ateom-gvisor/static-files/nvidia-libs"

// rootfsNVIDIALibDest is where libcuda.so.<v> et al. need to land inside
// the sandbox rootfs. /etc/ld.so.cache on every glibc distro searches
// this path, so dlopen("libcuda.so.1") just works.
const rootfsNVIDIALibDest = "/usr/lib/x86_64-linux-gnu"

// injectNVIDIAAssetsIntoRootfs walks nvidiaLibsStagingDir and mirrors
// every entry into <rootfs>/usr/lib/x86_64-linux-gnu — preserving
// symlinks as symlinks and copying real files byte-for-byte. Hard-fails
// if the staging dir is missing or empty so an operator misconfiguration
// surfaces immediately instead of crashing inside the sandbox.
func injectNVIDIAAssetsIntoRootfs(ctx context.Context, rootfsPath string) error {
	tracer := otel.Tracer("prepareOCIDirectory")
	_, span := tracer.Start(ctx, "injectNVIDIAAssetsIntoRootfs")
	defer span.End()

	entries, err := os.ReadDir(nvidiaLibsStagingDir)
	if err != nil {
		return fmt.Errorf("reading NVIDIA libs staging dir %q (run setup-host.sh): %w", nvidiaLibsStagingDir, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("NVIDIA libs staging dir %q is empty — re-run setup-host.sh", nvidiaLibsStagingDir)
	}

	destDir := filepath.Join(rootfsPath, rootfsNVIDIALibDest)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating dest dir %q: %w", destDir, err)
	}

	var copied, linked int
	for _, e := range entries {
		name := e.Name()
		src := filepath.Join(nvidiaLibsStagingDir, name)
		dst := filepath.Join(destDir, name)

		info, err := os.Lstat(src)
		if err != nil {
			return fmt.Errorf("lstat %q: %w", src, err)
		}

		_ = os.Remove(dst)

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(src)
			if err != nil {
				return fmt.Errorf("readlink %q: %w", src, err)
			}
			if err := os.Symlink(target, dst); err != nil {
				return fmt.Errorf("symlink %q -> %q: %w", dst, target, err)
			}
			linked++
		case info.Mode().IsRegular():
			if err := copyRegularFile(src, dst, info.Mode().Perm()); err != nil {
				return fmt.Errorf("copy %q -> %q: %w", src, dst, err)
			}
			copied++
		default:
			return fmt.Errorf("unexpected file type in %q: %s", src, info.Mode())
		}
	}

	slog.InfoContext(ctx, "Injected NVIDIA driver assets into rootfs",
		slog.String("source", nvidiaLibsStagingDir),
		slog.String("dest", destDir),
		slog.Int("files_copied", copied),
		slog.Int("symlinks_created", linked),
	)
	return nil
}

func copyRegularFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
