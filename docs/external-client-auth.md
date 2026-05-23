# Connecting to `ate-api-server` from outside the cluster

The `Control` service that `ate-api-server` exposes is the right
entry point for non-Go consumers (a Rust gRPC client like
[`openshell-driver-substrate`](https://github.com/NVIDIA/OpenShell);
a `grpcurl` debug call; any CI smoke test) -- but the security
configuration is not obvious. This page documents the moving parts so
you do not have to reverse-engineer the server's flags the way the
first OpenShell driver implementation did.

## What the server requires

`ate-api-server` runs with these auth-related flags:

```text
--grpc-listen-addr=0.0.0.0:443
--grpc-server-cred-bundle=/run/servicedns.podcert.ate.dev/credential-bundle.pem
--client-jwt-issuer=@env       # set by Helm to the K8s API server's issuer
--client-jwt-audience=api.ate-system.svc
```

In words:

- The server's TLS cert is a [Kubernetes Pod
  Certificate](https://kep.k8s.io/4633) signed by the
  `servicedns.podcert.ate.dev/identity` signer. The cert's only SAN
  is `api.ate-system.svc`.
- Clients must present a Bearer JWT in the `Authorization` gRPC
  metadata header. The JWT is validated against the K8s API server's
  issuer; the audience must be exactly `api.ate-system.svc`.

There is no plaintext fallback and no anonymous path.

## What you need to assemble

| Material | Where it comes from |
|---|---|
| The trust bundle for the server cert | The `ClusterTrustBundle` named `servicedns.podcert.ate.dev:identity:primary-bundle` |
| A Bearer JWT with the right audience | `kubectl create token <some-sa> --audience=api.ate-system.svc` |
| A network path to the server | Either run inside the cluster, or `kubectl port-forward` |

## Recipe (port-forward + SA token)

```sh
# 1. Mint a JWT for any ServiceAccount that the cluster's RBAC permits
#    to call ateapi.Control. `ate-controller` is a safe default for
#    read-only operations.
kubectl -n ate-system create token ate-controller \
  --audience=api.ate-system.svc \
  > /tmp/ate-bearer.token

# 2. Extract the ClusterTrustBundle that signs the api-server's cert.
kubectl get clustertrustbundles \
  servicedns.podcert.ate.dev:identity:primary-bundle \
  -o jsonpath='{.spec.trustBundle}' \
  > /tmp/ate-servicedns-ca.pem

# 3. Port-forward ate-api-server (it listens on :443 inside the pod).
API_POD=$(kubectl -n ate-system get pods \
  -l app=ate-api-server -o name | head -1)
kubectl -n ate-system port-forward "$API_POD" 18443:443 &

# 4. (Optional) Sanity-check the TLS chain.
echo "" | openssl s_client \
  -connect 127.0.0.1:18443 \
  -servername api.ate-system.svc \
  -CAfile /tmp/ate-servicedns-ca.pem \
  -verify_return_error 2>&1 | grep "Verify return code"
# expect: Verify return code: 0 (ok)

# 5. Use the materials from your client. Example with `grpcurl`:
grpcurl \
  -authority api.ate-system.svc \
  -cacert /tmp/ate-servicedns-ca.pem \
  -H "authorization: Bearer $(cat /tmp/ate-bearer.token)" \
  127.0.0.1:18443 \
  ateapi.Control/ListActors
```

## Sharp edges

- **Service-account tokens expire.** `kubectl create token` defaults
  to a one-hour TTL. Re-mint as needed. Consumers that need long-running
  connections should read the token from a file path that the platform
  rotates (Kubernetes projected SA token volumes are the standard
  Linux pattern).
- **The audience MUST be `api.ate-system.svc`.** Other values
  (e.g. the default API audience) are rejected with `Unauthenticated`.
- **The TLS SAN is `api.ate-system.svc`.** When port-forwarding, set
  the gRPC `:authority` header (or your TLS client's `domain_name`)
  to that string -- the cert has no other SAN.
- **The ClusterTrustBundle, not the leaf cert, is the right trust
  anchor.** Pinning the leaf cert works once but breaks every time
  the pod's cert rotates (Pod Certificates default to a 24 h
  lifetime).
- **No anonymous read.** Even `GetCapabilities`-style endpoints need
  a valid Bearer token.

## RBAC checklist for a new ServiceAccount

If you want a dedicated ServiceAccount for your client (recommended
over reusing `ate-controller`), it needs at minimum:

- `audiences: ["api.ate-system.svc"]` allowed on its token.
  This is the default for any namespace's projected token API; no
  extra configuration is required unless the cluster has restricted
  audiences.
- ClusterRole bindings for whichever `Control` RPCs the client uses.
  At time of writing, `ate-api-server` enforces audience-only on
  authentication and does not check RBAC on individual Control RPCs
  beyond authentication; that is expected to change as the project
  matures.

## See also

- `proto/ateapipb/ateapi.proto` -- the `Control` service definition.
- [`openshell-driver-substrate`](https://github.com/NVIDIA/OpenShell)
  (NVIDIA/OpenShell, branch `chore/gvisor-degraded-netns`,
  `crates/openshell-driver-substrate/`) -- a working Rust client that
  uses this recipe; see `tests/live.rs` for the integration-test
  shape.
