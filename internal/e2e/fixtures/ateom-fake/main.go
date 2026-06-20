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

// Command ateom-fake is a no-isolation, filesystem-only ateom backend used by
// the compliance suite to exercise the capability model with a second backend.
// It implements the Ateom gRPC contract but, unlike ateom-gvisor:
//
//   - it runs the actor's binary directly with chroot, with no sandbox and no
//     network isolation. The actor runs in the worker pod's network namespace,
//     so it binds the pod IP on port 80 and the router reaches it with no veth
//     or DNAT.
//   - it checkpoints only the writable filesystem (a tar of the rootfs), not
//     process memory, so an actor's in-RAM state resets on every resume while
//     its files survive.
//
// It is a test backend and provides no isolation; never run untrusted workloads
// on it. atelet still builds the OCI bundle and ships the snapshot files; this
// backend ignores the runsc path and reads only the bundle and the checkpoint
// and restore directories atelet prepares.
package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
)

var podUID = pflag.String("pod-uid", "", "The UID of the current pod")

func main() {
	pflag.Parse()
	if err := run(); err != nil {
		log.Fatalf("ateom-fake: %v", err)
	}
}

func run() error {
	ateomDir := ateompath.AteomPath(*podUID)
	if err := os.MkdirAll(ateomDir, 0o700); err != nil {
		return fmt.Errorf("creating ateom dir %q: %w", ateomDir, err)
	}
	sockPath := ateompath.AteomSocketPath(*podUID)
	if err := os.RemoveAll(sockPath); err != nil {
		return fmt.Errorf("removing stale socket %q: %w", sockPath, err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listening on %q: %w", sockPath, err)
	}
	svr := grpc.NewServer()
	ateompb.RegisterAteomServer(svr, &service{procs: map[string]*exec.Cmd{}})
	log.Printf("ateom-fake serving on %s", sockPath)
	return svr.Serve(lis)
}

type service struct {
	ateompb.UnimplementedAteomServer
	mu    sync.Mutex
	procs map[string]*exec.Cmd // app container name -> running process
}

func (s *service) RunWorkload(_ context.Context, req *ateompb.RunWorkloadRequest) (*ateompb.RunWorkloadResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range req.GetSpec().GetContainers() {
		if err := s.start(req.GetActorTemplateNamespace(), req.GetActorTemplateName(), req.GetActorId(), c.GetName()); err != nil {
			return nil, fmt.Errorf("running %q: %w", c.GetName(), err)
		}
	}
	return &ateompb.RunWorkloadResponse{}, nil
}

func (s *service) RestoreWorkload(_ context.Context, req *ateompb.RestoreWorkloadRequest) (*ateompb.RestoreWorkloadResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ns, tmpl, id := req.GetActorTemplateNamespace(), req.GetActorTemplateName(), req.GetActorId()
	img := filepath.Join(ateompath.RestoreStateDir(ns, tmpl, id), "checkpoint.img")
	for _, c := range req.GetSpec().GetContainers() {
		// atelet rebuilt a fresh rootfs from the image; lay the saved filesystem
		// over it so on-disk state survives. Memory does not (fresh process).
		if err := untar(img, rootfsPath(ns, tmpl, id, c.GetName())); err != nil {
			return nil, fmt.Errorf("restoring filesystem for %q: %w", c.GetName(), err)
		}
		if err := s.start(ns, tmpl, id, c.GetName()); err != nil {
			return nil, fmt.Errorf("restoring %q: %w", c.GetName(), err)
		}
	}
	return &ateompb.RestoreWorkloadResponse{}, nil
}

func (s *service) CheckpointWorkload(_ context.Context, req *ateompb.CheckpointWorkloadRequest) (*ateompb.CheckpointWorkloadResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ns, tmpl, id := req.GetActorTemplateNamespace(), req.GetActorTemplateName(), req.GetActorId()
	cpDir := ateompath.CheckpointStateDir(ns, tmpl, id)
	if err := os.MkdirAll(cpDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating checkpoint dir: %w", err)
	}
	for _, c := range req.GetSpec().GetContainers() {
		s.stop(c.GetName())
		if err := tarDir(rootfsPath(ns, tmpl, id, c.GetName()), filepath.Join(cpDir, "checkpoint.img")); err != nil {
			return nil, fmt.Errorf("checkpointing filesystem for %q: %w", c.GetName(), err)
		}
	}
	// atelet ships checkpoint.img plus pages.img and pages_meta.img, and its
	// restore download expects all three. This backend has no memory pages, so
	// write empty placeholders to keep that contract satisfied.
	for _, f := range []string{"pages.img", "pages_meta.img"} {
		if err := os.WriteFile(filepath.Join(cpDir, f), nil, 0o600); err != nil {
			return nil, fmt.Errorf("writing %s placeholder: %w", f, err)
		}
	}
	return &ateompb.CheckpointWorkloadResponse{}, nil
}

// start runs one app container's binary under chroot in the worker pod's network
// namespace. The identity file is written fresh on every start so an actor
// restored from a shared golden snapshot reports its own id, not the golden's.
func (s *service) start(ns, tmpl, id, container string) error {
	rootfs := rootfsPath(ns, tmpl, id, container)
	spec, err := readSpec(filepath.Join(ateompath.OCIBundlePath(ns, tmpl, id, container), "config.json"))
	if err != nil {
		return err
	}
	if len(spec.Process.Args) == 0 {
		return fmt.Errorf("config.json for %q has no process args", container)
	}
	if err := writeIdentity(rootfs, id); err != nil {
		return err
	}
	cwd := spec.Process.Cwd
	if cwd == "" {
		cwd = "/"
	}
	cmd := &exec.Cmd{
		Path:        spec.Process.Args[0],
		Args:        spec.Process.Args,
		Dir:         cwd,
		Env:         spec.Process.Env,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{Chroot: rootfs},
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting process: %w", err)
	}
	s.procs[container] = cmd
	return nil
}

// stop kills a running container process and reaps it.
func (s *service) stop(container string) {
	cmd := s.procs[container]
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	delete(s.procs, container)
}

func rootfsPath(ns, tmpl, id, container string) string {
	return filepath.Join(ateompath.OCIBundlePath(ns, tmpl, id, container), "rootfs")
}

func readSpec(path string) (*specs.Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, fmt.Errorf("parsing %q: %w", path, err)
	}
	if spec.Process == nil {
		return nil, fmt.Errorf("%q has no process", path)
	}
	return &spec, nil
}

func writeIdentity(rootfs, actorID string) error {
	dir := filepath.Join(rootfs, "run", "ate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Raw id, no trailing newline, mirroring atelet's identity contract.
	return os.WriteFile(filepath.Join(dir, "actor-id"), []byte(actorID), 0o644)
}

// tarDir writes a tar of root to dst, skipping run/ate (identity is re-injected
// per actor, never snapshotted, so a golden-derived actor never inherits the
// golden's id).
func tarDir(root, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	defer tw.Close()

	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == filepath.Join("run", "ate") {
			return fs.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var link string
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
}

// untar extracts the tar at src into root, confining entries to root.
func untar(src, root string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	cleanRoot := filepath.Clean(root)
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(root, filepath.FromSlash(hdr.Name))
		if target != cleanRoot && !strings.HasPrefix(target, cleanRoot+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry %q escapes %q", hdr.Name, root)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fs.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fs.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			// Skip device/fifo/etc.; the actor rootfs contains only dirs, files,
			// and symlinks.
		}
	}
}
