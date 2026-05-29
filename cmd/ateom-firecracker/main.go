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

//go:build linux

// Command ateom-firecracker is the Firecracker (microVM) implementation of the
// Ateom runtime contract. It is a sibling of cmd/ateom-gvisor: same gRPC
// service, different backend. atelet selects it via WorkerPool.Backend +
// AteomImage and passes microVM parameters in RuntimeConfig.microvm.
//
// It implements RunWorkload / CheckpointWorkload / RestoreWorkload /
// GetCapabilities by driving the Firecracker VMM over its HTTP API. The
// rootfs/kernel/vmm artifacts are staged by atelet and referenced by path in
// MicroVMParams; ateom-firecracker just boots and snapshots them.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"google.golang.org/grpc"

	"github.com/agent-substrate/substrate/internal/ategcs"
	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
)

const defaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/init"

func main() {
	socket := flag.String("socket", "", "unix socket to listen on (overrides pod-derived path)")
	workDir := flag.String("workdir", "/run/ateom-firecracker", "base dir for per-actor VM state + snapshots")
	podNamespace := flag.String("pod-namespace", "", "namespace of this ateom pod (for socket path)")
	podName := flag.String("pod-name", "", "name of this ateom pod (for socket path)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	sockPath := *socket
	if sockPath == "" {
		if *podNamespace == "" || *podName == "" {
			slog.Error("must set -socket or both -pod-namespace and -pod-name")
			os.Exit(1)
		}
		sockPath = ateompath.AteomSocketPath(*podNamespace, *podName)
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		slog.Error("mkdir socket dir", slog.Any("err", err))
		os.Exit(1)
	}
	_ = os.Remove(sockPath)

	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		slog.Error("listen", slog.Any("err", err))
		os.Exit(1)
	}

	if err := os.MkdirAll(*workDir, 0o755); err != nil {
		slog.Error("mkdir workdir", slog.Any("err", err))
		os.Exit(1)
	}

	svr := grpc.NewServer()
	ateompb.RegisterAteomServer(svr, &fcService{workDir: *workDir})
	slog.Info("ateom-firecracker serving", slog.String("socket", sockPath), slog.String("workdir", *workDir))
	if err := svr.Serve(lis); err != nil {
		slog.Error("serve", slog.Any("err", err))
		os.Exit(1)
	}
}

// fcService implements ateompb.AteomServer backed by a Firecracker microVM.
// One workload at a time (serialized), mirroring ateom-gvisor.
type fcService struct {
	ateompb.UnimplementedAteomServer

	workDir string

	lock    sync.Mutex
	proc    *exec.Cmd // running firecracker process, if any
	apiSock string
}

var _ ateompb.AteomServer = (*fcService)(nil)

func (s *fcService) GetCapabilities(_ context.Context, _ *ateompb.GetCapabilitiesRequest) (*ateompb.Capabilities, error) {
	return &ateompb.Capabilities{
		SupportsLocalPause:       true,  // snapshot mem+state to local disk
		SupportsIncremental:      false, // diff snapshots are dev-preview
		SupportsMemorySnapshot:   true,  // full guest RAM + device state
		RestoreRequiresSameHost:  true,  // same VMM ver + kernel + compatible CPU
		SnapshotPortabilityClass: "cpu-template",
	}, nil
}

func (s *fcService) RunWorkload(ctx context.Context, req *ateompb.RunWorkloadRequest) (*ateompb.RunWorkloadResponse, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	micro := req.GetRuntime().GetMicrovm()
	if micro == nil {
		// Cluster mode: driven by the (unmodified) atelet, which passes no
		// MicroVMParams. Derive the rootfs + entrypoint from the shared hostPath
		// atelet populated and use the baked-in kernel/firecracker.
		return s.runCluster(ctx, req)
	}
	dir := s.actorDir(req.GetActorId())
	if err := s.setupTap(ctx, micro); err != nil {
		return nil, err
	}
	if err := s.startFirecracker(ctx, micro.GetVmmBinaryPath(), filepath.Join(dir, "fc.sock"), dir); err != nil {
		return nil, err
	}
	bootArgs := micro.GetKernelCmdline()
	if bootArgs == "" {
		bootArgs = defaultBootArgs
	}
	steps := [][2]string{
		{"/boot-source", fmt.Sprintf(`{"kernel_image_path":%q,"boot_args":%q}`, micro.GetKernelImagePath(), bootArgs)},
		{"/drives/rootfs", fmt.Sprintf(`{"drive_id":"rootfs","path_on_host":%q,"is_root_device":true,"is_read_only":false}`, micro.GetRootfsImagePath())},
		{"/machine-config", fmt.Sprintf(`{"vcpu_count":%d,"mem_size_mib":%d}`, vcpu(micro), mem(micro))},
		{"/network-interfaces/eth0", fmt.Sprintf(`{"iface_id":"eth0","host_dev_name":%q,"guest_mac":%q}`, micro.GetTapDeviceName(), micro.GetGuestMac())},
	}
	for _, st := range steps {
		if err := s.api(ctx, "PUT", st[0], st[1]); err != nil {
			return nil, err
		}
	}
	if err := s.api(ctx, "PUT", "/actions", `{"action_type":"InstanceStart"}`); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "microVM started", slog.String("actor", req.GetActorId()), slog.String("ip", micro.GetGuestIp()))
	return &ateompb.RunWorkloadResponse{Ready: true, WorkloadIp: micro.GetGuestIp()}, nil
}

func (s *fcService) CheckpointWorkload(ctx context.Context, req *ateompb.CheckpointWorkloadRequest) (*ateompb.CheckpointWorkloadResponse, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if req.GetRuntime().GetMicrovm() == nil {
		return s.checkpointCluster(ctx, req)
	}

	dir := s.actorDir(req.GetActorId())
	snapDir := filepath.Join(dir, "snap")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return nil, err
	}
	vmstate := filepath.Join(snapDir, "vmstate")
	memfile := filepath.Join(snapDir, "memory")

	if err := s.api(ctx, "PATCH", "/vm", `{"state":"Paused"}`); err != nil {
		return nil, fmt.Errorf("pause: %w", err)
	}
	body := fmt.Sprintf(`{"snapshot_type":"Full","snapshot_path":%q,"mem_file_path":%q}`, vmstate, memfile)
	if err := s.api(ctx, "PUT", "/snapshot/create", body); err != nil {
		return nil, fmt.Errorf("snapshot/create: %w", err)
	}
	// Reset to available: free the worker by tearing the VM down. The snapshot
	// (and rootfs disk) remain on local disk.
	s.killFirecracker()

	micro := req.GetRuntime().GetMicrovm()
	artifacts := []string{"vmstate", "memory"}
	if req.GetDestination() == ateompb.Destination_DESTINATION_DURABLE && req.GetSnapshotUriPrefix() != "" {
		// SUSPENDED: push the snapshot (memory + VM state + rootfs disk) to
		// durable object storage so the actor can resume on any compatible node.
		// In the full architecture atelet owns this upload (driven by the
		// SnapshotManifest); doing it here keeps the PoC self-contained.
		store, err := newObjectStorage(ctx)
		if err != nil {
			return nil, fmt.Errorf("object storage: %w", err)
		}
		uri := strings.TrimSuffix(req.GetSnapshotUriPrefix(), "/")
		uploads := []struct{ obj, local string }{
			{uri + "/vmstate", vmstate},
			{uri + "/memory", memfile},
			{uri + "/rootfs", micro.GetRootfsImagePath()},
		}
		for _, u := range uploads {
			if err := ategcs.SendLocalFileToGCSWithZstd(ctx, store, u.obj, u.local); err != nil {
				return nil, fmt.Errorf("upload %s: %w", u.obj, err)
			}
		}
		artifacts = []string{"vmstate", "memory", "rootfs"}
		slog.InfoContext(ctx, "durable checkpoint uploaded", slog.String("uri", uri))
	}

	manifest := &ateompb.SnapshotManifest{
		ArtifactNames: artifacts,
		Backend:       "firecracker",
		VmmVersion:    s.vmmVersion(micro.GetVmmBinaryPath()),
		CpuTemplate:   micro.GetCpuTemplate(),
		Provenance:    map[string]string{"rootfs": micro.GetRootfsImagePath()},
	}
	slog.InfoContext(ctx, "microVM checkpointed", slog.String("actor", req.GetActorId()), slog.String("dest", req.GetDestination().String()))
	return &ateompb.CheckpointWorkloadResponse{Manifest: manifest}, nil
}

func (s *fcService) RestoreWorkload(ctx context.Context, req *ateompb.RestoreWorkloadRequest) (*ateompb.RestoreWorkloadResponse, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	micro := req.GetRuntime().GetMicrovm()
	if micro == nil {
		return s.restoreCluster(ctx, req)
	}
	dir := s.actorDir(req.GetActorId())
	snapDir := filepath.Join(dir, "snap")
	vmstate := filepath.Join(snapDir, "vmstate")
	memfile := filepath.Join(snapDir, "memory")

	// If the local snapshot is absent (e.g., resuming on a different node than
	// the one that checkpointed), pull it from durable object storage.
	if _, statErr := os.Stat(vmstate); statErr != nil && req.GetSnapshotUriPrefix() != "" {
		if err := s.fetchDurableSnapshot(ctx, req.GetSnapshotUriPrefix(), snapDir, vmstate, memfile, micro.GetRootfsImagePath()); err != nil {
			return nil, err
		}
	}

	if err := s.setupTap(ctx, micro); err != nil { // recreate the same tap (name/IP from request)
		return nil, err
	}
	if err := s.startFirecracker(ctx, micro.GetVmmBinaryPath(), filepath.Join(dir, "fc-restore.sock"), dir); err != nil {
		return nil, err
	}
	body := fmt.Sprintf(`{"snapshot_path":%q,"mem_backend":{"backend_type":"File","backend_path":%q},"resume_vm":true}`, vmstate, memfile)
	if err := s.api(ctx, "PUT", "/snapshot/load", body); err != nil {
		return nil, fmt.Errorf("snapshot/load: %w", err)
	}
	slog.InfoContext(ctx, "microVM restored", slog.String("actor", req.GetActorId()), slog.String("ip", micro.GetGuestIp()))
	return &ateompb.RestoreWorkloadResponse{Ready: true, WorkloadIp: micro.GetGuestIp()}, nil
}

// ---- firecracker driver ----

func (s *fcService) actorDir(actorID string) string {
	if actorID == "" {
		actorID = "default"
	}
	d := filepath.Join(s.workDir, actorID)
	_ = os.MkdirAll(d, 0o755)
	return d
}

func (s *fcService) unixClient() *http.Client {
	sock := s.apiSock
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

func (s *fcService) api(ctx context.Context, method, path, body string) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.unixClient().Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s -> %d: %s", method, path, resp.StatusCode, string(rb))
	}
	return nil
}

func runCmd(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, string(out))
	}
	return nil
}

func (s *fcService) setupTap(_ context.Context, micro *ateompb.MicroVMParams) error {
	tap := micro.GetTapDeviceName()
	if tap == "" {
		return fmt.Errorf("microvm.tap_device_name is required")
	}
	_ = runCmd("ip", "link", "del", tap) // ignore if absent
	if err := runCmd("ip", "tuntap", "add", "dev", tap, "mode", "tap"); err != nil {
		return err
	}
	if cidr := micro.GetHostTapCidr(); cidr != "" {
		if err := runCmd("ip", "addr", "add", cidr, "dev", tap); err != nil {
			return err
		}
	}
	return runCmd("ip", "link", "set", tap, "up")
}

func (s *fcService) startFirecracker(ctx context.Context, vmmBin, sockPath, dir string) error {
	if vmmBin == "" {
		return fmt.Errorf("microvm.vmm_binary_path is required")
	}
	s.apiSock = sockPath
	_ = os.Remove(sockPath)
	logf, err := os.Create(filepath.Join(dir, "firecracker.log"))
	if err != nil {
		return err
	}
	cmd := exec.Command(vmmBin, "--api-sock", sockPath)
	cmd.Dir = dir
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	s.proc = cmd
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := s.api(ctx, "GET", "/version", ""); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("firecracker API socket %s not ready", sockPath)
}

func (s *fcService) killFirecracker() {
	if s.proc != nil && s.proc.Process != nil {
		_ = s.proc.Process.Kill()
		_, _ = s.proc.Process.Wait()
		s.proc = nil
	}
}

func (s *fcService) vmmVersion(vmmBin string) string {
	if vmmBin == "" {
		return "unknown"
	}
	out, err := exec.Command(vmmBin, "--version").Output()
	if err != nil {
		return "unknown"
	}
	if i := bytes.IndexByte(out, '\n'); i > 0 {
		return string(bytes.TrimSpace(out[:i]))
	}
	return string(bytes.TrimSpace(out))
}

func vcpu(m *ateompb.MicroVMParams) uint32 {
	if v := m.GetVcpuCount(); v > 0 {
		return v
	}
	return 1
}

func mem(m *ateompb.MicroVMParams) uint32 {
	if v := m.GetMemSizeMib(); v > 0 {
		return v
	}
	return 256
}

// fetchDurableSnapshot downloads {vmstate, memory, rootfs} from object storage
// into the local snapshot dir (rootfs only if missing locally).
func (s *fcService) fetchDurableSnapshot(ctx context.Context, uriPrefix, snapDir, vmstate, memfile, rootfs string) error {
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return err
	}
	store, err := newObjectStorage(ctx)
	if err != nil {
		return fmt.Errorf("object storage: %w", err)
	}
	uri := strings.TrimSuffix(uriPrefix, "/")
	if err := ategcs.FetchLocalFileFromGCSWithZstd(ctx, store, uri+"/vmstate", vmstate); err != nil {
		return fmt.Errorf("download vmstate: %w", err)
	}
	if err := ategcs.FetchLocalFileFromGCSWithZstd(ctx, store, uri+"/memory", memfile); err != nil {
		return fmt.Errorf("download memory: %w", err)
	}
	if _, statErr := os.Stat(rootfs); statErr != nil {
		if err := ategcs.FetchLocalFileFromGCSWithZstd(ctx, store, uri+"/rootfs", rootfs); err != nil {
			return fmt.Errorf("download rootfs: %w", err)
		}
	}
	slog.InfoContext(ctx, "fetched snapshot from durable storage", slog.String("uri", uri))
	return nil
}

// newObjectStorage builds an object-storage client from env, mirroring atelet:
// ATE_STORAGE_BACKEND=s3 uses the AWS SDK (honoring AWS_ENDPOINT_URL + creds,
// path-style for S3-compatible stores like minio/rustfs); otherwise GCS.
func newObjectStorage(ctx context.Context) (ategcs.ObjectStorage, error) {
	if os.Getenv("ATE_STORAGE_BACKEND") == "s3" {
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, err
		}
		cl := s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = true })
		return ategcs.NewS3Client(cl), nil
	}
	cl, err := storage.NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return ategcs.NewGCSClient(cl), nil
}
