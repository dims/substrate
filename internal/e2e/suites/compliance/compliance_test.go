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

// Package compliance holds the sandbox-backend compliance suite. It runs the
// same battery of checks against every registered ateom backend, so a new
// backend (for example a micro-VM runtime) is validated by adding one table row
// rather than writing a new suite. See docs/dev/ateom-backend.md for the
// contract a backend must satisfy.
package compliance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/internal/e2e"
	"github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const templateName = "compliance"

// backend is one sandbox runtime to validate. Adding a new ateom backend is a
// new row here; the suite runs the identical battery against it.
type backend struct {
	name string
	// ateomImage is the ko:// import reference of the backend's ateom binary,
	// which ko builds and resolves at deploy time.
	ateomImage   string
	sandboxClass string
	// sandboxConfigName selects the SandboxConfig that supplies the backend's
	// binaries. Empty uses the cluster-default SandboxConfig for the class.
	sandboxConfigName string
	// preservesMemory is true for a full-state backend that restores process
	// memory across a checkpoint (e.g. gVisor). If false, the backend preserves
	// only the filesystem and an actor's in-RAM state resets on every resume.
	// The battery still requires the filesystem and the identity to survive on
	// every backend; only the in-RAM check is conditioned on this.
	preservesMemory bool
	// skip returns a non-empty reason to skip this backend (for example, a
	// micro-VM backend on a runner without nested KVM), or "" to run it.
	skip func(t *testing.T) string
}

var backends = []backend{
	{
		name:            "gvisor",
		ateomImage:      "ko://github.com/agent-substrate/substrate/cmd/ateom-gvisor",
		sandboxClass:    "gvisor",
		preservesMemory: true,
		// An empty sandboxConfigName resolves to the cluster-default gvisor
		// SandboxConfig (gvisor-default), installed with the platform.
	},
	{
		// ateom-fake is a no-isolation, filesystem-only backend: it runs the
		// actor's binary directly with chroot (no sandbox) and checkpoints only
		// the writable filesystem, so in-RAM state resets on every resume. It
		// needs no gVisor or KVM, so it runs anywhere the suite does, and it
		// exercises the capability model (preservesMemory=false). sandboxClass is
		// "gvisor" only to satisfy the WorkerPool enum; the backend is chosen by
		// ateomImage, and the runsc atelet fetches is ignored by this backend.
		name:            "fake",
		ateomImage:      "ko://github.com/agent-substrate/substrate/internal/e2e/fixtures/ateom-fake",
		sandboxClass:    "gvisor",
		preservesMemory: false,
	},
	// A micro-VM backend goes here once cmd/ateom-microvm exists. It needs nested
	// KVM, which stock CI runners do not provide, so gate it with skip:
	//
	//	{
	//		name:              "microvm",
	//		ateomImage:        "ko://github.com/agent-substrate/substrate/cmd/ateom-microvm",
	//		sandboxClass:      "microvm",
	//		sandboxConfigName: "microvm-default",
	//		preservesMemory:   true,
	//		skip:              skipUnlessKVM,
	//	},
}

// TestSandboxCompliance runs the same suspend/resume and pause/resume battery
// against every registered backend. A backend works when it preserves in-memory
// state, on-disk state, and per-actor identity across a checkpoint and restore,
// and serves HTTP on port 80 through the router.
func TestSandboxCompliance(t *testing.T) {
	env, err := e2e.CheckEnv("BUCKET_NAME", "KO_DOCKER_REPO")
	if err != nil {
		t.Fatalf("CheckEnv failed: %v", err)
	}
	ctx := context.Background()
	clients := e2e.GetClients()

	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			if b.skip != nil {
				if reason := b.skip(t); reason != "" {
					t.Skip(reason)
				}
			}
			runCompliance(t, ctx, clients, b, env["BUCKET_NAME"])
		})
	}
}

func runCompliance(t *testing.T, ctx context.Context, clients *e2e.Clients, b backend, bucket string) {
	ns := "ate-e2e-compliance-" + b.name
	deployBackend(t, b, ns, bucket)
	waitForGolden(t, ctx, clients, ns)

	id := "comp-" + b.name
	if _, err := clients.SubstrateAPI.CreateActor(ctx, &ateapipb.CreateActorRequest{
		ActorId:                id,
		ActorTemplateNamespace: ns,
		ActorTemplateName:      templateName,
	}); err != nil {
		t.Fatalf("CreateActor %q: %v", id, err)
	}
	t.Cleanup(func() {
		// DeleteActor requires the actor to be suspended first.
		_, _ = clients.SubstrateAPI.SuspendActor(ctx, &ateapipb.SuspendActorRequest{ActorId: id})
		_, _ = clients.SubstrateAPI.DeleteActor(ctx, &ateapipb.DeleteActorRequest{ActorId: id})
	})

	rc, err := e2e.NewRouterClient(ctx)
	if err != nil {
		t.Fatalf("NewRouterClient: %v", err)
	}
	defer rc.Close()

	// The on-disk counter and the identity must survive every restore on every
	// backend, so /fs advances 1, 2, 3 and /whoami stays the actor's own id. The
	// in-RAM counter only advances on a memory-preserving backend; on a
	// filesystem-only backend each resume is a fresh process, so it resets to 1.
	wantMem := func(stage int) int {
		if b.preservesMemory {
			return stage
		}
		return 1
	}

	// First activation restores from the golden snapshot. The counters start at
	// the golden's value (0, since the golden takes no traffic), so the first
	// call to each returns 1.
	resumeAndWait(t, ctx, clients, id)
	assertIdentity(t, ctx, rc, id)
	assertCount(t, ctx, rc, id, "/mem", wantMem(1), "first resume")
	assertCount(t, ctx, rc, id, "/fs", 1, "first resume")

	// EXTERNAL snapshot round-trip: suspend writes the snapshot to object
	// storage, resume reads it back. The filesystem must advance to 2; memory
	// advances to 2 only on a memory-preserving backend.
	suspendAndWait(t, ctx, clients, id)
	resumeAndWait(t, ctx, clients, id)
	assertIdentity(t, ctx, rc, id)
	assertCount(t, ctx, rc, id, "/mem", wantMem(2), "suspend/resume")
	assertCount(t, ctx, rc, id, "/fs", 2, "suspend/resume")

	// LOCAL snapshot round-trip: pause keeps the snapshot on the node.
	pauseAndWait(t, ctx, clients, id)
	resumeAndWait(t, ctx, clients, id)
	assertIdentity(t, ctx, rc, id)
	assertCount(t, ctx, rc, id, "/mem", wantMem(3), "pause/resume")
	assertCount(t, ctx, rc, id, "/fs", 3, "pause/resume")
}

// deployBackend renders the manifest template for one backend and applies it
// with the repo's pinned ko, which builds and pushes the actor image and
// resolves the ko:// references. It mirrors the identity suite's deploy: ko
// resolves .ko.yaml from its working directory, so KO_CONFIG_PATH points it at
// the repo root, and ko apply forwards args after `--` to kubectl.
func deployBackend(t *testing.T, b backend, ns, bucket string) {
	t.Helper()
	root, err := e2e.FindRepoRoot()
	if err != nil {
		t.Fatalf("FindRepoRoot: %v", err)
	}
	tmpl, err := os.ReadFile(filepath.Join(root, "internal/e2e/fixtures/compliance/compliance.yaml.tmpl"))
	if err != nil {
		t.Fatalf("reading compliance manifest template: %v", err)
	}

	sandboxConfigLine := ""
	if b.sandboxConfigName != "" {
		sandboxConfigLine = "  sandboxConfigName: " + b.sandboxConfigName
	}
	rendered := strings.NewReplacer(
		"${NAMESPACE}", ns,
		"${POOL_LABEL}", "compliance-"+b.name,
		"${ATEOM_IMAGE}", b.ateomImage,
		"${SANDBOX_CLASS}", b.sandboxClass,
		"${SANDBOX_CONFIG_LINE}", sandboxConfigLine,
		"${BUCKET_NAME}", bucket,
	).Replace(string(tmpl))

	manifest := filepath.Join(t.TempDir(), "compliance.yaml")
	if err := os.WriteFile(manifest, []byte(rendered), 0o644); err != nil {
		t.Fatalf("writing rendered manifest: %v", err)
	}

	applyArgs := []string{"ko", "apply", "-f", manifest}
	if e2e.KubeContext != "" {
		applyArgs = append(applyArgs, "--", "--context="+e2e.KubeContext)
	}
	e2e.RunCmdWithEnv(t, []string{"KO_CONFIG_PATH=" + root}, filepath.Join(root, "hack/run-tool.sh"), applyArgs...)

	t.Cleanup(func() {
		delArgs := []string{"delete", "--ignore-not-found", "-f", manifest}
		if e2e.KubeContext != "" {
			delArgs = append([]string{"--context=" + e2e.KubeContext}, delArgs...)
		}
		e2e.RunCmd(t, "kubectl", delArgs...)
	})
}

func waitForGolden(t *testing.T, ctx context.Context, clients *e2e.Clients, ns string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		at, err := clients.SubstrateK8s.ApiV1alpha1().ActorTemplates(ns).Get(ctx, templateName, metav1.GetOptions{})
		if err == nil {
			switch at.Status.Phase {
			case v1alpha1.PhaseReady:
				t.Logf("compliance ActorTemplate ready in %s, golden=%s", ns, at.Status.GoldenActorID)
				return
			case v1alpha1.PhaseFailed:
				t.Fatalf("compliance ActorTemplate in %s entered PhaseFailed", ns)
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for compliance ActorTemplate in %s to be Ready", ns)
}

func resumeAndWait(t *testing.T, ctx context.Context, clients *e2e.Clients, id string) {
	t.Helper()
	if _, err := clients.SubstrateAPI.ResumeActor(ctx, &ateapipb.ResumeActorRequest{ActorId: id}); err != nil {
		t.Fatalf("ResumeActor %q: %v", id, err)
	}
	waitForActorStatus(t, ctx, clients, id, ateapipb.Actor_STATUS_RUNNING)
}

func suspendAndWait(t *testing.T, ctx context.Context, clients *e2e.Clients, id string) {
	t.Helper()
	if _, err := clients.SubstrateAPI.SuspendActor(ctx, &ateapipb.SuspendActorRequest{ActorId: id}); err != nil {
		t.Fatalf("SuspendActor %q: %v", id, err)
	}
	waitForActorStatus(t, ctx, clients, id, ateapipb.Actor_STATUS_SUSPENDED)
}

func pauseAndWait(t *testing.T, ctx context.Context, clients *e2e.Clients, id string) {
	t.Helper()
	if _, err := clients.SubstrateAPI.PauseActor(ctx, &ateapipb.PauseActorRequest{ActorId: id}); err != nil {
		t.Fatalf("PauseActor %q: %v", id, err)
	}
	waitForActorStatus(t, ctx, clients, id, ateapipb.Actor_STATUS_PAUSED)
}

func waitForActorStatus(t *testing.T, ctx context.Context, clients *e2e.Clients, id string, want ateapipb.Actor_Status) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := clients.SubstrateAPI.GetActor(ctx, &ateapipb.GetActorRequest{ActorId: id})
		if err == nil && resp.GetActor().GetStatus() == want {
			return
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("timed out waiting for actor %q to reach %v", id, want)
}

type countResponse struct {
	Value int `json:"value"`
}

type whoamiResponse struct {
	File  string `json:"file"`
	Error string `json:"error"`
}

// assertCount calls path through the router and checks the returned counter. A
// value that does not match want means the backend lost in-memory or on-disk
// state across the restore named by stage.
func assertCount(t *testing.T, ctx context.Context, rc *e2e.RouterClient, id, path string, want int, stage string) {
	t.Helper()
	resp, err := rc.Get(ctx, id, path)
	if err != nil {
		t.Fatalf("GET %s for %q (%s): %v", path, id, stage, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s for %q (%s): status %d, body %q", path, id, stage, resp.StatusCode, body)
	}
	var c countResponse
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatalf("decoding %s for %q (%s): %v", path, id, stage, err)
	}
	if c.Value != want {
		t.Errorf("%s after %s = %d, want %d (state not preserved across restore)", path, stage, c.Value, want)
	}
}

// assertIdentity checks the actor reports its own id from /run/ate/actor-id, and
// that the value is stable across restores.
func assertIdentity(t *testing.T, ctx context.Context, rc *e2e.RouterClient, id string) {
	t.Helper()
	resp, err := rc.Get(ctx, id, "/whoami")
	if err != nil {
		t.Fatalf("GET /whoami for %q: %v", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /whoami for %q: status %d, body %q", id, resp.StatusCode, body)
	}
	var who whoamiResponse
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
		t.Fatalf("decoding /whoami for %q: %v", id, err)
	}
	if who.File != id {
		t.Errorf("/whoami for %q = %q, want %q (per-actor identity wrong; probe read error: %q)", id, who.File, id, who.Error)
	}
}
