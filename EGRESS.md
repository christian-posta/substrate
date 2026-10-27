# Actor egress

Substrate sends an Actor's external TCP connections through `atunnel` and an egress gateway, and filters its DNS lookups against the same policy. Two dataplanes implement the gateway: AgentGateway, and Envoy with an `atenet` policy sidecar. They share the Actor identity and `EgressPolicy` API. [IDENTITY.md](./IDENTITY.md) covers the tunnel identity; [CREDENTIALS.md](./CREDENTIALS.md) covers credential injection.

## Data path and isolation

Each running Actor has its own network namespace, even when several Actors share one worker. When the Actor dials an external IP and port, its TCP connection is redirected to `atunnel` in the trusted worker supervisor; link-local destinations such as a cloud metadata address are rejected before that. `atunnel` opens an mTLS CONNECT to the destination through the egress gateway, presenting the Actor's `worker-for-actor` certificate. The gateway verifies it, looks up the Actor by (atespace, name), and requires the Actor to be running or resuming. Egress is enabled before the workload starts, so an Actor can reach the network before its wakeup probe passes. The gateway presents its own service certificate to `atunnel`; the external destination sees neither certificate. The Actor process does not hold the `worker-for-actor` private key.

DNS goes through a local relay in `atunnel`, not through the egress gateway. The relay forwards a lookup to the worker's resolvers only when the name matches a hostname in one of the Actor's `EgressPolicy` rules, of any protocol and on any port; every other lookup, and every lookup from an Actor with no policy, returns `NXDOMAIN`. The relay reads the policy through atelet, caches it for five seconds, and keeps using a stale copy while atelet is unreachable. Ordinary external UDP and other non-TCP protocols have no external route.

The gateway sees the CONNECT destination IP and port. For cleartext HTTP it can also see the HTTP request. For TLS passthrough it can see connection metadata and usually SNI, but cannot inspect HTTP headers or bodies. Optional TLS interception lets it evaluate HTTPS requests and inject headers; the Actor must trust the interception CA to complete that TLS connection.

## Dataplanes

The installer selects Envoy by default; `--atenet-dataplane=agentgateway` selects AgentGateway for both ingress and egress. A NetworkPolicy admits CONNECTs only from Pods labeled `ate.dev/worker-pool`, where the cluster's network plugin enforces it; anyone who can create a Pod can set that label, so the `worker-for-actor` client certificate is the authenticating control. Nothing in the checked-in installation restricts where the gateway itself can connect.

As of October 27, 2026, the AgentGateway image pinned in `manifests/ate-install/components/agentgateway/` (commit `9d36620d`) is older than three Substrate changes. It accepts only the earlier `ateom-for-actor` certificate path, calls ateapi and the credential provider under their earlier gRPC package names, and omits a field ateapi now requires on egress-policy lookups. With that image, AgentGateway denies every Actor's egress. The AgentGateway behavior described below comes from that commit's source; it shows the design, not a working checked-in path.

## EgressPolicy enforcement

An Actor can have one `EgressPolicy`, named `default`. Its rules are an unordered set of allows, and no match denies the traffic. Rules distinguish cleartext HTTP, intercepted HTTPS, and TLS passthrough, and match hostnames and destination ports; HTTP and HTTPS rules default to ports 80 and 443, while TLS passthrough rules must specify a port list or all ports. When more than one rule matches, an exact hostname wins over a wildcard and a specific port wins over all ports. Only the winning rule's effects apply. Writing a policy requires the editor role on the Actor's atespace; see [RBAC.md](./RBAC.md).

Both dataplanes route HTTP to the authorized hostname on the port the Actor originally dialed; a port in `Host` cannot change the dial target. They route permitted TLS passthrough to the authorized SNI and dialed port, and reject arbitrary opaque TCP. HTTP and HTTPS rules may also request credential injection. The policy API has no HTTP-method matcher.

Authorizing a hostname does not establish that its resolved IP is public. The worker's link-local block applies only to addresses the Actor dials; the gateway resolves an authorized hostname itself and does not check the resulting IP. Blocking cloud metadata or internal services behind a hostname requires network restrictions on the gateway.

An ActorTemplate can carry a golden egress policy behind the `GoldenEgressPolicy` preview gate. It applies only to the template's golden Actor during initialization; Actors created from the template do not inherit it.

### Where each dataplane enforces it

| Traffic | AgentGateway | Envoy (installer default) |
| --- | --- | --- |
| New CONNECT | Checks Actor identity and state only. An Actor with no policy gets a tunnel, but every use of it is denied. | Checks Actor identity and state and requires a non-empty policy before admitting the tunnel. |
| Cleartext HTTP | Fetches the policy for each request, checks it against `http` rules, and routes to the authorized hostname on the dialed port. | Same check and routing, with the policy cached for up to ten seconds. |
| Intercepted HTTPS | Intercepts a ClientHello whose SNI and port match an `https` rule. It also intercepts an unmatched SNI, completing TLS with the interception CA, and then returns 403. Checks each decrypted request against `https` rules. | Intercepts a ClientHello whose SNI and port match an `https` rule and closes one that matches no rule. Checks each decrypted request against `https` rules. |
| TLS passthrough | Checks SNI and port once per connection against `tlsPassthrough` rules and routes to the authorized SNI on the dialed port. | Same. |
| WebSocket, HTTP CONNECT, and other TCP | Denies. | Denies. |

With no `EgressPolicy`, or when no rule matches, the traffic is denied. A connection that begins with neither HTTP nor a TLS ClientHello, or uses a server-first protocol, is closed. WebSocket upgrades and nested HTTP CONNECT requests are rejected with 403.

## Identity and lifecycle limits

Both gateways check the named Actor on a new CONNECT, not continuously on an established stream. The `worker-for-actor` certificate carries the Actor UID, but neither gateway reads it, so a still-valid certificate for a deleted Actor can identify a new Actor at the same address. Ateapi issues these one-hour certificates only for the Worker an Actor is assigned to, and normal Actor teardown closes the worker's active tunnels. An expired certificate blocks new tunnels but does not close admitted ones. See [IDENTITY.md](./IDENTITY.md) for the certificate lifecycle.

The actor-identity CA used for `worker-for-actor` certificates is separate from the optional TLS-interception CA. Actors must explicitly receive the latter trust bundle to accept intercepted HTTPS.

Policy changes are not instantaneous everywhere. Requests inside an admitted HTTP or HTTPS connection are re-decided one by one, against a policy that may be up to ten seconds old on Envoy and fetched fresh on AgentGateway. Admitted TLS passthrough connections and tunnels are not revisited, and the DNS relay may answer from a policy up to five seconds old, or older while atelet is unreachable.

## Credential injection

An `http` or `https` rule can request outbound credential-header replacement. Both dataplanes are designed to replace an Actor-supplied placeholder header on cleartext HTTP and intercepted HTTPS. An absent header skips injection; failure to obtain a required provider credential denies the request. The installer requires an explicit credential-provider choice: the bundled Kubernetes Secrets provider, an external provider, or none. Cleartext HTTP exposes the injected credential on the connection to the destination.

Envoy can also obtain an Actor JWT from ateapi and replace an outbound request header with it, allowing the destination to verify the Actor without the workload holding the token. This path does not require a credential provider. AgentGateway does not implement Actor JWT injection. See [CREDENTIALS.md](./CREDENTIALS.md) for the request path and its trust limits.

<!-- verified-against: agent-substrate/substrate@635c521cc682e312d01008f3aa2912588d394938 2026-10-27 -->
