# Running PR #460 (M2 Networking) locally — runbook

Reproduces the working setup for
[agent-substrate/substrate#460](https://github.com/agent-substrate/substrate/pull/460)
(ingress/egress via agentgateway + atunnel) in a local kind cluster.
Verified working 2026-07-22 on macOS (arm64, Docker Desktop).

## Context worth having open

- **PR #460** — replaces `atenet-router` (Envoy+xDS+ExtProc) with:
  - `ateway-ingress` / `ateway-egress`: [agentgateway](https://github.com/agentgateway/agentgateway)
    deployments using image `ghcr.io/howardjohn-dev/agentgateway:v0.0.0-substrate.3`.
  - `atunnel`: new package (`internal/atunnel/`) embedded in ateom on every worker —
    mTLS ingress termination on :443 (verifies active actor, else 421 +
    `X-Ate-Assignment-Stale`), transparent egress capture on :15001 (dormant until an
    egress API exists).
- **Substrate-side Rust code** lives in fork **`howardjohn/agentgateway` branch `ate/m2`**
  (commit `9928080`, "Add substrate router support"): `substrateIngress` /
  `substrateEgress` policies in `crates/agentgateway/src/http/substrate/`, stale-assignment
  retry in `proxy/httpproxy.rs`.
- The egress *policy API* was reverted out of the PR (commit `020e723f` on the PR branch) —
  apply that commit in reverse to light up the full egress tunnel path.
- Predecessor POCs: #338 (egress capture), #393 (pluggable ingress). Design issues: #326, #430.
- This PR branch is pinned at commit `00490024abaf2aae0dd3a583abc2d6f35cec7814`.

## Prerequisites

- Docker, `kind`, `kubectl`, `jq`, Go (1.26 worked)
- **bash >= 4** — macOS ships 3.2 and the hack scripts use `mapfile`.
  `brew install bash`, then run the scripts with `/opt/homebrew/bin/bash` (or put
  homebrew first in PATH).
- `ko` is fetched automatically by `hack/run-tool.sh`; first run downloads a lot of Go deps.

## Steps

```bash
# 1. Get the code (PR ref works on any clone; branch also pushed to the fork)
git fetch upstream pull/460/head:pr-460   # upstream = agent-substrate/substrate
git checkout pr-460

# 2. Sanity build + unit tests
go build ./... && go test ./internal/atunnel/...

# 3. Create a dedicated kind cluster (registry on :5001, feature gates for
#    ClusterTrustBundle/PodCertificateRequest; gVisor works without KVM)
KIND_CLUSTER_NAME=substrate ./hack/create-kind-cluster.sh

# 4. Install substrate + demos (ko-builds every image; takes several minutes)
KIND_CLUSTER_NAME=substrate KUBECTL_CONTEXT=kind-substrate \
  /opt/homebrew/bin/bash ./hack/install-ate-kind.sh \
  --deploy-ate-system --deploy-demo-counter --deploy-demo-egress

# 5. Build the CLI
go build -o bin/kubectl-ate ./cmd/kubectl-ate

# 6. Create actors
./bin/kubectl-ate --context kind-substrate create atespace demo
./bin/kubectl-ate --context kind-substrate create actor my-counter \
  --atespace demo --template ate-demo-counter/counter
./bin/kubectl-ate --context kind-substrate create actor egress-demo \
  --atespace demo --template ate-demo-egress/egress

# 7. Exercise it
kubectl --context kind-substrate port-forward -n ate-system service/ateway-ingress 18000:80 &

# counter through the ingress (first call cold-boots the sandbox, ~15s; warm ~5ms)
curl -X POST localhost:18000/increment \
  -H 'Host: my-counter.demo.actors.resources.substrate.ate.dev'

# egress demo actor fetches a URL (direct-egress path; tunnel is dormant)
curl -X POST localhost:18000/ \
  -H 'Host: egress-demo.demo.actors.resources.substrate.ate.dev' \
  -H 'Content-Type: application/json' -d '{"url":"http://example.com/"}'

# transparent resume: suspend, then request again (~300ms, durable state intact)
./bin/kubectl-ate --context kind-substrate suspend actor my-counter -a demo
curl -X POST localhost:18000/increment \
  -H 'Host: my-counter.demo.actors.resources.substrate.ate.dev'
```

Useful observability:

```bash
# gateway request log with ate.* attributes and per-request latency
kubectl --context kind-substrate logs -n ate-system deploy/ateway-ingress -f | grep request
# worker-side ateom/atunnel lifecycle
kubectl --context kind-substrate logs -n <worker-ns> <worker-pod> -f
```

## Gotchas / troubleshooting

- **Install script "timed out waiting for the condition"** — the 120s rollout waits are
  aggressive on first install (image pulls). The manifests are already applied; just wait
  for `kubectl get pods -n ate-system` to go Ready, then rerun the demo-deploy flags.
- **First request returns 504** — `substrateIngress` has a 15s ResumeActor timeout and a
  cold sandbox boot can exceed it. Retry; once the actor is up it's milliseconds.
- **Actor stuck in `STATUS_RESUMING`, atelet logs show
  `while fetching sandbox asset "runsc" ... context canceled`** — atelet streams `runsc`
  (~64MB) from `gs://gvisor` inside a ~28s RPC deadline; on a slow network it never
  finishes and restarts from scratch each retry. Workaround: pre-seed the node cache
  (cache hit is a plain stat):

  ```bash
  # sha from the atelet log line for your arch:
  #   arm64: 62eee121f8c188e347c428acc96f111568ede3be37b906046b6f28bbe2cc40c0
  #   amd64: f18a948bf9c8bbb54eb998549a3a8d719a1c7de2efbe8fdd2ff0ee5fecd06f19
  SHA=62eee121f8c188e347c428acc96f111568ede3be37b906046b6f28bbe2cc40c0
  curl -sSL -o runsc https://storage.googleapis.com/gvisor/releases/release/20260622/aarch64/runsc
  shasum -a 256 runsc   # MUST match $SHA before copying
  docker exec substrate-control-plane mkdir -p /var/lib/ateom-gvisor/static-files
  docker cp runsc substrate-control-plane:/var/lib/ateom-gvisor/static-files/runsc-$SHA
  docker exec substrate-control-plane chmod 755 /var/lib/ateom-gvisor/static-files/runsc-$SHA
  ```

- **Direct actor access checks**: worker `:80` should refuse (DNAT rule removed);
  `:443` should reject TLS without the ingress's podidentity client cert. If either
  succeeds, something is off.
- A pre-existing kind cluster is not reusable — the cluster needs the
  `ClusterTrustBundle`/`PodCertificateRequest` feature gates and the local-registry
  containerd config from `hack/create-kind-cluster.sh`. Use a separate
  `KIND_CLUSTER_NAME` if you have other kind clusters you care about.

## Architecture cheat-sheet

```
client → ateway-ingress:80/443 (agentgateway)
           substrateIngress: parse <actor>.<atespace>.actors.resources.substrate.ate.dev
           → ResumeActor (ate-api gRPC), cache assignment 5s
           → dynamic backend: mTLS to <worker-ip>:443
                 atunnel (in ateom): verify client SPIFFE = ateway-ingress SA,
                 verify Host = active actor (else 421 + X-Ate-Assignment-Stale → gateway
                 evicts cache + retries ≤3), reverse-proxy → actor veth 169.254.17.2:80

actor egress (dormant until egress API lands):
  nftables REDIRECT (only if egress_gateway_address set) → atunnel:15001
  → SO_ORIGINAL_DST → CONNECT+mTLS to ateway-egress:443
      (X-Ate-Atespace / X-Ate-Actor / X-Ate-Actor-Version)
  → substrateEgress policy (today: telemetry only) → dynamic forward to :authority
```
