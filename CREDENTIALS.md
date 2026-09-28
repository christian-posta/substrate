# Egress credential injection

Credential injection lets an Actor call a service without placing a static secret in its sandbox. A matching `EgressPolicy` HTTP or HTTPS rule names an HTTP header and either a credential URI or Actor JWT. When provider injection is attempted, the gateway resolves the credential through its configured provider or, on AgentGateway, its cache. A successful resolution overwrites the header on the outbound request. The dataplanes differ in caching and Actor JWT support. See the table below for injection behavior and [EGRESS.md](./EGRESS.md) for the broader policy and traffic paths. This is separate from the certificate `atunnel` uses to authenticate to the gateway, described in [IDENTITY.md](./IDENTITY.md).

## Credential providers

A credential provider is a separate service that looks up the secret named by a credential URI and returns its bytes to the gateway. It applies its own authorization policy. This repository includes one implementation, the Kubernetes Secrets credential provider. The installer deploys it when `k8s.io` is selected; a deployment may instead configure an external provider.

For example, `ate-secret://k8s.io/default/ns1/example-api/token` selects the local cluster (`default`), namespace `ns1`, Secret `example-api`, and required key `token`; `k8s.io` selects the Kubernetes Secrets credential provider. Its startup-loaded policy grants an Actor atespace access to specified Kubernetes namespaces; a grant covers **every Secret and key in the namespace**. The provider also needs Kubernetes permission to read those Secrets. The sample manifest grants cluster-wide Secret `get`; deployments should scope that Kubernetes permission to the granted namespaces.

Newer upstream can deploy and configure the bundled Kubernetes Secrets provider for either dataplane, starting with empty, default-deny grants. Grants can also require static Secret labels to narrow access within a namespace; without a selector, a namespace grant still covers every Secret and key there. Grant changes require restarting the provider. A Google Secret Manager provider is also available upstream, with its own authorization policy.

The namespace containing Substrate's own signing keys must not be granted. A caller who can write an Actor's egress policy could otherwise name an identity-CA key as the credential, and the provider would return it for injection. General ateapi authorization does not yet constrain who can write that policy; see [RBAC.md](./RBAC.md).

The Kubernetes Secrets credential provider returns stored Kubernetes Secret bytes. This mechanism does not mint an Actor JWT or give the destination a cryptographic Actor identity. An `actorJwt` header source is a separate path: the default Envoy gateway asks ateapi to mint a short-lived Actor JWT without calling a credential provider. Token exchange of that JWT for another credential remains a distinct design and trust decision.

Gateway header replacement cannot supply credentials an SDK must read inside the sandbox, such as a token file or signing key. Per-Actor projection on Run/Restore remains proposed and must account for credentials captured in process-memory snapshots; see [IDENTITY.md](./IDENTITY.md#the-actor-itself).

## Trust and request path

On a provider fetch for a request where the gateway can inspect HTTP:

1. The Actor dials an external destination. Its TCP connection is redirected to `atunnel` in the trusted worker supervisor. `atunnel` opens an mTLS CONNECT through the egress gateway, presenting an `ateom-for-actor` certificate for that Actor.
2. The gateway selects the Actor's `EgressPolicy` rule. Only an `http` or `https` rule can request header injection.
3. The gateway derives a name-based Actor SPIFFE ID (`spiffe://substrate-actor.local/actor/<atespace>/<name>`) from the `ateom-for-actor` identity presented by `atunnel`. It sends that ID and the credential URI as fields in a fetch request to the provider. The ID is a claim the provider uses for authorization, not a certificate used to contact it. The Kubernetes Secrets credential provider authenticates the gateway's own client certificate over mTLS and trusts the gateway's claim about the Actor.
4. The provider resolves the URI under its own authorization policy and returns opaque bytes. In the Kubernetes Secrets implementation, it checks the Actor's atespace against its namespace grants. The gateway prepends the policy's optional prefix, overwrites the configured header, and forwards the request.

The policy author chooses the URI; the Actor chooses requests that may match the rule. The provider receives neither the destination hostname nor the policy reference, and the Actor identity it receives does not include an Actor UID. It cannot distinguish a deleted-and-recreated Actor at the same (atespace, name) from its predecessor.

`Control.MintActorCertificate` can put the same `actor/…` URI in a separately issued Actor certificate's URI SAN, but the gateway does not mint or present that certificate in this path. `Control.MintActorJWT` uses a different `sub` format; see [IDENTITY.md](./IDENTITY.md#the-actor-itself).

The secret stays out of the sandbox on the request path, but that does **not** guarantee secrecy from the Actor. A destination can echo the injected header in its response. Both dataplanes route a hostname-authorized request to that hostname rather than an Actor-selected IP. Keeping the secret out of Actor state requires a trusted destination and routing that binds the authorized hostname to the actual destination.

## Dataplane behavior

| | Default Envoy dataplane | Checked-in AgentGateway dataplane |
| --- | --- | --- |
| Where provider injection runs | Policy-evaluated cleartext HTTP and intercepted HTTPS | Policy-evaluated cleartext HTTP and intercepted HTTPS |
| Provider deployment | Installer can deploy the bundled provider or configure an external provider | Same |
| Matching header without a provider | Denies the request | Denies the request |
| Provider secret caching | Fetches for each attempted injection | Caches by name-based Actor ID and URI for up to five minutes, so deletion or rotation is delayed and a recreated Actor at the same name may receive a cached value |
| Actor JWT source | Implemented without a credential provider | Not implemented in the pinned AgentGateway image; see [IDENTITY.md](./IDENTITY.md#the-actor-itself) for tracking |

TLS passthrough does not apply HTTP effects or inject credentials. Unsupported opaque TCP is denied.

Both newer upstream dataplanes replace only a header the Actor supplies on HTTP and intercepted HTTPS, so the request must carry a placeholder; an absent header causes no credential fetch or replacement. A request needing a provider credential fails closed if no provider is configured or the credential cannot be obtained. Cleartext HTTP replacement is implemented: without separate transport protection, the injected secret travels unencrypted from the gateway to the destination.
