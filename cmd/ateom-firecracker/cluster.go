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
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-substrate/substrate/internal/ateompath"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
)

// Cluster mode: driven by the unmodified atelet, which passes no MicroVMParams.
// ateom-firecracker derives everything it needs from the shared hostPath that
// atelet already populates (the extracted OCI rootfs + the runc config.json),
// boots a microVM, and maps its snapshot artifacts onto the gVisor-named files
// (checkpoint.img / pages.img) that atelet uploads/downloads — so no atelet,
// ate-api-server, proto, or CRD changes are required. The kernel, firecracker
// binary, and busybox are baked into the ateom-firecracker image.
const (
	clusterGuestIP  = "172.16.0.2"
	clusterTap      = "fc-tap0"
	clusterHostCIDR = "172.16.0.1/24"
	clusterWorkBase = "/run/ateom-firecracker"
	clusterKernel   = "/opt/ateom-fc/vmlinux"
	clusterFCBin    = "/usr/bin/firecracker"
	clusterBusybox  = "/opt/ateom-fc/busybox"
	clusterMAC      = "06:00:AC:10:00:02"
	clusterBootArgs = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/init"
)

func clusterWorkDir(actorID string) string { return filepath.Join(clusterWorkBase, actorID) }

func firstContainerName(spec *ateompb.WorkloadSpec) string {
	for _, c := range spec.GetContainers() {
		if c.GetName() != "" {
			return c.GetName()
		}
	}
	return ""
}

// parseEntrypoint reads process.args from an OCI runtime config.json (the one
// atelet writes for the container).
func parseEntrypoint(configPath string) ([]string, error) {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", configPath, err)
	}
	var cfg struct {
		Process struct {
			Args []string `json:"args"`
		} `json:"process"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", configPath, err)
	}
	if len(cfg.Process.Args) == 0 {
		return nil, fmt.Errorf("no process.args in %s", configPath)
	}
	return cfg.Process.Args, nil
}

// buildExt4 turns an extracted OCI rootfs directory into a bootable ext4 image:
// it copies the rootfs, injects a static busybox + an /init that brings up
// networking and execs the container entrypoint, and runs mkfs.ext4 -d.
func buildExt4(rootfsDir, ext4Path string, args []string) error {
	stage := ext4Path + ".stage"
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	if err := runCmd("cp", "-a", rootfsDir, stage); err != nil {
		return fmt.Errorf("staging rootfs: %w", err)
	}
	for _, d := range []string{"bin", "proc", "sys", "dev"} {
		_ = os.MkdirAll(filepath.Join(stage, d), 0o755)
	}
	if err := runCmd("cp", clusterBusybox, filepath.Join(stage, "bin", "busybox")); err != nil {
		return fmt.Errorf("copying busybox: %w", err)
	}
	for _, app := range []string{"sh", "mount", "ifconfig", "route", "ip", "mkdir", "ln", "cat", "ls"} {
		_ = os.Symlink("busybox", filepath.Join(stage, "bin", app))
	}
	init := "#!/bin/sh\n" +
		"export PATH=/bin:/sbin:/ko-app:/usr/bin:/usr/sbin\n" +
		"mount -t proc proc /proc\n" +
		"mount -t sysfs sysfs /sys\n" +
		"mount -t devtmpfs devtmpfs /dev 2>/dev/null\n" +
		"ifconfig lo 127.0.0.1 up\n" +
		"ifconfig eth0 " + clusterGuestIP + " netmask 255.255.255.0 up\n" +
		"route add default gw 172.16.0.1 2>/dev/null\n" +
		"echo \"ateom-fc init: launching " + strings.Join(args, " ") + "\"\n" +
		"exec " + strings.Join(args, " ") + "\n"
	if err := os.WriteFile(filepath.Join(stage, "init"), []byte(init), 0o755); err != nil {
		return err
	}
	_ = os.Remove(ext4Path)
	if err := runCmd("mkfs.ext4", "-q", "-F", "-L", "rootfs", "-d", stage, ext4Path, "256M"); err != nil {
		return fmt.Errorf("mkfs.ext4: %w", err)
	}
	_ = os.RemoveAll(stage)
	return nil
}

func clusterSetupTap() error {
	_ = runCmd("ip", "link", "del", clusterTap)
	if err := runCmd("ip", "tuntap", "add", "dev", clusterTap, "mode", "tap"); err != nil {
		return err
	}
	if err := runCmd("ip", "addr", "add", clusterHostCIDR, "dev", clusterTap); err != nil {
		return err
	}
	return runCmd("ip", "link", "set", clusterTap, "up")
}

// clusterSetupDNAT makes the guest reachable at the worker pod's IP:80 (which is
// where atenet/kube-proxy route), by DNAT'ing port 80 to the guest and
// MASQUERADE'ing toward the tap so guest replies return through the pod.
func clusterSetupDNAT() {
	_ = runCmd("sysctl", "-w", "net.ipv4.ip_forward=1")
	addRule := func(table string, rule ...string) {
		check := append([]string{"-t", table, "-C"}, rule...)
		if runCmd("iptables", check...) != nil {
			add := append([]string{"-t", table, "-A"}, rule...)
			_ = runCmd("iptables", add...)
		}
	}
	addRule("nat", "PREROUTING", "-p", "tcp", "--dport", "80", "-j", "DNAT", "--to-destination", clusterGuestIP+":80")
	addRule("nat", "POSTROUTING", "-o", clusterTap, "-j", "MASQUERADE")
	// allow forwarding both ways
	if runCmd("iptables", "-C", "FORWARD", "-j", "ACCEPT") != nil {
		_ = runCmd("iptables", "-A", "FORWARD", "-j", "ACCEPT")
	}
}

func (s *fcService) bootCluster(ctx context.Context, ext4 string, sockName, dir string) error {
	if err := clusterSetupTap(); err != nil {
		return fmt.Errorf("tap: %w", err)
	}
	if err := s.startFirecracker(ctx, clusterFCBin, filepath.Join(dir, sockName), dir); err != nil {
		return err
	}
	return nil
}

func (s *fcService) runCluster(ctx context.Context, req *ateompb.RunWorkloadRequest) (*ateompb.RunWorkloadResponse, error) {
	ns, tmpl, id := req.GetActorTemplateNamespace(), req.GetActorTemplateName(), req.GetActorId()
	container := firstContainerName(req.GetSpec())
	if container == "" {
		return nil, fmt.Errorf("runCluster: no container in spec")
	}
	bundle := ateompath.OCIBundlePath(ns, tmpl, id, container)
	rootfsDir := filepath.Join(bundle, "rootfs")
	args, err := parseEntrypoint(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, err
	}
	dir := clusterWorkDir(id)
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ext4 := filepath.Join(dir, "rootfs.ext4")
	slog.InfoContext(ctx, "cluster: building rootfs", slog.String("from", rootfsDir), slog.Any("entrypoint", args))
	if err := buildExt4(rootfsDir, ext4, args); err != nil {
		return nil, err
	}
	if err := s.bootCluster(ctx, ext4, "fc.sock", dir); err != nil {
		return nil, err
	}
	steps := [][2]string{
		{"/boot-source", fmt.Sprintf(`{"kernel_image_path":%q,"boot_args":%q}`, clusterKernel, clusterBootArgs)},
		{"/drives/rootfs", fmt.Sprintf(`{"drive_id":"rootfs","path_on_host":%q,"is_root_device":true,"is_read_only":false}`, ext4)},
		{"/machine-config", `{"vcpu_count":1,"mem_size_mib":256}`},
		{"/network-interfaces/eth0", fmt.Sprintf(`{"iface_id":"eth0","host_dev_name":%q,"guest_mac":%q}`, clusterTap, clusterMAC)},
	}
	for _, st := range steps {
		if err := s.api(ctx, "PUT", st[0], st[1]); err != nil {
			return nil, err
		}
	}
	if err := s.api(ctx, "PUT", "/actions", `{"action_type":"InstanceStart"}`); err != nil {
		return nil, err
	}
	clusterSetupDNAT()
	slog.InfoContext(ctx, "cluster: microVM started", slog.String("actor", id), slog.String("guest", clusterGuestIP))
	return &ateompb.RunWorkloadResponse{Ready: true, WorkloadIp: clusterGuestIP}, nil
}

func (s *fcService) checkpointCluster(ctx context.Context, req *ateompb.CheckpointWorkloadRequest) (*ateompb.CheckpointWorkloadResponse, error) {
	ns, tmpl, id := req.GetActorTemplateNamespace(), req.GetActorTemplateName(), req.GetActorId()
	dir := clusterWorkDir(id)
	snapDir := filepath.Join(dir, "snap")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return nil, err
	}
	vmstate := filepath.Join(snapDir, "vmstate")
	memory := filepath.Join(snapDir, "memory")
	ext4 := filepath.Join(dir, "rootfs.ext4")

	if err := s.api(ctx, "PATCH", "/vm", `{"state":"Paused"}`); err != nil {
		return nil, fmt.Errorf("pause: %w", err)
	}
	if err := s.api(ctx, "PUT", "/snapshot/create",
		fmt.Sprintf(`{"snapshot_type":"Full","snapshot_path":%q,"mem_file_path":%q}`, vmstate, memory)); err != nil {
		return nil, fmt.Errorf("snapshot/create: %w", err)
	}
	s.killFirecracker()

	// Map firecracker's snapshot onto the files atelet uploads: pack the VM
	// state + the rootfs disk into checkpoint.img (a tar), and the memory file
	// into pages.img. atelet zstd-uploads both; pages_meta.img must exist
	// because atelet's restore downloads all three unconditionally.
	ckDir := ateompath.CheckpointDir(ns, tmpl, id)
	if err := os.MkdirAll(ckDir, 0o700); err != nil {
		return nil, err
	}
	if err := tarPack(ateompath.CheckpointImgPath(ns, tmpl, id), map[string]string{"vmstate": vmstate, "rootfs.ext4": ext4}); err != nil {
		return nil, fmt.Errorf("pack checkpoint.img: %w", err)
	}
	if err := runCmd("cp", memory, ateompath.PagesImgPath(ns, tmpl, id)); err != nil {
		return nil, fmt.Errorf("stage pages.img: %w", err)
	}
	if err := os.WriteFile(ateompath.PagesMetaImgPath(ns, tmpl, id), []byte("firecracker\n"), 0o644); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "cluster: checkpointed", slog.String("actor", id))
	return &ateompb.CheckpointWorkloadResponse{Manifest: &ateompb.SnapshotManifest{
		ArtifactNames: []string{"checkpoint.img(vmstate+rootfs)", "pages.img(memory)"},
		Backend:       "firecracker",
		VmmVersion:    s.vmmVersion(clusterFCBin),
	}}, nil
}

func (s *fcService) restoreCluster(ctx context.Context, req *ateompb.RestoreWorkloadRequest) (*ateompb.RestoreWorkloadResponse, error) {
	ns, tmpl, id := req.GetActorTemplateNamespace(), req.GetActorTemplateName(), req.GetActorId()
	dir := clusterWorkDir(id)
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	snapDir := filepath.Join(dir, "snap")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		return nil, err
	}
	vmstate := filepath.Join(snapDir, "vmstate")
	memory := filepath.Join(snapDir, "memory")
	ext4 := filepath.Join(dir, "rootfs.ext4")

	// atelet has already downloaded checkpoint.img + pages.img into the actor's
	// checkpoint dir. Unpack the VM state + rootfs disk, and stage the memory.
	if err := tarUnpack(ateompath.CheckpointImgPath(ns, tmpl, id), map[string]string{"vmstate": vmstate, "rootfs.ext4": ext4}); err != nil {
		return nil, fmt.Errorf("unpack checkpoint.img: %w", err)
	}
	if err := runCmd("cp", ateompath.PagesImgPath(ns, tmpl, id), memory); err != nil {
		return nil, fmt.Errorf("stage memory: %w", err)
	}
	if err := s.bootCluster(ctx, ext4, "fc-restore.sock", dir); err != nil {
		return nil, err
	}
	if err := s.api(ctx, "PUT", "/snapshot/load",
		fmt.Sprintf(`{"snapshot_path":%q,"mem_backend":{"backend_type":"File","backend_path":%q},"resume_vm":true}`, vmstate, memory)); err != nil {
		return nil, fmt.Errorf("snapshot/load: %w", err)
	}
	clusterSetupDNAT()
	slog.InfoContext(ctx, "cluster: restored", slog.String("actor", id), slog.String("guest", clusterGuestIP))
	return &ateompb.RestoreWorkloadResponse{Ready: true, WorkloadIp: clusterGuestIP}, nil
}

func tarPack(tarPath string, entries map[string]string) (err error) {
	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	tw := tar.NewWriter(f)
	for name, src := range entries {
		st, serr := os.Stat(src)
		if serr != nil {
			return serr
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: st.Size(), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		sf, oerr := os.Open(src)
		if oerr != nil {
			return oerr
		}
		_, cerr := io.Copy(tw, sf)
		sf.Close()
		if cerr != nil {
			return cerr
		}
	}
	return tw.Close()
}

func tarUnpack(tarPath string, dest map[string]string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		dst, ok := dest[hdr.Name]
		if !ok {
			continue
		}
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		out.Close()
	}
	return nil
}
