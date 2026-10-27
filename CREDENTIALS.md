# Egress credential injection

Credential injection lets an Actor call a service without placing a static secret in its sandbox. A matching `EgressPolicy` HTTP or HTTPS rule names an HTTP header and either a credential URI or an Actor JWT. For a credential URI, the gateway resolves the secret through its configured credential provider and overwrites the header on the outbound request. For an Actor JWT, Envoy asks ateapi to mint one instead. See [EGRESS.md](./EGRESS.md) for the broader policy and traffic paths. This is separate from the certificate `atunnel` uses to authenticate to the gateway, described in [IDENTITY.md](./IDENTITY.md).

## Credential providers

A credential provider is a separate service that looks up the secret named by a credential URI and returns its bytes to the gateway. It applies its own authorization policy. The installer requires an explicit provider choice: the bundled Kubernetes Secrets provider (`k8s.io`), an external provider address, or none, and it wires the choice into either dataplane. The repository also contains a Google Secret Manager provider, which maps atespaces to allowed projects and is configured as an external provider.

For example, `ate-secret://k8s.io/default/ns1/example-api/token` selects the local cluster (`default`), namespace `ns1`, Secret `example-api`, and required key `token`; `k8s.io` selects the Kubernetes Secrets credential provider. Its policy is loaded at startup, starts empty, and grants an Actor's atespace access to specified Kubernetes namespaces. A grant covers **every Secret and key in the namespace** unless it sets `secretSelector.matchLabels`, which narrows it to Secrets carrying all of those labels. Grant changes require restarting the provider. The installed provider has cluster-wide Secret `get`; deployments should scope that Kubernetes permission to the granted namespaces.

Only the egress gateway can call the provider: it requires the gateway's client certificate, whose SPIFFE ID is `spiffe://cluster.local/ns/ate-system/sa/atenet-egress`, and a NetworkPolicy admits only egress gateway Pods.

The namespace containing Substrate's own signing keys must not be granted. Any editor of an Actor's atespace can write its egress policy and could otherwise name an identity-CA key as the credential, and the provider would return it for injection. See [RBAC.md](./RBAC.md).

The Kubernetes Secrets credential provider returns stored Kubernetes Secret bytes. This mechanism does not mint an Actor JWT or give the destination a cryptographic Actor identity. An `actorJwt` header source is a separate path: the Envoy gateway asks ateapi to mint a short-lived Actor JWT without calling a credential provider. Token exchange of that JWT for another credential remains a distinct design and trust decision.

Gateway header replacement cannot supply credentials an SDK must read inside the sandbox, such as a token file or signing key. No path delivers those into the sandbox; see [IDENTITY.md](./IDENTITY.md#the-actor-itself).

## Trust and request path

On a provider fetch for a request where the gateway can inspect HTTP:

1. The Actor dials an external destination. Its TCP connection is redirected to `atunnel` in the trusted worker supervisor. `atunnel` opens an mTLS CONNECT through the egress gateway, presenting a `worker-for-actor` certificate for that Actor.
2. The gateway selects the Actor's `EgressPolicy` rule. Only an `http` or `https` rule can request header injection.
3. The gateway derives a name-based Actor SPIFFE ID (`spiffe://substrate-actor.local/actor/<atespace>/<name>`) from the `worker-for-actor` identity presented by `atunnel`. It sends that ID and the credential URI as fields in a fetch request to the provider. The ID is a claim the provider uses for authorization, not a certificate used to contact it. The provider authenticates the gateway's own client certificate and trusts the gateway's claim about the Actor.
4. The provider resolves the URI under its own authorization policy and returns opaque bytes. The Kubernetes Secrets provider checks the Actor's atespace against its namespace grants and the Secret's labels against any selector; a missing Secret and a refused one return the same error. The gateway prepends the policy's optional prefix, overwrites the configured header, and forwards the request.

The policy author chooses the URI; the Actor chooses requests that may match the rule. The provider receives neither the destination hostname nor the policy reference. The `worker-for-actor` certificate carries the Actor UID, but the gateway does not forward it, so the provider cannot distinguish a deleted-and-recreated Actor at the same (atespace, name) from its predecessor.

`Control.MintActorCertificate` can put the same `actor/…` URI in a separately issued Actor certificate's URI SAN, but no gateway mints or presents that certificate in this path. `Control.MintActorJwt` uses `actor/<atespace>/<name>` as its `sub`, not a SPIFFE URI; see [IDENTITY.md](./IDENTITY.md#the-actor-itself).

The secret stays out of the sandbox on the request path, but that does **not** guarantee secrecy from the Actor. A destination can echo the injected header in its response. Both dataplanes route a hostname-authorized request to that hostname rather than an Actor-selected IP. Keeping the secret out of Actor state requires a trusted destination and routing that binds the authorized hostname to the actual destination.

## Dataplane behavior

As of October 27, 2026, the pinned AgentGateway image cannot complete this path with this Substrate commit: it rejects `worker-for-actor` certificates and calls the credential provider under an earlier gRPC package name. The AgentGateway column describes the design in that image's source. See [EGRESS.md](./EGRESS.md#dataplanes).

| | AgentGateway | Envoy (installer default) |
| --- | --- | --- |
| Where provider injection runs | Policy-evaluated cleartext HTTP and intercepted HTTPS | Same |
| Matching header without a provider | Denies the request | Denies the request; also denies a URI whose authority names a different provider |
| Provider unavailable or slow | Denies with a retryable 503 when the provider reports itself unavailable; sets no fetch deadline | Denies with a retryable 503, including after a four-second fetch timeout |
| Provider secret caching | Caches by name-based Actor ID and URI for up to five minutes, so deletion or rotation is delayed and a recreated Actor at the same name may receive a cached value | Fetches for each attempted injection |
| Actor JWT source | Not implemented; a rule using it denies requests that carry the header | Mints through ateapi and caches by Actor name, so a recreated Actor may receive its predecessor's token |

TLS passthrough does not apply HTTP effects or inject credentials. Unsupported opaque TCP is denied.

Both dataplanes replace only a header the Actor supplies on HTTP and intercepted HTTPS, so the request must carry a placeholder; an absent header causes no credential fetch or replacement. A request needing a provider credential fails closed if no provider is configured or the credential cannot be obtained. Without separate transport protection, a credential injected on cleartext HTTP travels unencrypted from the gateway to the destination.

<!-- verified-against: agent-substrate/substrate@635c521cc682e312d01008f3aa2912588d394938 2026-10-27 -->
