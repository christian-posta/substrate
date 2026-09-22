# Authorization and RBAC

> How Agent Substrate intends to authorize control-plane and infrastructure operations with
> OpenFGA, what is implemented today, and what remains before operators can safely delegate
> access. This document uses “RBAC” as the familiar umbrella term, but the implementation is
> relationship-based access control (ReBAC): roles are relationships and permissions can follow
> relationships between users, atespaces, actors, workers, and nodes. The implementation is being
> tracked in [issue #1563](https://github.com/agent-substrate/substrate/issues/1563).

## Executive summary

Agent Substrate has a checked-in OpenFGA authorization model and starts an embedded,
PostgreSQL-backed OpenFGA server as part of `ateapi`. The model defines global and per-atespace
roles, inherited permissions for actors and actor templates, and a scheduling relationship that
can restrict actor-credential minting to the node that currently hosts the actor.

That foundation is real, but authorization is **not enforced yet**. `ateapi` currently:

- authenticates every caller with mTLS or a configured bearer-JWT provider;
- starts OpenFGA, migrates its database schema, creates or reuses its store, and installs the
  current model;
- does **not** provide a supported API or CLI for assigning roles;
- does **not** write resource, role, or scheduling relationships to OpenFGA;
- does **not** ask OpenFGA before serving control-plane RPCs; and
- does **not** filter list results according to the caller's permissions.

The practical security rule today is therefore unchanged from
[the authentication guide](docs/authentication.md): configure only identity providers whose
authenticated users may control the entire Substrate control plane. A user cannot yet make a
principal a read-only viewer, an atespace editor, or an atespace owner through a supported
Substrate interface.

The shipped implementation is best described as **model and storage bootstrap complete;
policy management, relationship synchronization, and enforcement still open**.

## Scope and terminology

- **Authentication** proves who called `ateapi`. It accepts either the first SPIFFE URI SAN from
  a verified mTLS client certificate or the `sub` claim from a verified bearer JWT.
- **Principal** is the authenticated caller identity carried with an `ateapi` request. The
  authentication layer also records whether it came from mTLS or JWT and, for JWTs, its issuer.
- **Authorization** decides whether that principal may perform one operation on one object.
- **Role** is a direct relationship such as `owner`, `editor`, or `viewer`.
- **Permission** is a derived relation beginning with `can_`, such as `can_get` or
  `can_create_actor`. Applications check permissions, not role names.
- **Relationship tuple** is one OpenFGA fact in the form
  `(user or object, relation, object)`. For example, an atespace owner assignment can be modeled
  as `(user:alice, owner, atespace:team-a)`.
- **Structural relationship** connects resources, such as an actor to its parent atespace.
- **Dynamic relationship** follows runtime state, such as an actor's current worker and that
  worker's host node.
- **Global scope** is the singleton `global:root` object. It is not a Kubernetes namespace or
  cluster role.
- **Atespace** is Substrate's tenancy and authorization boundary. It is independent of a
  Kubernetes namespace.

## Kubernetes RBAC is separate

Substrate also installs Kubernetes `Role`, `ClusterRole`, `RoleBinding`, and
`ClusterRoleBinding` resources. Those grants let Substrate components operate Kubernetes
resources such as Pods, Deployments, Secrets, NetworkPolicies, and trust bundles.

Kubernetes RBAC answers questions such as “may the `ate-controller` service account update this
Deployment?” It does not answer “may Alice delete this actor?” The OpenFGA model is intended to
answer the latter. Tight Kubernetes RBAC remains necessary even after application authorization
is complete, because a compromised component must not receive broader Kubernetes privileges than
its job requires.

## Architecture

The intended request path is:

```text
caller
  |
  | mTLS certificate or bearer JWT
  v
ateapi authentication
  |
  | authenticated principal
  v
operation + target resolution
  |
  | principal, can_<operation>, object
  v
embedded OpenFGA Check
  |                         |
  | allowed                 | denied / unavailable
  v                         v
RPC handler              PermissionDenied / fail closed
```

Only the first stage and the OpenFGA server itself exist today. The translation from an
authenticated principal and RPC request into an OpenFGA check, and the enforcement of its result,
have not been implemented.

OpenFGA is embedded as a library in the `ateapi` process. There is no separate OpenFGA Deployment,
Service, or externally exposed OpenFGA API. On every `ateapi` startup, the server:

1. opens a dedicated PostgreSQL connection pool configured like the main Substrate pool;
2. applies OpenFGA's PostgreSQL migrations;
3. creates or reuses the OpenFGA store named `substrate`;
4. compiles the checked-in DSL model;
5. reuses an identical latest model or writes a new model version; and
6. logs the active OpenFGA store ID and authorization-model ID.

Startup migration and store/model initialization are serialized across `ateapi` replicas. If
OpenFGA initialization fails, `ateapi` startup fails. OpenFGA uses the same configured PostgreSQL
database and search path as Substrate but maintains its own tables and migration history.

## Current authorization model

The model uses OpenFGA schema 1.1 and defines seven object types.

| Type | Meaning |
| --- | --- |
| `user` | Unified human, service-account, or workload principal |
| `global` | Singleton installation-wide scope, conventionally `global:root` |
| `atespace` | Tenant and policy boundary |
| `actor_template` | Template whose permissions come from its parent atespace |
| `actor` | Actor whose user permissions come from its atespace and infrastructure permissions from its current placement |
| `worker` | Worker hosted by a node and able to receive an actor assignment |
| `node` | Physical or virtual host running `atelet` |

### Global roles

`global:root` has two direct roles:

| Role | Effective permissions |
| --- | --- |
| `owner` | `can_create_atespace`, `can_get`, `can_set_policy`, `can_get_policy`; also inherits owner access in every child atespace |
| `viewer` | `can_get`, `can_get_policy`; also inherits viewer access in every child atespace |

A global owner is also a global viewer. Global roles propagate only through an atespace's
`parent_global` relationship. Every atespace must therefore be linked to `global:root` for global
inheritance to work.

### Atespace roles

An atespace has an `owner` → `editor` → `viewer` hierarchy:

| Role | Atespace permissions |
| --- | --- |
| `owner` | Set and read policy; create actors and templates; get, update, and delete the atespace |
| `editor` | Read policy; create actors and templates; get and update the atespace |
| `viewer` | Read the atespace and its policy |

More precisely:

- an `owner` is automatically an `editor` and `viewer`;
- an `editor` is automatically a `viewer`;
- a global owner becomes an owner of every atespace linked to `global:root`; and
- a global viewer becomes a viewer of every linked atespace.

Direct user bindings stop at the atespace boundary. The model deliberately does not define a way
to make a user the owner, editor, or viewer of one actor or one actor template. Those resources
inherit from their parent atespace.

### Actor-template permissions

An actor template has a `parent_atespace` relationship and derives:

| Permission | Required atespace role |
| --- | --- |
| `can_get` | Viewer |
| `can_update` | Editor |
| `can_delete` | Editor |

Deleting a template currently requires editor rather than owner according to the model.

### Actor permissions

An actor also has a `parent_atespace` relationship and derives:

| Permission | Who receives it |
| --- | --- |
| `can_get` | Atespace viewer, editor, or owner |
| `can_update` | Atespace editor or owner |
| `can_delete` | Atespace editor or owner |
| `can_resume` | Atespace editor/owner or a directly granted `user` |
| `can_suspend` | Atespace editor/owner or a directly granted `user` |
| `can_revert` | Atespace editor/owner or a directly granted `user` |
| `can_mint_ateom_actor_credential` | Host node of the actor's currently scheduled worker |

The direct grants on resume, suspend, and revert are intended for infrastructure or workload
identities that need one narrow lifecycle capability without receiving a broad atespace role.

### Scheduling and credential minting

The model represents placement as:

```text
node:node-a
  --host_node--> worker:worker-1
  --scheduled_worker, through worker-1--> actor:actor-1
```

In OpenFGA tuple notation, the relationships are oriented toward the protected object:

```text
(node:node-a, host_node, worker:worker-1)
(worker:worker-1, scheduled_worker, actor:actor-1)
```

The actor derives `host_node` through its scheduled worker. The model then grants
`can_mint_ateom_actor_credential` only to that node. A different node is denied by the model even
if it hosts another worker.

This is an important intended security property, but it is not active in the credential-minting
RPCs. `MintActorJWT` and `MintActorCertificate` still contain authorization TODOs and do not
perform the OpenFGA check. See [IDENTITY.md](IDENTITY.md) for the resulting identity-path risk.

## Relationship examples

These examples explain the model; there is currently no supported Substrate command for applying
them.

### Installation administrator

```text
(user:alice, owner, global:root)
```

Alice can administer the global scope and becomes owner of every atespace correctly linked to
`global:root`.

### Read-only installation observer

```text
(user:bob, viewer, global:root)
```

Bob can read the global scope and becomes viewer of every linked atespace and its actors and
templates.

### Atespace-local editor

```text
(global:root, parent_global, atespace:team-a)
(user:carol, editor, atespace:team-a)
```

Carol can create and modify actors and templates in `team-a`, but cannot delete the atespace or
change its role policy.

### Actor inheritance

```text
(atespace:team-a, parent_atespace, actor:team-a/agent-1)
```

Once the model has a collision-safe object identifier for the actor, every `team-a` viewer can
read it and every `team-a` editor can update, delete, resume, suspend, and revert it. The precise
production object-ID format has not been decided; `actor:team-a/agent-1` here is illustrative.

### Narrow router grant

```text
(user:ingress-router, can_resume, actor:team-a/agent-1)
```

The direct relation grants resume without granting read, update, delete, suspend, or revert.

## Authentication is present; authorization is not

The public gRPC server runs an authentication interceptor before request handlers. A caller with
a verified client certificate is identified by the certificate's first URI SAN. A caller without
a client certificate must present a bearer JWT from a configured issuer; its verified `sub` claim
becomes the principal ID. Requests without either accepted credential are rejected as
`Unauthenticated`.

That principal is available to handlers, but it is not currently converted into an OpenFGA
`user` or checked against the requested resource. Consequently, authentication today means “this
is a recognized identity,” not “this identity has a configured role.”

There are a few narrower checks outside OpenFGA. For example, `WorkerService` authenticates
specific `atelet` callers, and `MintActorCertificate` requires a verified client certificate.
Those checks protect particular infrastructure paths; they are not general RBAC for the `Control`
service.

## What works today

| Capability | Status | Notes |
| --- | --- | --- |
| Authenticate mTLS callers | Implemented | Principal is the first SPIFFE URI SAN |
| Authenticate bearer-JWT callers | Implemented | Provider, issuer, signature, audience, time, and subject are validated |
| Carry principal identity in request context | Implemented | Includes authentication kind and JWT issuer |
| Define global owner/viewer roles | Implemented in model | Not assigned or enforced by Substrate |
| Define atespace owner/editor/viewer roles | Implemented in model | Not assigned or enforced by Substrate |
| Inherit actor/template access from atespace | Implemented in model | Requires structural tuples that are not written |
| Restrict credential minting to current host node | Implemented in model | Scheduling tuples and RPC check are missing |
| Persist OpenFGA data in PostgreSQL | Implemented | Embedded server uses a dedicated pool and OpenFGA tables |
| Initialize store and model idempotently | Implemented | Serialized for multiple `ateapi` replicas |
| Test model semantics | Implemented | Checked-in OpenFGA test vectors cover the current relations |
| Assign or revoke roles through Substrate | Not implemented | No policy API or CLI |
| Bootstrap the first global owner | Not implemented | No supported break-glass/bootstrap flow |
| Maintain resource relationships | Not implemented | Atespace, actor, and template lifecycle does not write tuples |
| Maintain scheduling relationships | Not implemented | Assignment and movement do not write/revoke tuples |
| Authorize `Control` RPCs | Not implemented | No production OpenFGA `Check` calls |
| Filter list results | Not implemented | List semantics are not represented in the model |
| Authorization audit events and metrics | Not implemented | OpenFGA initialization is logged, not policy decisions |

## How to use it today

### As an operator

There is no supported granular-RBAC workflow yet. Until enforcement lands:

1. Treat every configured JWT provider as a source of control-plane administrators.
2. Do not rely on tuples inserted directly into OpenFGA; `ateapi` will not consult them.
3. Restrict network reachability to `ateapi` and tightly control who receives accepted mTLS
   certificates or JWTs.
4. Continue to minimize Kubernetes RBAC for the Substrate service accounts independently.
5. Watch for the initialization log after an `ateapi` rollout:

   ```sh
   kubectl logs -n ate-system deployment/ate-api-server \
     | grep 'OpenFGA server initialized'
   ```

The log confirms that the OpenFGA schema, store, and model initialized. It does not prove that an
application request was authorized, because no application request is checked today.

### As a model developer

The checked-in model tests can be run with the OpenFGA CLI container:

```sh
docker run --rm \
  -v "$PWD/internal/authz:/workspace" \
  -w /workspace \
  openfga/cli:latest \
  model test --tests model_test.fga.yaml
```

The Go integration test starts PostgreSQL through Testcontainers, applies the embedded migrations,
writes representative tuples, performs OpenFGA checks, and verifies restart idempotency:

```sh
go test ./internal/authz
```

Both exercises validate OpenFGA itself and the model bootstrap. They do not validate RPC
enforcement because that integration does not exist yet.

### Intended operator experience

The repository does not yet define the final policy API, but a complete workflow should look like:

1. Install Substrate and bootstrap one global owner through a narrowly controlled mechanism.
2. Authenticate to `ateapi` with mTLS or a configured OIDC provider.
3. Create an atespace; Substrate atomically records its link to `global:root`.
4. A global or atespace owner assigns `owner`, `editor`, and `viewer` roles through a Substrate
   policy API or CLI.
5. Resource creation records parent relationships automatically; users never manage structural
   tuples by hand.
6. Scheduling records the actor → worker → node path automatically and revokes obsolete placement
   relationships before an old host may mint credentials.
7. Every RPC resolves a permission and object, asks OpenFGA, and fails closed on denial or an
   unavailable authorization decision.
8. List operations return only objects the principal is allowed to see, without leaking names or
   counts from other atespaces.
9. Policy changes and authorization decisions produce useful audit events and bounded-cardinality
   metrics.

The policy API should expose domain concepts such as role bindings and policy, not raw OpenFGA
store IDs, model IDs, or unrestricted tuple writes. Raw tuple access would let clients bypass
Substrate's invariants and create relationships the API never intended to support.

## Implementation tracker

### 1. Foundation — implemented

- [x] Define an OpenFGA schema 1.1 authorization model.
- [x] Define global owner/viewer and atespace owner/editor/viewer roles.
- [x] Define inherited actor and actor-template permissions.
- [x] Define the node → worker → actor placement graph.
- [x] Add model test vectors.
- [x] Embed OpenFGA in `ateapi`.
- [x] Add PostgreSQL migrations and persistent OpenFGA storage.
- [x] Idempotently create/reuse the store and authorization model.
- [x] Serialize initialization across replicas.
- [x] Fail `ateapi` startup when OpenFGA initialization fails.

### 2. Stable identity and object naming — open

- [ ] Define the canonical OpenFGA user string for JWT principals, including issuer so equal `sub`
  values from different trusted issuers cannot collide.
- [ ] Define the canonical OpenFGA user string for SPIFFE/mTLS principals and prevent type or
  encoding ambiguity with JWT principals.
- [ ] Define collision-safe object IDs for atespace-scoped actors and templates. A bare actor name
  is not globally unique.
- [ ] Decide whether authorization follows the resource name or immutable UID across
  delete-and-recreate. Security-sensitive actor grants should not silently transfer to a new
  incarnation at a reused name.
- [ ] Define canonical IDs for workers and nodes, including how Kubernetes UID changes affect
  authorization.

### 3. Policy administration and bootstrap — open

- [ ] Define the public policy resource and `GetPolicy`/`SetPolicy` behavior.
- [ ] Provide a safe first-global-owner bootstrap flow that cannot be replayed or accidentally
  reopened.
- [ ] Validate that callers cannot grant roles or relations outside the public model.
- [ ] Require `can_set_policy` for policy mutation and `can_get_policy` for policy inspection.
- [ ] Define optimistic concurrency and lost-update behavior for policy changes.
- [ ] Add CLI support for viewing, granting, and revoking global and atespace roles.
- [ ] Define a recoverable, audited break-glass procedure.

### 4. Structural relationship synchronization — open

- [ ] Link every atespace to `global:root` when it is created.
- [ ] Link every actor and actor template to its parent atespace.
- [ ] Remove or tombstone relationships when resources are deleted.
- [ ] Make resource changes and relationship changes atomic or recoverable. Because the resource
  store and OpenFGA tables share PostgreSQL but use separate APIs, a crash between writes must not
  leave authorization state permanently ahead of or behind resource state.
- [ ] Reconcile existing resources after upgrades and repair missing or stale relationships.
- [ ] Specify behavior when a tuple references a resource that no longer exists.

### 5. Dynamic infrastructure relationships — open

- [ ] Record each worker's current host node.
- [ ] Record and revoke the actor's scheduled worker as assignments change.
- [ ] Order tuple changes so an old node loses credential-minting permission before, or no later
  than, a new activation becomes valid.
- [ ] Reconcile placement tuples after crashes, retries, migration, pause, suspend, revert, and
  worker deletion.
- [ ] Bind credential minting to the actor incarnation as well as its readable name.
- [ ] Test stale-node and deleted/recreated-actor denial end to end.

### 6. Enforcement — open

- [ ] Add one authorization layer that consistently maps RPC methods to permissions and objects.
- [ ] Pass the embedded OpenFGA server, store ID, and active model ID to that layer.
- [ ] Deny with `PermissionDenied` without leaking whether an unauthorized object exists.
- [ ] Define and test fail-closed behavior for OpenFGA errors, database outages, deadlines, and
  model mismatch.
- [ ] Enforce actor, actor-template, and atespace operations already represented in the model.
- [ ] Enforce `can_mint_ateom_actor_credential` in both actor-credential minting paths.
- [ ] Ensure trusted internal workflows receive narrow explicit permissions rather than bypassing
  authorization broadly.
- [ ] Add table-driven authorization tests for every protected RPC.

### 7. Complete API coverage — open

The current model does not yet express every `Control` RPC or resource. Decisions are still needed
for:

- [ ] list operations and result filtering;
- [ ] `PauseActor`;
- [ ] actor egress-policy get/create/update/delete;
- [ ] tag get/list/create/update/delete and published-tag behavior;
- [ ] worker get/list/create/update/drain/delete;
- [ ] worker-assignment inspection;
- [ ] actor and template list operations;
- [ ] any future watch APIs; and
- [ ] separation of actor-JWT minting, atunnel-certificate minting, and future actor-held
  credentials.

The model should not infer that every operation named “get,” “update,” or “delete” has identical
security consequences. Credential issuance, policy changes, tag publication, egress-policy
changes, and worker mutation deserve explicit permissions even when they are nested under an
otherwise editable actor or atespace.

### 8. Query, audit, and operations — open

- [ ] Define list authorization with OpenFGA `ListObjects`, batched checks, an indexed projection,
  or another bounded-cost strategy.
- [ ] Set latency and availability budgets for authorization checks.
- [ ] Add decision metrics without high-cardinality principal or resource labels.
- [ ] Emit audit events for grants, revocations, denials, bootstrap, break-glass use, and model
  changes.
- [ ] Document database backup and restore requirements for authorization tuples.
- [ ] Verify that model upgrades preserve intended access before activating a new model ID.
- [ ] Define garbage collection for obsolete models and stale tuples.
- [ ] Add an operator-facing way to inspect effective access without exposing unrestricted raw
  OpenFGA administration.

## Security invariants to preserve

1. **Authentication is not authorization.** A valid token or certificate supplies a principal; it
   does not grant a default role.
2. **Default deny.** Missing relationships, unknown operations, OpenFGA failures, and incomplete
   target resolution must not become authorization success.
3. **No cross-atespace name collision.** Authorization object IDs must preserve the atespace or an
   immutable globally unique identifier.
4. **No delete-and-recreate inheritance by accident.** Grants intended for one resource
   incarnation must not silently attach to a replacement at the same name.
5. **Structural relationships are server-owned.** Clients assign supported roles; Substrate owns
   parent, scheduling, worker, and node tuples.
6. **Placement grants are short-lived facts.** A node's ability to mint an actor credential must be
   revoked as part of moving or stopping that actor.
7. **Policy administration cannot self-escalate past its scope.** Atespace owners may administer
   their atespace but cannot create global owners or alter another atespace.
8. **List APIs do not leak.** Names, counts, pagination tokens, and error differences must not
   reveal inaccessible resources.
9. **Authorization state is recoverable.** Crashes between resource and tuple changes must be
   detected and reconciled.
10. **Infrastructure identities remain narrow.** Routers, nodes, workers, and actors receive only
    the explicit lifecycle or credential permission their workflow needs.

## Open design questions

### What is the canonical principal key?

JWT `sub` is unique only within an issuer. The current authentication context retains the issuer,
but the OpenFGA model has one unified `user` type. The encoding must include issuer and subject,
and it must distinguish JWT identities from SPIFFE identities without relying on ambiguous string
concatenation.

### What identifies a protected resource?

Actors and templates are named within an atespace, while OpenFGA object strings are store-wide.
The production encoding must include the atespace or use a globally unique UID. For actors, the
choice also determines whether a role or direct lifecycle grant survives delete-and-recreate.

### Is the checked-in role hierarchy the desired public contract?

The current model lets atespace editors delete actors and templates, directly grants selected users
resume/suspend/revert, and reserves policy mutation for owners. Those are meaningful product
semantics and should be reviewed before clients depend on them.

### How are resource and tuple writes coordinated?

OpenFGA and Substrate share PostgreSQL but use different storage abstractions. A resource commit
followed by a failed tuple write can expose an object without its intended parent relationship;
the reverse ordering can leave permissions for an object that does not yet exist. The design needs
transactional integration, a durable outbox, or an authoritative reconciler with carefully chosen
fail-closed intermediate states.

### How are list operations authorized?

Checking every returned object one at a time is expensive and can make pagination inconsistent.
OpenFGA reverse queries may help, but the final design must combine permissions, storage filters,
stable pagination, and non-disclosure semantics.

### What is the availability contract?

Because OpenFGA is embedded and uses the same database as the resource store, many failures will
be correlated. The authorization layer still needs explicit deadlines, error mapping, caching
rules, and a statement that stale cached allow decisions cannot outlive revocation guarantees.

### Who may inspect effective policy?

`can_get_policy` currently follows viewer access. Effective-access explanation can reveal user and
infrastructure relationships, so the API must decide whether viewers see full policy, only their
own effective permissions, or a redacted view.

## Verification

The following checks describe the current boundary without changing cluster state.

Confirm the model and tests are present:

```sh
sed -n '1,220p' internal/authz/model.fga
sed -n '1,280p' internal/authz/model_test.fga.yaml
```

Confirm OpenFGA is initialized during `ateapi` startup:

```sh
rg -n 'authz.NewServer|OpenFGA authorization server' cmd/ateapi/main.go
```

Confirm there are still no production OpenFGA checks or tuple writes:

```sh
rg -n --glob '*.go' --glob '!vendor/**' --glob '!**/*_test.go' \
  'FGAServer\(\)\.(Check|Write)\(|openfgav1\.(CheckRequest|WriteRequest)\{' .
```

At the time of this document, that search produces no results.

Confirm the known credential-minting gaps:

```sh
rg -n 'TODO\(authz\)' cmd/ateapi/internal/controlapi/actor.go
```

Run the focused integration test, with Docker available for Testcontainers:

```sh
go test ./internal/authz
```

## Implementation history

- [PR #1233](https://github.com/agent-substrate/substrate/pull/1233) introduced the initial
  three-tier atespace model, inherited resource access, node restriction, and model tests.
- [PR #1670](https://github.com/agent-substrate/substrate/pull/1670) embedded OpenFGA in `ateapi`,
  added PostgreSQL migrations and persistent storage, renamed the singleton scope from `cluster`
  to `global`, and made initialization safe across replicas.
- [Issue #1563](https://github.com/agent-substrate/substrate/issues/1563) tracks the broader
  authorization implementation.

## Reference map

| Area | Reference |
| --- | --- |
| OpenFGA model | `internal/authz/model.fga` |
| Model behavior tests | `internal/authz/model_test.fga.yaml` |
| Embedded server, migrations, store, and model initialization | `internal/authz/server.go` |
| `ateapi` startup integration | `cmd/ateapi/main.go` |
| Caller authentication | `internal/ateapiauth` and `docs/authentication.md` |
| Principal representation | `internal/principal` |
| Control-plane RPC surface | `pkg/proto/ateapipb/ateapi.proto` |
| Known credential authorization gaps | `cmd/ateapi/internal/controlapi/actor.go` and `IDENTITY.md` |
| Kubernetes component permissions | `manifests/ate-install` and controller `kubebuilder:rbac` markers |
