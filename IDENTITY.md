# Actor identity

An Actor is a logical workload whose identity follows it across workers. The worker Pod has its own infrastructure identity; it does not become the Actor's identity. This page describes the identity used today. [EGRESS.md](./EGRESS.md) describes how the egress gateway uses it, and [RBAC.md](./RBAC.md) describes the separate authorization model.

## Actor, template, and incarnation

An Actor is addressed by **(atespace, name)**. Its server-assigned UID identifies one incarnation at that address. The UID survives updates, suspend/resume, and movement between workers; deleting and recreating the Actor gives it a new UID. The Actor's atespace is its tenancy boundary. A Kubernetes namespace does not form part of its identity.

An ActorTemplate is a separate Substrate resource, also addressed by (atespace, name). An Actor in atespace B may reference a template in atespace A; with newer upstream's experimental authorization enabled, creating or updating an Actor also requires permission to use the referenced template. The template supplies the workload and placement configuration; it does not assign the Actor's atespace, name, or UID. An Actor can change templates while retaining its own identity.

## From worker identity to Actor identity

| Identity | What it identifies | Where it is used | Actor UID in certificate? |
| --- | --- | --- | --- |
| Worker Pod identity | The hosting Worker Pod, which may run many Actors | `atunnel` presents it to the ingress router and to the separate, node-local atelet Pod | No; it identifies the Pod |
| `spiffe://substrate-actor.local/ateom-for-actor/<atespace>/<name>` | Trusted infrastructure acting for one Actor | Held by `atunnel` in the worker supervisor and presented to the egress gateway | No |
| `spiffe://substrate-actor.local/actor/<atespace>/<name>` | The Actor itself | Issued by ateapi's `Control.MintActorCertificate` RPC; no production component delivers it to workloads | Yes, in an `ActorIdentity` extension; not in the URI |

### Worker Pod

Each worker runs in a Worker Pod with a projected Pod certificate. `atunnel` is part of the trusted worker process, so it can use that certificate in two roles: as the TLS server when the ingress router connects to the worker, and as a TLS client when it asks atelet for an `ateom-for-actor` certificate. Atelet runs in a separate DaemonSet Pod on the same node, serves that node's workers, and uses its own Pod certificate when calling ateapi. These certificates establish which infrastructure Pods are talking; they do not identify an Actor.

### Atunnel acting for an Actor

One Worker Pod can host many Actors. Its Pod certificate alone would tell the egress gateway which worker sent traffic, but not which Actor owns a connection or which Actor's policy applies. For each Actor activation, `atunnel` creates a separate private key and asks atelet to obtain a short-lived `ateom-for-actor` certificate. Atelet calls ateapi's `WorkerService.MintAteomActorCertificate` RPC using its own Pod identity; ateapi checks the requested Actor's atespace, name, and UID before signing. The issued certificate has the URI `spiffe://substrate-actor.local/ateom-for-actor/<atespace>/<name>`, but does not carry the UID that ateapi checked.

Only the certificate signing request is sent to atelet; the private key stays in `atunnel`, outside the Actor sandbox and its snapshots. A new activation gets a new key and `ateom-for-actor` certificate with the same name-based URI.

These are the credentials `atunnel` uses on each connection:

| Connection | Credential presented by `atunnel` | Purpose |
| --- | --- | --- |
| Ingress router → worker | Worker Pod certificate, as the TLS server | Proves the router reached a Worker Pod; the router also presents its own client certificate |
| Worker → atelet | Worker Pod certificate, as a TLS client | Authenticates the worker to its node-local atelet when requesting an `ateom-for-actor` certificate |
| Worker → egress gateway | `ateom-for-actor` certificate, as a TLS client | Names the Actor whose outbound connection the worker is handling |

The egress gateway sees the `ateom-for-actor` identity rather than the worker Pod identity; the external destination sees neither.

The Actor record's UID survives movement, but the `ateom-for-actor` certificate has no Actor UID: its SPIFFE URI names only (atespace, name), and it has no `ActorIdentity` extension. The egress gateway verifies that certificate, resolves the Actor by (atespace, name), and requires the Actor to be running before accepting a new tunnel. It cannot compare the certificate with the current Actor UID. If an Actor is deleted and recreated at the same address, a still-valid `ateom-for-actor` certificate from the old incarnation can identify the new one until it expires. Opening a tunnel with it requires the old private key, which normally remains in trusted worker memory. Existing tunnels are not rechecked on every request; worker teardown closes them during normal lifecycle transitions.

`WorkerService.MintAteomActorCertificate` checks the requested UID against the stored Actor, but it does not yet verify that the requesting worker hosts that Actor. These infrastructure checks are narrower than general caller authorization.

### The Actor itself

Ateapi implements `Control.MintActorCertificate`, an RPC that signs an X.509 certificate for the Actor with the URI `spiffe://substrate-actor.local/actor/<atespace>/<name>`. This is distinct from `WorkerService.MintAteomActorCertificate`, which issues the credential `atunnel` uses.

The `actor/…` URI omits the UID, but its certificate also has an `ActorIdentity` X.509 extension: a Substrate-defined field inside the signed certificate containing the Actor's atespace, name, and UID as JSON. Ordinary TLS verification does not interpret that field; a verifier must explicitly read it to use the UID. The `ateom-for-actor` certificate does **not** have this extension.

No production component in this repository calls `Control.MintActorCertificate`. The API describes egress-gateway client-certificate injection as an intended use, but the gateway does not call this RPC today, and no path delivers the certificate to an Actor workload. The RPC requires a verified TLS client certificate and checks that the requested UID matches the current Actor record. It does not check whether that caller is allowed to mint for that Actor; knowing the UID is not authorization.

This path is still evolving. [Actor Identity issue #124](https://github.com/agent-substrate/substrate/issues/124) tracks exposing Actor credentials. [SystemInfo issue #802](https://github.com/agent-substrate/substrate/issues/802) proposes `actorIdentityCertificate` as a later volume source that would project the Actor certificate and private key into its sandbox. That certificate source has no implementation PR linked from the issue. The missing per-Actor mint authorization is marked in the RPC implementation, but these issues do not establish its final policy or delivery path.

Ateapi also implements `Control.MintActorJWT`, which signs a short-lived, audience-bound actor JWT under a required, install-configured issuer. Its `sub` identifies the reusable (atespace, name) address, while a separate `ate.dev.actorUID` claim identifies the incarnation. It is not a SPIFFE JWT-SVID, and the repository has no production delivery or renewal path inside an Actor sandbox, nor a deployed public key-discovery service for external verifiers. The RPC has no UID precondition and mints for whichever Actor currently holds the name, including its stored UID, but does not authorize the caller for that Actor. The egress gateway can deliver the JWT on an outbound request. [Draft PR #1114](https://github.com/agent-substrate/substrate/pull/1114) proposes projecting an Actor JWT through `systemInfo`; it does not implement X.509 certificate delivery. Neither actor-held credential should be assumed available inside a sandbox.

The default Envoy egress path can obtain an Actor JWT from ateapi and replace an outbound request header with it on allowed HTTP and intercepted HTTPS requests. The destination can verify that signed Actor identity without requiring the Actor workload to hold the token. The gateway's name-based cache can give a recreated Actor a token carrying its predecessor's UID; a UID claim alone does not bind the token to the Actor behind the tunnel. Ateapi can generate verification-key sets and operators can rotate signing keys, but serving OpenID discovery and the verification-key set remains unfinished.

The gateway JWT work to watch is below. Status is current as of October 12, 2026.

| Work | What it enables |
| --- | --- |
| [Gateway JWT minting and caching (#2135)](https://github.com/agent-substrate/substrate/pull/2135) | **Merged.** The gateway obtains and caches Actor JWTs from ateapi. This alone does not inject them. |
| [JWT injection on HTTP and HTTPS rules (#2141)](https://github.com/agent-substrate/substrate/pull/2141) | **Merged.** Replaces an Actor-supplied placeholder header with the JWT on allowed HTTP and intercepted HTTPS requests, without a static-secret provider. HTTP sends the token unencrypted to the destination. |
| [JWT injection E2E coverage (#2256)](https://github.com/agent-substrate/substrate/pull/2256) | **Open.** Exercises the Envoy implementation on HTTP and HTTPS; it does not add AgentGateway support. |
| [JWT mint authorization (#2026)](https://github.com/agent-substrate/substrate/pull/2026) | **Open.** Restricts minting to the configured gateway's authenticated mTLS identity when experimental authorization enforcement is enabled. Depends on the proposed component-role grants. |
| [Discovery and verification keys (#2099)](https://github.com/agent-substrate/substrate/pull/2099) | **Open.** Serves issuer discovery and verification keys. The proposed server reads the signing-key pool, placing it inside the private-key trust boundary. Deployment remains follow-up work. |

AgentGateway parity is not implemented. The pinned AgentGateway `substrateEgress` path handles only `credential_uri` sources and denies an `actor_jwt` source. No dedicated parity PR or issue was found in either repository as of October 12, 2026; [Actor JWT credential injection (#1660)](https://github.com/agent-substrate/substrate/issues/1660) remains the closest umbrella tracker. [Binding gateway JWTs to the Actor UID (#2208)](https://github.com/agent-substrate/substrate/issues/2208) separately tracks the name-reuse and cache limitation.

Templates can explicitly project Actor metadata and trust bundles as files into the sandbox through `systemInfo`. Newer upstream can combine public roots and the egress-interception CA in one projected bundle. Those files are information and trust roots, not an actor private key or JWT. Egress credential injection is a different mechanism: a gateway obtains an external secret or Actor JWT and adds it to an outbound request. See [CREDENTIALS.md](./CREDENTIALS.md).

[Per-Actor credential projection](https://github.com/agent-substrate/substrate/issues/2335) proposes delivering provider-backed credentials or Actor JWTs as files regenerated on Run/Restore. It remains unimplemented. A full snapshot can still capture credentials the application has read into memory, even if the projected file is excluded.

## Trust boundaries

Pod certificates authenticate atelet, workers, and control-plane components. The actor-identity CA signs both `actor/…` and `ateom-for-actor` certificates; verifiers must distinguish their URI paths, not just trust the CA. The gateway presents a separate service certificate that `atunnel` verifies. Optional TLS interception uses another CA that Actors must explicitly trust.

Control-plane callers authenticate with mTLS or a configured JWT provider. Newer upstream has opt-in Actor and template authorization checks; lifecycle, egress-policy, and mint authorization remain incomplete. See [RBAC.md](./RBAC.md).
