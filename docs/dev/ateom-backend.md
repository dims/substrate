# Writing an ateom sandbox backend

An ateom backend runs an actor's workload and can checkpoint and restore it.
There are two backends today: `cmd/ateom-gvisor` (a full-state sandbox that
preserves both memory and filesystem) and `internal/e2e/fixtures/ateom-fake` (a no-isolation,
test-only backend that preserves only the filesystem). This guide is the
contract a new backend (for example a micro-VM runtime such as Firecracker or
Cloud Hypervisor) must satisfy, and how to prove it works.

The short version: a backend is another implementation of the `Ateom` gRPC
service. It works when, for the things it claims to preserve, nothing above it
can tell which backend is running. A backend declares one capability: whether it
preserves process memory across a checkpoint. The compliance suite then
requires the filesystem and the per-actor identity to survive on every backend,
and requires in-RAM state to survive only on a memory-preserving one. You add a
backend by adding one row to that suite and watching the battery pass.

## 1. What you are implementing

A backend is one process that runs inside each worker pod and serves the `Ateom`
gRPC service on a unix socket. atelet (the node agent) is the only caller. The
service is defined in `internal/proto/ateompb/ateom.proto` and has three calls:

- `RunWorkload`: start a fresh workload from an OCI bundle.
- `CheckpointWorkload`: freeze the running workload and write its state to local
  files, then reset to a clean state.
- `RestoreWorkload`: bring a workload back from a checkpoint.

atelet builds the OCI bundle, fetches the backend's binaries, moves snapshots to
and from object storage, and writes the actor's identity file. The backend's job
is the sandbox itself: run it, checkpoint it, restore it.

## 2. The contracts a backend must satisfy

### 2.1 The gRPC service

Implement all three RPCs with the same request and response messages. Honor the
same behavior gVisor does:

- A backend runs at most one actor at a time. gVisor serializes the three calls
  with a single mutex.
- Validate the path-building fields (`actor_template_namespace`,
  `actor_template_name`, `actor_id`, container names) before using them. The
  socket is unauthenticated, so these fields are a trust boundary.

### 2.2 The snapshot files

`CheckpointWorkload` writes the sandbox state to local files. atelet then moves
those files to and from object storage. Today atelet expects a gVisor-shaped
file set: a required `checkpoint.img`, optional `pages.img` and
`pages_meta.img`, and a `manifest.json`. A backend that produces a different set
of files cannot be stored or restored until atelet's storage mover is
generalized to move whatever files the backend declares. Decide which path you
are taking before you start, because it is the part most likely to break
silently.

The `manifest.json` records which backend binaries wrote the snapshot, by
content hash. `RestoreWorkload` must fetch and use the same binary versions, so
that a restore never runs against a different runtime build than the one that
made the checkpoint.

### 2.3 The backend binaries (SandboxConfig)

A backend's binaries come from a cluster-scoped `SandboxConfig` whose
`sandboxClass` matches the backend (for example `microvm`). Each binary is a
content-addressed `{url, sha256}` pair under an architecture key. atelet fetches
them, checks the hash, and caches them. Ship a default `SandboxConfig` for your
class so pools work without extra configuration, the way `gvisor-default` does.

### 2.4 The worker pod shape

`WorkerPool.spec.sandboxClass` and `spec.sandboxConfigName` select the backend.
**Today the WorkerPool controller ignores these fields when it renders the
worker pod**, so every pod comes out identical (privileged, one hostPath mount,
no `runtimeClassName`, no device mounts). A micro-VM backend needs a different
pod shape, for example a `/dev/kvm` mount and vhost devices. So before you can
test a micro-VM backend, you must first teach the WorkerPool controller to
render the right pod for each `sandboxClass`. That controller change needs its
own test.

### 2.5 The actor runtime environment

This is the contract the actor's own code sees, and it must be identical on
every backend. The full actor contract is in [api-guide.md](../api-guide.md);
the parts a backend is responsible for are:

- The actor serves plain HTTP on TCP port 80, reachable on the sandbox network.
- The file `/run/ate/actor-id` is present, read-only, and holds this actor's id.
- Memory and the writable filesystem survive a checkpoint and restore unchanged.
- The actor receives no shutdown signal on suspend. A suspend is a checkpoint
  that freezes the process in place.

### 2.6 The network

Inbound traffic must reach the actor on port 80 through the sandbox network, so
the router reaches it the same way it reaches a gVisor actor. gVisor sets up a
veth pair, a network namespace, and a DNAT rule to the actor's port 80.

## 3. How to prove it works

### 3.1 The compliance suite

`internal/e2e/suites/compliance` runs one battery of checks against every
registered backend. It deploys a small actor (`internal/e2e/fixtures/compliance`)
on a WorkerPool of your `sandboxClass`, then for each of suspend/resume and
pause/resume it checks:

- an on-disk counter keeps advancing (the filesystem survived) — required of
  every backend,
- the actor still reports its own id from `/run/ate/actor-id` — required of every
  backend,
- the actor serves HTTP on port 80 through the router — required of every backend,
- an in-memory counter keeps advancing (memory survived) — required only of a
  backend that declares `preservesMemory: true`. A filesystem-only backend resets
  it on every resume, and the suite asserts that instead.

To validate a new backend, add a row to the `backends` table in
`compliance_test.go`, declaring whether it preserves memory:

```go
{
    name:              "microvm",
    ateomImage:        "ko://github.com/agent-substrate/substrate/cmd/ateom-microvm",
    sandboxClass:      "microvm",
    sandboxConfigName: "microvm-default",
    preservesMemory:   true,
    skip:              skipUnlessKVM,
},
```

`internal/e2e/fixtures/ateom-fake` is the worked example of a `preservesMemory: false` backend. The
suite then runs the identical battery against your backend. You did not write a
new test; you declared a new backend.

### 3.2 Where it can run

gVisor runs in normal CI because it needs no hardware virtualization. A micro-VM
backend needs nested KVM, which stock GitHub-hosted runners do not have. So gate
its row with a `skip` that returns a reason unless `/dev/kvm` is present, and run
it on a self-hosted runner that has KVM. The gVisor row keeps running in normal
CI.

### 3.3 Failure paths to add

The compliance suite checks the happy path. A production-ready backend also
needs tests that a restore from a missing or corrupt checkpoint fails loudly
rather than starting a half-restored sandbox, and that a crash mid-checkpoint
leaves no orphaned sandbox behind.

## 4. Checklist

- [ ] A new `cmd/ateom-<backend>` that serves the `Ateom` gRPC service.
- [ ] Snapshot files that atelet can store and restore (match the gVisor set, or
      generalize atelet's storage mover).
- [ ] A default `SandboxConfig` for the new `sandboxClass`.
- [ ] WorkerPool controller renders the right pod shape for the class.
- [ ] The actor runtime environment matches section 2.5.
- [ ] A row in the compliance suite, gated on KVM if needed.
- [ ] Failure-path tests for corrupt restore and orphan cleanup.
