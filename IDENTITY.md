# Actor identity

An Actor is a logical workload whose identity follows it across workers. The Worker Pod has its own infrastructure identity; it does not become the Actor's identity. This page describes the identities Substrate issues and who checks them. [EGRESS.md](./EGRESS.md) describes how the egress gateway uses them, and [RBAC.md](./RBAC.md) describes the separate authorization model.

## Actor, template, and incarnation

An Actor is addressed by **(atespace, name)**. Its server-assigned UID identifies one incarnation at that address. The UID survives updates, suspend/resume, and movement between workers; deleting and recreating the Actor gives it a new UID. The Actor's atespace is its tenancy boundary. A Kubernetes namespace does not form part of its identity.

An ActorTemplate is a separate Substrate resource, also addressed by (atespace, name). An Actor in atespace B may reference a template in atespace A; creating or updating the Actor requires permission to use that template. The template supplies the workload and placement configuration; it does not assign the Actor's atespace, name, or UID. An Actor can change templates while retaining its own identity.

## From worker identity to Actor identity

| Identity | What it identifies | Where it is used | Actor UID in certificate? |
| --- | --- | --- | --- |
| Worker Pod identity, `spiffe://cluster.local/ns/<namespace>/sa/<serviceaccount>` | The hosting Worker Pod, which may run many Actors | `atunnel` presents it to the ingress router and to the separate, node-local atelet Pod | No; a separate extension names the Pod and node |
| `spiffe://substrate-actor.local/worker-for-actor/<atespace>/<name>` | Trusted infrastructure acting for one Actor | Held by `atunnel` in the worker supervisor and presented to the egress gateway | Yes, in an `ActorIdentity` extension; no gateway reads it |
| `worker-for-actor` JWT, `sub` `worker-for-actor/<atespace>/<name>` | Trusted infrastructure acting for one Actor | Requested by atelet for the Actor's snapshot transfers and exchanged for per-Actor storage credentials | Not a certificate; the UID is in a `worker-for-actor.ate.dev` claim |
| `spiffe://substrate-actor.local/actor/<atespace>/<name>` | The Actor itself | Issued by ateapi's `Control.MintActorCertificate` RPC; no production component requests or delivers it | Yes, in an `ActorIdentity` extension |

### Worker Pod

Each worker runs in a Worker Pod with a projected Pod certificate. Its SPIFFE URI names the namespace and ServiceAccount (`ateworker` by default); a separate Pod identity extension carries the Pod UID and node. `atunnel` is part of the trusted worker process, so it can use that certificate in two roles: as the TLS server when the ingress router connects to the worker, and as a TLS client when it asks atelet for a `worker-for-actor` certificate. Atelet runs in a separate DaemonSet Pod on the same node and accepts only Pods whose Pod identity names its own node; ateapi later requires the Actor to be assigned to that Pod's Worker. Atelet uses its own Pod certificate when calling ateapi, which accepts only the atelet SPIFFE ID. These certificates establish which infrastructure Pods are talking; they do not identify an Actor.

The Envoy ingress router requires the worker to present the exact SPIFFE ID recorded for the Worker it routes to. Because that ID names a ServiceAccount, another Pod running as the same ServiceAccount in the same namespace would also match. The checked-in AgentGateway router configuration sets `insecureHost: true`, so it checks only that the worker's certificate chains to the Pod-identity trust bundle.

### Atunnel acting for an Actor

One Worker Pod can host many Actors. Its Pod certificate alone would tell the egress gateway which worker sent traffic, but not which Actor owns a connection or which Actor's policy applies. For each Actor activation, `atunnel` creates a separate private key and asks atelet to obtain a one-hour `worker-for-actor` certificate. Atelet calls ateapi's `WorkerService.MintWorkerActorCertificate` RPC using its own Pod identity. Ateapi signs only if the Actor is assigned to the requesting Worker on atelet's node and the requested UID matches the stored Actor; a suspended Actor gets no certificate. The certificate's URI names (atespace, name), and its `ActorIdentity` extension carries the UID that ateapi checked.

Atelet can also obtain a one-hour `worker-for-actor` JWT from `WorkerService.MintWorkerActorJwt` for an Actor on its node, and uses it to get per-Actor snapshot-storage credentials. It is signed by the same issuer and key pool as Actor JWTs, so a relying party must check the `sub` prefix or claim namespace to tell infrastructure acting for an Actor from the Actor itself; the storage credential exchange accepts only the `worker-for-actor` kind.

Only the certificate signing request is sent to atelet; the private key stays in `atunnel`, outside the Actor sandbox and its snapshots. `atunnel` renews the certificate with the same key while the activation lasts. A new activation gets a new key and certificate with the same name-based URI.

These are the credentials `atunnel` uses on each connection:

| Connection | Credential presented by `atunnel` | Purpose |
| --- | --- | --- |
| Ingress router → worker | Worker Pod certificate, as the TLS server | Proves the router reached a Worker Pod; the router also presents its own client certificate, which `atunnel` requires |
| Worker → atelet | Worker Pod certificate, as a TLS client | Authenticates the worker to its node-local atelet when requesting a `worker-for-actor` certificate |
| Worker → egress gateway | `worker-for-actor` certificate, as a TLS client | Names the Actor whose outbound connection the worker is handling |

The egress gateway sees the `worker-for-actor` identity rather than the Worker Pod identity; the external destination sees neither.

The certificate carries the Actor UID, but no egress gateway reads it. The Envoy gateway verifies the certificate, takes (atespace, name) from its URI, and requires that Actor to be running or resuming before accepting a new tunnel. If an Actor is deleted and recreated at the same address, a still-valid `worker-for-actor` certificate from the old incarnation can identify the new one until it expires. Opening a tunnel with it requires the old private key, which normally remains in trusted worker memory. Existing tunnels are not rechecked on every request; worker teardown closes them during normal lifecycle transitions.

The AgentGateway image pinned in the checked-in manifests (commit `9d36620d`) accepts only the earlier `ateom-for-actor` URI path, so as of October 27, 2026 it rejects every `worker-for-actor` certificate this Substrate commit issues. Envoy, the installer's default dataplane, is unaffected. See [EGRESS.md](./EGRESS.md#dataplanes).

### The Actor itself

Ateapi implements `Control.MintActorCertificate`, an RPC that signs an X.509 certificate for the Actor with the URI `spiffe://substrate-actor.local/actor/<atespace>/<name>`. This is distinct from `WorkerService.MintWorkerActorCertificate`, which issues the credential `atunnel` uses.

Both certificate kinds carry the same `ActorIdentity` X.509 extension: a non-critical, Substrate-defined field inside the signed certificate containing the Actor's atespace, name, and UID as JSON. Ordinary TLS verification does not interpret it; a verifier must read it explicitly to use the UID. Because both kinds are signed by the same actor-identity CA and carry the same extension, only the URI path tells an Actor's own certificate from infrastructure acting for it.

Only the egress gateway's mTLS identity may call `Control.MintActorCertificate`; the RPC also checks that the requested UID matches the current Actor record. The API describes egress-gateway client-certificate injection as its intended use, but no gateway calls it today, and no path delivers the certificate to an Actor workload.

Ateapi also implements `Control.MintActorJwt`, which signs a short-lived (5 to 60 minutes), audience-bound Actor JWT under an install-configured issuer. Its `sub` is `actor/<atespace>/<name>`, the reusable address; an `actor.ate.dev` claim carries the atespace, name, and UID of the incarnation. It is not a SPIFFE JWT-SVID. Like the certificate RPC, it is restricted to the egress gateway's identity, and it mints for whichever Actor currently holds the name. Every install deploys `ate-idp-server`, which serves OpenID discovery and the verification-key set at the default issuer `https://idp.<namespace>.svc`, so in-cluster relying parties can verify Actor JWTs. It reads the same key pool ateapi signs with, so it sits inside the signing-key trust boundary. Exposing it outside the cluster is left to the operator.

The Envoy egress gateway obtains an Actor JWT from ateapi and replaces an outbound request header with it on allowed HTTP and intercepted HTTPS requests, so the destination can verify the Actor without the workload holding the token. The gateway caches tokens by Actor name, so a recreated Actor can receive a token carrying its predecessor's UID; the UID claim alone does not bind the token to the Actor behind the tunnel. The pinned AgentGateway image does not implement Actor JWT injection.

No path delivers an Actor certificate or JWT into the sandbox. Templates can explicitly project Actor metadata and trust bundles as files through `systemInfo`, including a bundle that combines public roots with the egress-interception CA. Those files are information and trust roots, not a private key or token. Egress credential injection is a different mechanism: a gateway adds an external secret or Actor JWT to an outbound request. See [CREDENTIALS.md](./CREDENTIALS.md). A full snapshot can capture any credential the application has read into memory.

Open work, as of October 27, 2026:

| Work | What it would enable |
| --- | --- |
| [Bind gateway JWTs to the Actor UID (#2208)](https://github.com/agent-substrate/substrate/issues/2208) | **Open.** Close the name-reuse and cache gap by tying gateway-minted JWTs to the UID in the `worker-for-actor` certificate. |
| [Actor JWT through `systemInfo` (#1114)](https://github.com/agent-substrate/substrate/pull/1114) | **Open draft.** Project an Actor JWT into the sandbox. |
| [Per-Actor credential projection (#2335)](https://github.com/agent-substrate/substrate/issues/2335) | **Open.** Deliver provider-backed credentials or Actor JWTs as files regenerated on Run/Restore, kept out of snapshots. |

## Trust boundaries

Pod certificates authenticate atelet, workers, and control-plane components. The actor-identity CA signs both `actor/…` and `worker-for-actor` certificates; verifiers must distinguish their URI paths, not just trust the CA. The Envoy egress gateway reloads that CA bundle for each new tunnel. The gateway presents a separate service certificate that `atunnel` verifies. Optional TLS interception uses another CA that Actors must explicitly trust.

Control-plane callers authenticate with mTLS or a configured JWT provider, and every ateapi call is authorized. Infrastructure RPCs such as `WorkerService.MintWorkerActorCertificate` authorize the calling atelet in the handler instead. See [RBAC.md](./RBAC.md).

<!-- verified-against: agent-substrate/substrate@635c521cc682e312d01008f3aa2912588d394938 2026-10-27 -->
