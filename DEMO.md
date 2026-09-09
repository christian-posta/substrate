# Running the Identity + Egress Demo (Envoy or agentgateway)

> Companion to [EGRESS.md](./EGRESS.md) (architecture, as-built) and
> [IDENTITY.md](./IDENTITY.md) (design option space). This is the **operational runbook**: how
> to stand the demo up on a local kind cluster, drive it by hand, and what to check. It
> supplements the upstream walkthrough in `demos/egress/README.md` with the **agentgateway
> dataplane variant** and the upgrade gotchas we've hit on this machine.
>
> The repository's local-development default is the kind cluster `kind-substrate`. Override it
> with `KIND_CLUSTER_NAME` when a second local cluster is needed.
>
> Last verified 2026-09-08 against `main` at `353c21f2` on `kind-substrate`, using
> `--atenet-router=agentgateway`. The locally built binaries report `060b0d88-dirty`; the only
> later upstream commit in that range changes documentation, not image inputs.

## What the demo shows (and doesn't)

**Shows:** transparent actor egress (nftables REDIRECT → atunnel → mTLS CONNECT to the gateway)
and **actor identity**. Both dataplanes require a certificate chaining to the actor-identity CA.
The default Envoy path additionally parses the `ActorIdentity` extension and requires a matching,
RUNNING actor on every CONNECT; agentgateway applies its native `substrateEgress` check on the
inner HTTP route. See EGRESS.md for the opaque TLS/TCP distinction.

**Does not show:** enforcement of the control-plane `EgressPolicy` API or Substrate-provided
credential injection. TLS interception exists behind `--experimental-use-sdsmint`, and an
Envoy-only hook can call an operator-supplied external processor, but neither is enabled here.

## Prerequisites

- `kind` ≥ v0.31.0 (older node images lack the `PodCertificateRequest` feature gate — kubelet
  refuses to start), `kubectl` ≥ v1.23, `ko`, `docker`, `go`.
- Local registry on :5001 (created by `hack/create-kind-cluster.sh`).
- `kubectl-ate`: `go install ./cmd/kubectl-ate`. It takes `--context kind-substrate` directly.

## Fresh install (from nothing)

```bash
hack/create-kind-cluster.sh                       # kind-substrate + local registry :5001

# Pick ONE dataplane. Envoy is the default; agentgateway serves BOTH ingress and egress:
hack/install-ate-kind.sh --deploy-ate-system                                # Envoy
hack/install-ate-kind.sh --deploy-ate-system --atenet-router=agentgateway   # agentgateway

hack/install-ate-kind.sh --deploy-demo-egress     # egress demo template + golden snapshot
go install ./cmd/kubectl-ate
```

Gotcha: `--deploy-ate-system`'s rollout wait can time out on cold image pulls while pods are
still ContainerCreating — that is not a failure; wait for pods and re-run or continue.

## Scripted verification (easiest)

```bash
hack/verify-egress-demo.sh
```

This lightweight smoke test creates an actor, drives an external HTTP fetch through it, and checks
the selected dataplane's access log for the CONNECT. Exit 0 plus `== PASS ==` is the win condition.

The richer upstream script is `demos/egress/test-egress.sh` (in-cluster target, positive +
negative identity tests, `--cleanup`).

## Manual walkthrough

Upstream's step-by-step (in-cluster whoami target, create/resume actor, curl through the
router, inspect gateway logs, negative test from a non-actor pod) lives in
**`demos/egress/README.md` → "Manual walkthrough"** — it applies unchanged on either
dataplane. Condensed:

```bash
# create + resume the actor
kubectl ate --context kind-substrate create atespace demo
kubectl ate --context kind-substrate create actor egress-demo -a demo --template ate-demo-egress/egress
kubectl ate --context kind-substrate resume actor egress-demo -a demo    # wait: ACTOR_STATE_RUNNING

# drive egress through the actor (any external URL; the actor fetches it)
kubectl --context kind-substrate -n ate-system port-forward svc/atenet-router 18000:80 &
curl -s -X POST http://localhost:18000/ \
  -H "Host: egress-demo.demo.actors.resources.substrate.ate.dev" \
  -d '{"url":"http://example.com/"}'          # expect HTTP 200 + page body

# Envoy: proof of the authenticated actor identity on CONNECT
kubectl --context kind-substrate -n ate-system logs deploy/atenet-egress -c ext-proc --tail=20 \
  | grep 'egress identity authenticated'
```

What to look for in the identity log line: `atespace`, `actor`, **`actorUid`** (the
incarnation binding — delete/recreate the actor and it changes), and `destination` as resolved
`IP:port` (DNS happens in the clear before the tunnel; the gateway never sees hostnames).

### agentgateway-specific checks

```bash
# agentgateway is the sole egress dataplane container; it has no atenet ext_proc sidecar
kubectl --context kind-substrate -n ate-system get pod -l app=atenet-egress \
  -o jsonpath='{.items[0].spec.containers[*].name}'

# Its structured access log records the CONNECT authority.
kubectl --context kind-substrate -n ate-system logs deploy/atenet-egress -c agentgateway \
  | grep 'substrate.connect.authority'
```

## Switching dataplanes on a running cluster

Re-run `--deploy-ate-system` with the other `--atenet-router=` value; it rewrites the router
and egress Deployments in place. The demo fixture and actors are unaffected.

## Upgrade gotchas (learned the hard way)

- **Do not upgrade a legacy unversioned install in place.** If nodes lack
  `ate.dev/substrate-version` or the atelet DaemonSet has no version suffix, recreate the local
  kind cluster. The rolling upgrade runbook requires the versioned installation layout.
- **A rebuild does not move an already labeled node to a new atelet.** The node label is the
  rollout control and the installer deliberately leaves it alone. On disposable `kind-substrate`,
  use the in-place relabel commands in `IDENTITY.md`'s verification cookbook, or recreate the
  cluster. Do not relabel every node at once in a production cluster.
- **Prefer the maintained verification.** `demos/egress/test-egress.sh` tests both the positive
  actor path and the negative case where a pod identity attempts to open an actor tunnel.
