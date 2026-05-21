# Counter Demo

This directory contains a demo of a stateful counter application running on Agent Substrate.

It deploys a simple Go HTTP server (`counter.go`) that increments a counter on every request and preserves state across suspends and resumes.

## Prerequisites

- A Kubernetes cluster with Agent Substrate installed.
- `ko` installed for building images.
- A snapshot store appropriate to the cluster:
  - On GKE, a GCS bucket whose name is supplied via the `BUCKET_NAME` environment variable.
  - On a local `kind` cluster, the in-cluster `rustfs` S3-compatible store provisioned by the kind overlay. `BUCKET_NAME` defaults to `ate-snapshots` and does not need to be set.

Two install scripts are available and accept the same flag set:

| Target | Script |
|---|---|
| GKE | `./hack/install-ate.sh` |
| Local `kind` | `./hack/install-ate-kind.sh` |

The remainder of this guide uses `./hack/install-ate.sh`. For a `kind` cluster, substitute `./hack/install-ate-kind.sh` in every command below. The end-to-end `kind` bring-up is described in the [Quickstart in the top-level README](../../README.md#quickstart-development).

## How to Run on Agent Substrate

### 1. Build and Deploy

> [!NOTE]
> Do not manually edit `demos/counter/counter.yaml.tmpl`. The installation script automatically injects your `${BUCKET_NAME}` environment variable during deployment.

Use the core installation script to build the image and apply the resolved manifests to your cluster:

```bash
./hack/install-ate.sh --deploy-demo-counter
```

This command will:
- Build the counter server image using `ko`.
- Create the `ate-demo-counter` namespace.
- Create the `WorkerPool` and `ActorTemplate`.

Wait until the template is ready:
```bash
kubectl wait --for=condition=Ready actortemplate/counter -n ate-demo-counter --timeout=5m
```

### 2. Create a Counter Actor

Use `kubectl ate` to create an instance of the counter actor with a chosen ID (e.g., `my-counter-1`):

```bash
# Install the CLI as a kubectl plugin if not already installed
go install ./cmd/kubectl-ate

# Create the actor using the counter template.
kubectl ate create actor my-counter-1 --template ate-demo-counter/counter
```

### 3. Port-Forward Services

To interact with the router locally:

```bash
# Port-forward the Atenet Router
kubectl port-forward -n ate-system svc/atenet-router 8000:80
```

## How to Use

When you send an HTTP request through the router, Substrate automatically detects the session, activates (resumes) the actor onto an available worker pod, and proxies the traffic.

1. Send an HTTP POST request to increment the counter:
```bash
curl -X POST -H "Host: my-counter-1.actors.resources.substrate.ate.dev" http://localhost:8000
```

2. Verify that the actor is now in a `RUNNING` state and assigned to a worker pod:
```bash
kubectl ate get actor my-counter-1
```

3. When finished, you can manually suspend the actor back to snapshot storage:
```bash
kubectl ate suspend actor my-counter-1
```

4. To permanently delete the suspended actor:
```bash
kubectl ate delete actor my-counter-1
```

## How to Uninstall

To remove the counter demo resources from your cluster, run:

```bash
./hack/install-ate.sh --delete-demo-counter
```
