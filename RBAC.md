# Authorization and RBAC

Substrate authenticates callers to ateapi with mTLS client certificates or configured bearer-JWT providers. General caller authorization is disabled by default. With the default configuration, an authenticated caller can use the control-plane APIs across atespaces, including reading templates and writing Actor or egress-policy resources. Experimental enforcement covers only the registered resources described below. Only trusted principals should be given control-plane access. [IDENTITY.md](./IDENTITY.md) covers Actor credentials; [CREDENTIALS.md](./CREDENTIALS.md) covers the separate provider authorization used for injected secrets.

## Boundaries

An **atespace** is Substrate's resource and intended authorization boundary. It is independent of a Kubernetes namespace. Actors, ActorTemplates, and egress policies are named within an atespace. An Actor in atespace B can reference a template in atespace A; with experimental authorization enabled, creating or updating it requires permission to use that template.

Kubernetes RBAC protects Substrate components' access to Kubernetes resources. It does not decide whether an ateapi caller may modify an Actor. The Kubernetes Secrets credential provider has another, separate allowlist that grants an Actor's atespace access to Secrets in specified Kubernetes namespaces; it does not authorize ateapi policy writers.

## Intended authorization model

**Upstream authorization remains partial.** Experimental checks now cover Atespace, Actor, and ActorTemplate management, but enforcement is disabled by default. AccessPolicy operations always require permission. Checks for lifecycle actions, egress-policy writes, and JWT minting are still being developed, along with grants for the trusted components that call ateapi. These changes do not yet establish complete atespace isolation.

The proposed component grants bind controller, ingress-router, and egress-gateway permissions to configured mTLS identities; a bearer JWT claiming the same identity does not receive those grants. Proposed JWT mint authorization permits only the egress gateway, and only when experimental authorization enforcement is enabled. Without enforcement, authenticated callers can still mint for other Actors.

Ateapi embeds OpenFGA and initializes a checked-in relationship model in PostgreSQL. The model describes a global owner/viewer and atespace owner/editor/viewer roles. Actors and templates inherit user permissions from their parent atespace; an Actor may also have narrow direct lifecycle grants. Placement relationships are intended to let only the Actor's current host node mint its tunnel credential.

The upstream Actor create/update checks also require permission to use the referenced template, including a template in another atespace. These checks apply only when experimental authorization enforcement is enabled.

## Checks that do exist

Infrastructure paths apply specific identity checks. Atelet must authenticate to the tunnel-certificate minting RPC, and minting checks the requested Actor UID against the stored Actor. The minting path does not yet prove that the requesting worker hosts that Actor. The public actor-certificate API requires a verified client certificate; the actor-JWT API requires an authenticated caller. Neither API authorizes that caller for the named Actor. The egress gateway checks that a tunnel certificate names a running Actor, while [EgressPolicy enforcement varies by dataplane](./EGRESS.md#where-each-dataplane-enforces-it).

These checks do not amount to atespace RBAC. In particular, authorization is disabled by default and does not cover egress-policy writes, so any authenticated caller able to write an egress policy can choose a credential URI that the provider permits for that Actor's atespace. Keep provider grants and ateapi access narrow until authorization is complete and enabled.

## Appendix: Snapshot tags and scope

Suspending an Actor saves its state as a snapshot in object storage. A **Tag** is an atespace-owned Substrate resource served by `ateapi`, not a Kubernetes CRD. Its record lives in PostgreSQL, and it owns a copy of an Actor snapshot in object storage. A new Actor can start from the snapshot a Tag names.

Every Tag has a `scope` field: `atespace` means it can initialize Actors only in the tag's atespace, while `published` means it can initialize Actors in other atespaces. In either case, the snapshot must come from the same ActorTemplate. This scope limits where an Actor can start from the snapshot; it does not authorize callers to read or manage the Tag.

For each template, Substrate suspends a temporary Actor in the reserved `ate-golden` atespace and copies its snapshot to a published **golden tag**. This lets Actors in other atespaces start from the template's prepared state. See the [golden snapshot workflow](./docs/api-guide.md#the-golden-snapshot) for the lifecycle details.
