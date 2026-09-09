# Actor Egress

> How an actor's outbound traffic leaves the sandbox, how it is authenticated with the actor's own
> identity, and what the egress gateway does and does not enforce. Companion to
> [IDENTITY.md](./IDENTITY.md), which covers how the actor certificate is minted. The running
> example is an actor calling an MCP server outside the Kubernetes cluster. To run the demo, see
> [DEMO.md](./DEMO.md) and `demos/egress/README.md`.

## The flow on one screen (default Envoy dataplane)

```
┌────────────────────────── worker pod (pod netns) ───────────────────────────┐
│  ┌─── actor sandbox (gVisor netns or micro-VM guest) ─┐                     │
│  │  actor process                                     │                     │
│  │    dial tcp <mcp-ip>:443     (thinks it is         │                     │
│  │    eth0 = 169.254.17.2/30     talking directly)    │                     │
│  │    default route → 169.254.17.1                    │                     │
│  └───────────────────────┬────────────────────────────┘                     │
│                          │ veth                                             │
│                   ateom0 (169.254.17.1)                                     │
│                          │                                                  │
│        nftables prerouting: saddr==169.254.17.2 && tcp                      │
│                → REDIRECT to :15001  (preserves SO_ORIGINAL_DST)            │
│                          │                                                  │
│  atunnel egress (inside the ateom process, :15001)                          │
│    • getsockopt SO_ORIGINAL_DST → "<mcp-ip>:443"                            │
│    • mTLS to atenet-egress.ate-system.svc:443                               │
│         client cert = the ACTOR's one-hour certificate                      │
│    • HTTP/1.1 CONNECT <mcp-ip>:443 — no identity/auth headers or tokens    │
└──────────────────────────┬──────────────────────────────────────────────────┘
                           │
┌───────────────── atenet-egress pod (2 containers) ──────────────────────────┐
│  Envoy (static config)                 atenet --mode=egress (ext_proc)      │
│    • :443, require client cert           • re-verify chain in Go            │
│      trusted_ca = actor-identity CA      • parse ActorIdentity extension    │
│    • terminate CONNECT                   • GetActor: UID match + RUNNING     │
│    • XFCC carries the full chain  ───▶   • allow (empty response) or 403/503│
│    • dynamic_forward_proxy                                                  │
└──────────────────────────┬──────────────────────────────────────────────────┘
                           │ plain TCP to the CONNECT authority
                           ▼
                 MCP server, <mcp-ip>:443 (outside the cluster)
                 sees the gateway pod IP as its client;
                 the actor's own TLS rides opaquely inside the tunnel
```

Three load-bearing facts:

1. **The actor is untouched.** It dials plain TCP. Interception is an nftables redirect in the
   worker pod's network namespace. No proxy environment variables, no SDK, nothing inside the
   sandbox.
2. **Identity is the client certificate, not a header.** atunnel generates the outer CONNECT and
   supplies no actor-controlled identity or authentication header. Nothing the actor writes into
   the tunneled connection contributes to the gateway's identity decision.
3. **The gateway authenticates the actor and checks its current incarnation; it does not yet
   authorize destinations.** The default Envoy path requires the UID to match a RUNNING actor. An
   egress policy API exists in the control plane, but no built-in dataplane reads it and nothing on
   `main` injects credentials. Details under [Egress policy](#egress-policy).

## The actor network

ateom builds the actor's network in the worker pod's network namespace on every Run and Restore:

- A point-to-point `/30` veth pair. The pod side is `ateom0` at `169.254.17.1`; the actor side is
  `eth0` at `169.254.17.2`, created directly inside the actor's namespace.
- Inside the sandbox: loopback up, `eth0` addressed, and a default route via `169.254.17.1`. That
  default route is the only network configuration the actor ever sees.
- On gVisor, runsc joins the named namespace ateom created. On the micro-VM runtime, a tap device
  is cross-connected to the veth with a TC redirect, guest networking is pushed over the kata
  agent, and the gateway's ARP entry is pinned because snapshots freeze the guest ARP cache.
- Actor networking is IPv4-only.

## Interception

One nftables table with three chains in the worker pod's namespace:

| Chain | Rule | Purpose |
| --- | --- | --- |
| prerouting | `ip saddr 169.254.17.2 && tcp → redirect :15001` | steer **all actor TCP** into atunnel. REDIRECT rather than TPROXY so the original destination is recoverable with `SO_ORIGINAL_DST` |
| postrouting | `ip saddr 169.254.17.2 → masquerade` | escape hatch for non-TCP traffic, notably DNS over UDP |
| forward | accept | let the pod kernel route between the actor veth and the pod's `eth0` |

The redirect has no destination or port carve-outs. Everything TCP goes through the tunnel;
everything non-TCP leaves masqueraded as the worker pod, unauthenticated and unpoliced. The
`forward` chain's policy is accept. Restricting the masquerade to DNS toward the cluster resolver
and dropping other non-tunneled egress remains a code TODO.

**Tunneling is turned on by the control plane, not by the actor.** `ate-api-server` is started with
`--egress-gateway-address=atenet-egress.ate-system.svc:443` in the default install. ateapi
stamps that address onto every atelet Run/Restore, atelet relays it to ateom, and ateom installs
the redirect. An empty address means no redirect rule and actor TCP takes the masquerade path.
The flag is described in the manifest as temporary, pending the egress policy API; it has not
been retired.

**Fail-closed boot order.** Before any container starts, ateom mints the actor certificate; if that
fails, the actor does not start. Then it installs the redirect, boots containers, waits for
readiness, and only then activates atunnel ingress and egress. Between redirect installation and
activation, redirected connections are accepted and closed, so nothing leaks untunneled.

## atunnel

`atunnel` is a Go package compiled into the ateom binaries. Everything runs in the worker pod,
nothing in the sandbox, nothing on the host. It opens three listeners at ateom boot:

| Listener | Port | Role |
| --- | --- | --- |
| HTTP ingress | `:443` | mTLS reverse proxy from the ingress router to the actor's HTTP port (`169.254.17.2:80`, or the port named by `X-Ate-Target-Port`). Accepts only the router's SPIFFE ID `spiffe://cluster.local/ns/ate-system/sa/atenet-router`. Used by the Envoy ingress dataplane. |
| CONNECT ingress | `:8443` | mTLS CONNECT listener that relays opaque bytes to an actor port. Used by the agentgateway ingress dataplane as its backend tunnel. |
| Egress | `:15001` | accepts redirected actor TCP, recovers the original destination, opens an mTLS CONNECT to the egress gateway |

atunnel authenticates to the ingress router and to atelet with the worker's pod certificate
(`/run/podidentity.podcert.ate.dev`), and verifies the egress gateway's serving certificate
against the service-DNS trust bundle (`/run/servicedns.podcert.ate.dev`).

The egress tunnel protocol is deliberately minimal:

1. The destination must be a literal `IP:port`; atunnel never sees a hostname.
2. TCP + TLS to the gateway, presenting the actor certificate. Once the certificate has expired
   atunnel refuses to present it, so new tunnels fail until renewal succeeds.
3. A Go HTTP/1.1 `CONNECT <ip:port>` request with ordinary protocol headers, but no identity or
   authentication headers.
4. Non-2xx closes the actor's socket. 2xx starts raw byte shuttling with half-close propagation.

Certificate lifecycle (minting is described in IDENTITY.md): renew at 90% of remaining lifetime;
past expiry, block new tunnels while retrying and let established tunnels drain; stop renewing for
the activation if the broker says the actor was reassigned or deleted; on checkpoint or suspend,
force-close every live tunnel. Key and certificate are per activation and in memory only, so
nothing lands in a snapshot.

## DNS

The actor's `/etc/resolv.conf` is the worker pod's: bind-mounted on gVisor, copied into the
rootfs on the micro-VM runtime. The actor therefore resolves external names against the cluster
resolver. Those UDP queries miss the TCP-only redirect and leave masqueraded as the worker pod.

Consequences: the gateway on the default path knows only the destination `IP:port`. The hostname
the actor resolved, the SNI, the URL, and the protocol all ride opaquely inside the tunnel. The
threat model lists "do not mount the worker pod's resolv.conf into actors" as an unimplemented
mitigation.

## Hop by hop: actor to external MCP server

1. The actor resolves `mcp.example.com` over UDP to the cluster resolver; the query is masqueraded
   out and returns the real external IP.
2. The actor dials `tcp <mcp-ip>:443`; the default route sends it over the veth.
3. nftables redirects it to atunnel `:15001`; conntrack records the original destination.
4. atunnel checks that the activation is live and the certificate unexpired, and recovers
   `<mcp-ip>:443`.
5. atunnel dials the egress gateway **from the pod's own IP**, so it does not re-enter the redirect
   (which matches only the actor's source address), completes mTLS with the actor certificate, and
   sends `CONNECT <mcp-ip>:443`.
6. The gateway refuses the handshake unless the client certificate chains to the actor-identity
   CA. This alone shuts out every non-actor client.
7. On the Envoy dataplane, ext_proc authenticates the identity and checks the actor is RUNNING:
   2xx opens the tunnel; 403 or 503 makes atunnel's dial fail and the actor sees a closed
   connection.
8. Envoy's dynamic forward proxy dials `<mcp-ip>:443`. The MCP server sees the **gateway pod IP**
   as its client.
9. The actor's own TLS handshake with the MCP server now proceeds end-to-end inside the tunnel.
   The gateway cannot see or modify it.
10. On suspend, every tunnel is closed. On resume, a new certificate is minted and egress is
    re-activated on whatever node the actor lands on.

## The default egress gateway: Envoy + atenet ext_proc

### Shape

One Deployment, two containers, in `manifests/ate-install/atenet-egress.yaml`:

- **Envoy** (digest-pinned v1.39, the same image as the ingress router), fully static config with
  no xDS. One listener on `:443` bound to `::` with IPv4 compatibility: `require_client_certificate`,
  `trusted_ca` = the actor-identity CA (loaded from a static filename), serving certificate via filesystem SDS
  (so kubelet's certificate rotation is picked up). HTTP connection manager with CONNECT upgrade;
  the full client chain is forwarded to ext_proc in `x-forwarded-client-cert`. The route timeout is
  disabled because Envoy applies it to the whole lifetime of a CONNECT tunnel. Filter order:
  **ext_proc (fail closed) → dynamic_forward_proxy → router**. The forward proxy resolves both
  address families.
- **ext_proc sidecar** is the `atenet` binary started with `--mode=egress` and
  `--actor-identity-ca-file`. In egress mode it runs no xDS server, no template controller, and
  creates no Kubernetes clients at all; the Deployment carries no RBAC. A bad or missing CA file
  fails startup rather than turning into per-request errors.

### The per-CONNECT decision

For each request the ext_proc handler, in order:

1. requires the method to be CONNECT;
2. fails with 503 if no CA roots are loaded (misconfiguration, deliberately not 403);
3. extracts the certificate chain from `x-forwarded-client-cert`. Exactly one element is
   required; Envoy's `SANITIZE_SET` makes it the sole writer of that header;
4. **re-verifies the chain in Go** even though Envoy already did: validity window, leaf is not a
   CA, an *explicit* ClientAuth EKU (an empty EKU means "any" to Go's verifier, which is a tested
   regression), chain to the actor-identity roots, and exactly one valid `ActorIdentity` extension
   with purpose `atunnel`;
5. requires the atespace and actor name to be syntactically valid resource names; an illegal
   name implies a compromised CA and is a 403;
6. calls `GetActor` on ateapi and authorizes on **UID equality** (defeating delete-and-recreate
   under the same name) and `RUNNING` state. Errors fail closed: not found is 403, unavailable or
   timeout is 503;
7. on success returns an **empty response**: no header mutation, no rewrite. The CONNECT proceeds
   to the forward proxy unchanged.

The SPIFFE URI SAN is not used for the built-in CONNECT authorization; only the extension is. The
SAN appears in Envoy's access log, which is the operational proof that identity came from the
verified peer certificate rather than from anything atunnel or the actor sent.

The handler does **not** read an egress policy. Step 7's "proceed unchanged" is the only outcome
after authentication.

### Sharing one binary between ingress and egress safely

The ext_proc mux dispatches on the Envoy-asserted attribute `xds.filter_chain_name` (the egress
filter chain is named `egress` in the manifest), never on anything in the request. Keying on
CONNECT would let any external client reach the egress handler and use its errors as an
actor-existence oracle. An absent or unknown attribute falls through to ingress, whose trust model
assumes hostile headers, and a mode-restricted instance returns 404 for the direction it does not
serve. Client-forged `xds.filter_chain_name` headers are tested not to work.

Substrate-owned dataplane keys are rooted at `dev.ate.` (for example `dev.ate.authority` for the
resolved original destination, `dev.ate.actor.identity`, `dev.ate.extproc.direction`), distinct
from the dotted `ate.` telemetry namespace and the `ate.dev/` label form.

| | ingress (`atenet-router`) | egress (`atenet-egress`) |
| --- | --- | --- |
| Envoy config | dynamic via xDS from the co-located router | static ConfigMap |
| Identity model | headers are hostile input; actor looked up by Host | identity only from the verified client certificate |
| Upstream | original-destination cluster to the worker's atunnel `:443` over mTLS, with the resolved address carried as filter state and `:authority` kept as the actor DNS name; upstream mTLS credentials delivered via SDS so they rotate | dynamic forward proxy to the CONNECT authority, plain TCP |
| Kubernetes access | actor templates and endpoint slices | none |
| Extras | request parking and resume-on-demand; HTTP `:8080`, HTTPS `:8443`; arbitrary-port CONNECT ingress on `:8081` and `:8444` (HTTP(S) inside CONNECT, terminated and re-entered through the same ext_proc path) | — |

## The agentgateway dataplane

`hack/install-ate.sh --atenet-router=envoy|agentgateway` selects the ingress and egress dataplane;
Envoy is the default. With agentgateway, the router and egress Deployments run `agentgateway`
alone: no Envoy, no `atenet` sidecar. agentgateway's native `substrateIngress` and
`substrateEgress` policies call ateapi directly over pod-identity mTLS.

**Ingress.** Public HTTP `:8080` and HTTPS `:8443`; CONNECT ingress on `:8081` and `:8444`
re-enters an internal HTTP listener. Every worker connection is wrapped in mTLS CONNECT to
atunnel's `:8443` listener. This supports HTTP(S) inside CONNECT, not raw TCP to arbitrary actor
ports.

**Egress.** An outer `:8443` HTTPS listener validates the actor certificate against the
actor-identity CA and accepts CONNECT, then re-enters a protocol-detecting internal bind:

- inner **HTTP** applies `substrateEgress` against ateapi and forwards to the CONNECT authority;
- inner **TLS** and generic **TCP** forward to the CONNECT authority with no further policy.

So on agentgateway, opaque TLS and TCP egress get the outer CA-chain gate only. There is no
`ActorIdentity` extension or purpose check, and no per-CONNECT UID/RUNNING lookup, on those
routes. The stronger per-CONNECT behavior described above is specific to the Envoy dataplane.

## TLS interception (opt-in)

`--experimental-use-sdsmint` deploys a variant of the egress gateway that can terminate recognized
TLS traffic and re-originate it, so the gateway can see and act on HTTP inside the tunnel.

- On Envoy, a separate `sdsmint` sidecar mints a short-lived leaf per SNI on demand and delivers it
  over SDS. It is a separate container so the interception signing key never lands on the
  dataplane container.
- On agentgateway, the native `dynamicCa` feature is used instead; the CA certificate and key are
  exported into the `egress-mitm-ca-pool` Secret.

On the Envoy variant, protocol inspection splits the inner stream three ways: TLS with SNI is
terminated and parsed as HTTP, cleartext HTTP/h2c is parsed directly, and opaque raw TCP is passed
through without decryption. A TLS client that omits SNI receives a certificate for
`sni-required.egress.ate.invalid`, so normal verification fails rather than silently weakening the
hostname boundary. The agentgateway variant similarly applies its HTTP policy to intercepted HTTP
while its generic TCP route remains passthrough.

Actors must trust the interception CA or their TLS fails. The trust pipeline: atecontroller
publishes the `egress-mitm-ca-pool` roots as ClusterTrustBundle
`egress-mitm.ate.dev:mitm:primary-bundle`; an ActorTemplate declares a `systemInfo` volume with a
`trustBundle` data source named `egress-mitm.ate.dev`; atelet resolves it on the node and projects
the PEM into the sandbox on every Run/Restore, on both runtimes. Templates must declare this
themselves; automatic injection is under review. Runtimes with bundled CA stores (Node.js, for
instance) also need runtime-specific configuration to honor the projected file. The operator guide
is `docs/egress-trust-bundle.md`.

Interception adds L7 **visibility** for recognized HTTP/HTTPS traffic. Opaque raw TCP remains
passthrough. Interception does not by itself enforce `EgressPolicy` or inject credentials.

### External authorization hook (opt-in)

With the Envoy dataplane and TLS interception enabled,
`--experimental-additional-egress-extproc-service=<namespace>/<service>:<port>` splices a
fail-closed external `ext_proc` filter into both inspectable HTTP chains, ahead of dynamic forward
proxying. Envoy sends request headers plus the verified actor identity in filter state and reaches
the service over pod-identity mTLS with service-DNS name verification. The hook does not affect
opaque TCP, and Substrate does not ship the policy or credential-resolution service behind it.

## Egress policy

The control plane has an actor egress policy API. No built-in dataplane consumes it.

**What exists:**

- `GetActorEgressPolicy`, `CreateActorEgressPolicy`, `UpdateActorEgressPolicy`,
  `DeleteActorEgressPolicy` on the `Control` service, persisted in PostgreSQL, deleted with the
  actor.
- One `EgressPolicy` per actor, named `default`. `rules` are evaluated in order; the first
  matching rule authorizes the request and only its effects apply; a request with no matching rule
  is denied.
- Each `EgressRule` has exactly one matcher: `hostnames` (lowercase DNS names, optionally a single
  `*` replacing the leftmost label), `ip_blocks` (canonical IPv4/IPv6 CIDRs against the original
  destination IP), or `all`.
- Effects exist only on hostname rules: `inject_static_headers`, a list of
  `{header, prefix, credential_uri}` where `credential_uri` is a
  `substrate-secret://<provider-class>/<provider-name>/<tail>` reference to be interpreted by a
  registered credential provider.
- Validation is declarative and thorough (unique header names, well-formed names and CIDRs,
  policy atespace matching the actor's).

**What does not exist:**

- No built-in enforcement point reads the policy: not the Envoy ext_proc handler, not agentgateway's
  shipped configuration, not atunnel. The policy is not carried in the atelet or ateom protocols.
- No credential provider registry and nothing that resolves a `substrate-secret://` URI into a
  header value. The injection field is inert.
- The `--egress-gateway-address` flag the policy API was meant to replace is still how tunneling
  is enabled.

**Open PRs, not on `main`:**
[PR #1335](https://github.com/agent-substrate/substrate/pull/1335) adds a credential-provider gRPC
service that resolves `substrate-secret://kubernetes.io/...` references to Kubernetes Secrets.
[PR #1360](https://github.com/agent-substrate/substrate/pull/1360) adds a separate ext_proc for the
gateway's decrypted TLS-interception leg; it fetches the actor's egress policy, matches the
destination hostname, and sets the resolved credential as a request header. The generic
external-processor hook described above is on `main`, but these concrete provider and enforcement
implementations are not.

Two structural notes for the MCP scenario. First, header injection attaches only to *hostname*
rules, so it presupposes the gateway knows the hostname, which means TLS interception or an
L7-aware dataplane, not the default opaque CONNECT path. Second, the non-TCP masquerade path
described under [Interception](#interception) bypasses the gateway entirely, so any destination
policy is only as strong as that hole is closed.

## Trust bundles at the gateway

| Bundle | Signs | Used by whom to verify whom | Rotation today |
| --- | --- | --- | --- |
| Actor-identity CA (`actor-id-ca-pool` Secret; signing key mounted only in ateapi) | actor certificates | gateway downstream `trusted_ca`; Envoy ext_proc re-verification | the gateway's copy is a cert-only Secret `actor-id-ca-certs`, derived at install by the installer and loaded from a static filename; nothing refreshes it, and SDS does not cover `trusted_ca`, so rotating the actor CA means re-deriving the Secret and restarting the gateway |
| Pod identity | atelet, workers, router and control-plane pods | broker socket; router↔worker atunnel ingress; ateapi client auth | Kubernetes PodCertificateRequests and ClusterTrustBundles; projected volumes rotate |
| Service DNS | in-cluster serving certificates | atunnel verifying the gateway's serving certificate | the gateway's serving cert is loaded via filesystem SDS, so `watched_directory` fires on kubelet rotation |
| Interception CA (`egress-mitm-ca-pool`) | per-SNI leaves in interception mode | actors, via `systemInfo.trustBundle` | published as a ClusterTrustBundle by atecontroller |

## Verification cookbook (`kind-substrate`)

These commands exercise the actual deployed path and inspect the selected dataplane. Install the
core system before the demo:

```bash
# Omit --atenet-router=agentgateway to use the default Envoy dataplane.
hack/install-ate-kind.sh --atenet-router=agentgateway --deploy-ate-system
hack/install-ate-kind.sh --deploy-demo-egress
```

The checks require `kubectl`, `kubectl-ate`, `jq`, `openssl`, and `curl`. The demo test owns the
default `ate-demo-egress/egress-demo` actor and `egress-target` namespace; do not repoint it at
shared resources.

```bash
export CTX=kind-substrate
export ATESPACE=ate-demo-egress
export ACTOR=egress-demo
```

### Identify the installed dataplane and exact images

```bash
git fetch upstream main

kubectl --context "$CTX" -n ate-system rollout status \
  deployment/atenet-egress --timeout=120s

kubectl --context "$CTX" get deployment -n ate-system \
  ate-api-server atenet-router atenet-egress -o json |
  jq -r '.items[] | .metadata.name as $deployment |
    .spec.template.spec.containers[] |
    [$deployment,.name,.image] | @tsv'

kubectl ate --context "$CTX" get actor-template egress \
  -a "$ATESPACE" -o json |
  jq -r '.actorTemplates[0].containers[] | [.name,.image] | @tsv'

kubectl --context "$CTX" exec -n ate-system deploy/ate-api-server -- \
  /ko-app/ateapi --version
kubectl --context "$CTX" exec -n "$ATESPACE" deploy/egress -- \
  /ko-app/ateom-gvisor --version

API_VERSION="$(kubectl --context "$CTX" exec -n ate-system \
  deploy/ate-api-server -- /ko-app/ateapi --version)"
DEPLOYED_COMMIT="$(printf '%s\n' "$API_VERSION" |
  sed -E 's/.*commit=([0-9a-f]{40}).*/\1/')"

if test "$DEPLOYED_COMMIT" = "$(git rev-parse upstream/main)"; then
  echo 'ateapi commit exactly matches upstream/main'
elif git diff --quiet "$DEPLOYED_COMMIT"..upstream/main -- . \
  ':(exclude)docs/**'; then
  echo 'deployed source matches; upstream differs only under docs/'
else
  echo 'deployment is missing upstream source changes' >&2
  false
fi
```

The first container name in `atenet-router` and `atenet-egress` is `envoy` or `agentgateway`.
Every locally built image should have a digest, and the Substrate binaries should report the
revision intended for the install. A `-dirty` suffix is expected for images built with local
changes. The final check also accepts a newer docs-only upstream commit, since that does not
change an image's executable source.

### Confirm tunneling is enabled and inspect the live gateway config

```bash
kubectl --context "$CTX" get deployment -n ate-system ate-api-server -o json |
  jq -r '.spec.template.spec.containers[0].args[] |
    select(startswith("--egress-gateway-address="))'

DATAPLANE="$(kubectl --context "$CTX" -n ate-system \
  get deployment/atenet-egress \
  -o jsonpath='{.spec.template.spec.containers[0].name}')"

case "$DATAPLANE" in
  envoy)
    kubectl --context "$CTX" -n ate-system get configmap atenet-egress \
      -o go-template='{{index .data "envoy.yaml"}}'
    ;;
  agentgateway)
    kubectl --context "$CTX" -n ate-system get configmap \
      atenet-egress-agentgateway-substrate-config \
      -o go-template='{{index .data "config.yaml"}}'
    ;;
  *)
    printf 'unsupported dataplane: %s\n' "$DATAPLANE" >&2
    exit 1
    ;;
esac
```

The ateapi flag should name `atenet-egress.ate-system.svc:443`. On Envoy, look for required
client certificates, the actor CA, CONNECT, fail-closed `ext_proc`, and dynamic forward proxying.
On agentgateway, the outer HTTPS listener should use the actor CA; the inner HTTP route should
carry `substrateEgress`, while the TLS and TCP routes only forward to the CONNECT authority. That
visible split is the reason the agentgateway caveat in this document is explicit.

### Fingerprint the actor CA trusted by the gateway

```bash
kubectl --context "$CTX" get secret actor-id-ca-certs -n ate-system \
  -o jsonpath='{.data.ca\.crt}' |
  base64 -d |
  openssl x509 -noout -subject -issuer -fingerprint -sha256

kubectl --context "$CTX" get secret actor-id-ca-certs -n ate-system -o json |
  jq -r '.data | keys[]'
```

The gateway copy should expose only `ca.crt`, never the actor CA signing key.

### Run the positive and negative end-to-end proof

```bash
KUBECTL_CONTEXT="$CTX" demos/egress/test-egress.sh
```

The script proves all of the following in one run:

- a real RUNNING actor can make an ordinary HTTP request without proxy configuration;
- the target sees the egress gateway pod IP as its client;
- the selected gateway logs the CONNECT;
- a pod with a valid pod-identity certificate, but no actor certificate, cannot open the tunnel;
- the failed client-certificate handshake produces no successful CONNECT.

The script cleans up its actor, target namespace, and probe by default. To retain them for
the next inspection block, run with `KEEP=1`; remove them afterward:

```bash
KEEP=1 KUBECTL_CONTEXT="$CTX" demos/egress/test-egress.sh
KUBECTL_CONTEXT="$CTX" demos/egress/test-egress.sh --cleanup
```

### Inspect current actor placement and gateway evidence

This assumes the demo actor exists, either from the install or a `KEEP=1` test run.

```bash
kubectl ate --context "$CTX" get actors "$ACTOR" -a "$ATESPACE" -o json |
  jq '.actors[0] | {
    uid: .metadata.uid,
    state: .status.state,
    worker: .status.workerAssignment.worker.name,
    workerPod: .status.workerAssignment.workerPod,
    workerPodIP: .status.workerAssignment.workerPodIp
  }'

kubectl --context "$CTX" get pods -n "$ATESPACE" \
  -l ate.dev/worker-pool=egress -o wide

DATAPLANE="$(kubectl --context "$CTX" -n ate-system \
  get deployment/atenet-egress \
  -o jsonpath='{.spec.template.spec.containers[0].name}')"

case "$DATAPLANE" in
  envoy) ACCESS_LOG_PATTERN='\[egress\]' ;;
  agentgateway) ACCESS_LOG_PATTERN='substrate.connect.authority' ;;
esac

kubectl --context "$CTX" -n ate-system logs deployment/atenet-egress \
  -c "$DATAPLANE" --tail=-1 |
  grep -E "$ACCESS_LOG_PATTERN" |
  tail -n 10
```

Envoy entries carry the actor SPIFFE SAN and response code. agentgateway entries carry the
CONNECT authority and, for its HTTP path, the resolved actor and atespace attributes.

### Confirm Kubernetes NetworkPolicy is not the egress enforcement point

```bash
kubectl --context "$CTX" get networkpolicy -n "$ATESPACE" -o json |
  jq '.items[] | {
    name: .metadata.name,
    podSelector: .spec.podSelector,
    policyTypes: .spec.policyTypes,
    egress: .spec.egress
  }'
```

The generated worker policy should list only `Ingress`, with `egress: null`. This confirms the
current deployment does not use Kubernetes NetworkPolicy for outbound enforcement; it does not,
by itself, prove the nftables rules inside the actor network namespace.

### Check whether TLS interception is installed

```bash
kubectl --context "$CTX" get clustertrustbundle \
  'egress-mitm.ate.dev:mitm:primary-bundle' 2>/dev/null ||
  echo 'TLS interception trust bundle is not installed'
```

Absence is expected for the default install. When interception is enabled, inspect the
ActorTemplate too: it must explicitly declare a `systemInfo.trustBundle` source for
`egress-mitm.ate.dev`; the bundle is not injected automatically.

### Audit the policy API versus shipped enforcement

```bash
rg -n 'rpc (Get|Create|Update|Delete)ActorEgressPolicy' \
  pkg/proto/ateapipb/ateapi.proto

if rg -n 'GetActorEgressPolicy|ActorEgressPolicy' \
  cmd/atenet/internal/router/egress \
  internal/atunnel \
  manifests/ate-install/atenet-egress.yaml \
  manifests/ate-install/components/agentgateway; then
  echo 'unexpected built-in policy-consumer match' >&2
  exit 1
else
  echo 'no built-in egress dataplane policy consumer found'
fi
```

The first command shows the CRUD surface. The second is a source-tree guardrail for the current
claim that neither shipped dataplane nor atunnel consumes it. Because absence checks can become
stale as files move, treat a new match as a prompt to review the implementation and this document,
not automatically as a regression.

### Run focused egress regressions

```bash
go test ./internal/atunnel \
  ./cmd/atenet/internal/router/egress \
  ./cmd/atenet/internal/router/extproc \
  ./cmd/ateapi/internal/controlapi -count=1
```

These cover CONNECT exchange and rejection, activation ordering, renewal/expiry/deactivation,
certificate verification, actor UID/RUNNING authorization, ext_proc direction hardening, and the
egress-policy CRUD and validation API. The nftables package is Linux-only; run its tests on a
Linux development host or in the repository's Linux CI environment:

```bash
go test ./internal/ateomnet -count=1
```

## What is real, and what is not

**Real and tested**

- Transparent nftables interception with original-destination recovery, and fail-closed
  activation ordering.
- Per-actor one-hour certificates with the `ActorIdentity` extension, brokered
  ateom → atelet → ateapi: node name and node UID binding on the worker↔atelet hop, and node
  name plus reciprocal worker↔actor assignment checks at ateapi; renewal at 90% of lifetime; in
  memory only.
- Envoy egress gateway: mTLS gate at the handshake, CONNECT termination, dynamic forward proxy,
  ext_proc re-verification plus UID/RUNNING check, fail closed everywhere, zero Kubernetes access.
- Direction dispatch hardened against client forgery.
- agentgateway as a full alternative dataplane for ingress and egress, with native
  `substrateIngress`/`substrateEgress` policies.
- Opt-in TLS interception on both dataplanes, and the ClusterTrustBundle → `systemInfo.trustBundle`
  pipeline to deliver the interception CA into sandboxes.
- An Envoy-only install hook for a separately supplied, fail-closed ext_proc on the decrypted HTTP
  legs.
- The egress policy API in the control plane (CRUD, validation, persistence).
- Arbitrary-port CONNECT **ingress** (HTTP(S) inside CONNECT).
- Demo (`demos/egress/`, written to work on either dataplane) and e2e suites
  (`internal/e2e/suites/{networking,egressauthz,egressmitm,identity}`).

**Demo readiness.** A full end-to-end demo of transparent egress plus actor-certificate identity
runs today on either dataplane. Substrate's credential provider and injection implementation is
not on `main`; the generic external ext_proc hook requires a separately supplied service.

**Not yet**

| Gap | Where it shows |
| --- | --- |
| No built-in destination enforcement: the policy API exists but no shipped PEP reads it | egress ext_proc handler; agentgateway egress config |
| No built-in upstream credential or token injection: `credential_uri` has no provider registry or resolver | egress policy API |
| Non-TCP actor egress bypasses the gateway via masquerade; `forward` chain is accept | `internal/ateomnet` nftables rules |
| Actor networking and original-destination recovery are IPv4-only (the gateway side is not the blocker) | `internal/ateomnet`, `internal/atunnel` |
| Envoy path does a control-plane `GetActor` on every CONNECT; a load concern | egress ext_proc handler |
| agentgateway opaque TLS/TCP routes have only the outer CA gate, no extension/purpose or UID/RUNNING check | agentgateway egress config |
| Kubernetes NetworkPolicy egress is deliberately unmanaged; the nftables redirect is the only enforcement, and an empty `--egress-gateway-address` puts actor TCP on the masquerade path | atecontroller NetworkPolicy controller |
| Actor-identity trust bundle at the gateway is a hand-derived install-time Secret with no rotation path | installer; egress manifest `trusted_ca` |
| Drain policy for long-lived CONNECT tunnels on gateway shutdown is undecided | egress manifest |
| Actor JWT / OIDC federation: mint exists, uncalled; issuer not OIDC-compliant; no actor-database cross-check | `ActorIdentity.MintJWT` |
| Raw TCP (non-HTTP) public ingress to arbitrary actor ports | both ingress dataplanes |
| Automatic injection of the interception trust bundle into every actor | atelet / ateapi ([PR #1252](https://github.com/agent-substrate/substrate/pull/1252)) |
| The egress redirect port `15001` collides with Istio's outbound listener on meshed clusters; [PR #1429](https://github.com/agent-substrate/substrate/pull/1429) proposes avoiding the collision | `internal/ateomnet`, WorkerPool controller |
| Actor addressing on ingress by Host header is under debate; [PR #1333](https://github.com/agent-substrate/substrate/pull/1333) proposes explicit actor and atespace headers | ingress router |

## File map

| Area | Files |
| --- | --- |
| Interception and actor network | `internal/ateomnet` |
| Tunnel (ingress, CONNECT ingress, egress, credential) | `internal/atunnel` |
| Sandbox supervisor wiring | `cmd/ateom-gvisor`, `cmd/ateom-microvm` |
| Node credential broker | `cmd/atelet` (`credentialbroker.go`); client side `internal/ateletdial` |
| Certificate and JWT minting | `cmd/ateapi/internal/actoridentity`, `cmd/ateapi/internal/ateletauth`, `internal/actoridjwt` |
| X.509 extensions | `internal/substratex509` |
| CA pools | `internal/localca`; `kubectl ate admin make-ca-pool`; `hack/install-ate.sh` |
| Envoy egress ext_proc | `cmd/atenet/internal/router/egress`, `cmd/atenet/internal/router/extproc` |
| TLS interception | `cmd/atenet/internal/sdsmint`; `manifests/ate-install/atenet-egress-with-sdsmint.yaml`; `manifests/ate-install/components/agentgateway-egress-mitm` |
| Optional external egress processor | `hack/experimental-additional-egress-extproc.sh`; `hack/install-ate.sh` |
| Gateway deployments | `manifests/ate-install/atenet-egress.yaml`; `manifests/ate-install/components/agentgateway` |
| Control-plane opt-in | `cmd/ateapi` (`--egress-gateway-address`); `manifests/ate-install/ate-api-server.yaml` |
| Egress policy API | `pkg/proto/ateapipb/ateapi.proto`; `cmd/ateapi/internal/controlapi/egress_policy.go` |
| Interception trust pipeline | `cmd/atecontroller` (`EgressMITMTrustReconciler`); `cmd/atelet` (`trustbundle.go`, `systemInfo` projection) |
| Demo and e2e | `demos/egress`; `internal/e2e/suites/{networking,egressauthz,egressmitm}` |
