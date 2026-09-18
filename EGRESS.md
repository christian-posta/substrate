# Actor egress through agentgateway

This document describes the agentgateway egress data plane shipped by Agent
Substrate. It follows an outbound TCP connection from an actor, through
`atunnel`, into the pinned agentgateway deployment, and then to the destination.
[IDENTITY.md](./IDENTITY.md) covers how the actor certificate is minted.

agentgateway is not the default. `hack/install-ate.sh --atenet-dataplane`
selects the dataplane for both the ingress router and the egress gateway, and
its default is `envoy`. The two gateways enforce the same control-plane
contract by different means: Envoy calls out to a co-located `atenet` ext_proc
sidecar written in Go, while agentgateway implements the checks natively in
Rust. Where they diverge, this document says so; `cmd/atenet/internal/router/README.md`
is the reference for the Envoy legs.

## Data path

```text
actor sandbox (untrusted)
+---------------------------------------------------------------+
| actor process                                                 |
|     |                                                         |
|     | normal TCP socket                                       |
|     v                                                         |
| actor network stack                                           |
|     eth0: 169.254.17.2/30                                     |
|     default route via 169.254.17.1                            |
+---------------- sandbox / worker boundary --------------------+
                          |
                          | packet crosses the veth boundary
                          v
trusted ateom worker pod network namespace
+---------------------------------------------------------------+
| ateom0: 169.254.17.1/30                                       |
|     |                                                         |
|     | nftables PREROUTING in the worker namespace             |
|     | matches source 169.254.17.2 + IPv4 TCP                  |
|     | REDIRECT to local port 15001                            |
|     v                                                         |
| atunnel: 0.0.0.0:15001                                       |
+---------------------------------------------------------------+
                          |
                          | mTLS + HTTP/1.1
                          | CONNECT <destination-IP>:<port>
                          v
atenet-egress.ate-system.svc:443
                          |
                          | Service targetPort -> agentgateway :8443
                          v
agentgateway outer listener (CONNECT frontend)
                          |
                          | terminates mTLS against the actor-identity CA
                          | substrateEgressActorResolution authorizes the actor
                          |   before the CONNECT upgrade is accepted
                          | preserves peer identity on re-entry
                          v
agentgateway inner listener (protocol: AUTO)
                          |
                          +-- clear HTTP: substrateEgress evaluates the
                          |     actor's EgressPolicy, then proxies HTTP
                          +-- TLS: inspect ClientHello/SNI, then pass through
                          |     (no per-request policy on this listener)
                          +-- other TCP: pass opaque bytes through
                          |     (no per-request policy on this listener)
                          v
destination
```

> [!WARNING]
> **The shipped data path is not a complete sandbox egress-control boundary.**
> `atunnel` intercepts IPv4 TCP sourced from the configured actor address.
> Forwarded actor UDP is dropped for every destination port but 53, which closes
> external QUIC/HTTP-3 and custom-UDP paths, but DNS to *any* host is still forwarded,
> and ICMP, SCTP, and every other IPv4 protocol still reach the compatibility
> masquerade. Traffic addressed to the worker network namespace traverses `INPUT`,
> for which this table has no filter chain, and there are no IPv6 rules at all.
> Those flows carry no actor mTLS
> identity to the gateway and receive none of the gateway's authorization,
> logging, TLS interception, or destination policy. Deployments must not
> describe this configuration alone as locking down all sandbox egress.

The nftables redirect is not installed inside the actor sandbox. `ateom`
installs it in the worker pod network namespace, on the trusted side of the
sandbox's veth boundary. `atunnel` also runs there. An actor process, including
one running as root, has no namespace access that would let it edit the
worker-side `ateom_actor` table. Actor-local routing or firewall changes can
break the actor's own connectivity, but cannot remove that table.

Both sandbox runtimes converge on this same worker-side enforcement point. The
gVisor runtime attaches its network stack to the interior side of the veth. The
microVM runtime connects the guest's virtio-net device through a TAP and traffic
control redirects to that veth. In both cases, externally-bound actor packets
arrive on worker-side `ateom0` before they can reach the worker pod's `eth0`.

This enforcement has important qualifications. Every rule matches IPv4 packets
by source address `169.254.17.2` and protocol. None of them match the ingress
interface or enforce source-address anti-spoofing, and the forward chain's
policy is still `accept`. Actor containers do not receive `NET_ADMIN` or
`NET_RAW` by default, but an ActorTemplate may explicitly grant them. A
network-privileged actor can then alter its sandbox network stack or construct
packets, and the current worker rules are not sufficient to claim robust
anti-bypass enforcement against that actor. Non-TCP traffic other than UDP also
bypasses `atunnel` through the compatibility masquerade described below. The
strong statement supported by the code is therefore: ordinary IPv4 TCP emitted
with the configured actor address is redirected outside the sandbox and cannot
bypass `atunnel` by editing actor-local nftables.

The worker installs the redirect only when its Run or Restore request contains
an egress gateway. With no gateway, `ateom` passes redirect port zero, installs
no TCP redirect, and actor TCP remains on the masquerade path. The UDP drop rule
is installed either way. The standard installation configures a gateway, but the
data-path guarantee is conditional on that configuration reaching the worker.

A stronger anti-bypass boundary would match traffic arriving on `ateom0`, drop
spoofed actor source addresses, default-deny forwarded actor traffic with narrow
exceptions such as the configured DNS resolver, define an IPv6 policy, and
either deny network-administration capabilities or test them as part of the
threat model. The repository does not implement that complete rule set today.

Four facts are central to the current implementation:

1. The actor container does not run an egress proxy. The worker's `atunnel`
   process receives traffic redirected from the actor network namespace.
2. `atunnel` authenticates to agentgateway with the actor's short-lived mTLS
   certificate.
3. The pinned agentgateway image authorizes the actor before it accepts the
   CONNECT upgrade: it parses the `ActorIdentity` certificate extension, calls
   `GetActor` on `ateapi`, compares the certified UID with the live one, and
   requires the actor to be `RUNNING`.
4. The actor's `EgressPolicy` is enforced, but **how completely depends on the
   dataplane**. Envoy evaluates it at the CONNECT itself, so an actor with no
   policy gets no tunnel of any kind. agentgateway attaches `substrateEgress` to
   the inner HTTP listener only, and its CONNECT frontend policy does not fetch
   the policy at all, so on agentgateway an actor with no policy is still
   refused cleartext HTTP but **can open TLS-passthrough and opaque-TCP egress
   to any address**. See [the default-deny caveat](#default-deny-is-not-uniform).

## Actor network namespace

Each actor receives a small, private network namespace created by its worker
runtime:

| Interface | Address | Role |
|---|---|---|
| worker-side `ateom0` | `169.254.17.1/30` | Default gateway and interception point |
| actor-side `eth0` | `169.254.17.2/30` | Actor network interface |

The actor's default route points through `169.254.17.1`. The `ateom` worker
installs one nftables table, `ateom_actor`, in its own pod network namespace.
It is created in the `ip` family, so **there are no IPv6 rules**; a code TODO
records that the actor network is IPv4-only for now. The table has three chains:

| Chain | Rule |
|---|---|
| `prerouting` (NAT) | IPv4 TCP from `169.254.17.2` is redirected to local port 15001. No destination or destination-port exclusions |
| `forward` (filter, policy `accept`) | UDP from `169.254.17.2` to any destination port **other than 53** is counted and dropped; everything else is accepted |
| `postrouting` (NAT) | everything from `169.254.17.2` that was still forwarded is masqueraded. The rule matches on source address only, with no protocol condition |

Rule order in the forward chain matters: the accept is a catch-all, so the drop
precedes it. The table has no `input` chain. Packets addressed to the worker
network namespace itself, including `169.254.17.1` and other local worker-pod
addresses, therefore do not traverse the UDP drop rule. No worker-local UDP
service is intentionally part of the egress path, but the nftables rules do not
make a blanket claim that all non-DNS actor UDP is blocked.

The redirect is transparent to the actor application. The application opens a
normal socket and does not need proxy environment variables or CONNECT support.

### TCP and non-TCP traffic differ

`atunnel` handles TCP only. What happens to everything else:

| Traffic from `169.254.17.2` | Result |
|---|---|
| TCP, any destination port | redirected into `atunnel`, tunneled, authorized |
| Forwarded UDP to port 53, **any** destination host | accepted and masqueraded |
| Forwarded UDP to any other port | counted and dropped |
| UDP and non-TCP traffic addressed to the worker namespace | not covered by the `forward` rule; reaches local `INPUT` if a service is listening |
| Forwarded ICMP, SCTP, GRE, ESP, any other IPv4 protocol | accepted and masqueraded |
| Anything over IPv6 | no rules; outside this table entirely |

Dropping forwarded non-DNS UDP closed the largest external hole: QUIC and HTTP/3
on UDP 443, and custom UDP or DTLS command-and-control channels, no longer leave
the worker through the forwarding path.
The drop rule carries a counter deliberately, so a workload that legitimately
needs UDP shows up as a rising counter rather than as an unexplained timeout.
Reading it takes a host with `nft` that can enter the worker pod's network
namespace — the worker image itself ships no `nft` binary, so
`kubectl exec … -- nft list table ip ateom_actor` does not work.

Two gaps remain in the same place. The DNS exception is still *any* destination
on port 53, not the configured cluster resolver, so an actor can reach an
attacker-controlled resolver. And the catch-all accept still passes every
non-TCP, non-UDP IPv4 protocol. Neither is covered by the gateway: the
configured agentgateway path does not authorize or observe DNS queries, and it
never sees the other protocols at all.

NOTE: We should keep a close eye on this implementation and how it evolves.

### Example non-TCP bypasses

An actor does not need special Linux capabilities to open a UDP socket. What
that still buys it:

- **DNS tunneling or exfiltration.** Still open. An actor can encode data in
  queries for an attacker-controlled domain, either through the configured
  resolver or by sending UDP port 53 traffic to any other reachable resolver.
  The worker rule selects on destination port alone, not on the resolver
  address. Agentgateway does not receive the queries and cannot associate them
  with the actor's mTLS identity.
- **QUIC or HTTP/3 over UDP port 443.** Closed for external destinations. The forward chain drops actor
  UDP to every port but 53, so an actor can no longer reach an external QUIC
  endpoint outside agentgateway's CONNECT, TLS, or HTTP processing.
- **Custom UDP or DTLS channels.** Closed on the external forwarding path for
  the same reason, on any port but 53. A worker-local listener remains outside
  that forward-chain rule.
- **Other IPv4 protocols.** Still open. The forward chain's catch-all accept
  passes ICMP, SCTP, GRE, ESP, and anything else the actor's stack can emit.
  Crafted raw-packet channels additionally require an ActorTemplate that grants
  `NET_RAW`, which is absent from the default actor capability set; ICMP does
  not.
- **IPv6.** Untouched. The table is IPv4-only, so nothing in this document
  applies to an actor with IPv6 connectivity.

These bypasses are independent of `EgressPolicy`. Policy enforcement at the
gateway covers only traffic that reaches the gateway; the worker must
separately restrict or route what does not, for that policy to form a
sandbox-wide egress boundary.

The namespace receives the node's generated `/etc/resolv.conf`. A typical flow
is therefore:

1. the actor resolves a hostname over UDP;
2. the actor connects to one of the returned IP addresses;
3. nftables redirects that TCP connection to `atunnel`;
4. `atunnel` sends the resolved IP address and port as the CONNECT authority.

The original hostname is absent from CONNECT unless the application protocol
reveals it later.

> [!CAUTION]
> This has the same two failure modes encountered with Istio-style transparent
> egress interception: non-TCP traffic is outside the proxy path, and L4
> interception sees a resolved destination IP rather than the hostname the
> application originally used.
>
> `EgressPolicy` addresses the second one directly, and the shape of the
> solution is worth understanding before writing a policy. A `hostnames` rule
> can only match where a hostname exists, which is inside the tunnel — the
> `Host` of a cleartext request, or an intercepted TLS request on the sdsmint
> gateway. At the CONNECT itself there is only an `IP:port`, so a `hostnames`
> rule never matches there and only `cidrs` and `all` rules can. An actor that
> dials its destination by address therefore needs a `cidrs` or `all` rule even
> when a `hostnames` rule names the same server; this is what
> `TestActorEgressPolicyDeniesUnlistedHost` pins.
>
> The first failure mode is still a worker-side problem, not a policy one. See
> the table above for what leaves the worker without reaching the gateway.

## Fail-closed behavior of intercepted TCP

The fail-closed property in this section is limited to IPv4 TCP selected by the
worker's redirect rule. It does not cover the non-TCP compatibility masquerade.

The worker runtime sets up egress in this order:

1. request an actor certificate from `ateapi`;
2. create the actor network namespace and install the TCP redirect;
3. start or restore the actor containers; the worker's `atunnel` listener is already running;
4. wait for actor readiness;
5. activate the tunnel.

Before activation, redirected TCP connections are accepted locally and closed.
They do not bypass the gateway. If certificate issuance or tunnel setup fails,
actor startup fails rather than allowing direct TCP egress.

Deactivation closes active streams. Certificate expiry prevents new tunnels,
and certificate renewal is attempted at 90 percent of the certificate lifetime.
The configured actor certificate lifetime is one hour.

## `atunnel`

`atunnel` binds `0.0.0.0:15001` in the worker pod network namespace. In the
normal transparent flow, the actor does not address that listener or use it as
an explicit proxy. The actor connects to the original destination IP and port;
after the packet crosses the sandbox boundary, the worker's nftables REDIRECT
rule delivers it to local port 15001 while preserving the original destination
in connection-tracking state.

For each connection accepted through that redirect, `atunnel`:

1. recovers the socket's original destination;
2. formats that destination as an IP address and port;
3. establishes mTLS to `atenet-egress.ate-system.svc:443` using the actor
   certificate;
4. sends an HTTP/1.1 `CONNECT <IP>:<port>` request;
5. relays bytes in both directions after a successful response.

Both runtimes expose the same actor IP and gateway and converge on the same
worker-side veth. Their sandbox-specific path to that veth differs:

| Runtime | Path from the actor network stack to the veth |
|---|---|
| gVisor | gVisor `eth0` -> AF_PACKET endpoint -> interior `eth0` |
| microVM | guest `eth0` -> virtio-net -> TAP -> traffic-control redirect -> interior `eth0` |

The interior `eth0` is paired with worker-side `ateom0`:

```text
actor sandbox                  interior netns       ateom worker pod netns
eth0: 169.254.17.2/30  ------> eth0  <--- veth ---> ateom0: 169.254.17.1/30
                                                            |
                                                 atunnel: 0.0.0.0:15001
```

The gVisor actor cannot inspect or administer the worker namespace, and the
microVM guest cannot inspect or administer the host-side interior or worker
namespaces. Both can still send IP traffic through their virtual network path to
addresses exposed on the other side. Namespace and VM isolation protect network
configuration and ownership; the virtual NIC, TAP, and veth deliberately
provide network connectivity across those boundaries. For an external
destination, `169.254.17.1` is the next-hop gateway while the packet's IP
destination remains external. For a deliberate connection to
`169.254.17.1:15001`, that gateway address is itself the IP destination. Both
packets reach worker-side `ateom0`.

There is a separate reachability detail: `169.254.17.1` is the actor's adjacent
gateway address, `atunnel` uses a wildcard bind, and the worker rules do not
include an input filter that hides port 15001. A deliberate actor connection to
`169.254.17.1:15001` is therefore technically able to reach the listener. It is
not a configured proxy interface: `atunnel` still derives the CONNECT authority
from the kernel's original-destination state rather than accepting a
caller-supplied target. If the intended security property is that actors cannot
address the listener directly, the current bind and nftables rules do not
provide that property.

> [!CAUTION]
> Track direct addressing of the egress listener as a hardening question and
> test it for both gVisor and microVM workers. Because the PREROUTING rule sends
> every selected actor TCP connection to port 15001 before the INPUT path, an
> actor connection originally addressed to worker ports 443, 8443, or 8080 also
> reaches the egress `atunnel`, not the worker service bound to that original
> port. The concern is therefore limited to how `atunnel` handles worker-local
> original destinations and adversarial connection patterns; the other
> wildcard-bound TCP listeners are not directly exposed through this path while
> the redirect is installed. If worker-local destinations have no valid egress
> use, `atunnel` or the worker rules should reject them explicitly.

`atunnel` does not add actor identity headers to CONNECT. Authentication comes
from the client certificate. The relay supports TCP half-close so protocols that
depend on an EOF in one direction can complete normally.

The default install supplies the gateway address with:

```text
--egress-gateway-address=atenet-egress.ate-system.svc:443
```

## What agentgateway can observe

Visibility depends on the application protocol inside CONNECT.

| Traffic | Information visible to agentgateway | Information protected from agentgateway |
|---|---|---|
| Any tunneled TCP | Actor mTLS identity and CONNECT destination IP:port | None of the connection metadata listed at left |
| Clear HTTP | Method, host, path, headers, and body | Nothing at the HTTP layer |
| HTTPS passthrough | TLS ClientHello metadata, normally including SNI; connection sizes and timing | HTTP method, path, headers, body, and response content |
| Other TCP | CONNECT destination plus connection sizes and timing | Application bytes, when the application encrypts them |

The outer listener sees the CONNECT request. After CONNECT, it forwards the
stream into an inner listener configured with `protocol: AUTO`. The inner
listener selects an HTTP, TLS, or generic TCP route.

For HTTPS passthrough, destination information can therefore appear in two
places:

- CONNECT carries the resolved destination IP and port;
- the TLS ClientHello commonly carries the hostname as SNI.

These values need not agree, and nothing compares them. On agentgateway the TLS
listener carries no policy at all, so a passthrough connection is authorized by
its CONNECT address alone whatever SNI it then presents. On Envoy the
passthrough chain is likewise decided by address at the CONNECT. Only a leg the
gateway terminates — cleartext HTTP, or TLS under the interception overlay —
decides on a name, and there the name it uses is the request's `Host`, not the
ClientHello's SNI.

## Shipped agentgateway deployment

The `agentgateway-egress` installation overlay deploys one agentgateway
container using:

```text
ghcr.io/agentgateway/agentgateway:v0.0.0-alpha.9f9744cf
```

> [!IMPORTANT]
> That is an unreleased commit build, not a release tag: `9f9744cf` is upstream
> agentgateway's `substrate: Fix custom port (#3428)` of 2026-09-10. The
> repository moved off `cr.agentgateway.dev/agentgateway:v1.5.0` because the
> Substrate egress policies this configuration depends on landed after that
> tag. Security claims about this data path are claims about that exact commit,
> and they do not transfer to either the older release or upstream `main`.

The Kubernetes Service exposes port 443 and targets the container's named port,
which listens on 8443. The egress pod does not contain an `atenet` sidecar; the
kustomize component removes it. The container runs with `RUST_LOG=debug`.

The checked-in configuration lives in the
`atenet-egress-agentgateway-substrate-config` ConfigMap. It defines two logical
stages in the same process, plus one frontend policy that runs before either.

### Outer listener

The outer listener:

- binds to port 8443 with `tunnelProtocol: connect`;
- terminates mTLS using the gateway serving certificate;
- trusts the actor identity CA bundle mounted at
  `/run/actor-id-ca-certs/ca.crt`;
- runs the `substrateEgressActorResolution` frontend policy **before** it
  accepts the CONNECT upgrade;
- sends the CONNECT stream to the inner listener;
- attaches the resolved `ActorIdentity` to the upgraded socket, so the inner
  listener does not have to re-parse the certificate.

A certificate from an unrelated CA fails during the TLS handshake, before any
policy runs. A certificate from the right CA but for a deleted, replaced, or
suspended actor fails in `substrateEgressActorResolution`, with a 403.

### Inner listener

The inner listener uses automatic protocol detection:

- the HTTP route applies the `substrateEgress` policy and forwards to the
  CONNECT authority;
- the TLS route passes encrypted TLS traffic to the CONNECT authority;
- the TCP route passes all remaining streams to the CONNECT authority.

The `substrateEgress` policy is attached to the HTTP route only. TLS
passthrough and generic TCP therefore carry no per-request `EgressPolicy`
decision and no actor fields in their access-log lines; both are authorized by
the CONNECT check alone. The e2e suite records this as a gap rather than a
design choice — `agentGatewayAtenetDataplane.SupportsTLSPassthroughEgressPolicy`
returns false with a TODO, and `TestActorEgressPolicyDeniesUnlistedHost`'s
passthrough case is skipped on this dataplane.

## What the gateway policies actually do

The image pinned by this repository must be the reference point for security
claims. Two distinct policies run, at two different points.

### `substrateEgressActorResolution` (CONNECT frontend)

It runs inside `terminate_connect_tunnel`, *before* the CONNECT upgrade is
accepted, and it:

1. takes the PEM of the peer certificate the TLS layer already verified;
2. finds the `ActorIdentity` extension by OID `1.3.6.1.4.1.11129.2.12.2` and
   rejects a certificate carrying more than one;
3. parses the extension value as **raw JSON** — Substrate writes it as JSON, not
   as a DER-wrapped payload, so a non-Go verifier needs no ASN.1 library;
4. validates that `Atespace` and `ActorName` are well-formed resource names,
   that `ActorUid` is non-empty, and that `Purpose` is exactly `atunnel`;
5. calls `GetActor` on `api.ate-system.svc:443` over mTLS, with the gateway's
   own pod-identity certificate;
6. requires the live actor's UID to equal the certified `ActorUid`;
7. requires the live actor's state to be `RUNNING`.

Any failure refuses the tunnel. The refusal distinguishes two cases, which
matters operationally: a denial answers **403**, while a control-plane
`Unavailable` or `DeadlineExceeded` answers **503**. A control-plane outage
therefore blocks every new tunnel rather than failing open.

Note what it does *not* read: the SPIFFE URI SAN. agentgateway authorizes from
the `ActorIdentity` extension alone. The Envoy path does both — it
cross-checks the URI SAN against the extension and refuses a certificate whose
single URI SAN is not exactly `resources.ActorSPIFFEID(ref)`.

### `substrateEgress` (inner HTTP route)

For each request inside the tunnel, it:

1. reads the `ActorIdentity` that the CONNECT policy attached to the socket —
   it does not re-parse the certificate. Without that frontend policy every
   request is refused with 403 `missing CONNECT-authorized actor identity`;
2. records `ate.actor.name`, `ate.actor.uid` and `ate.atespace` on the access
   log line;
3. fetches the actor's `EgressPolicy` from the control plane;
4. walks the rules in order and lets the first match decide, denying with 403
   when none matches. The body names which case it was:
   `actor egress policy denied destination` when the rules did not match, and
   `actor egress policy denied: ... EgressPolicy not found` when the actor has
   no policy at all.

It does **not** apply a matched rule's `inject_static_headers` effects. At this
pinned commit that is an explicit TODO: the matched rule is discarded. Upstream
agentgateway implements injection in a later change than this pin. Credential
injection on Substrate today is an Envoy-only feature; see below.

## `EgressPolicy` enforcement

`EgressPolicy` is no longer an unwired API. Both dataplanes read it from the
control plane and enforce it. How completely they do so differs, and the
difference is security-relevant — see the caveat immediately below before
relying on default-deny.

### Default-deny is not uniform

On **Envoy**, "an actor with no policy gets no tunnel" is literally true: the
CONNECT leg itself calls `lookupPolicy`, and `errNoPolicy` answers 403 before
any byte is relayed.

On **agentgateway** it is true only of the routes `substrateEgress` is attached
to, which is the inner HTTP listener alone. `substrateEgressActorResolution`,
the CONNECT frontend policy, authorizes the *actor* and never fetches the
policy. So an authenticated actor with **no `EgressPolicy` at all** is refused
cleartext HTTP and still gets TLS-passthrough and opaque-TCP egress to any
address it can name.

Observed on a kind cluster, same actor, no policy, both legs:

```text
# cleartext HTTP -> denied
error request ... route=default/route0 http.status=403 protocol=http
  ate.actor.name=alpha ate.actor.uid=a731bf64-...
  error="actor egress policy denied: ... \"EgressPolicy not found\"" reason=Authorization

# TLS passthrough, same actor, still no policy -> relayed
info request ... route=default/tcproute0 endpoint=10.96.0.1:443 tls.sni= protocol=tcp
  duration=39ms substrate.connect.authority="10.96.0.1:443"
```

The second request reached the destination: the actor's own error was
`x509: certificate signed by unknown authority`, which it could only produce
after the origin presented a certificate.

Treat agentgateway's default-deny as covering policy-evaluated routes, not the
tunnel. An actor whose egress must be confined has to be on Envoy, or on the
sdsmint gateway where TLS is terminated and therefore policy-evaluated. The
repository tracks the gap as
`agentGatewayAtenetDataplane.SupportsTLSPassthroughEgressPolicy` returning false.

### What a rule can match

A policy is an ordered list of at most 256 rules, evaluated first-match-wins,
with no match meaning deny. Each rule carries exactly one matcher:

| Matcher | Matches on | Notes |
|---|---|---|
| `hostnames` | the request's hostname | exact lowercase DNS names, or `*` replacing the complete leftmost label. Only matcher that can carry effects |
| `cidrs` | the resolved destination IP | canonical IPv4/IPv6 prefixes. Renamed from `IPBlockRule` |
| `all` | everything | |

There is **no port matching and no method matching**. A rule cannot allow
`:443` and deny `:8080` on the same host, and the request method is logged but
never matched.

Because a `hostnames` rule needs a hostname, it can only match inside the
tunnel. See the caution above for what that means when an actor dials by IP.

### Where each dataplane enforces it

| | Envoy | agentgateway |
|---|---|---|
| Enforced by | the co-located `atenet` ext_proc sidecar (`--mode=egress`) | the proxy itself, in Rust |
| CONNECT leg | `egress` filter chain | `substrateEgressActorResolution` |
| Cleartext HTTP inside the tunnel | `egress_cleartext` chain, all rules | `substrateEgress` on the inner HTTP route |
| Intercepted TLS (sdsmint only) | `egress_tls_mitm` chain, all rules | inner HTTPS route under the MITM overlay |
| TLS passthrough and opaque TCP | decided by address at the CONNECT; with no allowed address the ORIGINAL_DST cluster has nothing to dial and the connection closes | **not policy-enforced** |
| Denial | 403 with the fixed body `egress denied`; the reason is in the sidecar log | 403 with a body that names the reason, all starting `actor egress policy denied` |
| Policy caching | per-actor, `--egress-policy-cache-ttl`, 10s default; 0 disables | internal to the proxy |

On Envoy the TTL is exactly how stale a decision can be: a create, update or
delete becomes visible to new requests within one TTL, and a deleted policy
becomes a deny.

One Envoy behavior is worth knowing because it looks like a hole and is not. A
CONNECT that no address rule allows still opens when the policy has `hostnames`
rules, because a request inside it may be allowed by name; the tunnel opens with
no address to dial, and the passthrough chains close the connection before a
byte is relayed. It is refused outright when the policy has no hostname rules,
and on any dataplane that calls out for the CONNECT alone.

### Creating a policy

```bash
# Allow everything — the pre-policy behavior.
kubectl ate create egress-policy egress-demo -a ate-demo-egress --all

# Allow one hostname, and one CIDR by address.
kubectl ate create egress-policy egress-demo -a ate-demo-egress \
  --hostnames api.example.com --cidrs 10.244.0.0/16

kubectl ate get egress-policy egress-demo -a ate-demo-egress
```

Rules are created `--hostnames` first, then `--cidrs`, then `--all`, whatever
order the flags appear in: the CLI receives them grouped by flag, not
interleaved, so it cannot reconstruct a mixed ordering. That puts the
catch-all last, which is usually what you want, but a policy whose rules must
interleave has to be written as a manifest and passed with `-f`. Each actor has
at most one policy, named `default`; deleting the actor deletes it.

## Credential injection

An `EgressRuleEffects.inject_static_headers` entry on a **`hostnames`** rule
names a header, a literal prefix, and a `credential_uri`. `cidrs` and `all`
rules have no effects field, so they can never inject.

This is implemented on Envoy only.

- The gateway does not read Kubernetes Secrets. It calls a pluggable gRPC
  `CredentialProvider` (`pkg/proto/credproviderpb`) with the credential URI and
  the actor's attested SPIFFE ID and resolves the secret itself. The normal dial
  uses mutual TLS; an explicit `--credential-provider-insecure` development
  option permits plaintext and logs a warning.
- The SPIFFE ID sent to the provider is the current name-based value,
  `spiffe://substrate-actor.local/atespace/<atespace>/actor/<name>`. It omits the
  actor UID. The gateway verified the certificate's UID against the live actor
  at CONNECT, but the provider request cannot independently distinguish an
  actor deleted and recreated at the same name. The provider is therefore
  trusting the authenticated gateway's assertion for this request, not
  receiving an incarnation-bound subject of its own.
- **No provider implementation ships in this repository** — only the proto. The
  default URI class is `ate-secret://kubernetes.io` and the default address is
  `credprovider.ate-system.svc:50051`, but the workload behind that address is
  deployed separately. `hack/install-ate.sh` splices the client flags into the
  egress sidecar; it deploys no provider.
- Injection happens on the `egress_tls_mitm` leg only. On a cleartext leg, or
  with no provider configured, it is **skipped and the request is allowed
  through without the credential** — the reasoning being not to put a secret on
  a cleartext wire, and not to block egress the policy allowed.
- Once attempted it fails closed: an unusable header name, an unparseable URI,
  a URI naming a different provider class, or a fetch failure all deny.
- The injected header is set with overwrite semantics, so a value the actor
  pre-seeded does not survive.

The installer hard-errors on `--experimental-egress-credential-injection` with
`--atenet-dataplane=agentgateway`, and it implies `--experimental-use-sdsmint`.

> [!NOTE]
> `cmd/atenet/internal/router/README.md` and `demos/egress/README.md` both
> described injection as unimplemented and answering 501, because neither was
> updated when it landed. Both are corrected on this branch; the code remains
> the reference.

## Identity and trust material

The egress path uses two distinct certificate relationships.

| Relationship | Presenter | Verifier | Purpose |
|---|---|---|---|
| Actor identity | `atunnel` | agentgateway outer listener | Authenticate the actor tunnel |
| Gateway service identity | agentgateway | `atunnel` | Authenticate the gateway endpoint |

The actor certificate contains a SPIFFE URI SAN and a custom ActorIdentity
extension. The control plane produces certificates with an `atunnel` purpose.
The pinned gateway validates the certificate chain at the TLS layer, then
authorizes the connection from the ActorIdentity extension. It ignores the URI
SAN entirely; the Envoy dataplane is the one that cross-checks the two.

The actor CA signing pool is the `ate-system/actor-id-ca-pool` Secret, mounted
only into ateapi. The install path derives its public root into the separate
`ate-system/actor-id-ca-certs` Secret under the `ca.crt` key. That cert-only
Secret is mounted into the gateway at `/run/actor-id-ca-certs/ca.crt`. Rotating
the signing pool therefore requires regenerating the derived Secret and
restarting or otherwise reloading the gateway; the install path does not
continuously reconcile the copy.

The gateway serving certificate and key come from the projected
`servicedns.podcert.ate.dev/identity` PodCertificate at
`/run/servicedns.podcert.ate.dev/credential-bundle.pem`. `atunnel` verifies that
certificate against its projected Service DNS ClusterTrustBundle and the
expected `atenet-egress.ate-system.svc` server name when establishing the outer
TLS connection.

## Optional TLS interception overlay

The `agentgateway-egress-mitm` overlay changes the inner TLS route from
passthrough to HTTPS interception with a dynamic certificate authority. It
mounts `ate-system/egress-mitm-ca-pool` at:

```text
/run/egress-mitm/tls.crt
/run/egress-mitm/tls.key
```

With this overlay, agentgateway can terminate destination TLS, observe and proxy
HTTP, and establish a separate TLS connection to the destination. Actors must
trust the interception CA for this to succeed without application TLS errors.
atecontroller publishes that trust as ClusterTrustBundle
`egress-mitm.ate.dev:mitm:primary-bundle`; a template must explicitly project
the logical `egress-mitm.ate.dev` trust bundle into its `systemInfo` volume.
atelet refreshes that file for registered running actors when the
ClusterTrustBundle changes.

The shell installer selects this overlay and creates the CA pool when
`--atenet-dataplane=agentgateway --experimental-use-sdsmint` is used. The Go
`ate-setup` implementation currently does neither for agentgateway:
`renderAtenetEgressManifest` returns `agentgateway-egress` unconditionally for
that dataplane — `agentgateway-egress-mitm` is referenced only by
`hack/install-ate.sh` — and `EnsureEgressMITMCAPoolSecret` returns early when
the dataplane is agentgateway. Until those paths converge, use the shell
installer or apply and provision the MITM overlay explicitly.

Interception widens what the policy can see, not what it enforces: a TLS
request the gateway terminates is decided per request like a cleartext one,
instead of by address at the CONNECT. Generic non-TLS TCP remains passthrough
either way, and actor and UID authorization at CONNECT is unchanged.

## Verification

The following checks target only the agentgateway deployment.

### Confirm the installed image

```bash
kubectl -n ate-system get deployment atenet-egress \
  -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"\t"}{.image}{"\n"}{end}'
```

Expected container and image, with no second container:

```text
agentgateway    ghcr.io/agentgateway/agentgateway:v0.0.0-alpha.9f9744cf
```

An `envoy` plus `ext-proc` pair means the install selected the Envoy dataplane,
and nothing below applies.

### Inspect the active configuration

```bash
kubectl -n ate-system get configmap \
  atenet-egress-agentgateway-substrate-config -o yaml
```

Confirm the `substrateEgressActorResolution` frontend policy, the outer CONNECT
listener, the inner `protocol: AUTO` listener, and the HTTP/TLS/TCP routes
described above. Configuration must be checked along with the image version:
the image decides whether a policy is implemented, and the configuration decides
where it is attached and therefore which protocols it covers. A build that
implements CONNECT-time authorization still does none if
`substrateEgressActorResolution` is not configured.

### Exercise the local demo

```bash
kubectl ate create egress-policy egress-demo -a ate-demo-egress --all
demos/egress/test-egress.sh
```

The policy is not optional. The gateway denies by default, so without it the
positive case fails with 403 and the script reports no CONNECT.

The positive case sends clear HTTP through the actor tunnel and demonstrates:

- actor TCP interception;
- successful actor-CA authentication;
- CONNECT through the agentgateway Service;
- CONNECT-time actor resolution against `ateapi`;
- an HTTP request reaching the destination;
- the actor's name, UID and atespace on the gateway's access-log line.

The negative case presents a certificate from the pod identity CA. A TLS
`UnknownIssuer` failure demonstrates that the gateway rejects a client chain
outside the configured actor CA — before any policy runs.

This demo does not demonstrate:

- rejection of a deleted, replaced, or non-running actor;
- denial of a destination the policy does not name;
- TLS interception policy;
- credential injection.

`demos/egress/multi-actor-identity.sh` covers the first of those, across several
actors sharing one worker.

### Probe direct addressing of worker-side `atunnel`

With the demo actor running, forward the ingress router in one terminal:

```bash
kubectl -n ate-system port-forward service/atenet-router 18101:80
```

The actor's `EgressPolicy` has to allow this destination, or the probe is denied
at the policy rather than at the dial and proves nothing about reachability. An
`--all` policy is the simplest way to take the policy out of the picture:

```bash
kubectl ate create egress-policy egress-demo -a ate-demo-egress --all
```

Then ask the actor to connect to the worker-side gateway address and
egress-listener port:

```bash
curl -i -X POST http://127.0.0.1:18101/ \
  -H 'Host: egress-demo.ate-demo-egress.actors.resources.substrate.ate.dev' \
  -H 'Content-Type: application/json' \
  -d '{"url":"http://169.254.17.1:15001/"}'
```

Then inspect recent agentgateway logs:

```bash
kubectl -n ate-system logs deployment/atenet-egress -c agentgateway \
  --since=1m | grep '169.254.17.1:15001'
```

A matching log entry proves the following chain occurred:

1. the actor addressed `169.254.17.1:15001`;
2. the packet crossed the sandbox boundary and reached the worker's redirect;
3. worker-side `atunnel` accepted the connection and recovered
   `169.254.17.1:15001` as its original destination;
4. `atunnel` authenticated to agentgateway and sent that value as the CONNECT
   authority.

The actor receives `503 Service Unavailable`, because agentgateway cannot
connect to that worker-local address from the gateway pod. That status is not
the reachability proof; the authenticated agentgateway log containing the exact
CONNECT authority is. Observed:

```text
error request ... endpoint=169.254.17.1:15001 src.addr=10.244.0.21:44062
  http.status=503 ate.actor.name=egress-demo ate.actor.uid=74ace1cf-...
  ate.atespace=ate-demo-egress error="upstream call failed: Connect: Connection
  refused (os error 111)" reason=UpstreamFailure
  substrate.connect.authority="169.254.17.1:15001"
``` The standard `egress` demo actor uses gVisor. Repeat the
same probe with the microVM demo template to verify the equivalent live path for
that runtime.

### Inspect logs

```bash
kubectl -n ate-system logs deployment/atenet-egress -c agentgateway
```

A request the gateway could read logs one `info request` line carrying both
agentgateway's own fields and this repository's. Actual output, trimmed:

```text
info request request.id=5 gateway=default/default listener=listener1 route=default/route0
  endpoint=10.96.154.103:80 src.addr=10.244.0.21:38142 http.method=GET
  http.host=10.96.154.103 http.path=/ http.status=200 protocol=http
  ate.actor.name=echo ate.actor.uid=6ef041ba-4708-4bd9-9c25-bcca8e90fec1
  ate.atespace=ate-demo-egress duration=3ms
  substrate.connect.authority="10.96.154.103:80"
```

`substrateEgress` sets the three `ate.*` fields, which are built into
agentgateway; `substrate.connect.authority` is the one this repository's
ConfigMap adds through `accessLog.add`, and it is the only quoted value.
`src.addr` is the worker pod's address — the actor's own address never leaves
the sandbox.

Those actor fields come from the HTTP route, so **passthrough TLS and opaque TCP
lines carry the authority and no actor fields at all**. The same actor, dialing
a TLS destination by address, logs only:

```text
info request gateway=default/default listener=listener0 route=default/tcproute0
  endpoint=10.96.0.1:443 src.addr=10.244.0.21:34318 tls.sni= protocol=tcp
  duration=29ms substrate.connect.authority="10.96.0.1:443"
```

`route=…/tcproute0` and `protocol=tcp` are the tell. Do not read the missing
actor fields as an unauthenticated connection — the CONNECT was authorized the
same way — and do not infer HTTP-layer visibility or a per-request policy
decision from a successful connection log. `tls.sni` is empty here because the
actor dialed an address, which is the normal case: DNS resolves in the clear
before the tunnel.

### Run focused unit tests

```bash
go test ./internal/atunnel ./internal/egresspolicy \
  ./cmd/atenet/internal/router/egress ./cmd/ateapi/internal/controlapi/... \
  -count=1
```

These cover the tunnel, policy evaluation, the Envoy egress handler, and the
control-plane API. The nftables rules are `//go:build linux`, so they need a
Linux host or a container:

```bash
go test ./internal/ateomnet -count=1   # linux only
```

None of this substitutes for an integration test against the exact agentgateway
image and configuration: nothing in this repository exercises the Rust policies
except the e2e suite.

## Current capabilities and gaps

Worker-side, which is the same on both dataplanes:

| Capability | Status |
|---|---|
| Complete sandbox network-egress lockdown | Not implemented |
| Transparent expected-source actor IPv4/TCP interception | Implemented |
| Fail-closed path for redirected TCP before tunnel activation | Implemented |
| Anti-bypass enforcement against a network-privileged actor | Not established; source-only match and an `accept` forward policy need hardening |
| Forwarded actor UDP confined to DNS | Implemented, but to any host on port 53, not the configured resolver; worker-local `INPUT` is not covered |
| ICMP and other IPv4 protocols | Not enforced; the catch-all accept passes them |
| IPv6 | No rules at all |

Gateway-side:

| Capability | Envoy (default) | agentgateway |
|---|---|---|
| Actor mTLS certificate required | Implemented | Implemented |
| ActorIdentity extension and `atunnel` purpose checked at CONNECT | Implemented | Implemented |
| URI SAN cross-checked against the extension | Implemented | Not done; the SAN is ignored |
| `GetActor` lookup, UID match and `RUNNING` check at CONNECT | Implemented | Implemented |
| Control-plane outage fails closed | Implemented (503) | Implemented (503) |
| `EgressPolicy` on cleartext HTTP | Implemented | Implemented |
| `EgressPolicy` on TLS passthrough and opaque TCP | Implemented, by address at the CONNECT | **Not implemented** |
| `EgressPolicy` on intercepted TLS | Implemented (sdsmint) | Implemented (MITM overlay) |
| Port or method matching in a policy | Not in the API | Not in the API |
| Actor identity on access-log lines | Implemented, on every leg | HTTP route only |
| Clear HTTP parsing and proxying | Implemented | Implemented |
| TLS ClientHello/SNI inspection with payload passthrough | Implemented | Implemented |
| TLS interception overlay | `--experimental-use-sdsmint` | Same flag; shell installer only, not Go `ate-setup` |
| Credential injection | Implemented; needs an out-of-tree provider | Not implemented at the pinned commit |
| DNS authorization through the gateway | Not implemented; DNS uses the worker-side bypass | Same |

## Code and manifest map

| Area | Path |
|---|---|
| Actor network namespace and redirect | `internal/ateomnet/` |
| Tunnel implementation | `internal/atunnel/` |
| Actor certificate request and renewal | `internal/atunnel/` and `cmd/atelet/ateomsupport.go` |
| Actor certificate issuance | `cmd/ateapi/internal/controlapi/actor.go` |
| Actor SPIFFE ID construction and parsing | `internal/resources/spiffe.go` |
| Envoy egress handler: identity, policy, injection | `cmd/atenet/internal/router/egress/` |
| Envoy egress leg reference | `cmd/atenet/internal/router/README.md` |
| Policy compilation and evaluation | `internal/egresspolicy/` |
| Credential-provider plugin API | `pkg/proto/credproviderpb/` |
| Envoy egress listener configuration | `manifests/ate-install/atenet-egress.yaml` and `atenet-egress-with-sdsmint.yaml` |
| agentgateway image and Deployment patches | `manifests/ate-install/components/agentgateway/` |
| agentgateway egress configuration | `manifests/ate-install/components/agentgateway/configmap.yaml` |
| Standard agentgateway egress overlay | `manifests/ate-install/agentgateway-egress/` |
| Composed TLS interception overlay | `manifests/ate-install/agentgateway-egress-mitm/` |
| TLS interception component patch | `manifests/ate-install/components/agentgateway-egress-mitm/` |
| Egress demo | `demos/egress/` |
| Egress control-plane API | `pkg/proto/ateapipb/` and `cmd/ateapi/internal/controlapi/egress_policy.go` |
| Egress policy CLI | `cmd/kubectl-ate/internal/cmd/create_egress_policy.go` |
