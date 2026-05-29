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

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/agent-substrate/substrate/internal/proto/ateompb"
)

// TestFirecrackerAteomGRPC drives the firecracker ateom backend through the
// real generated ateompb gRPC client: GetCapabilities, RunWorkload, then a
// Checkpoint -> (VM torn down) -> Restore cycle, asserting that the workload's
// in-RAM state survives.
//
// This is an integration test that boots a real Firecracker microVM and so
// requires /dev/kvm, root, and pre-staged artifacts. It is skipped unless
// ATEOM_FC_E2E=1. Defaults assume the artifacts built by the PoC under
// /root/fc-demo (firecracker, vmlinux, counter-rootfs.ext4); override with
// ATEOM_FC_ARTIFACTS.
func TestFirecrackerAteomGRPC(t *testing.T) {
	if os.Getenv("ATEOM_FC_E2E") != "1" {
		t.Skip("set ATEOM_FC_E2E=1 to run the Firecracker integration test (needs /dev/kvm + root + artifacts)")
	}
	art := os.Getenv("ATEOM_FC_ARTIFACTS")
	if art == "" {
		art = "/root/fc-demo"
	}
	for _, f := range []string{"firecracker", "vmlinux", "counter-rootfs.ext4"} {
		if _, err := os.Stat(filepath.Join(art, f)); err != nil {
			t.Fatalf("missing artifact %s: %v", f, err)
		}
	}
	// Best-effort cleanup of any leftovers from prior runs.
	_ = exec.Command("pkill", "-9", "firecracker").Run()
	_ = exec.Command("ip", "link", "del", "fc-tap0").Run()
	time.Sleep(500 * time.Millisecond)

	ctx := context.Background()

	// Start the ateom-firecracker gRPC server on a unix socket.
	sock := filepath.Join(t.TempDir(), "ateom.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	svc := &fcService{workDir: t.TempDir()}
	srv := grpc.NewServer()
	ateompb.RegisterAteomServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		svc.killFirecracker()
		_ = exec.Command("ip", "link", "del", "fc-tap0").Run()
		srv.Stop()
	})

	conn, err := grpc.NewClient("unix:"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	client := ateompb.NewAteomClient(conn)

	// 1) Capabilities
	caps, err := client.GetCapabilities(ctx, &ateompb.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	t.Logf("capabilities: %+v", caps)
	if !caps.GetSupportsMemorySnapshot() || !caps.GetSupportsLocalPause() {
		t.Fatalf("firecracker backend should support memory snapshot + local pause")
	}

	const guestIP = "172.16.0.2"
	runtime := &ateompb.RuntimeConfig{
		Backend: &ateompb.RuntimeConfig_Microvm{Microvm: &ateompb.MicroVMParams{
			VmmBinaryPath:   filepath.Join(art, "firecracker"),
			KernelImagePath: filepath.Join(art, "vmlinux"),
			RootfsImagePath: filepath.Join(art, "counter-rootfs.ext4"),
			VcpuCount:       1,
			MemSizeMib:      256,
			TapDeviceName:   "fc-tap0",
			GuestMac:        "06:00:AC:10:00:02",
			GuestIp:         guestIP,
			HostTapCidr:     "172.16.0.1/24",
		}},
	}
	const actorID = "counter-e2e"

	// 2) Run
	runResp, err := client.RunWorkload(ctx, &ateompb.RunWorkloadRequest{
		ActorId: actorID,
		Spec:    &ateompb.WorkloadSpec{Containers: []*ateompb.Container{{Name: "counter"}}},
		Runtime: runtime,
	})
	if err != nil {
		t.Fatalf("RunWorkload: %v", err)
	}
	t.Logf("RunWorkload -> ready=%v ip=%s", runResp.GetReady(), runResp.GetWorkloadIp())
	if err := waitReachable(guestIP, 20*time.Second); err != nil {
		t.Fatalf("workload never came up: %v", err)
	}

	// 3) drive in-RAM state
	var before int
	for i := 0; i < 3; i++ {
		n, body, err := hitCounter(guestIP)
		if err != nil {
			t.Fatalf("hit: %v", err)
		}
		t.Logf("  %s", body)
		before = n
	}
	t.Logf("count before checkpoint = %d", before)

	// 4) Checkpoint (LOCAL / PAUSED) — pauses, snapshots, tears down VM
	ckResp, err := client.CheckpointWorkload(ctx, &ateompb.CheckpointWorkloadRequest{
		ActorId:     actorID,
		Spec:        &ateompb.WorkloadSpec{Containers: []*ateompb.Container{{Name: "counter"}}},
		Runtime:     runtime,
		Destination: ateompb.Destination_DESTINATION_LOCAL,
	})
	if err != nil {
		t.Fatalf("CheckpointWorkload: %v", err)
	}
	t.Logf("CheckpointWorkload -> manifest=%+v", ckResp.GetManifest())
	if _, _, err := hitCounter(guestIP); err == nil {
		t.Fatalf("workload still reachable after checkpoint; VM should be gone")
	}
	t.Logf("verified: workload unreachable after checkpoint (worker freed)")

	// 5) Restore
	rsResp, err := client.RestoreWorkload(ctx, &ateompb.RestoreWorkloadRequest{
		ActorId: actorID,
		Spec:    &ateompb.WorkloadSpec{Containers: []*ateompb.Container{{Name: "counter"}}},
		Runtime: runtime,
	})
	if err != nil {
		t.Fatalf("RestoreWorkload: %v", err)
	}
	t.Logf("RestoreWorkload -> ready=%v ip=%s", rsResp.GetReady(), rsResp.GetWorkloadIp())
	if err := waitReachable(guestIP, 20*time.Second); err != nil {
		t.Fatalf("workload not reachable after restore: %v", err)
	}

	// 6) verify state continuity: a lost-state restore resets the counter to ~1.
	after, body, err := hitCounter(guestIP)
	if err != nil {
		t.Fatalf("hit after restore: %v", err)
	}
	t.Logf("  %s", body)
	if after <= before {
		t.Fatalf("FAIL: count was %d before, %d after — state NOT preserved (looks reset)", before, after)
	}
	t.Logf("PASS: count continued %d -> %d across checkpoint/restore via the gRPC Ateom contract", before, after)
}

// TestFirecrackerAteomDurable proves the SUSPENDED (durable) tier: checkpoint
// uploads the snapshot to object storage, then a *fresh* fcService (different
// workdir, simulating a different node with no local snapshot) restores it by
// pulling from object storage. Requires a running S3-compatible store; skipped
// unless ATEOM_FC_E2E=1 and ATEOM_FC_DURABLE_URI (+ AWS_* / ATE_STORAGE_BACKEND
// env) are set.
func TestFirecrackerAteomDurable(t *testing.T) {
	if os.Getenv("ATEOM_FC_E2E") != "1" {
		t.Skip("set ATEOM_FC_E2E=1 to run the Firecracker durable test")
	}
	uri := os.Getenv("ATEOM_FC_DURABLE_URI")
	if uri == "" {
		t.Skip("set ATEOM_FC_DURABLE_URI (e.g. s3://bucket/path) + storage env to run the durable test")
	}
	art := os.Getenv("ATEOM_FC_ARTIFACTS")
	if art == "" {
		art = "/root/fc-demo"
	}
	rootfs := filepath.Join(art, "counter-rootfs.ext4")
	_ = exec.Command("pkill", "-9", "firecracker").Run()
	_ = exec.Command("ip", "link", "del", "fc-tap0").Run()
	time.Sleep(500 * time.Millisecond)

	ctx := context.Background()
	const guestIP = "172.16.0.2"
	const actorID = "counter-durable"
	mkRuntime := func() *ateompb.RuntimeConfig {
		return &ateompb.RuntimeConfig{Backend: &ateompb.RuntimeConfig_Microvm{Microvm: &ateompb.MicroVMParams{
			VmmBinaryPath:   filepath.Join(art, "firecracker"),
			KernelImagePath: filepath.Join(art, "vmlinux"),
			RootfsImagePath: rootfs,
			VcpuCount:       1,
			MemSizeMib:      256,
			TapDeviceName:   "fc-tap0",
			GuestMac:        "06:00:AC:10:00:02",
			GuestIp:         guestIP,
			HostTapCidr:     "172.16.0.1/24",
		}}}
	}

	// "node A": run, drive state, checkpoint to DURABLE storage.
	nodeA := &fcService{workDir: t.TempDir()}
	t.Cleanup(func() { nodeA.killFirecracker() })
	if _, err := nodeA.RunWorkload(ctx, &ateompb.RunWorkloadRequest{ActorId: actorID, Runtime: mkRuntime()}); err != nil {
		t.Fatalf("RunWorkload: %v", err)
	}
	if err := waitReachable(guestIP, 20*time.Second); err != nil {
		t.Fatalf("workload never came up: %v", err)
	}
	var before int
	for i := 0; i < 3; i++ {
		n, body, err := hitCounter(guestIP)
		if err != nil {
			t.Fatalf("hit: %v", err)
		}
		t.Logf("  %s", body)
		before = n
	}
	ck, err := nodeA.CheckpointWorkload(ctx, &ateompb.CheckpointWorkloadRequest{
		ActorId:           actorID,
		Runtime:           mkRuntime(),
		SnapshotUriPrefix: uri,
		Destination:       ateompb.Destination_DESTINATION_DURABLE,
	})
	if err != nil {
		t.Fatalf("CheckpointWorkload(DURABLE): %v", err)
	}
	t.Logf("durable checkpoint manifest: %+v", ck.GetManifest())
	if _, _, err := hitCounter(guestIP); err == nil {
		t.Fatalf("workload still reachable after checkpoint")
	}

	// "node B": fresh fcService with a different workdir => no local snapshot,
	// so RestoreWorkload must pull {vmstate,memory} from object storage.
	nodeB := &fcService{workDir: t.TempDir()}
	t.Cleanup(func() {
		nodeB.killFirecracker()
		_ = exec.Command("ip", "link", "del", "fc-tap0").Run()
	})
	if _, err := nodeB.RestoreWorkload(ctx, &ateompb.RestoreWorkloadRequest{
		ActorId:           actorID,
		Runtime:           mkRuntime(),
		SnapshotUriPrefix: uri,
	}); err != nil {
		t.Fatalf("RestoreWorkload(from durable): %v", err)
	}
	if err := waitReachable(guestIP, 20*time.Second); err != nil {
		t.Fatalf("workload not reachable after durable restore: %v", err)
	}
	after, body, err := hitCounter(guestIP)
	if err != nil {
		t.Fatalf("hit after restore: %v", err)
	}
	t.Logf("  %s", body)
	if after <= before {
		t.Fatalf("FAIL: count %d -> %d, durable restore did not preserve state", before, after)
	}
	t.Logf("PASS: count continued %d -> %d across a DURABLE checkpoint + restore on a fresh node (snapshot via %s)", before, after, uri)
}

var countRE = regexp.MustCompile(`preserved memory count: (\d+)`)

func hitCounter(ip string) (int, string, error) {
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get("http://" + ip + ":80/")
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	m := countRE.FindSubmatch(b)
	if m == nil {
		return 0, string(b), nil
	}
	n, _ := strconv.Atoi(string(m[1]))
	return n, string(b), nil
}

func waitReachable(ip string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		c := &http.Client{Timeout: 2 * time.Second}
		resp, err := c.Get("http://" + ip + ":80/")
		if err == nil {
			resp.Body.Close()
			return nil
		}
		last = err
		time.Sleep(300 * time.Millisecond)
	}
	return last
}
