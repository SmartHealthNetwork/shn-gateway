# Deploying the Smart Gateway

The gateway is a stateless container. It runs as an unprivileged user
(`65532:65532`, distroless) and listens on **port 8080** by default.

Because it runs unprivileged it cannot bind ports below 1024. Do not try to make
it listen on 443 directly — publish it on 443 at your load balancer, or set
`PORT` to a high port such as `8443`. Both are one-line changes.

## Two supported topologies

### 1. Terminate TLS at your load balancer (recommended)

The common production shape. Your ALB / NLB / ingress controller holds the
certificate and forwards to the container.

```
Internet ──https:443──▶ ALB (ACM cert) ──http:8080──▶ gateway container
```

- Target group protocol HTTP, port 8080.
- Health check path: `/health`.
- The address you register (`--base-url`) is the **load balancer's** public
  https URL, not the container's.

### 2. Terminate TLS inside the container

For hops that must not carry plaintext even inside your own network — for
example an EHR or interface engine pushing PHI into the Da Vinci ingress, which
is authenticated with short-lived bearer tokens. Mount a certificate and set
both variables:

```
-e PORT=8443 \
-e TLS_CERT_FILE=/etc/shn/tls/tls.crt \
-e TLS_KEY_FILE=/etc/shn/tls/tls.key \
-v /your/certs:/etc/shn/tls:ro \
-p 8443:8443
```

- Both variables are required together. Setting one without the other is a
  startup error — the gateway will not fall back to plaintext.
- TLS 1.2 is the minimum accepted version.
- A bad or unreadable certificate fails at startup with a clear message, before
  the gateway reports that it is listening.
- The certificate is read once at startup. To rotate, restart the container.
- Files must be readable by uid `65532`.
- With TLS enabled the listener also offers HTTP/2 via ALPN (standard Go
  behavior); the plain-HTTP default speaks HTTP/1.1 only. All gateway routes
  are ordinary request/response, so both protocols behave identically.

On success the startup line names the scheme:

```
gateway: role=provider holder=<id> listening on https://0.0.0.0:8443
```

## Your registered address must be https

Registration **rejects** any `baseURL` that is not a publicly resolvable https
URL — this is enforced, not advisory. A gateway fronted by plain http cannot be
registered at all. Private, loopback, and link-local addresses are refused too.

This applies to responders (payer, facility) that receive delivered requests.
Originator-only participants are never dialed, but the URL must still validate.

## What is already protected without any TLS configuration

TLS protects the hop. It is not what protects the payload:

- **Payloads are encrypted end to end** to the recipient's key. Intermediaries
  route on metadata and cannot read the contents.
- **Every inbound delivery is authenticated** with a signed, single-use,
  short-TTL assertion that is verified before the payload is touched. This
  verification has no "off" state — a gateway that cannot verify it will not
  start.
- **Authority is carried separately** from the channel, in a bound token
  checked per leg. Neither substitutes for the other.

So the case for configuring TLS is defense in depth and your own network policy
— not payload confidentiality on the delivery path, which does not depend on it.

## Running more than one replica

**Multi-replica ingress is supported when `SHN_STORE_DATABASE_URL` is set.** With the
database configured, every replica of a holder shares the ingress bearer signing key
(rotated daily; issued bearers carry a `kid`; kept only where the Da Vinci ingress is
enabled — a gateway that mounts no token endpoint stores no key), the one-time-use records behind
`client_assertion` `jti`, Hub assertions and patient-access tokens, and the exchange
correlation records (kept for `EXCHANGE_TTL`, default 7 days). A token issued by one
replica verifies at any other; a replayed assertion is refused at any replica; a
restart or a rolling deploy keeps outstanding tokens valid.

Without the database the gateway signs with a key generated in memory at startup and
logs, once, at boot:

```
ingress bearer key is ephemeral (no SHN_STORE_DATABASE_URL): run a single reachable instance
```

Run exactly one reachable instance in that mode. (A companion posture summary line,
`gateway: shared state: in-process (…)` or `gateway: shared state: postgres (…)`, names
which backend the shared state came from and the exchange TTL in force.)

The demo `/scenario/*` routes — `/scenario/reset` included — keep their two-phase session
in the process that served them, with or without the database. Drive them against one
instance directly, never through the load balancer that spreads requests over replicas: a
session started on one replica cannot be completed on another, and a reset served by one
leaves the other's session state standing. Scope that load balancer to the Da Vinci
ingress paths and leave the demo surface off it.

### The signing key's trust boundary

The holder's ingress bearer signing key is stored in the database as a plaintext PKCS#8
PEM (a P-384 / ES384 private key, in `gw_ingress_key`). **Anyone who can `SELECT` from
that table can mint a bearer this gateway's ingress accepts.** The database role in
`SHN_STORE_DATABASE_URL` is therefore the signing authority for the holder's ingress
bearers, and the DSN is a credential of the same weight as a private key file: protect it
the same way.

- Require TLS on the connection to the database, and enable encryption at rest.
- Give the gateway a least-privilege role — only the `gw_*` tables it uses, no
  superuser.
- Where your deployment allows it, give each holder its own database or its own role, so
  one holder's DSN is not also the signing authority for another's ingress.
- Treat the DSN like key material in your secret store, your logs and your backups; a
  database dump contains the signing key.

To retire a key you believe is exposed, delete its `gw_ingress_key` row and restart the
replicas — the row alone is not enough, because a replica that has already cached the key
keeps verifying with it until the key's own `not_after` (see the cache note at the end of
this section).

### Behaviour during a database outage

If the database is unreachable when a client requests a token, `/oauth/token` answers
`503 server_error`; bearers already issued keep verifying from the replica's cache
until the database returns. **That one is retryable by the client, and the retry carries a
NEW `client_assertion`:** a `503` never returns a token, so nothing is lost by minting a
fresh assertion — which is what a `private_key_jwt` client does anyway, one per request.
Whether the previous `jti` was recorded is immaterial to such a client.

Re-sending the SAME assertion after a `503` **may be refused `401 invalid_client` as
replayed**, and that refusal is correct. The endpoint runs everything that can refuse a
request — resolving the signing key, checking the scope, producing the signature — before
it writes the assertion's one-time-use record, so those refusals leave the `jti` unspent.
But the record's write is the last step and a failure there is ambiguous: the row may have
committed with only its acknowledgment lost. The gateway cannot tell, so it answers `503`
rather than accusing an honest client — and the one rule that always holds is the fresh
assertion. An `invalid_scope` `400` is not a `503`: it is decided before the record, so the
client fixes the `scope` and retries with the assertion it already holds.

An ingress route answers `503` with diagnostic `ingress key store unavailable` and
`Cache-Control: no-store` when the shared key store cannot be read at all — a well-formed
`kid` the gateway can neither resolve nor rule out. This is distinct from the `401` an
unknown or bad credential gets: the `401` still means "your bearer is not accepted", the
`503` means "this gateway could not tell". A client may retry a `503` with the same
bearer. On `$submit` and `$questionnaire-package`, this is a FHIR
`OperationOutcome` with media type `application/fhir+json`; CDS Hooks routes retain
the JSON `{"error":"ingress key store unavailable"}` envelope.

The one-time-use records are consulted on two more paths, and both **refuse, fail-closed,**
for the duration of a database outage: every Hub-authenticated delivery at
`/substrate/inbound` and every patient-access read check their record before the request
is served. Those legs are refused rather than served on a record the gateway could not
verify. They answer `503` rather than a denial, which makes the *cause* truthful — the
sender is not told its Hub assertion or its patient-access token was bad — but **the
request does not survive**: the Hub does not retry a forward, so it turns the `503` into
its own `502 "forward to recipient failed"` and audits the leg as failed. A database blip
at the recipient loses that delivery. Both paths resume as soon as the database returns.
The retry rule is the same on these two as on the token endpoint — a retry needs a fresh
credential (a new Hub assertion, a new patient-access read), because a `503` here can also
be a record whose write was committed but not acknowledged. Nothing retries them today:
the Hub does not re-forward, and the patient-access read is the caller's to repeat.
Without `SHN_STORE_DATABASE_URL` the same records are kept in each process and no
database is involved.

### Latency and back-pressure during a database outage

Two bounds are worth knowing when you size health checks and client timeouts:

- **An ingress bearer whose `kid` is not in the replica's cache is answered immediately**
  whenever the cache holds any key. The request is not held while the replica goes
  looking: it is refused with the ordinary `401`, and the reload it needed runs in the
  background (at most one miss-driven load per second per replica, plus the 30-second
  refresh and the load the token endpoint's own key rotation makes). A lagging warm
  replica can refuse a bearer from a sibling's later rotation until a successful reload
  learns that key. The one-second throttle limits queries; it does not guarantee
  acceptance within a second or on the next request. Nothing on this path waits on the
  database, which is deliberate: the bearer
  header is read before its signature is checked, so any wait here would be a wait an
  unauthenticated caller could ask for.

  **A replica that has just started is not in that window.** Until its first load has
  *succeeded* — normally milliseconds, since it starts loading before it serves — a
  replica is *cold*: its cache has never held a snapshot, so it cannot tell an unknown
  `kid` from one it has simply not loaded yet, and it never refuses a bearer on that
  evidence. A cold replica answers a miss only from a load that **started after the
  request arrived** — the only snapshot that can be trusted to carry a key minted before
  the request — and parks the request until one completes: on the load already running
  (started by the refresh loop, or by another request), then, if that one was older than
  the request, on the next load slot. So a request that arrives before the first load
  has finished is still answered correctly, and a restart, and therefore a rolling
  deploy, accepts bearers already in flight on the first call.

  What that costs, plainly: in **both** states the miss path puts at most **one load per
  second per replica** on the database, and at most one at a time, however many requests
  arrive — the once-a-second throttle applies while cold exactly as it does while warm,
  and a load that fails is logged once. (The 30-second refresh and the token endpoint's
  key rotation are separate loads on top of that.) What the cold state adds is one **parked request per
  concurrent request**, for **at most one store timeout (2 s)** each, measured from the
  request's arrival across every wait it takes. A request that reaches that bound is
  answered with a retryable `503`, never a `401` against a live bearer. Nothing here waits
  on the database itself: the parked request waits on the load's completion signal, and
  one load serves every waiter.

  A successful load that finds **no rows** ends the cold state like any other: a
  brand-new holder whose replicas read an empty table are on the ordinary once-a-second
  bill from then on. A replica holding no key still does not answer from that empty
  snapshot: when the throttle allows, the request runs the one load itself and returns
  when it does — at most one such request per throttle second, held for at most one
  store timeout, every other miss answered at once — and so verifies the first key a
  sibling minted on the very first call if that load succeeds and includes the key.
  An armed throttle or an incomplete reload can still cause refusal.

  **Concurrent initial creators adopt one committed key.** Both the token endpoint's
  creation path and the background refresh coordinate through a holder-specific
  database transaction lock, then read the newest readable eligible key after the lock.
  Once both initial creation/adoption operations have succeeded, both replicas cache
  the same committed signing key: the first authenticated cross-replica call succeeds
  even with an armed throttle or a later reload stalled. This guarantee does not cover
  an arbitrarily lagging replica during a later rotation or older-version writers that
  do not use this coordination. Existing sibling keys stay valid until their expiry.
  Begin, lock, selection, insertion and commit share one two-second deadline; this is
  not a two-second bound on the whole token request, which has other store operations
  and existing signing/reload waits.

  What keeps a replica cold
  is only a database it cannot read: an outage that outlasts start-up keeps it cold for as
  long as the outage lasts, and while it does each request that misses parks for up to
  that one store timeout while the replica's single load stream retries once per second,
  then comes back with its retryable `503`. Size the replica's request concurrency and
  your client's timeout for that. Once the first load has succeeded the wait is gone and
  the immediate answer above is the only path there is.
- **The exchange correlation seam stops calling the database for five seconds after a
  failure** and then lets exactly one request through as a probe: success resumes normal
  operation, failure re-opens the window. The seam is best-effort by contract, so a
  skipped call costs no more than the failed call it stands in for — it just stops a
  sustained outage from putting a database round trip in front of every request. The
  one-time-use records and the signing key get no such window: they decide whether a
  caller is admitted, so they always ask.

The shared-state connection pool is sized by `SHN_STORE_MAX_CONNS` (default `8`, with a
warm floor of 2) and each connection attempt is bounded at one second, so a database that
accepts packets but never completes a handshake fails fast instead of waiting out the
operating system's TCP timeout.

The demo `/scenario/*` routes keep their two-phase session in the process that served
them. A `/scenario/reset` therefore clears the holder's shared exchange records
everywhere and the demo session state of the replica that received the call — one more
reason to put those routes behind a single instance. They are mounted on the **provider**
role only, which is why: the reset carries no credential of its own, and the payer role's
mux is a public FHIR surface.

If the shared exchange records cannot be cleared — the database is unreachable —
`/scenario/reset` answers `503` with `{"error":"exchange store reset failed"}` rather than
`200`. A reset that cleared nothing must not read as a clean slate.

The payer role exposes no `/scenario/reset` route; requests return `404`.

A key row deleted from the database keeps verifying at any replica that has already
cached it, until the key's own `not_after` (its 24-hour rotation life plus a bearer
lifetime). To retire a key immediately, restart the replicas.

## Checklist

- [ ] Container runs as `65532:65532`; certificate files readable by that uid
- [ ] TLS terminated at the load balancer, or `TLS_CERT_FILE` + `TLS_KEY_FILE` set
- [ ] `PORT` matches what the load balancer targets
- [ ] Health check on `/health`
- [ ] Registered `--base-url` is the public https address, and does not redirect
- [ ] `SHN_STORE_DATABASE_URL` set if the Da Vinci ingress routes run at more than one replica (a single reachable instance otherwise)
- [ ] That DSN and its database protected as key material — TLS, encryption at rest, least-privilege role (see "The signing key's trust boundary")
