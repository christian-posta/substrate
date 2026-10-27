# Authorization and RBAC

Ateapi authenticates every caller with an mTLS client certificate or a bearer JWT from a configured provider, then authorizes the call against an OpenFGA relationship model it embeds. Authorization is always enforced: an RPC without an authorization rule is denied, and so is every streaming RPC except gRPC reflection. Infrastructure RPCs on `WorkerService` are authorized in their handlers instead. [IDENTITY.md](./IDENTITY.md) covers Actor credentials; [CREDENTIALS.md](./CREDENTIALS.md) covers the separate provider authorization used for injected secrets.

## Boundaries

An **atespace** is Substrate's resource and authorization boundary. It is independent of a Kubernetes namespace. Actors, ActorTemplates, and Tags are named within an atespace. An Actor's `EgressPolicy` is a singleton child of the Actor, always named `default`, and its permissions follow the Actor. An Actor in atespace B can reference a template in atespace A; creating or updating it requires the viewer role on atespace A.

Kubernetes RBAC protects Substrate components' access to Kubernetes resources; it does not decide whether an ateapi caller may modify an Actor. It matters indirectly, because the default ateapi credential is a Kubernetes ServiceAccount token (see below). The Kubernetes Secrets credential provider has another, separate allowlist that grants an Actor's atespace access to Secrets in specified Kubernetes namespaces; it does not authorize ateapi callers.

Ateapi is also behind a network boundary: `ate-system` denies ingress by default, and ate-api-server accepts API traffic only from control-plane Pods and from namespaces labeled `ate.dev/api-access=true`. Worker Pods cannot connect to it directly. The egress gateway's Pods are admitted, though, and the gateway does not check the IPs it resolves, so an Actor whose egress policy authorizes the ateapi hostname can reach it through the gateway; ateapi authentication remains the barrier.

## Roles

Ateapi stores relationships in PostgreSQL and initializes the checked-in model at startup. Users get roles from an AccessPolicy named `default` at one of two scopes. `kubectl ate` manages it with its `access-policy (-a <atespace> | --global)` commands. Members are individual principals; there are no groups or wildcards.

| Role | Grants |
| --- | --- |
| Atespace viewer | Read the atespace's Actors, templates, Tags, egress policies, and AccessPolicy; use its templates, including from another atespace |
| Atespace editor | Viewer, plus create, update, and delete Actors; create and delete templates; suspend, resume, and revert Actors; write egress policies; create, update, publish, delete, and start Actors from Tags |
| Atespace owner | Editor, plus manage the atespace AccessPolicy and delete the atespace |
| Global viewer | Viewer of every atespace, and list atespaces and workers |
| Global owner | Owner of every atespace, plus create atespaces, manage workers, and manage the global AccessPolicy |

Roles bind only at those two scopes. The model also defines direct per-Actor lifecycle grants and worker-placement relations, but no API writes them and no check reads the placement relations.

### Bootstrap owners

Ateapi refuses to start without at least one `--authz-bootstrap-owners` principal. A bootstrap owner is a global owner while listed, cannot be removed through the API, and changes only when ateapi restarts. `ate-setup` defaults this to `system:serviceaccount:<namespace>:ate-client`, and `kubectl ate` without `--token-file` requests a token for that ServiceAccount through Kubernetes. On a default install, then, anyone with Kubernetes permission to create tokens for `ate-client` is a global owner. Both tools warn about this; shared clusters should name individual principals instead.

A principal ID is the first URI SAN of an mTLS certificate or the bare `sub` of a JWT; it does not record the issuer. `ate-setup` configures only the cluster's ServiceAccount issuer. With more than one JWT provider, one issuer could mint a `sub` that matches another provider's principal, including a bootstrap owner.

### Component roles

Five component roles are granted only to the mTLS identities configured for those components. An AccessPolicy cannot grant them, and a bearer JWT claiming the same identity does not receive them. Each role applies in every atespace.

| Role | Permits |
| --- | --- |
| `controller` | Manage workers |
| `egress_gateway` | Read and list Actors, read egress policies, mint Actor JWTs and Actor certificates |
| `ingress_router` | List and resume Actors |
| `atelet` | Read egress policies |
| `api_server_sidecar` | Read Actors, to issue per-Actor storage credentials |

Minting an Actor JWT or Actor certificate is limited to `egress_gateway`; not even a global owner can mint one.

## Infrastructure checks

`WorkerService` handlers authenticate atelet by its exact SPIFFE ID and read its node from the Pod identity certificate extension. `MintWorkerActorCertificate` signs only for an Actor assigned to the requesting Worker on that node, and only if the requested UID matches the stored Actor. `MintWorkerActorJwt` requires the calling atelet's node to host the Actor. The egress gateway separately checks that a tunnel certificate names an Actor that is running or resuming; [EgressPolicy enforcement varies by dataplane](./EGRESS.md#where-each-dataplane-enforces-it).

## Consequential gaps

- Every node's atelet shares one SPIFFE ID, and the `atelet` role is not scoped to a node, so any atelet can read any Actor's egress policy ([#1564](https://github.com/agent-substrate/substrate/issues/1564), open as of October 27, 2026).
- Any editor of an atespace can tag, publish, and start Actors from a Tag of any Actor in it. A Tag is a durable copy of the Actor's memory and disk, including any secrets it holds.
- A credential provider grant applies to the Actor's atespace, not to the policy writer, so every editor of that atespace can name any credential URI the grant covers. Keep provider grants narrow.

## Appendix: Snapshot tags and scope

Suspending an Actor saves its state as a snapshot; by default the call returns once the snapshot is captured on the node, and the upload to object storage continues in the background. A **Tag** is an atespace-owned Substrate resource served by `ateapi`, not a Kubernetes CRD. Its record lives in PostgreSQL, and it owns a copy of an Actor snapshot in object storage. A new Actor can start from the snapshot a Tag names.

Every Tag has a `scope` field: `atespace` means it can initialize Actors only in the tag's atespace, while `published` means it can initialize Actors in other atespaces. In either case, the snapshot must come from the same ActorTemplate. This scope limits where an Actor can start from the snapshot; it does not authorize callers to read or manage the Tag.

For each template, Substrate suspends a golden Actor in the template's own atespace, named by the template UID, and copies its snapshot to a published **golden tag**. An Actor created without an explicit source Tag starts from it; that requires permission to use the template but not the Tag. See the [golden snapshot workflow](./docs/api-guide.md#the-golden-snapshot) for the lifecycle details.

<!-- verified-against: agent-substrate/substrate@635c521cc682e312d01008f3aa2912588d394938 2026-10-27 -->
