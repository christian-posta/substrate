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
> Last run end-to-end on 2026-09-17 against `upstream/main` at `85ce8ed5` on `kind-substrate`,
> using `--atenet-dataplane=agentgateway`.

## What the demo shows (and doesn't)

**Shows:** transparent actor egress (nftables REDIRECT → atunnel → mTLS CONNECT to the gateway),
**actor identity**, and **egress policy**. Both dataplanes now do the same three things: refuse a
certificate that does not chain to the actor-identity CA, authorize the actor at CONNECT time
against the control plane (ActorIdentity extension, `atunnel` purpose, UID match, `RUNNING`
state), and enforce the actor's `EgressPolicy`. They differ in where that happens and how much
they cover; see
[EGRESS.md](./EGRESS.md#where-each-dataplane-enforces-it).

**The gateway denies by default — with a caveat on agentgateway.** Every walkthrough below creates
an `EgressPolicy`, because without one the actor's HTTP fetches are refused. `kubectl ate create
egress-policy` is the verb. On Envoy that default-deny covers the whole tunnel. On agentgateway it
covers only the routes policy is attached to, so a policy-less actor is still refused cleartext
HTTP but can open TLS-passthrough and opaque-TCP egress; see
[EGRESS.md](./EGRESS.md#default-deny-is-not-uniform).

**Does not show:** credential injection (Envoy-only, and it needs an out-of-tree credential
provider), or TLS interception, which exists behind `--experimental-use-sdsmint` but is not
enabled here.

## Prerequisites

- `kind` ≥ v0.31.0 (older node images lack the `PodCertificateRequest` feature gate — kubelet
  refuses to start), `kubectl` ≥ v1.23, `ko`, `docker`, `go`.
- Local registry on :5001 (created by `hack/create-kind-cluster.sh`).
- `kubectl-ate`: `go install ./cmd/kubectl-ate`. It takes `--context kind-substrate` directly.

## Fresh install (from nothing)

```bash
hack/create-kind-cluster.sh                       # kind-substrate + local registry :5001

# Pick ONE dataplane. Envoy is the default; the flag selects BOTH ingress and egress:
hack/install-ate-kind.sh --deploy-ate-system                                   # Envoy
hack/install-ate-kind.sh --deploy-ate-system --atenet-dataplane=agentgateway   # agentgateway

hack/install-ate-kind.sh --deploy-demo-egress     # egress demo template + golden snapshot
go install ./cmd/kubectl-ate
```

Gotcha: `--deploy-ate-system`'s rollout wait can time out on cold image pulls while pods are
still ContainerCreating — that is not a failure; wait for pods and re-run or continue.

## Many actors, one worker

`demos/egress/multi-actor-identity.sh` is the multiplexing story in one command: five actors, one
worker pod, one identity each. It needs `--atenet-dataplane=agentgateway`.

```bash
demos/egress/multi-actor-identity.sh
demos/egress/multi-actor-identity.sh --cleanup
```

Last run on 2026-09-17, all checks passed:

```text
PASS alpha: HTTP 200 through worker pod egress-b48b9765c-trx2v
     the target saw the gateway (10.244.0.19) as its client, not the actor
...
PASS all 5 actors were served by the same worker pod
ate.actor.name=alpha   ate.actor.uid=2653b02a-... ate.atespace=ate-demo-egress
ate.actor.name=bravo   ate.actor.uid=8390faa7-... ate.atespace=ate-demo-egress
ate.actor.name=charlie ate.actor.uid=d274909e-... ate.atespace=ate-demo-egress
ate.actor.name=delta   ate.actor.uid=27bf7824-... ate.atespace=ate-demo-egress
ate.actor.name=echo    ate.actor.uid=0d43574f-... ate.atespace=ate-demo-egress
PASS the gateway named every actor: alpha bravo charlie delta echo
```

It leaves the WorkerPool at one replica. Scale it back with
`kubectl -n ate-demo-egress scale workerpool/worker --replicas=2`.

## Scripted verification (easiest)

```bash
hack/verify-egress-demo.sh
```

This lightweight smoke test creates an actor, drives an external HTTP fetch through it, and checks
the selected dataplane's access log for the CONNECT. Exit 0 plus `== PASS ==` is the win condition.

Neither this script nor `demos/egress/test-egress.sh` creates an `EgressPolicy`, so give the actor
one first or its fetch is denied with 403:

```bash
kubectl ate --context kind-substrate create egress-policy egress-demo -a ate-demo-egress --all
```

The richer upstream script is `demos/egress/test-egress.sh` (in-cluster target, positive +
negative identity tests, `--cleanup`).

## Manual walkthrough

Upstream's step-by-step (in-cluster whoami target, create/resume actor, curl through the
router, inspect gateway logs, negative test from a non-actor pod) lives in
**`demos/egress/README.md` → "Manual walkthrough"** — it applies unchanged on either
dataplane. Condensed:

```bash
# an in-cluster target; the actors dial it by ClusterIP
kubectl --context kind-substrate create namespace egress-target
kubectl --context kind-substrate -n egress-target create deployment whoami --image=traefik/whoami
kubectl --context kind-substrate -n egress-target expose deployment whoami --port=80
TARGET_IP=$(kubectl --context kind-substrate -n egress-target get svc whoami \
  -o jsonpath='{.spec.clusterIP}')

# create the actor, then its policy, then resume: the policy must exist before
# the actor's first outbound connection
kubectl ate --context kind-substrate create actor egress-demo -a ate-demo-egress --template egress
kubectl ate --context kind-substrate create egress-policy egress-demo -a ate-demo-egress --all
kubectl ate --context kind-substrate resume actor egress-demo -a ate-demo-egress  # ACTOR_STATE_RUNNING

# drive egress through the actor. ate-target-actor picks the actor; the URL in
# the body is that actor's egress destination and is unrelated to routing.
kubectl --context kind-substrate -n ate-system port-forward svc/atenet-router 18000:80 &
curl -s -X POST http://localhost:18000/ \
  -H "ate-target-actor: ate-demo-egress/egress-demo" \
  -H 'Content-Type: application/json' \
  -d "{\"url\":\"http://${TARGET_IP}:80/\"}"        # expect HTTP 200
```

The response body is `whoami`'s, and its `RemoteAddr` is the **gateway pod's** IP, not the
actor's — proof the request egressed through the gateway rather than directly.

### Envoy-specific checks

```bash
kubectl --context kind-substrate -n ate-system logs deploy/atenet-egress -c ext-proc --tail=20 \
  | grep 'egress tunnel opened\|egress denied'
```

What to look for: `actor`, **`actorUid`** (the incarnation binding — delete and recreate the actor
at the same name and it changes), `rule` (which policy rule matched), and `destination` as a
resolved `IP:port`. DNS happens in the clear before the tunnel, so the CONNECT never carries a
hostname.

### agentgateway-specific checks

```bash
# agentgateway is the sole egress dataplane container; it has no atenet ext_proc sidecar
kubectl --context kind-substrate -n ate-system get pod -l app=atenet-egress \
  -o jsonpath='{.items[0].spec.containers[*].name}'

# Its access log names the actor it authorized at CONNECT, plus the authority.
kubectl --context kind-substrate -n ate-system logs deploy/atenet-egress -c agentgateway \
  | grep -o 'ate\.actor\.name=[^ ]* ate\.actor\.uid=[^ ]* ate\.atespace=[^ ]*'
```

Those `ate.*` fields come from the inner HTTP route, so a TLS-passthrough or opaque-TCP tunnel
logs `substrate.connect.authority` and no actor fields. That is a logging gap, not an
unauthenticated connection: the CONNECT was authorized either way.

## Switching dataplanes on a running cluster

Re-run `--deploy-atenet` with the other `--atenet-dataplane=` value; it rewrites the router and
egress Deployments in place without touching the rest of the control plane. The demo fixture,
actors and their egress policies are unaffected.

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
