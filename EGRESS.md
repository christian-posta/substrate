# Actor egress through agentgateway

This document describes the agentgateway egress data plane shipped by Agent
Substrate. It follows an outbound TCP connection from an actor, through
`atunnel`, into the pinned agentgateway deployment, and then to the destination.
It also distinguishes behavior present in this repository from behavior that is
only planned or available in later upstream agentgateway changes.

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
agentgateway outer listener
                          |
                          | authenticates an actor certificate
                          | accepts the CONNECT tunnel
                          | preserves peer identity on re-entry
                          v
agentgateway inner listener (protocol: AUTO)
                          |
                          +-- clear HTTP: parse and proxy HTTP
                          +-- TLS: inspect ClientHello/SNI, then pass through
                          +-- other TCP: pass opaque bytes through
                          v
destination
```

> [!WARNING]
> **The shipped data path is not a complete sandbox egress-control boundary.**
> `atunnel` intercepts IPv4 TCP sourced from the configured actor address. The
> worker currently forwards and masquerades all other IPv4 protocols, so UDP,
> QUIC/HTTP/3, DTLS, ICMP, and custom non-TCP protocols bypass both `atunnel`
> and agentgateway. Those flows carry no actor mTLS identity to the gateway and
> receive no gateway authorization, destination logging, TLS interception, or
> destination-policy enforcement placed at the gateway. In addition, the pinned
> agentgateway v1.5.0 configuration does not yet enforce `EgressPolicy` even for
> intercepted TCP. Deployments must not describe this configuration alone as
> locking down all sandbox egress.

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

This enforcement has important qualifications. The current nftables redirect
matches IPv4 packets by source address `169.254.17.2` and TCP protocol. It does
not match the ingress interface, enforce source-address anti-spoofing, or use a
default-deny forwarding policy. Actor containers do not receive `NET_ADMIN` or
`NET_RAW` by default, but an ActorTemplate may explicitly grant them. A
network-privileged actor can then alter its sandbox network stack or construct
packets, and the current worker rules are not sufficient to claim robust
anti-bypass enforcement against that actor. Non-TCP traffic also bypasses
`atunnel` through the compatibility masquerade described below. The strong
statement supported by the code is therefore: ordinary IPv4 TCP emitted with
the configured actor address is redirected outside the sandbox and cannot
bypass `atunnel` by editing actor-local nftables.

The worker installs the redirect only when its Run or Restore request contains
an egress gateway. With no gateway, `ateom` passes redirect port zero, installs
no TCP redirect, and actor traffic remains on the masquerade path. The standard
installation configures a gateway, but the data-path guarantee is conditional
on that configuration reaching the worker.

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
3. The pinned agentgateway v1.5.0 image extracts the actor name and Atespace from
   the certificate URI SAN for HTTP request metadata and logging. It does not
   call `ateapi` to prove that the certificate represents the current, running
   actor incarnation.
4. No configured component currently enforces the `EgressPolicy` API against a
   destination. Once the tunnel is accepted, the configured routes proxy the
   connection.

## Actor network namespace

Each actor receives a small, private network namespace created by its worker
runtime:

| Interface | Address | Role |
|---|---|---|
| worker-side `ateom0` | `169.254.17.1/30` | Default gateway and interception point |
| actor-side `eth0` | `169.254.17.2/30` | Actor network interface |

The actor's default route points through `169.254.17.1`. The `ateom` worker
installs nftables rules in its own pod network namespace:

- IPv4 TCP sourced from `169.254.17.2` is redirected to local port 15001;
- non-TCP forwarded traffic is accepted and masqueraded;
- there are no destination or destination-port exclusions in the redirect
  rule;
- the forward chain has an accept policy and the masquerade rule matches the
  configured actor source address.

The redirect is transparent to the actor application. The application opens a
normal socket and does not need proxy environment variables or CONNECT support.

### TCP and non-TCP traffic differ

`atunnel` handles TCP only. The current worker rules forward and masquerade all
IPv4 non-TCP traffic sourced from `169.254.17.2`; UDP, ICMP, and other IPv4
protocols therefore bypass both `atunnel` and agentgateway. Keeping DNS over UDP
working is the reason stated in the code, but the rule is broader than DNS. A
code TODO calls this a compatibility masquerade and says it should be restricted
to the configured DNS resolver while other non-tunneled traffic is dropped.

Consequently, the configured agentgateway path does not authorize or observe
DNS queries, and the current implementation also permits other non-TCP IPv4
egress outside that path.

NOTE: We should keep a close eye on this implementation and how it evolves.

### Example non-TCP bypasses

An actor does not need special Linux capabilities to open a UDP socket. The
current compatibility masquerade therefore enables examples such as:

- **DNS tunneling or exfiltration.** An actor can encode data in queries for an
  attacker-controlled domain, either through the configured resolver or by
  sending UDP port 53 traffic to another reachable resolver. Agentgateway does
  not receive the queries and cannot associate them with the actor's mTLS
  identity.
- **QUIC or HTTP/3 over UDP port 443.** An actor can communicate with an
  external QUIC endpoint without traversing agentgateway's CONNECT, TLS, or
  HTTP processing. Gateway-side hostname rules, HTTP inspection, TLS
  interception, and request logs would not cover that flow.
- **Custom UDP or DTLS channels.** An actor can send data or maintain a
  command-and-control channel using an attacker-controlled UDP protocol. Using
  a common allowed-looking port does not change the path because the worker
  redirect selects TCP by protocol rather than by destination port.

ICMP and crafted raw-packet channels are additional possibilities when an
ActorTemplate grants `NET_RAW`; that capability is absent from the default
actor capability set.

These bypasses are independent of the unfinished `EgressPolicy`
implementation. Completing policy enforcement at agentgateway will cover only
traffic that reaches agentgateway; the worker must separately restrict or route
non-TCP traffic for that policy to form a sandbox-wide egress boundary.

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
> application originally used. A future egress policy must address both as one
> boundary problem: separately restrict or route UDP and other non-TCP traffic,
> and fail closed when intercepted TCP cannot be associated with an explicitly
> allowed hostname, IP address, or CIDR. If unmatched or IP-only connections
> are allowed through, an actor can dial an IP address directly and evade a
> hostname-only policy even though the TCP connection still traverses
> agentgateway.

## Fail-closed behavior of intercepted TCP

The fail-closed property in this section is limited to IPv4 TCP selected by the
worker's redirect rule. It does not cover the non-TCP compatibility masquerade.

The worker runtime sets up egress in this order:

1. request an actor certificate from `ateapi`;
2. create the actor network namespace and install the TCP redirect;
3. start `atunnel` and the actor containers;
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

These values need not agree. The current configuration does not define a policy
that compares them.

## Shipped agentgateway deployment

The `agentgateway-egress` installation overlay deploys one agentgateway
container using:

```text
cr.agentgateway.dev/agentgateway:v1.5.0
```

The Kubernetes Service exposes port 443 and targets the container's named port,
which listens on 8443. The egress pod does not contain an `atenet` sidecar.

The checked-in configuration lives in the
`atenet-egress-agentgateway-substrate-config` ConfigMap. It defines two logical
stages in the same process.

### Outer listener

The outer listener:

- binds to port 8443;
- terminates mTLS using the gateway serving certificate;
- trusts the actor identity CA bundle mounted at
  `/run/actor-id-ca-certs/ca.crt`;
- accepts HTTP CONNECT;
- sends the CONNECT stream to the inner listener;
- preserves the authenticated peer identity for inner HTTP processing.

A certificate from an unrelated CA fails during the TLS handshake. That is the
principal fail-closed identity check currently exercised by the local demo.

### Inner listener

The inner listener uses automatic protocol detection:

- the HTTP route applies the `substrateEgress` policy and forwards to the
  CONNECT authority;
- the TLS route passes encrypted TLS traffic to the CONNECT authority;
- the TCP route passes all remaining streams to the CONNECT authority.

The HTTP policy is not applied to the TLS or generic TCP routes in the pinned
configuration.

## Exact behavior of `substrateEgress` in v1.5.0

The version pinned by this repository must be the reference point for security
claims. In agentgateway v1.5.0, `substrateEgress`:

1. reads the authenticated peer's SPIFFE URI SAN;
2. extracts the Atespace and actor name from the expected URI path;
3. validates the resource-name shape;
4. adds the extracted values to request metadata used by logging.

It does not:

- parse the custom ActorIdentity certificate extension;
- extract or validate the actor UID;
- validate the certificate purpose as `atunnel`;
- call `GetActor` on `ateapi`;
- require the actor to exist;
- compare the certificate UID with the current actor UID;
- require the actor phase to be `RUNNING`;
- evaluate an `EgressPolicy`;
- decide whether the requested host, IP, or port is allowed.

The configured `ateapi` client is consequently unused by this v1.5.0 policy
implementation. The v1.5.0 test also describes egress authorization as not yet
implemented and expects no control-plane calls.

This has a direct security implication: a valid, unexpired actor certificate
issued by the trusted actor CA is sufficient to establish the outer tunnel.
The shipped gateway does not prove at CONNECT time that it belongs to the
current incarnation of a running actor.

### Later upstream implementation

Upstream agentgateway merged
[`substrate: authorize actor egress at CONNECT time`](https://github.com/agentgateway/agentgateway/pull/3237)
after the v1.5.0 tag. That change moves authorization to the CONNECT frontend,
parses the ActorIdentity extension, checks the `atunnel` purpose, calls
`GetActor`, compares UIDs, and requires a running actor.

Those checks are not present merely because the upstream change exists. Using
them requires both a newer image and the corresponding frontend-policy
configuration. The configuration in this repository still places
`substrateEgress` on the inner HTTP route.

## Identity and trust material

The egress path uses two distinct certificate relationships.

| Relationship | Presenter | Verifier | Purpose |
|---|---|---|---|
| Actor identity | `atunnel` | agentgateway outer listener | Authenticate the actor tunnel |
| Gateway service identity | agentgateway | `atunnel` | Authenticate the gateway endpoint |

The actor certificate contains a SPIFFE URI SAN and a custom ActorIdentity
extension. The control plane produces certificates with an `atunnel` purpose.
The pinned gateway validates the certificate chain and consumes the URI SAN, but
does not enforce the custom extension fields described above.

The actor CA bundle is projected into the gateway pod. The install path creates
the derived trust secret when it does not already exist. Rotating that CA
requires regenerating the derived material and ensuring the gateway consumes the
new bundle.

The gateway serving certificate and key are projected separately. `atunnel`
uses its configured trust roots and expected server identity when establishing
the outer TLS connection.

## Optional TLS interception overlay

The `agentgateway-egress-mitm` overlay changes the inner TLS route from
passthrough to HTTPS interception with a dynamic certificate authority. It
mounts the interception certificate and key at:

```text
/run/egress-mitm/tls.crt
/run/egress-mitm/tls.key
```

With this overlay, agentgateway can terminate destination TLS, observe and proxy
HTTP, and establish a separate TLS connection to the destination. Actors must
trust the interception CA for this to succeed without application TLS errors.
The project distributes that trust through the cluster trust-bundle machinery.

The overlay does not, by itself, add destination authorization or current-actor
validation to v1.5.0. Generic non-TLS TCP remains passthrough.

## `EgressPolicy` API status

Agent Substrate defines an `EgressPolicy` control-plane API with validation,
CRUD operations, and persistence. That API is not wired into the shipped
agentgateway routes or `atunnel`.

As a result, creating an `EgressPolicy` does not currently cause this data plane
to allow or deny a destination. Documentation and tests should avoid treating
API storage as runtime enforcement.

There is also no production credential-provider or credential-injection path in
this egress flow. TLS interception provides protocol visibility; it does not
imply that credentials are injected or managed.

## Verification

The following checks target only the agentgateway deployment.

### Confirm the installed image

```bash
kubectl -n ate-system get deployment atenet-egress \
  -o jsonpath='{range .spec.template.spec.containers[*]}{.name}{"\t"}{.image}{"\n"}{end}'
```

Expected container and image:

```text
agentgateway    cr.agentgateway.dev/agentgateway:v1.5.0
```

The startup log should also report agentgateway version 1.5.0.

### Inspect the active configuration

```bash
kubectl -n ate-system get configmap \
  atenet-egress-agentgateway-substrate-config -o yaml
```

Confirm the outer CONNECT listener, the inner `protocol: AUTO` listener, and
the HTTP/TLS/TCP routes described above. Configuration should be checked along
with the image version because the location of `substrateEgress` determines
which protocols it covers.

### Exercise the local demo

```bash
demos/egress/test-egress.sh
```

The positive case sends clear HTTP through the actor tunnel and can demonstrate:

- actor TCP interception;
- successful actor-CA authentication;
- CONNECT through the agentgateway Service;
- an HTTP request reaching the destination;
- actor name and Atespace appearing in gateway logs.

The negative case presents a certificate from the pod identity CA. A TLS
`UnknownIssuer` failure demonstrates that the gateway rejects a client chain
outside the configured actor CA.

This demo does not demonstrate:

- ActorIdentity UID or purpose validation;
- a `GetActor` lookup;
- rejection of a deleted, replaced, or non-running actor;
- enforcement of `EgressPolicy` destinations;
- TLS interception policy;
- credential injection.

### Probe direct addressing of worker-side `atunnel`

With the demo actor running, forward the ingress router in one terminal:

```bash
kubectl -n ate-system port-forward service/atenet-router 18101:80
```

Ask the actor to connect to the worker-side gateway address and egress-listener
port:

```bash
curl -i -X POST http://127.0.0.1:18101/ \
  -H 'Host: egress-demo.ate-demo-egress.actors.resources.substrate.ate.dev' \
  -H 'Content-Type: application/json' \
  -d '{"url":"http://169.254.17.1:15001/"}'
```

Then inspect recent agentgateway logs:

```bash
kubectl -n ate-system logs deployment/atenet-egress -c agentgateway \
  --since=1m | grep 'substrate.connect.authority="169.254.17.1:15001"'
```

A matching log entry proves the following chain occurred:

1. the actor addressed `169.254.17.1:15001`;
2. the packet crossed the sandbox boundary and reached the worker's redirect;
3. worker-side `atunnel` accepted the connection and recovered
   `169.254.17.1:15001` as its original destination;
4. `atunnel` authenticated to agentgateway and sent that value as the CONNECT
   authority.

The actor may receive `503 Service Unavailable` because agentgateway cannot
connect to that worker-local address from the gateway pod. That status is not
the reachability proof; the authenticated agentgateway log containing the exact
CONNECT authority is. The standard `egress` demo actor uses gVisor. Repeat the
same probe with the microVM demo template to verify the equivalent live path for
that runtime.

### Inspect logs

```bash
kubectl -n ate-system logs deployment/atenet-egress -c agentgateway
```

For clear HTTP, logs can include the extracted actor metadata and request
details. For passthrough TLS and opaque TCP, do not infer HTTP-layer visibility
or control-plane authorization from a successful connection log.

### Run focused unit tests

```bash
go test ./internal/atunnel ./cmd/ateapi/internal/controlapi -count=1
```

These cover tunnel and control-plane components, but they do not substitute for
an integration test against the exact agentgateway image and configuration.

## Current capabilities and gaps

| Capability | Current status |
|---|---|
| Complete sandbox network-egress lockdown | Not implemented |
| Transparent expected-source actor IPv4/TCP interception | Implemented |
| Fail-closed path for redirected TCP before tunnel activation | Implemented |
| Anti-bypass enforcement against a network-privileged actor | Not established; source-only match and permissive forwarding need hardening |
| Non-TCP IPv4 enforcement through agentgateway | Not implemented; current rules forward and masquerade it |
| Actor mTLS certificate required by the gateway | Implemented |
| Clear HTTP parsing and proxying | Implemented |
| TLS ClientHello/SNI inspection with payload passthrough | Implemented |
| Optional HTTP visibility through TLS interception overlay | Implemented |
| Actor name and Atespace extraction for HTTP metadata | Implemented in pinned v1.5.0 |
| Actor UID and certificate-purpose validation at CONNECT | Not implemented in pinned v1.5.0 |
| Current actor lookup and `RUNNING` check | Not implemented in pinned v1.5.0 |
| Destination allow/deny enforcement from `EgressPolicy` | Not wired to the data plane |
| DNS authorization through the gateway | Not implemented; DNS normally uses the non-TCP bypass |
| Credential injection | Not implemented |

## Code and manifest map

| Area | Path |
|---|---|
| Actor network namespace and redirect | `internal/ateomnet/` |
| Tunnel implementation | `internal/atunnel/` |
| Actor certificate request and renewal | `internal/atunnel/` and `cmd/atelet/credentialbroker.go` |
| Actor certificate issuance | `cmd/ateapi/internal/controlapi/` |
| agentgateway base deployment | `manifests/ate-install/components/agentgateway/` |
| agentgateway egress configuration | `manifests/ate-install/components/agentgateway/configmap.yaml` |
| Standard agentgateway egress overlay | `manifests/ate-install/agentgateway-egress/` |
| TLS interception overlay | `manifests/ate-install/components/agentgateway-egress-mitm/` |
| Egress demo | `demos/egress/` |
| Egress control-plane API | `pkg/proto/ateapipb/` and `cmd/ateapi/internal/controlapi/` |
