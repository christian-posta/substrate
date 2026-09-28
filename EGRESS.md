# Actor egress

Substrate sends an Actor's external TCP connections through `atunnel` and an egress gateway. The default gateway uses Envoy with an `atenet` policy sidecar; the checked-in AgentGateway deployment is an optional dataplane. They share the Actor identity and `EgressPolicy` API and now support the same traffic surface. Actor JWT injection remains available only in the Envoy path. [IDENTITY.md](./IDENTITY.md) covers the tunnel identity; [CREDENTIALS.md](./CREDENTIALS.md) covers credential injection.

## Data path and isolation

Each running Actor has its own network namespace, even when several Actors share one worker. When the Actor dials an external IP and port, its TCP connection is redirected to `atunnel` in the trusted worker supervisor. Atunnel opens an mTLS CONNECT to that destination through the egress gateway, presenting the Actor's `ateom-for-actor` certificate. The gateway verifies it, looks up the Actor by (atespace, name), and requires the Actor to be running. The gateway presents its own service certificate to `atunnel`; the external destination sees neither certificate. The Actor process does not hold the `ateom-for-actor` private key.

DNS goes through a local relay to the worker's resolvers; it does not pass through the egress gateway or `EgressPolicy`. Ordinary external UDP and other non-TCP protocols have no external route in the current IPv4 network setup.

The gateway sees the CONNECT destination IP and port. For cleartext HTTP it can also see the HTTP request. For TLS passthrough it can see connection metadata and usually SNI, but cannot inspect HTTP headers or bodies. Optional TLS interception lets it evaluate HTTPS requests and inject headers; the Actor must trust the interception CA to complete that TLS connection.

## EgressPolicy enforcement

An Actor can have one `EgressPolicy`, named `default`. Its rules are an unordered set of allows, and no match denies the traffic. Rules distinguish cleartext HTTP, intercepted HTTPS, and TLS passthrough, and match hostnames and destination ports. When more than one rule matches, an exact hostname wins over a wildcard and a specific port wins over all ports. Only the winning rule's effects apply. Existing policies using the older hostname, CIDR, or `all` rule API must be recreated.

Both dataplanes route HTTP to the authorized hostname on the port the Actor originally dialed; a port in `Host` cannot change the dial target. They route permitted TLS passthrough to the authorized SNI and dialed port, and reject arbitrary opaque TCP. HTTP and HTTPS rules may also request credential injection. The policy API has no HTTP-method matcher.

Authorizing a hostname does not establish that its resolved IP is public. Blocking cloud metadata or internal services also requires destination-IP checks at the gateway or network restrictions on the gateway.

### Where each dataplane enforces it

| Traffic | Default Envoy dataplane | Checked-in AgentGateway dataplane |
| --- | --- | --- |
| New CONNECT | Checks Actor identity and running state and requires a non-empty policy before admitting the tunnel. | Checks Actor identity and running state and resolves the Actor's policy. |
| Cleartext HTTP | Checks each request against `http` rules and routes to the authorized hostname on the dialed port. | Same policy and routing behavior. |
| Intercepted HTTPS | Uses SNI and port to select interception, then checks each decrypted request against `https` rules and routes to the authorized hostname. | Same policy and routing behavior. |
| TLS passthrough | Checks SNI and port once per connection against `tlsPassthrough` rules and routes to the authorized SNI on the dialed port. | Same policy and routing behavior. |
| WebSocket, HTTP CONNECT, and other TCP | Denies. | Denies. |

### Default-deny behavior

With no `EgressPolicy`, or when no rule matches, the gateway denies the traffic. An `https` rule causes TLS interception for the matching SNI and port; a `tlsPassthrough` rule permits matching TLS without decryption. A connection that begins with neither HTTP nor a TLS ClientHello, or uses a server-first protocol, is closed. WebSocket upgrades and nested HTTP CONNECT requests are rejected with 403.

## Identity and lifecycle limits

Both gateways check the named Actor on a new CONNECT, not continuously on an established stream. The `ateom-for-actor` certificate has no Actor UID, so a still-valid certificate for a deleted Actor can identify a new running Actor at the same address. Normal Actor teardown closes the worker's active tunnels. See [IDENTITY.md](./IDENTITY.md) for the certificate lifecycle.

The actor-identity CA used for `ateom-for-actor` certificates is separate from the optional TLS-interception CA. Actors must explicitly receive the latter trust bundle to accept intercepted HTTPS.

Policy decisions and changes are not instantaneous on every path: the Envoy sidecar caches policies briefly, and already admitted connections are not revoked by a policy update. See [RBAC.md](./RBAC.md) for the current lack of caller authorization on policy writes.

## Credential injection

An `http` or `https` rule can request outbound credential-header replacement. Both dataplanes replace an Actor-supplied placeholder header on cleartext HTTP and intercepted HTTPS. An absent header skips injection; failure to obtain a required provider credential denies the request. The installer can configure the bundled Kubernetes Secrets provider or another provider for either dataplane. Cleartext HTTP exposes the injected credential on the connection to the destination. See [CREDENTIALS.md](./CREDENTIALS.md) for the request path and its trust limits.

The default Envoy path can also obtain an Actor JWT from ateapi and replace an outbound request header with it, allowing the destination to verify the Actor without requiring the workload to hold the token. This path does not require a static-secret provider. The pinned AgentGateway image does not implement Actor JWT injection; see [IDENTITY.md](./IDENTITY.md#the-actor-itself) for status and tracking.
