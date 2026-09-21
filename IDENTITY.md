# Actor Identity

> How Agent Substrate gives each actor a cryptographic identity that survives suspend/resume and
> is independent of the worker pod hosting it. This document describes what exists, the design
> options that were weighed, and what is still open. [EGRESS.md](./EGRESS.md) covers the egress
> data path that consumes this identity. Tracking issue: `agent-substrate/substrate#124`.

## Requirements

1. **Stable across suspend/resume.** An actor incarnation keeps its identity when it is
   checkpointed on one worker and restored on another, possibly on a different node. Deleting and
   recreating an actor, even with the same name, creates a new identity.
2. **Independent of the worker pod.** The worker pod's certificate identifies the hosting
   infrastructure requesting a credential, not the workload. The actor's identity must never be
   the pod's identity.
3. **Aligned with SPIFFE profiles.** A bearer actor token should be a SPIFFE JWT-SVID. A future
   proof-of-possession token should use SPIFFE's WIT-SVID profile of WIMSE WIT, together with a
   standard WIMSE presentation protocol such as WPT. These are complementary profiles, not two
   names for the same token. Standards alignment should make external federation (GCS, S3, custom
   services) possible without per-actor IAM policies or a Substrate-specific JWT contract.

The shipped X.509 path meets requirements 1 and 2 fully and requirement 3 in shape only: the
certificate carries a SPIFFE URI SAN, but there is no external federation yet.

## Terminology

- **Atespace** is the tenancy unit. Actors, actor templates, snapshots, and egress policies are
  scoped by atespace. Kubernetes namespaces do not appear in actor identity.
- **Actor reference** is **(atespace, name)**. It is the human-readable, atespace-scoped address
  used to look up an actor. Names are DNS-1123 labels and can be reused after deletion.
- **Actor UID** is a server-assigned, globally unique identifier for one *incarnation* of an
  actor. It is preserved by updates, suspend/resume, and migration, but changes on
  delete-and-recreate.
- **Actor incarnation identity** is the actor reference plus its UID: `(atespace, name, UID)`. The
  reference locates the actor; the UID proves that it is the same created actor and prevents a new
  actor at a reused name from inheriting the old incarnation's identity.
- **Activation** is one execution of an actor incarnation on a worker. Restore creates a new
  activation and new atunnel key material, not a new actor incarnation or identity.
- **ActorTemplate** is a substrate resource (not a Kubernetes CRD), also atespace-scoped. Template
  coordinates are provenance, not identity, and do not appear in the credential.

## Components

| Component | Role | Where |
| --- | --- | --- |
| `Control` service (identity RPCs) | Issues actor credentials: `MintActorCertificate` (X.509) and `MintActorJWT` (JWT) | `cmd/ateapi/internal/controlapi/actor.go`, `pkg/proto/ateapipb` |
| Actor-identity CA | Signs actor certificates; the cached, rotation-ready signing pool is mounted only into ateapi | `internal/localca`; Secret `actor-id-ca-pool` |
| JWT authority pool | Signs actor JWTs; cached, with a selectable active signing key | `internal/localjwtauthority`, `internal/actoridjwt` |
| X.509 extensions | `ActorIdentity` and `PodIdentity` custom extensions | `internal/substratex509` |
| atelet caller authentication | Verifies that an ateapi caller is atelet, by SPIFFE ID and `PodIdentity` extension. Since #1315 it guards only `WorkerService` capacity reporting, **not** the minting RPCs | `cmd/ateapi/internal/ateletauth` |
| atelet dialer | Shared client TLS configuration for reaching atelet's node socket, including the same-node check | `internal/ateletdial` |
| `AteomSupport` service | atelet's node-local gRPC service that relays certificate requests from workers to ateapi, and takes their capacity reports | `cmd/atelet` (`ateomsupport.go`), `internal/proto/ateletpb` |
| atunnel | A library in the ateom process, outside the actor sandbox; holds the actor's private key and presents the actor certificate on egress | `internal/atunnel` |
| Pod identity | SPIFFE certificates for control-plane pods, atelet, and workers via Kubernetes PodCertificateRequests | `cmd/podcertcontroller` |
| ateapi authentication | mTLS client certs and configurable OIDC/JWT providers | `internal/ateapiauth`, `cmd/ateapi/internal/oidcjwt`, `docs/authentication.md` |
| `systemInfo` volumes | Projects actor metadata and trust bundles as files into the sandbox | `pkg/proto/ateapipb` (`SystemInfoVolumeSource`), `cmd/atelet` |

State (actors, workers, assignments, egress policies) lives in PostgreSQL.

## Topology

```
K8s node
├── 1 × atelet  (DaemonSet named atelet-<version>, nodes labeled ate.dev/substrate-version)
│       holds a pod-identity certificate
│       serves AteomSupport on a host Unix socket (mode 0600)
└── M × worker pods  (ordinary Kubernetes Pods; cluster-default container runtime)
        └── ateom container/process  (trusted supervisor, outside the actor sandbox)
                ├── atunnel library  (same process: ingress, egress, actor cert)
                └── launches one actor sandbox using one of:
                        ├── gVisor: runsc sandbox
                        │       ├── _pause sandbox-root container (reserved name)
                        │       └── actor application container(s)
                        └── micro-VM: Cloud Hypervisor VM
                                └── kata-agent → actor application container(s)

cluster
├── ateapi (`Control` and `WorkerService`, PostgreSQL-backed)
└── egress gateway (Envoy + atenet ext_proc by default, or agentgateway) — authorizes actor certs
```

atelet is per node, not per worker, and serves every actor on its node. This is the SPIRE Agent
shape. atelet and ateom speak a node-local protocol with no cross-version guarantee, which is why
the DaemonSet is versioned per node.

The worker Pod itself is not a gVisor or micro-VM Pod: the WorkerPool controller does not set a
Kubernetes `runtimeClassName`. Instead, its trusted ateom process launches the nested actor
runtime. `ateom-gvisor` invokes `runsc`; `ateom-microvm` launches Cloud Hypervisor and controls the
guest through kata-agent. In both cases atunnel is compiled into ateom and stays outside the actor
isolation boundary. It is not a sidecar and does not run inside runsc or the guest. Kubernetes CRI
also uses the term *pod sandbox*; in this document *actor sandbox* means this nested gVisor sandbox
or micro-VM, not the worker Pod's CRI sandbox.

The worker Pod certificate and actor certificate have separate jobs. Ateom and atunnel use the
worker Pod certificate for router-to-atunnel ingress and worker-to-atelet authentication. Atunnel
uses the actor certificate only as its client identity on egress to the Substrate gateway. Neither
credential is delivered to the actor sandbox. The ateom container is not privileged, but it is
trusted infrastructure with the Linux capabilities and seccomp/AppArmor relaxations needed to
launch the nested runtime.

The runtimes serve one logical actor per worker at a time, although that actor can contain several
application containers. The control-plane API already models more: workers report capacity and
allocation, and a worker can carry several actor assignments. The current certificate-minting RPC
does not consult those assignments: its request names one actor directly. atunnel still has a
single active identity and presents one certificate for every intercepted connection from the
active actor.

The active-workload statistics RPC now returns a list as groundwork for multi-actor workers, and
atelet folds every entry it receives. That wire-shape change does not make the runtimes multi-actor:
both implementations still cap the list at one active workload, and the singleton atunnel identity
and worker-side network state remain unchanged.

## The actor identity

### Current SPIFFE ID (shipped)

```
spiffe://substrate-actor.local/atespace/<atespace>/actor/<actor-name>
```

Since "Add the egresspolicy evaluator and share the actor SPIFFE ID format" (`e517df95`) this
string has one constructor, `resources.ActorSPIFFEID` in `internal/resources/spiffe.go`, with an
inverse parser `ActorRefFromSPIFFEID`. The trust domain `substrate-actor.local` is hard-coded, with a
`TODO(identity)` to make it per-install, and a second `TODO(identity)` proposes prefixing the path
with `atunnel` so a verifier cannot confuse an atunnel credential with an actor presenting its own.
There is no template segment. This URI identifies the reusable actor reference, not one actor
incarnation: the actor UID is carried only in the Substrate-specific certificate extension or JWT
private claim.

Both egress dataplanes compensate for that, differently. Envoy's ext_proc requires the
certificate's single URI SAN to equal exactly what `ActorSPIFFEID` builds from the extension's
`(atespace, name)`, then looks the actor up and authorizes only if the credential's UID equals the
current record's UID and the actor is RUNNING (`cmd/atenet/internal/router/egress/egress.go`).
agentgateway performs the same lookup, UID comparison and RUNNING check at CONNECT time, but reads
the extension alone and ignores the URI SAN, so it never cross-checks the two. Since
`docs/network-egress.md` landed, that is a contract deviation and not just a difference between
dataplanes: the contract says a PEP MUST require the certificate's actor URI SAN to identify the
same actor as the extension. See
[EGRESS.md](./EGRESS.md#what-the-gateway-policies-actually-do).

A generic SPIFFE verifier understands neither the extension nor either dataplane's check, and would
see an actor deleted and recreated at the same name as the same subject. That is the reason for the
canonical ID below.

### Canonical actor-incarnation SPIFFE ID (recommended; not implemented)

```
spiffe://substrate-actor.local/atespace/<atespace>/actor/<actor-name>/uid/<actor-uid>
```

Substrate's current authorization and storage model already treats the UID as the incarnation
boundary, so the canonical SPIFFE ID should include it.
[Issue #1594](https://github.com/agent-substrate/substrate/issues/1594) closed with that model
confirmed — user-provided `metadata.name` plus a server-generated `metadata.uid` — and with
explicit guidance that a system baking a resource into a token must carry *both*, which is what
this identity does. Atespace and name remain in the path for tenancy, policy readability, and
lookup; the UID prevents identity and permissions from silently crossing delete-and-recreate. Suspend, resume, migration, and ordinary updates preserve the UID,
so this identity remains stable for the actor's intended lifetime.

The X.509 URI SAN, JWT-SVID `sub`, and any future WIT-SVID `sub` should use this exact same value.
Atespace, name, and UID can also remain as structured fields for logging and policy convenience,
but verifiers must not need a private claim to repair an ambiguous primary subject. Whether an
atunnel credential and a future actor-held credential also need different purpose-specific SPIFFE
IDs is a separate question.

### X.509 actor certificate (in production use)

The certificate is a short-lived TLS **client** certificate that represents the actor on the
atunnel-to-egress-gateway hop. The actor process never receives either the certificate or its
private key.

| Property | Value |
| --- | --- |
| URI SAN | currently the name-based SPIFFE ID above; recommended target is the UID-bearing actor-incarnation ID |
| Validity | one hour, with a five-minute backdated `NotBefore` |
| Key usage | `DigitalSignature`; extended key usage `ClientAuth` **and** `ServerAuth` since #1315; not a CA. atunnel only requires `ClientAuth`, so the `ServerAuth` grant is currently unused and widens the credential beyond the client role the design intends |
| Signer | the actor-identity CA (Ed25519 self-signed root in the default install) |
| Custom extension | `ActorIdentity`, OID `1.3.6.1.4.1.11129.2.12.2`; allocated under Google's enterprise number and slated to change after the CNCF donation, per `docs/network-egress.md` |

The `ActorIdentity` extension payload is plain JSON, not ASN.1, so non-Go verifiers need no ASN.1
library:

```json
{ "Atespace": "...", "ActorName": "...", "ActorUid": "...", "Purpose": "atunnel" }
```

`Purpose` is a closed set. `atunnel` is the only value the signer will issue and the only value
parsers accept. The purpose is chosen by atelet's broker, never by the ateom that calls it, so a
compromised worker cannot widen its own credential's scope by asking. Note this holds only for
callers that go through the broker: ateapi itself takes `Purpose` from the request and rejects
anything other than `ATUNNEL` only via the `switch` default, so the guarantee rests on the broker
hop rather than on the signer.

There is no revocation (no CRL, no OCSP). Freshness comes from the one-hour lifetime plus a live
actor lookup on every CONNECT, and **both** egress dataplanes now perform that lookup: Envoy in its
Go ext_proc, agentgateway in its `substrateEgressActorResolution` frontend policy. Either one
refuses the tunnel when the certified UID no longer matches the live actor, or when the actor is
not RUNNING, and answers 503 rather than failing open if the control plane is unreachable. See
[EGRESS.md](./EGRESS.md#what-the-gateway-policies-actually-do).

An established tunnel is not re-checked. The lookup is per CONNECT, so the gateway never revokes a
tunnel it has already accepted, and certificate expiry does not close one either: expiry blocks new
tunnels and deliberately lets admitted connections drain (`internal/atunnel/egress.go`). What does
close them is the worker. Normal lifecycle teardown calls `atunnel`'s `Deactivate`, which cancels
active streams and waits for their forwarding goroutines to exit. The `RevertActor` RPC adds one
more route into that teardown — it moves the actor through `REVERTING` to `SUSPENDED` on its last
external snapshot — so new tunnels are refused by the gateway and open ones are closed from the
worker side.

The companion `PodIdentity` extension (OID `1.3.6.1.4.1.11129.2.12.1`) appears on atelet, worker,
and control-plane pod certificates. It carries namespace, service-account name and UID, pod name
and UID, and node name and UID. The pod UID identifies a worker incarnation; the node name and UID
pin node-local peers to a specific node incarnation.

### JWT actor credential (minting primitive implemented; delivery under review)

The JWT is an **actor-level credential**. It names the same actor incarnation as the X.509
credential (atespace, actor name, and actor UID), but it is meant for a different verifier and a
different layer of the connection:

- The X.509 credential is held by atunnel and authenticates the actor's outer mTLS connection to
  the Substrate egress gateway. It is a transport credential; the external destination never sees
  it.
- The JWT is an audience-bound bearer credential intended for an application-layer relying party:
  a service that trusts the Substrate issuer, or an identity provider that exchanges it for a
  cloud credential. The actor (or a future trusted egress injector) would put it in an application
  protocol, commonly `Authorization: Bearer <token>`.

Consequently the JWT complements the shipped mTLS path; it does not currently replace it. A
request could travel through the mTLS-authenticated egress tunnel and also carry a JWT to the final
service. The gateway uses the certificate to identify the connection for egress policy, while the
destination uses the JWT to identify the actor at the application layer.

```
shipped transport identity:
actor traffic → atunnel ── actor mTLS cert ──▶ Substrate gateway → destination
                              gateway verifies it             destination never sees it

projected JWT proposed by PR #1114:
ateapi Run/Restore → atelet writes token file → actor adds bearer token to request
                                                   └──▶ destination or STS verifies it

possible future proxy injection:
actor request → gateway identifies actor from mTLS → mint audience JWT → inject header → service
```

#### Why `MintActorJWT` exists but has no caller

The RPC predates the current *interior gVisor* architecture. In the earlier model an actor was
itself a Kubernetes Pod under a gVisor RuntimeClass, so the workload could present its Kubernetes
service-account token directly to an identity broker and exchange that infrastructure credential
for an actor credential. The code originally called this `SessionIdentity`; it was later renamed
to `ActorIdentity`.

[PR #670](https://github.com/agent-substrate/substrate/pull/670) moved the API to the current
atespace/name model and added the actor X.509 path. It explicitly left the JWT path with TODOs.
[PR #757](https://github.com/agent-substrate/substrate/pull/757) later restricted the RPC to a
configured JWT issuer, and the signing pool is now cached and rotation-ready, but neither change
adapted delivery to interior gVisor. An actor inside the nested sandbox has neither its own
Kubernetes Pod identity nor a route that calls the RPC, so it is presently stranded between the
old and new architectures. #1315 renamed it `MintJWT` → `MintActorJWT` and moved it from the
separate `ActorIdentity` service into `Control`; it still has no production caller.

What `MintActorJWT` on `main` does today:

1. Validates the request shape, and requires at least one audience.
2. Looks the actor up in the store by `(atespace, name)` and rejects with `Aborted` if the stored
   UID differs from the requested one — a cross-check the pre-#1315 version did **not** perform.
3. Signs a 15-minute bearer token with the actor-JWT signing pool.

Two authorization gaps remain, and #1315 widened one of them:

- **No caller authorization.** There is only a `TODO(authz)` where the check belongs. Any caller
  the authentication interceptor admits can request a token for any actor it can name.
- **The issuer restriction is no longer enforced.** PR #757's rule — that the caller must present
  a JWT from the provider named by `actorIdentityJWTProvider` — was dropped in the move. The
  config field is still required by `internal/ateapiauth/config.go` and is still threaded into
  `RPCService.actorIdentityJWTIssuer`, but nothing reads it any more, so it is now a vestigial
  knob. Callers no longer need to come from that provider at all, and unlike
  `MintActorCertificate` this RPC does not even require a client certificate.

The API comment saying the caller must be the Pod currently running the actor describes the
intended contract, not the implemented authorization. The RPC must not be exposed to actors as-is.

The token's `sub` is `atespaces:<atespace>:actors:<name>` — not the SPIFFE ID, and carrying no
UID — with a `TODO(identity)` in the code saying the format is likely to change. The issuer is
hard-coded to `https://api.ate-system.svc`, also marked as needing to be per-install. Both are
reasons the credential is not yet suitable for an external relying party.

#### Claims currently minted

```jsonc
{
  "iss": "https://api.ate-system.svc",               // not a resolvable OIDC issuer
  "sub": "atespaces:<atespace>:actors:<actor-name>", // not a SPIFFE ID
  "aud": ["..."],                                    // required; empty is rejected
  "exp": "<iat + 15 min>", "nbf": "<iat - 5 min>", "iat": "...", "jti": "...",
  "ate.dev": { "atespace": "...", "actorName": "...", "actorUID": "..." }
}
```

The signing pool reloads its key file periodically and records which key is active for signing, so
keys can be rotated without restarting ateapi. The administrative commands to add, activate, and
retire keys do not exist yet, however, and no JWKS endpoint publishes the pool's verification keys.

The `sub` claim identifies the reusable `(atespace, name)` reference, while the immutable actor
incarnation is only in `ate.dev.actorUID`. Because an actor name can be reused after deletion, a
relying party that needs incarnation-level isolation must currently check `actorUID`, not authorize
from `sub` alone. The recommended fix is to use the UID-bearing canonical SPIFFE ID above as `sub`.

#### SPIFFE classification: close to, but not yet, a JWT-SVID

The stable [SPIFFE JWT-SVID specification](https://github.com/spiffe/spiffe/blob/main/standards/JWT-SVID.md)
already defines the interoperable bearer-token profile Substrate needs. The current actor JWT
should converge on that profile rather than define a Substrate-specific kind of SPIFFE JWT.

| JWT-SVID requirement | Current actor JWT |
| --- | --- |
| Compact JWS and an allowed signature algorithm | Yes; the signer supports an allowed subset |
| `sub` is the workload's SPIFFE ID | **No**; it uses `atespaces:<atespace>:actors:<name>` |
| `aud` and `exp` are present and validated | Minted; `aud` is required and `exp` is 15 minutes |
| One narrowly scoped audience in normal use | Not enforced; the RPC accepts several audiences |
| `iss` or OIDC discovery | Not required by JWT-SVID; the current value matters only to consumers that impose an OIDC contract |
| `typ` is absent, or is `JWT`/`JOSE` | Yes; omission is allowed |
| Verification keys are JWKs in the SPIFFE bundle, each with `use: jwt-svid` and `kid` | **No**; the signing pool is not published as a SPIFFE bundle |

The `ate.dev` private claim is permitted by JWT-SVID, provided consumers understand that it is a
non-standard supplemental claim. It should not be the only place a security-critical identity
distinction lives. Because Substrate authorizes the current egress credential by UID, the
recommended JWT-SVID `sub` is the full UID-bearing actor-incarnation SPIFFE ID. JWT-SVID defines
`sub`, not a private claim, as the primary workload identity.

JWT-SVID does not require `iss` to be a resolvable URL, OIDC discovery, or an OIDC JWKS endpoint.
SPIFFE-native validation obtains JWT signing keys from the trust domain's SPIFFE bundle. A public
issuer and OIDC discovery/JWKS are a separate compatibility layer needed by relying parties such as
OIDC-based cloud federation services. The current internal `iss` is therefore an external-OIDC gap,
but is not what prevents this token from being a JWT-SVID; the non-SPIFFE `sub` and missing
`use: jwt-svid` bundle publication do.

#### The concrete continuation: projected tokens

Draft [PR #1114](https://github.com/agent-substrate/substrate/pull/1114), part 2 of
[issue #802](https://github.com/agent-substrate/substrate/issues/802), is the only open PR that
actually makes an actor JWT consumable. It proposes an `actorIdentityToken` data source in a
template's `systemInfo` volume:

```yaml
volumes:
- name: identity
  systemInfo:
    dataSources:
    - actorIdentityToken:
        audience: https://service.example
        expirationSeconds: 3600 # default 3600; allowed range 600..86400
        path: tokens/service.jwt
containers:
- name: app
  volumeMounts:
  - name: identity
    mountPath: /run/ate
```

This PR does **not** call the public `MintActorJWT` RPC. Ateapi already has the authoritative actor record
while executing Run/Restore, so it signs a token directly from that record immediately before
sending the workload spec to atelet. Atelet writes the already-minted bytes into the host-backed
SystemInfo volume; it has no JWT signing key. The actor reads the file and decides how to present
the token. This placement-time mint avoids the current RPC's caller-to-actor authorization gap
because no requester supplies the identity claims.

The projected-token design is opt-in per ActorTemplate and per audience. It mints a fresh token on
every Run/Restore, but it does not yet refresh an expired token while an actor remains running.
[PR #1231](https://github.com/agent-substrate/substrate/pull/1231) is now on `main` and adds live
refresh machinery for trust bundles specifically; it does not renew JWTs. Live token renewal still
needs a design for having ateapi mint and deliver replacement bytes without putting its signing key
on the node.
Because the audience is fixed in the template, an actor needing several relying parties declares
several token data sources. On-demand audiences would require a broker/Workload API instead.

As currently posted, PR #1114 also predates the active-key signing-pool work on `main`: it reads the
pool file itself and selects the first authority. Before merging it should be rebased to use the
shared `localjwtauthority.Pool.SignJWT` path (and ideally one shared claim builder), otherwise the
RPC and projected-token path can disagree during key rotation or future claim changes.

The SystemInfo file itself is outside snapshot state, but a bearer token that the actor has read
can be present in checkpointed process memory. A fresh token on restore does not revoke the old
one, and neither the current token nor the draft contains a proof-of-possession or activation ID
that a relying party can enforce. A copied token therefore remains replayable anywhere until
`exp`. Short lifetime and audience restriction bound that exposure; they do not eliminate it.

#### What remains before external use

| Piece | Status |
| --- | --- |
| JWT claims and local signing | On `main`; `MintActorJWT` signs 15-minute tokens |
| Actor-visible delivery | Draft PR #1114: SystemInfo file, minted on Run/Restore |
| Live renewal | Not implemented for JWTs |
| Actor authorization in the public `MintActorJWT` RPC | Not implemented (`TODO(authz)`) |
| JWT-SVID conformance (`sub` plus SPIFFE bundle keys with `use: jwt-svid`) | Not implemented |
| Stable public issuer plus OIDC discovery/JWKS for OIDC consumers | Not implemented; [issue #1756](https://github.com/agent-substrate/substrate/issues/1756) is the P0 proposal |
| Cloud federation (GCP/AWS) | Intended by issue #124; no end-to-end implementation |
| Automatic actor-JWT injection at egress | Not implemented |

[PR #1315](https://github.com/agent-substrate/substrate/pull/1315) has since merged. It is not the
delivery continuation: it moved `MintJWT`/`MintCert` into the main `Control` service (as
`MintActorJWT`/`MintActorCertificate`) in anticipation of unified authorization, but it landed the
move without the authorization, leaving the `TODO(authz)` markers described above. The egress
credential-injection stack from
[PR #1360](https://github.com/agent-substrate/substrate/pull/1360) is now on `main`, and it does not
mint actor JWTs either: the gateway calls an out-of-tree gRPC `CredentialProvider` with the actor's
attested SPIFFE ID and injects whatever opaque bytes come back into an HTTP header. That provider
request contains today's reusable, name-based SPIFFE ID, not the actor UID. The gateway has already
validated the certificate's UID against the live actor before making the request, but the provider
cannot independently distinguish a deleted-and-recreated actor incarnation from this field alone;
it is trusting the authenticated gateway's assertion for the current request.

A future actor-JWT egress provider could combine the two designs: use the actor certificate already
presented to the gateway to select the actor, ask ateapi for a token bound to the destination's
configured audience, and inject it after TLS interception. That would keep the bearer token out of
the sandbox, but it would work only for protocols the gateway understands and terminates. No issue
or PR currently implements that path.

## How a certificate is minted

The actor never participates. The worker pod's ateom drives the mint before the workload boots or
restores, and if the mint fails the actor does not start (tunneled egress fails closed).

```
atunnel, inside the ateom process (worker pod, outside the actor sandbox)
   │  generates a P-256 key in memory; only a CSR ever leaves the process
   │  dials atelet's credential-broker Unix socket over mTLS with the worker's pod certificate
   │  requires the peer to be atelet (SPIFFE ID spiffe://cluster.local/ns/ate-system/sa/atelet)
   │  AND to carry the same node name and node UID as the worker itself
   ▼
atelet AteomSupport (node)
   │  requires a pod certificate for this node (same node name/UID); access to the host socket
   │  is normally limited to worker pods by its mount placement
   │  authenticates *which* ateom is calling, but no longer derives the actor from it: the
   │    request now carries actor_atespace, actor_name and actor_uid, and the broker forwards
   │    them as given (see the TODO(identity) in cmd/atelet/ateomsupport.go)
   │  sets Purpose=atunnel itself; relays the CSR to ateapi over mTLS
   ▼
ateapi Control.MintActorCertificate
│  requires the peer to have authenticated with a client certificate, so a bearer credential
│    cannot bootstrap a proof-of-possession credential; in the shipped deployment the TLS
│    listener verifies presented client certificates against the pod-identity CA
│  does not require the certificate to identify atelet or inspect its PodIdentity extension
   │  looks the actor up by the (atespace, name) in the request; NotFound if absent
   │  rejects with Aborted if the stored actor UID differs from actor_uid ("actor has been
   │    deleted and recreated")
   │  does NOT check that the caller is atelet, that the caller's node hosts the actor, or that
   │    any worker assignment names this actor — that is an open TODO(authz)
   │  signs with the actor-identity CA pool
   ▼
actor certificate chain returned to atunnel
   │  atunnel verifies: key matches, lifetime valid, ClientAuth EKU, ActorIdentity extension
   │  present, Purpose=atunnel, UID equals the activation's expected UID

actor application container(s), inside the gVisor sandbox or micro-VM
   └── see no private key, certificate, or credential-broker socket
```

Design properties of this chain:

- **The caller now names the actor, and ateapi does not yet check that it may.** Before #1315,
  ateapi derived the actor from the authenticated worker's assignment, so a worker could not
  obtain an identity for an actor it was not running. That reciprocal lookup is gone: the request
  carries `(atespace, name, uid)` and ateapi verifies only that such an actor exists with that
  UID. In the shipped deployment, any workload holding a client certificate issued by the
  pod-identity CA and able to reach ateapi can therefore mint an atunnel certificate for any actor
  whose atespace, name and UID it knows; the RPC does not restrict that set to atelet. The
  replacement check is declared but not wired: `internal/authz/model.fga` defines
  `can_mint_ateom_actor_credential: host_node`, and no Go code imports that package yet. Treat
  this as the largest open gap in the shipped identity path.
- **The node-local broker hop authenticates infrastructure, but no longer binds it to the actor.**
  atunnel and atelet each require the other's pod certificate to carry their own node name *and*
  node UID, and the broker socket's mount placement normally limits broker callers to worker pods.
  Those checks constrain use of the broker. They do not constrain a pod-identity client that calls
  ateapi directly, and neither hop now proves that the authenticated workload hosts the requested
  actor.
- **Denials are no longer indistinguishable.** `NotFound` (no such actor) and `Aborted` (UID
  mismatch) are distinct, so a caller that can reach the RPC can probe which actor names and
  UIDs exist.
- **A fresh connection per mint** means rotated worker credentials and atelet's current node
  identity are re-verified every time. `TODO(identity)` in `internal/atunnel/credential.go`
  proposes replacing this with one long-lived connection at atunnel startup, which would trade
  that re-verification for fewer handshakes.
- **Mint time is not gated on the RUNNING state.** The mint happens during Run/Restore while the
  actor is still RESUMING. Both dataplanes check for RUNNING later, at CONNECT time, so a
  certificate minted for an actor that never reaches RUNNING opens no tunnel, and one whose actor
  leaves RUNNING — including through `RevertActor`'s `REVERTING` transition — opens no further one.

## Key placement and suspend/resume

The actor's private key exists only in atunnel's memory, for one activation. Nothing about the
credential is written to disk or captured in a snapshot.

- Renewal starts at 90% of the certificate's remaining lifetime.
- If the certificate expires before renewal succeeds, atunnel keeps retrying with jitter, refuses
  new egress connections, and lets established tunnels drain.
- If the broker answers `Aborted`, `FailedPrecondition` or `PermissionDenied` (the actor was
  reassigned or deleted), renewal stops for that activation. `Aborted` is what
  `MintActorCertificate` now returns on a UID mismatch.
- On checkpoint or suspend, deactivation cancels the activation context and force-closes every
  live tunnel.
- On restore, possibly on another node, a new key is generated and a fresh certificate is minted.
  The SPIFFE ID and UID are unchanged; only the key and certificate are new.

This gives the three properties the design set out to get: no stale credential can be frozen
into a snapshot, no key material is ever in a snapshot, and identity follows the actor record
regardless of which worker hosts it.

## What the sandbox can see

On `main` there is **no identity credential inside the sandbox**. The actor is unmodified and
unaware: no SDK, no environment variable, no Workload API socket. Draft PR #1114 would change that
for templates that explicitly request an `actorIdentityToken` SystemInfo data source.

What atelet does project into the sandbox, when a template asks for it, is a `systemInfo` volume:
regular files populated on every Run/Restore and kept outside durable storage so they never enter a
snapshot. Two data sources exist:

- `actorMetadata`: any of the actor's `name`, `atespace`, `uid`, each to a caller-chosen path.
- `trustBundle`: a PEM bundle resolved from a Kubernetes ClusterTrustBundle. The only allowlisted
  name is `egress-mitm.ate.dev`, the egress gateway's TLS-interception CA. Templates must declare
  it; nothing injects it automatically. Since #1231, atelet watches the backing
  `egress-mitm.ate.dev:mitm:primary-bundle` object and atomically refreshes this file for registered
  running actors, retaining the last good contents if the bundle is deleted or malformed. The
  registration remains in memory. If a worker crash leaves a stale registration and the same actor
  UID later starts again on that node, the new registration supersedes the stale one; it no longer
  panics. An atelet restart still loses the whole registry, so live refresh stops for an
  already-running actor until its next Run/Restore.

`systemInfo` volumes work on both the gVisor and micro-VM runtimes (the micro-VM guest receives
them over the shared virtio-fs tree).

## Design options

### Option A: SPIFFE JWT-SVID bearer token

The actor receives or fetches a per-audience token and sends it as a bearer credential. No private
key exists in the actor. Replay is bounded, but not prevented, by audience and token lifetime.
JWT-SVID is a stable SPIFFE specification designed for Layer 7 compatibility with existing JWT
applications. It is also the closest shape to the subject tokens accepted by OIDC-style cloud
federation, although those systems additionally need the public issuer/JWKS compatibility layer
described above. Delivery could be a projected file (PR #1114) or a future node-local Workload API.

Status: the minting primitive exists (`MintActorJWT`, with the authorization gaps above), and projected
file delivery is under review in PR #1114. No complete, externally verifiable or continuously
renewed path exists. This is still the right credential shape for external federation and for
per-actor snapshot storage access, because those relying parties never see the egress tunnel.

### Option B: SPIFFE WIT-SVID plus WIMSE proof of possession

[SPIFFE WIT-SVID](https://github.com/spiffe/spiffe/blob/main/standards/WIT-SVID.md) is an
**incubating** SPIFFE profile of the IETF
[WIMSE Workload Identity Token](https://datatracker.ietf.org/doc/draft-ietf-wimse-workload-creds/).
Every WIT-SVID is a WIMSE WIT, with SPIFFE narrowing the choices so implementations interoperate.
This is the standards path for proof of possession; Substrate should not create its own
`cnf`-bearing actor-token profile.

A WIT-SVID is not a replacement bearer JWT:

- Its `sub` is the actor SPIFFE ID, its JOSE `typ` is `wit+jwt`, and its `cnf` claim contains the
  actor's public key. Issuer verification keys are separate SPIFFE-bundle entries with
  `use: wit-svid`.
- It deliberately has no `aud`. Recipient scope belongs in the proof generated for a particular
  invocation. It must never be accepted as a bearer token or sent in `Authorization`. Its `iss`,
  if present, should not be OIDC-discoverable, specifically to prevent an OIDC validator from
  accepting the WIT-SVID without proof of possession.
- With the [WIMSE WPT protocol](https://datatracker.ietf.org/doc/draft-ietf-wimse-wpt/), the request
  sends the WIT-SVID in `Workload-Identity-Token` and a separate, very short-lived
  `Authorization: WPT <token>`. The WPT has `typ: wpt+jwt`, is signed by the `cnf` private key, and
  contains the target `aud`, expiry, a unique `jti`, and `wth`, a hash binding it to the WIT. The
  relying party validates both tokens and should maintain a replay cache.
- Because WPT occupies `Authorization`, the same request cannot also carry an ordinary bearer
  credential there. End-user or transaction context has to use a separate header and, where
  applicable, be bound into the proof.

This is useful when the relying party understands WIMSE and wants to know that the caller still
possesses the actor key for this invocation. Stealing the WIT alone is insufficient, and a receiver
cannot simply forward it to impersonate the actor as it can with a bearer JWT-SVID. It complements
the current mTLS tunnel: mTLS authenticates the atunnel-to-gateway channel, while WIT-SVID plus WPT
authenticates an application-layer invocation to a WIMSE-aware destination.

That split also matches WIMSE's own vocabulary: its Workload Identity Certificate (WIC) primarily
targets transport-layer authentication, while WIT primarily targets the application layer. The
current actor certificate plays the analogous transport role inside Substrate, but has not been
audited for WIMSE WIC conformance. Adding WIT-SVID/WPT would not require removing that mTLS path.

The hard Substrate question is key custody, not JWT construction. The SPIFFE Workload API WIT
profile returns both the WIT-SVID and its private key to the workload. Doing that here puts usable
key material in actor memory and therefore potentially in a checkpoint. Keeping the key outside
the sandbox instead requires a trusted component to sign every proof:

- atelet or atunnel could hold a per-activation key, but the actor needs a signing API and must
  provide the exact audience and other protected context. Today's atunnel is an L4 tunnel and
  cannot transparently construct end-to-end HTTP proofs it cannot see.
- The egress gateway could sign after TLS interception, but only for protocols it terminates, and
  storing per-actor signing keys there expands its authority and blast radius.
- Projecting only a WIT-SVID file, without its private key or a signing service, provides no usable
  credential because conforming verifiers must require proof of possession.

Status: not built. There is no Substrate issue or PR tracking WIT-SVID/WPT implementation today;
this is a design option, not an announced implementation plan. The repository pins `go-spiffe`
v2.7.0, which added experimental WIT-SVID, WIT-bundle, and Workload API client support, but no
Substrate code uses that credential flow. That initial version also predates the
[v2.8.1 fix](https://github.com/spiffe/go-spiffe/blob/main/CHANGELOG.md) that adds the required
`use: wit-svid` to WIT-bundle JWKS output, so it must be upgraded before Substrate relies on that
representation. The WIMSE documents are still Internet-Drafts and the SPIFFE WIT-SVID profile is
incubating, so an implementation should be feature-gated and tested against a concrete relying
party.

### Option C: proxy-held X.509 identity (what shipped)

atunnel, in the worker pod, holds the actor's key and presents the actor certificate on the
outer mTLS connection to Substrate's egress gateway. Traffic is intercepted transparently, so the
actor needs no code, no SDK, and no credential surface.

Costs:

- The actor certificate authenticates only the atunnel→gateway hop. The external destination
  never sees it; the gateway's own network identity is what the destination sees. Option C does
  not give an actor mTLS identity to an outside relying party.
- Identity is applied at L4. On the default opaque tunnel the gateway knows the destination
  IP:port and nothing else. Hostname visibility requires the opt-in TLS-interception mode.
- Anything the actor must present *inside* an application protocol (an `Authorization` header
  to an MCP server, a signed cloud API request) is out of reach. This is what keeps Option A on
  the roadmap. The egress policy API's header-injection effect closes part of that gap on the
  gateway side and is enforced on Envoy's TLS-interception leg, but it injects opaque bytes
  fetched from a credential provider, not an actor-scoped token: nothing in it mints or presents
  the actor's own identity to the destination (see
  [EGRESS.md](./EGRESS.md#credential-injection)).

### Trade-offs

| Property | A: JWT-SVID | B: WIT-SVID/WPT | C: proxy-held X.509 (shipped) |
| --- | --- | --- | --- |
| Private key in actor | No | Depends: yes with standard Workload API delivery; no with an external signer | No |
| Private key location | none | Open: actor, atelet/atunnel, or gateway | atunnel memory (worker pod), per activation |
| Actor-visible credential surface | projected file in PR #1114; socket/SDK possible | WIT plus key, or a per-request signing API | **none** |
| Actor code changes | yes | yes, unless an L7 proxy constructs proofs | **none** |
| Standards maturity | SPIFFE stable | SPIFFE incubating + IETF Internet-Drafts | custom current profile |
| WIMSE WIT conformant | no; this is a SPIFFE bearer profile | yes | no |
| Replay defense | audience + lifetime | PoP + audience + short lifetime; `jti` replay cache recommended | mTLS session + 1h lifetime; Envoy path adds a live actor check per CONNECT |
| Identity visible to external service | yes (token) | yes (token) | **no** |
| Works with cloud OIDC federation | yes, once issuer/JWKS exist and provider policy is configured | generally no; WIT-SVID is intentionally not an OIDC bearer token | no |
| Gives an external mTLS destination the actor identity | no | no | no; mTLS ends at the Substrate egress gateway |
| Broker/signing load | high unless tokens are cached | WIT mint is cacheable; each invocation needs a local signature | ~one mint per hour per activation |
| Key material may enter snapshots | none | **yes** with normal Workload API delivery; no with external signer | none |
| Usable credential may enter snapshot memory | **yes, until JWT expiry** | WIT plus key if actor-held; WIT alone is unusable | no |

**Recommendation.** Keep Option C as the transport-identity foundation. Make Option A a conforming
JWT-SVID before exposing it: use the UID-bearing actor-incarnation SPIFFE ID as `sub`, publish the
signing keys as `use: jwt-svid` SPIFFE-bundle entries, default to one audience, and share claim
construction across `MintActorJWT` and PR #1114. Add a public OIDC issuer/JWKS only as a separate adapter
for relying parties that require it. Do not gradually turn that bearer token into a WIT by adding a
`cnf` claim; implement Option B separately, with `typ: wit+jwt`, distinct `use: wit-svid` keys, and
a complete WPT or HTTP Message Signatures presentation flow. Adopt it only for a concrete relying
party and after deciding where its per-actor private key lives.

An issue for the near-term bearer path should stay narrowly scoped to JWT-SVID conformance:

1. Define the canonical actor SPIFFE ID as the UID-bearing actor-incarnation path above and use one
   shared constructor for certificate and token issuance.
2. Use that SPIFFE ID as JWT-SVID `sub`, issue one audience per token by default, and retain short
   expiry. Keep other fields as supplemental correlation data rather than identity repairs.
3. Publish rotation-overlapped verification keys in the trust domain's SPIFFE bundle with unique
   `kid` values and `use: jwt-svid`; test tokens with a conforming SPIFFE validator.
4. Make `MintActorJWT` and projected-token issuance share claim construction and derive identity from
   authoritative actor/assignment state.
5. Track public OIDC discovery/JWKS and live token delivery/renewal as related but separable work.

A WIT-SVID issue should be separate and begin with a relying-party interoperability test and a
decision about whether the actor or a trusted signer owns the `cnf` private key. Without those two
inputs, implementing WIT minting alone would create an unusable credential and prematurely commit
Substrate to an application-layer signing architecture.

## The credential broker

Since [#1501](https://github.com/agent-substrate/substrate/pull/1501) the separate
`CredentialBroker` and `WorkerCapacity` services are one service, `AteomSupport`, defined in
`internal/proto/ateletpb` and served from `cmd/atelet/ateomsupport.go`. It carries two RPCs,
`MintActorCertificate` and `SetWorkerCapacity`, over a host Unix socket at
`/var/lib/ateom-gvisor/ateom-support.sock` (the `ateompath.AteomSupportSocket` constant), chmodded
to `0600` and requiring mutual TLS. The minting half is an identity-issuance component, not an
egress component; some log strings still say "credential broker".

Because the socket is shared by every worker on the node, file permissions cannot distinguish
workers; mutual TLS with pod certificates does, and both sides additionally require the peer's node
name and node UID to match their own. The client side of that check is shared code used by every
ateom-to-atelet caller, so atunnel and the capacity reporter cannot drift apart.

> [!NOTE]
> File permissions are not the boundary in a second sense either. The socket's base path is mounted
> **writable** into worker pods, which `internal/ateompath` documents: a worker pod can unlink or
> replace the socket. mTLS authenticates who is calling; it does not stop a compromised worker from
> denying the node-local service to its neighbors.

The caller supplies the actor atespace, name, and UID as well as the CSR. It cannot choose the
certificate purpose: atelet fixes that to `ATUNNEL` before relaying, so a compromised worker cannot
widen its own credential by asking. atelet authenticates the caller's PodIdentity extension and
then discards the result — the `TODO(identity)` in `ateomsupport.go` asks whether it should check
that this ateom is in fact running the requested actor. It forwards the caller-supplied actor
fields unchanged. The private key never crosses the socket.

The original worker certificate is not forwarded end to end, so ateapi sees atelet's client
certificate. Since #1315, ateapi does not require that certificate to identify atelet and does not
resolve a reciprocal worker↔actor assignment. It verifies only that the requested actor exists and
has the requested UID. A compromised worker that can use its node's broker is therefore no longer
confined to its assignment, and any other pod-identity client able to reach ateapi can bypass the
broker and call the mint RPC directly. Both hops have explicit authorization TODOs.

This is also why `AteomSupport` has its own socket rather than sharing the per-pod ateom lifecycle
socket (Run, Restore, Checkpoint): ateom is trusted infrastructure, the actor is untrusted
workload, and the two trust boundaries should not share a channel. Merging the capacity service in
did not cross that line — both of its RPCs are ateom-to-atelet calls with the same peer
authentication.

The broker exposes only X.509 minting today. One possible JWT renewal design is a new broker RPC
with explicit audience and delivery authorization, deriving the actor through the authenticated
worker assignment as certificate minting did before #1315. The existing `MintActorJWT` RPC now
cross-checks the requested actor and UID against the database, but must not simply be exposed
through the broker until both hops authorize the authenticated caller for that actor.

PR #1114 chooses a different bootstrap path: ateapi signs from the actor record already in hand
during Run/Restore, then sends only the token bytes to atelet for projection. That is simpler and
avoids a separate authorization exchange at startup, but it does not by itself solve renewal while
the actor stays running. A second certificate `Purpose` value is needed only for a new non-atunnel
certificate consumer.

## Delivery channels for a future actor-facing credential

Option C needs none. Option A needs either a trusted component to place a token into the sandbox or
an actor-to-broker channel whose endpoint identifies the activation.

- **Files.** The `systemInfo` volume is the vehicle upstream has chosen for data into the sandbox.
  A signed actor identity token as a `systemInfo` data source is under review; it would be
  projected like a Kubernetes service-account token and regenerated on every Run/Restore.
- **Unix socket bind-mounted into the sandbox.** The classic SPIFFE Workload API pattern
  (`SPIFFE_ENDPOINT_SOCKET`), with atelet identifying the actor by which per-actor socket the
  connection arrived on. gVisor's gofer can pass a host socket through. Not built; would need a
  new streaming contract, since today's `systemInfo` volumes are regular files only.
- **vsock.** The natural channel for the micro-VM runtime. On gVisor, `AF_VSOCK` terminates in the
  sentry, so ateom would have to forward, adding a hop. Not built. A host Unix socket does not
  appear inside a micro-VM guest automatically, so a socket-based Workload API would need vsock or
  explicit forwarding there.

## Authentication of the control plane itself

ateapi accepts mTLS client certificates (pod identity) and bearer JWTs from configured OIDC
providers. The system components (atelet, atenet, atecontroller) dial ateapi with pod-identity
mTLS. Everything else, including `kubectl ate`, the setup tool, and the e2e harness, goes through
the shared client library, which attaches a bearer JWT: a token file if one is supplied, otherwise
a Kubernetes service-account token it mints for the `ate-client` service account in `ate-system`,
usually over a port-forward. `actorIdentityJWTProvider` used to name the one provider allowed to
call the actor-JWT RPC, but since #1315 nothing reads it (see above); the field is still required
by config validation. There is no general per-RPC authorization or RBAC layer; only checks inside
individual methods. Any configured JWT provider can call the general control-plane RPCs and
`MintActorJWT`; `MintActorCertificate` additionally requires mTLS. Configure only providers whose
users should control the whole control plane. A caller presenting a JWT from an issuer that is not
configured is told so in the error, and the installer accepts an override for the issuer ateapi
expects.

atelet authenticates to ateapi with its pod certificate. Worker pods authenticate to atelet the
same way. Network connections from ateapi to external CSI driver controllers can also use
pod-identity mTLS, but only when TLS is enabled on the `CSIDriverConfig`; with TLS absent or
disabled the gRPC connection is insecure, and pod-identity is the only TLS mode supported.
Node-side CSI connections go over Unix sockets without TLS.

## Trust bundles

| Trust material | Signs | Who verifies with it | Rotation |
| --- | --- | --- | --- |
| Actor-identity CA (`actor-id-ca-pool`; signing key mounted only in ateapi) | actor certificates | Envoy or agentgateway TLS listener; atenet's Envoy ext_proc verifies the chain again, agentgateway trusts the TLS layer's result | signing side is pool-based and rotation-ready; the gateway's copy is a cert-only Secret `actor-id-ca-certs` derived at install and loaded from a static filename, so rotating the actor CA requires re-deriving the Secret and restarting or otherwise reloading the gateway |
| Actor JWT pool (`actor-id-jwt-pool`; signing key mounted only in ateapi) | actor JWTs | nobody in the shipped data path; SPIFFE consumers need a JWT bundle with `use: jwt-svid`, while OIDC consumers may need discovery/JWKS | signing pool reloads and supports selecting an active key; administration and both publication paths are missing |
| Pod identity (`podidentity.podcert.ate.dev/identity`) | atelet, workers, control-plane pods | broker socket, atunnel ingress, ateapi client auth | Kubernetes PodCertificateRequests + ClusterTrustBundles; projected volumes rotate |
| Service DNS (`servicedns.podcert.ate.dev/identity`) | serving certificates for in-cluster services | atunnel verifying the egress gateway | projected PodCertificate and ClusterTrustBundle volumes rotate; the default Envoy gateway watches its serving-credential directory through filesystem SDS |
| Egress TLS-interception CA (`egress-mitm-ca-pool`) | per-SNI leaf certificates in TLS-interception mode | actors, via the `systemInfo.trustBundle` data source | published as ClusterTrustBundle `egress-mitm.ate.dev:mitm:primary-bundle` by atecontroller |

## Verification cookbook (`kind-substrate`)

These checks divide the claims above into two groups. Deployment checks show the state and wiring
that Kubernetes and the Substrate API can expose. Focused tests cover properties that should not
be externally observable, such as private-key placement, certificate contents, mint
authorization, renewal, and fail-closed behavior. A successful command proves the stated signal;
it is not a substitute for a broader security review.

The examples assume the core system was installed before the demo:

```bash
# Omit --atenet-dataplane=agentgateway to use the default Envoy dataplane.
hack/install-ate-kind.sh --atenet-dataplane=agentgateway --deploy-ate-system
hack/install-ate-kind.sh --deploy-demo-egress
```

They require `kubectl`, `kubectl-ate`, `jq`, and `openssl`. The broker-socket check additionally
requires `kind` and Docker. They deliberately print live UIDs, pod names, image digests, and
certificate fingerprints rather than embedding values that will go stale.

```bash
export CTX=kind-substrate
export ATESPACE=ate-demo-egress
export ACTOR=egress-demo
```

### Confirm the deployed revision and images

```bash
git fetch upstream main
git rev-parse --short=8 upstream/main

kubectl --context "$CTX" get nodes -L ate.dev/substrate-version

kubectl --context "$CTX" get daemonset -n ate-system -l app=atelet -o json |
  jq -r '.items[] | [.metadata.name,.spec.template.spec.containers[0].image] | @tsv'

kubectl --context "$CTX" get deployment -n ate-system ate-api-server -o json |
  jq -r '.spec.template.spec.containers[] | [.name,.image] | @tsv'

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

The node label, versioned atelet DaemonSet, ateapi binary, and demo worker should report the
revision intended for the install. A `-dirty` suffix is expected when images were built from a
worktree with local changes. The final check also accepts a newer docs-only upstream commit, since
that does not change an image's executable source. The image queries should include immutable
digests.

The node label is also the atelet upgrade control: the installer intentionally does not overwrite
an existing label. On this disposable single-node kind cluster only, an in-place rebuild can be
advanced to the version already reported by the newly deployed ateapi with:

```bash
SUBSTRATE_VERSION="$(kubectl --context "$CTX" exec -n ate-system \
  deploy/ate-api-server -- /ko-app/ateapi --version | awk '{print $1}')"

kubectl --context "$CTX" label nodes --all \
  "ate.dev/substrate-version=$SUBSTRATE_VERSION" --overwrite

kubectl --context "$CTX" rollout status -n ate-system \
  "daemonset/atelet-$SUBSTRATE_VERSION" --timeout=120s
```

Do not use the all-nodes form for a production rollout; move nodes between versioned atelet
DaemonSets according to the cluster's upgrade plan.

### Show the actor UID and reciprocal worker assignment

Since #1627 a single named resource prints as a bare object rather than a
one-element list, so these read `.metadata`, not `.actors[0].metadata`.

```bash
ACTOR_JSON="$(kubectl ate --context "$CTX" get actors "$ACTOR" \
  -a "$ATESPACE" -o json)"

printf '%s\n' "$ACTOR_JSON" |
  jq '{
    actorUID: .metadata.uid,
    state: .status.state,
    worker: .status.workerAssignment.worker.name,
    workerPod: .status.workerAssignment.workerPod
  }'

WORKER="$(printf '%s\n' "$ACTOR_JSON" |
  jq -r '.status.workerAssignment.worker.name')"

kubectl ate --context "$CTX" get workers -o json |
  jq --arg worker "$WORKER" '.workers[] |
    select(.metadata.name == $worker) | {
      worker: .metadata.name,
      nodeName,
      workerNamespace,
      workerPod,
      capacity: .status.capacity,
      allocated: .status.allocated
    }'
```

The actor's worker name must resolve to a real worker, and the namespace and pod in both records
must agree. This reciprocal assignment used to be what `MintCert` checked before signing; since
#1315 `MintActorCertificate` no longer consults it, so this is now a consistency check on
scheduler state rather than a view of an enforced authorization step.

### Prove the UID survives suspend/resume

This block checkpoints and restores the disposable demo actor. It interrupts that actor and
should not be aimed at a shared or production workload. If the command is interrupted after the
suspend, run the resume command manually.

```bash
ACTOR_UID_BEFORE="$(kubectl ate --context "$CTX" get actors "$ACTOR" \
  -a "$ATESPACE" -o json | jq -r '.metadata.uid')"

kubectl ate --context "$CTX" suspend actor "$ACTOR" -a "$ATESPACE"
kubectl ate --context "$CTX" resume actor "$ACTOR" -a "$ATESPACE"

ACTOR_UID_AFTER="$(kubectl ate --context "$CTX" get actors "$ACTOR" \
  -a "$ATESPACE" -o json | jq -r '.metadata.uid')"

test "$ACTOR_UID_BEFORE" = "$ACTOR_UID_AFTER" &&
  printf 'stable actor UID: %s\n' "$ACTOR_UID_AFTER"

kubectl ate --context "$CTX" get actors "$ACTOR" -a "$ATESPACE"
```

The final state should be `ACTOR_STATE_RUNNING`, and the equality check must print the stable
UID. This proves identity-record continuity. A fresh in-memory key and certificate are an
implementation property checked by the focused tests below, not by this API output.

### Inspect pod-identity and service-DNS projections

```bash
kubectl --context "$CTX" get daemonset -n ate-system -l app=atelet -o json |
  jq '.items[0].spec.template.spec.volumes[] |
    select(.projected != null) |
    {name, sources: [.projected.sources[] | keys[]]}'

kubectl --context "$CTX" get deployment -n "$ATESPACE" egress -o json |
  jq '.spec.template.spec.volumes[] |
    select(.projected != null) |
    {name, sources: [.projected.sources[] | keys[]]}'

kubectl --context "$CTX" get clustertrustbundles
```

The atelet and worker projections should include `podCertificate` and
`clusterTrustBundle` sources. A default non-intercepting install has pod-identity and service-DNS
bundles but no `egress-mitm.ate.dev` bundle; that bundle appears only when TLS interception is
enabled.

### Check signer-key placement and the gateway trust copy

This prints secret names and data-key names only; it does not decode private material.

```bash
kubectl --context "$CTX" get pods -A -o json |
  jq -r '.items[] as $pod |
    $pod.spec.volumes[]? |
    [(.secret.secretName // empty),
     (.projected.sources[]?.secret.name // empty)][] |
    select(. == "actor-id-ca-pool") |
    [$pod.metadata.namespace,$pod.metadata.name] | @tsv'

kubectl --context "$CTX" get secret -n ate-system \
  actor-id-ca-pool actor-id-ca-certs -o json |
  jq -r '.items[] | [.metadata.name, (.data | keys | join(","))] | @tsv'

kubectl --context "$CTX" get secret actor-id-ca-certs -n ate-system \
  -o jsonpath='{.data.ca\.crt}' |
  base64 -d |
  openssl x509 -noout -subject -issuer -fingerprint -sha256
```

Only ateapi pods should mount `actor-id-ca-pool`. The pool contains signing material, while
`actor-id-ca-certs` should contain only `ca.crt`; the final command fingerprints the public CA
the gateway uses to authenticate actors.

### Check the node-local `AteomSupport` socket

```bash
KIND_CLUSTER="${CTX#kind-}"
KIND_NODE="$(kind get nodes --name "$KIND_CLUSTER" | head -n 1)"
docker exec "$KIND_NODE" stat -c '%a %U:%G %F %n' \
  /var/lib/ateom-gvisor/ateom-support.sock
```

The first field should be `600` and the file type should be `socket`. The path changed with #1501,
which merged the credential broker and the worker-capacity service into one `AteomSupport` service
on one socket. Permissions alone do not distinguish worker pods because they share the host mount;
the mTLS pod identity and node name/UID checks are the authorization boundary — and the mount is
writable, so a worker can replace the socket even though it cannot impersonate a caller on it.

### Run focused identity regressions

```bash
# #1315 deleted cmd/ateapi/internal/actoridentity and its 13-case suite, including
# every MintCert authorization test. What replaced it server-side is one case:
go test ./cmd/ateapi/internal/controlapi/functionaltest \
  -run '^TestMintActorJWT_Success$' -count=1

# #1315 also removed TestCredentialBrokerForwardsAuthenticatedWorkerIdentity, the test
# that pinned the broker deriving the worker from its certificate rather than the request.
# #1501 then renamed the file to ateomsupport_test.go. What remains of the minting
# hop's own coverage is the same-node check; no test calls MintActorCertificate:
go test ./cmd/atelet -run '^TestVerifyClientOnSameNode$' -count=1

go test ./internal/atunnel \
  -run 'Test(BrokerCertificateSource|Egress)' -count=1

go test ./internal/substratex509 ./internal/localca \
  ./internal/localjwtauthority -count=1
```

For the complete set of directly related packages:

```bash
go test ./cmd/ateapi/internal/controlapi/... ./cmd/atelet \
  ./internal/ateletdial ./internal/atunnel ./internal/substratex509 \
  ./internal/localca ./internal/localjwtauthority ./internal/actoridjwt
```

Useful source-level spot checks are:

```bash
# One P-256 key is created per BrokerCertificateSource; only a CSR is sent.
rg -n 'GenerateKey|CreateCertificateRequest|CertificateSigningRequest' \
  internal/atunnel/credential.go

# Socket creation and the explicit 0600 chmod.
rg -n 'AteomSupportSocket|Chmod.*0o600' cmd/atelet/main.go

# Production references show MintActorJWT's implementation, but no caller.
rg -n 'MintActorJWT' --glob '*.go' --glob '!**/*_test.go' \
  --glob '!**/*.pb.go' --glob '!**/*_grpc.pb.go'

# Both minting RPCs still carry their authorization TODOs.
rg -n 'TODO\(authz\)' cmd/ateapi/internal/controlapi/actor.go

# The intended check is declared, and OpenFGA is now a real dependency wired
# into ateapi's bootstrap, but no minting path consults either.
rg -n 'can_mint_ateom_actor_credential' internal/authz/model.fga
rg -n 'internal/authz' --glob '*.go'
rg -n 'openfga' go.mod

# The shared SPIFFE ID constructor and everything that uses it.
rg -n 'ActorSPIFFEID|ActorRefFromSPIFFEID' --glob '*.go' --glob '!**/*_test.go'
```

## Not built yet

- **Canonical actor-incarnation SPIFFE ID.** The current certificate URI SAN omits UID and relies
  on a Substrate-specific extension for incarnation authorization. The JWT `sub` is not a SPIFFE
  ID at all and likewise leaves UID in a private claim. Both should use the same UID-bearing SPIFFE
  ID, and JWT verification keys should be published in a SPIFFE bundle with `use: jwt-svid`.
- **JWKS / OIDC discovery compatibility** for OIDC-based actor federation. Nothing serves it, and
  `iss` is not a resolvable URL. This is separate from SPIFFE-native JWT-SVID bundle distribution.
  [Issue #1756](https://github.com/agent-substrate/substrate/issues/1756) is the upstream proposal
  and is scoped ahead of the M3 cutoff: a read-only `ate-oidc-server` serving discovery and JWKS,
  an `--actor-jwt-issuer` flag for the public issuer, RS256 signing and RFC 7638 thumbprint key
  ids, and a rotation story. Egress-side JWT injection
  ([#1660](https://github.com/agent-substrate/substrate/issues/1660)) and RFC 8693 token exchange
  ([#1661](https://github.com/agent-substrate/substrate/issues/1661)) both block on it.
- **Trust domain as configuration.** `substrate-actor.local` and the atelet SPIFFE identity are
  hard-coded.
- **Server-side authorization tests for the minting RPCs.** #1315 deleted
  `cmd/ateapi/internal/actoridentity` along with its 13-case suite — including
  `TestMintCertAuthorization`, `TestMintCertAuthorizesBeforeSigning`,
  `TestMintCertDeniesUnassignedActorWhateverItsState` and
  `TestMintJWTRequiresConfiguredJWTProvider` — and added one replacement,
  `TestMintActorJWT_Success`. It also removed
  `TestCredentialBrokerForwardsAuthenticatedWorkerIdentity` from `cmd/atelet`. No test now
  exercises `MintActorCertificate` on the server side at all. The behavior those tests pinned is the behavior that regressed, so restoring coverage and
  closing the `TODO(authz)` are the same piece of work.
- **Caller authorization in the public minting RPCs.** #1315 added the actor-database cross-check
  that was previously missing from the JWT path, but neither `MintActorJWT` nor
  `MintActorCertificate` checks that the *caller* is entitled to that actor; both carry a bare
  `TODO(authz)`. `internal/authz/model.fga` already defines the intended relation
  (`can_mint_ateom_actor_credential: host_node`), and since
  [PR #1670](https://github.com/agent-substrate/substrate/pull/1670) that model is no longer
  inert: `ateapi` boots an embedded, PostgreSQL-backed OpenFGA server, compiles the model into it
  and serializes startup with an advisory lock, `cmd/ateapi/main.go` imports `internal/authz`, and
  the top-level scope was renamed `cluster` to `global`. What is still missing is the call site —
  no minting path performs a check, so the relation grants nothing yet. PR #1114 sidesteps this at
  startup by deriving claims inside the trusted Run/Restore workflow; any on-demand API still
  needs it.
- **Administrative rotation of the JWT signing pool.** The pool can already switch its active key
  without a restart; the commands to add, activate, and retire keys are not written.
- **Per-actor credentials for snapshot storage.** atelet reads and writes every actor's snapshots
  with its own node-level credential (GCS via application default credentials or GKE Workload
  Identity; S3 via the default AWS credential chain), constructed once at startup. The intended
  end state is a per-actor federated credential and a bucket policy keyed on the actor UID, which
  depends on the JWKS and `sub` items above.
- **Rotation of the actor-identity trust bundle at the gateway** (see the table above).
- **A second certificate purpose.** Parsers reject anything but `atunnel`, so any non-atunnel
  consumer needs the enum extended first.
- **An actor-facing credential**: the `actorIdentityToken` `systemInfo` data source
  ([PR #1114](https://github.com/agent-substrate/substrate/pull/1114)), a Workload API socket, or
  vsock delivery.
- **A WIT-SVID and proof-of-possession path.** No issue or PR chooses the private-key holder,
  implements WPT or HTTP Message Signatures, or identifies a WIMSE-aware relying party. The
  experimental `go-spiffe` dependency support is not a Substrate implementation.
- **Live renewal of projected actor JWTs.** PR #1114 mints only on Run/Restore. PR #1231 refreshes
  trust bundles, not tokens; JWT renewal must keep signing in ateapi and deliver replacements to the
  hosting atelet.
- **Automatic injection of the egress trust bundle** into every actor
  ([PR #1252](https://github.com/agent-substrate/substrate/pull/1252)); today templates must declare
  it.
- **An actor-JWT credential provider.** Generic egress credential injection *is* on `main` since
  [PR #1360](https://github.com/agent-substrate/substrate/pull/1360), but it resolves an opaque
  `ate-secret://` URI through a gRPC `CredentialProvider` whose only in-repo artifact is the proto.
  No provider implementation ships here — [PR #1335](https://github.com/agent-substrate/substrate/pull/1335)
  proposes an example Kubernetes-Secret one and is still open, and the `kagent-dev/substrate` fork
  ships `cmd/credential-provider` on its `main` for the agentgateway path — and none of them mints
  actor JWTs. It is also Envoy-only, on the TLS-interception leg. The provider receives the current
  name-based actor SPIFFE ID without the incarnation UID, so a provider that needs incarnation-level authorization also needs that
  protocol gap closed. An actor-JWT provider/injector remains unimplemented. See EGRESS.md.
- **Persistence of live trust-bundle refresh across an atelet restart.**
  [PR #1231](https://github.com/agent-substrate/substrate/pull/1231) is merged and refreshes bundles
  for actors registered in memory, but an atelet restart loses that registry until each actor's
  next Run/Restore.
- **Per-actor node attestability.** High-density actors are invisible to standard workload
  attestation; no node-attestable per-actor property exists for tools like SPIRE selectors.

## Open design questions

- **Should the actor ever hold its own identity credential?** Option C's chief advantage is that
  today the answer is no. Introducing a Workload API or projected token reverses that and
  reintroduces the risk of a stale credential frozen into a snapshot. Per-actor cloud federation is
  the one case the proxy cannot broker invisibly, unless egress-side injection grows a "mint a
  per-audience token and attach it" capability, which would keep the answer no.
- **One trust domain per installation or per cluster?** Affects external federation; should be
  settled before JWKS ships so the issuer URL and trust domain agree.
- **Which relying party justifies WIT-SVID?** The bearer path can stop inventing by adopting stable
  JWT-SVID now. WIT-SVID/WPT should remain a separate, feature-gated path until a concrete relying
  party and key-custody model justify tracking the incubating SPIFFE profile and moving WIMSE
  drafts.
- **Freshness versus load.** Both egress paths perform a control-plane lookup on every CONNECT to
  compensate for the one-hour lifetime and lack of revocation, which puts ateapi on the critical
  path of every new actor connection and makes a control-plane outage a cluster-wide egress
  outage — both dataplanes answer 503 rather than failing open. The lookup is per CONNECT, not per
  request, so a long-lived tunnel is never re-authorized. Shorter certificates, a push-based
  revocation signal, or a bounded re-check on open tunnels are the options.
- **Node-local blast radius.** atelet is on the renewal path for live egress. An atelet outage
  longer than the remaining certificate lifetime blocks new egress for every actor on that node
  until atelet restarts; established tunnels drain rather than drop.
- **Should the atunnel credential and the actor's own identity be different subjects?** Today
  one SPIFFE ID covers both, distinguished only by the `Purpose` field in a Substrate-specific
  X.509 extension. A proposal under discussion moves that distinction into the SPIFFE URI itself,
  so a third-party egress gateway that understands SPIFFE but not the extension cannot confuse an
  atunnel connection with an actor presenting its own certificate. The motivating case is
  dropping a vendor gateway in place of Substrate's.
- **Multiple actors per worker.** The API models per-worker capacity and several assignments,
  but certificate minting no longer selects among them: the caller names the actor directly.
  atunnel holds one key per activation and presents one certificate on every intercepted
  connection. Serving several actors from one worker would require atunnel to choose a key per
  connection based on which sandbox the traffic came from.
- **Which client authentication should be the default for the control plane?** The
  port-forward plus Kubernetes-token path is convenient locally but assumes the caller has
  Kubernetes permissions on the install cluster; it is under discussion whether that should
  remain the default.
- **How does an in-cluster, user-owned gateway identify an actor?** Header forwarding from
  Substrate's gateway versus presenting actor mTLS are both under discussion.
- **Policy that changes while an actor is checkpointed.** There is no restore-time extension
  point to reconcile a resumed actor against authorization that changed while it was suspended.
