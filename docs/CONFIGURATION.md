# Configuration reference

The gateway is configured entirely by environment variables. `SHN_DISCOVERY_URL`
resolves almost everything else; a typical deployment sets only a handful of
variables. This document is the complete reference; for a task-oriented walk
through wiring your own systems, see [INTEGRATION.md](INTEGRATION.md).

- [Required (every role)](#required-every-role)
- [Validation (required to boot — FR-36)](#validation-required-to-boot--fr-36)
- [Per-role](#per-role)
- [Networking](#networking)
- [Observer stream (optional — local tooling)](#observer-stream-optional--local-tooling)
- [Operational checks (optional)](#operational-checks-optional)
- [Connect your system of record](#connect-your-system-of-record)
- [Accept Da Vinci requests from a provider EHR](#accept-da-vinci-requests-from-a-provider-ehr-provider-optional)
- [Advanced overrides](#advanced-overrides-rarely-needed)
- [Native-forward payer mode (`PAYER_DAVINCI_*`)](#native-forward-payer-mode-payer_davinci_)
- [Provider DTR population (`PROVIDER_DTR_*`)](#provider-dtr-population-provider_dtr_)
- [Sealed message frames (v1)](#sealed-message-frames-v1)
- [Exchange contract lines (`SHN_CONTRACT_VERSIONS`)](#exchange-contract-lines-shn_contract_versions)
- [Demo-only egress narrowing (`SHN_DEMO_EGRESS_NATIVE_LINES`)](#demo-only-egress-narrowing-shn_demo_egress_native_lines)
- [Demo-only pre-seal edge capture (`SHN_DEMO_EDGE_CAPTURE`)](#demo-only-pre-seal-edge-capture-shn_demo_edge_capture)

---

## Required (every role)

| Env var | Description |
|---|---|
| `SHN_DISCOVERY_URL` | The single anchor — resolves the network's trust-plane endpoints (Hub, Authorization Framework, registrar, consent, audit, PHG, …) and trust anchors. Example: `https://accounts.shn-preview.org/discovery`. |
| `ROLE` | `provider`, `payer`, `facility`, or `phg`. Must match the role you registered. |
| `SHN_SECRETS` | Path to the bundle directory written by `shn register -out`. |

## Validation (required to boot — FR-36)

The gateway **refuses to start without a FHIR validator** — every resource is
validated at your gateway's own edge before the leg proceeds. The published
discovery descriptor does not advertise a validator, so you must supply one:

| Env var | Description |
|---|---|
| `FHIR_VALIDATE_URL` | A FHIR `$validate` endpoint (a HAPI server with the Da Vinci CRD/DTR/PAS + US Core IGs loaded). The production path. |
| `SHN_FAKE_VALIDATOR` | Set to `1` to use a no-op validator. **Dev only** — skips real profile validation. Use for a first wiring smoke test; never in production. |

If neither is set (and discovery advertises none), the gateway exits with
`refusing to run without per-message validation (FR-36)`.

`FHIR_VALIDATE_URL` is the **`2.0` contract line's** lane. If you declare a `2.1` or `2.2`
line, each needs its own `$validate` endpoint — see
[Exchange contract lines](#exchange-contract-lines-shn_contract_versions).

**Recommended: co-locate the validator in your own boundary.** Run the IG-loaded
`$validate` as a sidecar alongside the gateway and point `FHIR_VALIDATE_URL` at it
(e.g. `http://validator:8080/fhir`). Because `$validate` needs the full (PHI-bearing)
resource, co-location keeps PHI **inside your boundary** — it is never sent to an
SHN-operated validator. This is the config-only deployment posture: the gateway image
plus a co-located IG-loaded validator, with no separate validator host to stand up.

This repository ships that wiring ready-made: **`deploy/bundle/compose.yml`** pairs the
gateway with a co-located IG-loaded `$validate` sidecar as a config-only unit. Clone the
repo, set `SHN_DISCOVERY_URL` / `ROLE` / `SHN_SECRETS`, and
`docker compose -f deploy/bundle/compose.yml up --build` — no separate validator host.
See [`deploy/bundle/README.md`](../deploy/bundle/README.md).

## Per-role

| Env var | Applies to | Description |
|---|---|---|
| `PAYER_DIRECTORY` | provider | **Static override.** Path to a JSON file mapping a member's Coverage payor identity (`{"system","value"}`) to the payer holder id you originate to. When set, it takes precedence over the default feed-derived routing described below — use it for bootstrap/testing, or when you route to a payer that does not publish its identity in the network feed. Example row: `[{"system":"urn:oid:2.16.840.1.113883.6.300","value":"00001","holderId":"payer"}]`. |

**Payer routing is coverage-derived, off the network feed by default (FR-G41).** A provider
gateway resolves the recipient of every payer leg from the patient's **own Coverage** payor
identity — there is **no default payer**. By default it uses `FeedPayerRouter`: it indexes the
converged `/holders` feed, where each `role=payer` holder publishes its operator-attested payer
identities (`payerIds`), and maps `Coverage.payor → holder id`. This is the drop-in, many-to-many
property — a new payer holder self-registers its identity and providers discover it with **no
config change**. Resolution is fail-closed: a miss (no coverage / no parseable payer / no holder
claims that identity) **fails closed with 422**, and an ambiguous identity claimed by more than one
holder also fails closed (`AI-G12`). Set `PAYER_DIRECTORY` only to override this default with a
static map.

The trust and resolution model behind this — how a payer's identity gets published into the
network feed, how the payload-blind Hub maps a holder id to a gateway URL, and the failure modes
at each stage — is covered in the SHN participant onboarding materials referenced from
`PARTICIPANT_PROTOCOL.md` in the `shn-sdk` repo.

**How a payer publishes its identity into the feed (payer-onboarding path).** A payer identity is
**operator-attested, never self-asserted** (`AI-G12`/`OWD-G11`): the applicant *claims* its payer
identities on the access request; the operator *vouches* them at approval (into the org's
authorized grant); and client registration enforces `declared ⊆ authorized` before the identity
lands in the registrar (`UNIQUE(system,value)` globally). Only then does a provider's
`FeedPayerRouter` route to it.

Responder roles (`payer`/`facility`/`phg`) need no `PAYER_DIRECTORY`: they receive
at `POST /substrate/inbound` and reply to whoever the Hub delivers from. A payer has no
built-in decision policy — it answers Da Vinci legs by native-forwarding to your own
Da Vinci endpoint (`PAYER_DAVINCI_BASE_URL`, required); see
[INTEGRATION.md](INTEGRATION.md#payer-decisioning) for the config and the (advanced,
Go-only) in-process alternative.

## Networking

| Env var | Description |
|---|---|
| `PORT` | Listening port. Default `8080`. |
| `HOST` | Bind address. Default `0.0.0.0`. |
| `TLS_CERT_FILE` | PEM certificate for in-container TLS on the main listener. Must be set together with `TLS_KEY_FILE`; setting one alone is a startup error. Unset (default) = plain HTTP, with TLS terminated upstream at your load balancer. Conventionally paired with `PORT=8443`. TLS 1.2 floor; read once at startup, so restart to rotate. See [DEPLOYMENT.md](DEPLOYMENT.md). |
| `TLS_KEY_FILE` | PEM private key matching `TLS_CERT_FILE`. Must be readable by uid `65532` (the container's unprivileged user). |

## Observer stream (optional — local tooling)

The gateway can emit a live, structured stream of its own leg, ingress, and
validation events over a loopback-only SSE endpoint — a window onto what this
gateway is doing, for local tooling (for example, the SHN Kit's flow
inspector) rather than another participant. It is off unless configured, and
the address it binds must be loopback: **the events include the request/response
payloads flowing through this gateway's edge, so enabling it exposes the
contents of exchanges with your connected systems to whatever process you
point it at** — treat it like any other local access to your data, not a
network-facing feature.

| Env var | Description |
|---|---|
| `OBSERVER_ADDR` | Loopback `host:port` for the observer stream (SSE `GET /events`, `GET /health`): structured leg/ingress/validation events **including request/response payloads as seen at this gateway's edge**. Off unless set; non-loopback values are refused at startup. Intended for local tooling (the SHN Kit flow inspector); enabling it exposes payloads from your connected systems to local processes. |

## Exchange metrics (optional — CloudWatch EMF)

The gateway can emit per-leg `LegOutcome`/`LegError` CloudWatch EMF metrics
(counts only — no payloads, no PHI) at the origination round-trip seam. Off
unless configured; the published binary defaults OFF.

| Env var | Description |
|---|---|
| `METRICS_SERVICE` | Names this gateway service for the EMF `Service` dimension (e.g. `provider-data-gw`). Empty (default) disables metric emission entirely. |
| `METRICS_NAMESPACE` | CloudWatch metrics namespace. Default `SHN/Preview`. |
| `METRICS_ENV` | EMF `Env` dimension value. Default `shn-preview`. |

## Operational checks (optional)

The gateway can probe its own outbound dependencies — your FHIR system of
record, a partner Da Vinci payer, and every other endpoint URL you've
configured — and report what it found, both automatically at startup and on
demand. This is an operator diagnostic only: a failing probe never affects
`/health` or the request-serving path.

| Env var | Description |
|---|---|
| `CHECKS_TOKEN` | Bearer token that gates `GET`/`POST /internal/checks`. When set, a request must present `Authorization: Bearer <token>` exactly, or it gets `401`. When unset (the default), the endpoint instead accepts only requests that reach it directly from the gateway's own host (loopback), returning `403` for anything else — **note that behind any reverse proxy or load balancer, the connecting address the gateway sees is the proxy's, not the original caller's**, so leaving `CHECKS_TOKEN` unset behind a proxy means only that proxy's own host can reach it, not any external caller. Set a token to allow probing from off-host operator tooling. |

**`GET /internal/checks`** returns the most recent completed run: `503`
(`{"error":"checks have not completed yet"}`) if the gateway hasn't completed
its first run yet (normally moments after startup), otherwise `200` with the
run's results:

```json
{
  "results": [
    {
      "id": "FHIR_DATA_URL",
      "target": "https://sor.example",
      "ok": true,
      "detail": "CapabilityStatement (FHIR 4.0.1)",
      "checkedAt": "2026-01-01T00:00:00Z",
      "latencyMs": 42
    },
    {
      "id": "FHIR_TOKEN_URL",
      "target": "https://idp.example",
      "ok": false,
      "detail": "credential check failed (HTTP 401)",
      "checkedAt": "2026-01-01T00:00:00Z",
      "latencyMs": 87,
      "failure": { "code": "credential-rejected", "hint": "HTTP 401" }
    }
  ],
  "checkedAt": "2026-01-01T00:00:00Z"
}
```

Each result's `target` is redacted to `scheme://host` — never a path, query
string, or credential, even if the URL you configured carried one.

A failing result also carries a machine-readable `failure` object: `code` is one of
`unreachable` (the endpoint never answered), `http-status` (it answered with a failing
status), `invalid-capability-statement` (a 2xx answer that is not a valid
CapabilityStatement), `credential-rejected` (the credential check failed), `not-checked`
(the run deadline left this probe unprobed), or `internal` (a gateway-side bug, not a
network condition); `hint` carries the redaction-safe specifics (for example `HTTP 401`)
and is omitted when the code says it all. Passing results carry no `failure` key. The
same redaction rule applies to `hint` as to `target` and `detail`.

**`POST /internal/checks`** runs the probes immediately and returns the same
shape, with two safety limits:

- **Single-flight.** A `POST` while a run is already in progress returns `409`
  (`{"error":"checks already running"}`) instead of starting a second,
  overlapping run.
- **30-second cooldown.** A `POST` within 30 seconds of the last completed run
  returns the cached results instead of re-probing — some probes exercise the
  same credential exchange your traffic path uses, and probing too often risks
  tripping a partner's own rate limiting.

The gateway also runs this same set of probes once automatically, shortly
after startup, so the first `GET` after boot typically already has results.

What gets probed is derived from what you've configured — no separate list to
maintain. `FHIR_DATA_URL` and `PAYER_DAVINCI_BASE_URL` are checked with a live
FHIR `$metadata` fetch; `FHIR_TOKEN_URL`, `PAYER_DAVINCI_TOKEN_URL` and `PROVIDER_DTR_POPULATE_TOKEN_URL` are
checked with a live credential fetch against your configured client; every
other endpoint URL you've set — the per-operation `PAYER_DAVINCI_DTR_BASE_URL` /
`PAYER_DAVINCI_PAS_BASE_URL` and the [advanced
overrides](#advanced-overrides-rarely-needed) included — is checked with a plain
reachability request. `PAYER_DIRECTORY` is a local file path, not a network
endpoint, and is never probed.

## Connect your system of record

See [INTEGRATION.md](INTEGRATION.md) for how these fit together.

| Env var | Description |
|---|---|
| `ORIGINATION_PROFILE` | provider. Set to `provider-data` to originate every prior-auth UC off your seeded FHIR system of record and drive real payer verdicts — the config-only provider lane, no custom code. `demo` originates the shipped demo order set instead. Both lanes answer a REAL payer's questionnaire, so both require `PROVIDER_DTR_POPULATE_URL` (the operated `$populate` endpoint, validated at boot). |
| `FHIR_DATA_URL` | FHIR R4 base URL for your system of record. **Required on every role.** The gateway reads its members, coverage and clinical facts from your own FHIR server; there is no built-in persona stub any more, so an unset value is a boot error naming this variable. |
| `FHIR_TOKEN_URL` | SMART Backend Services token endpoint, if your FHIR server requires authenticated access. Requires the client credential block below. |
| `FHIR_CLIENT_ID` | SMART client id. |
| `FHIR_CLIENT_KEY` | Path to the SMART client's private-key PEM file (the value is a path, not the key text — mount the file into the container). Required for `private_key_jwt` mode (i.e. when `FHIR_CLIENT_SECRET` is unset). |
| `FHIR_CLIENT_ALG` | `ES384` or `RS384`. Required for `private_key_jwt` mode (i.e. when `FHIR_CLIENT_SECRET` is unset). |
| `FHIR_CLIENT_SCOPE` | Requested scope. Default `system/*.read` — must be a scope your server grants this client. |
| `FHIR_CLIENT_KID` | Key id for the client assertion JWK, if your server requires it. |
| `FHIR_CLIENT_SECRET` | OAuth2 client secret for the `client_secret_post` `client_credentials` grant — for authorization servers that cannot issue asymmetric credentials. The value is the secret **itself, not a path** (unlike `FHIR_CLIENT_KEY`). Mutually exclusive with `FHIR_CLIENT_KEY`/`_ALG`/`_KID`; prefer `private_key_jwt` when your server supports it. |
| `SHN_STORE_DATABASE_URL` | Postgres DSN for durable claim-state storage **and the shared replica state: ingress signing key (where the Da Vinci ingress is enabled), one-time-use records, exchange correlation**. Omit for in-memory (non-durable across restarts; single instance only — see [DEPLOYMENT.md](DEPLOYMENT.md), "Running more than one replica"). **The database behind this DSN holds the ingress bearer signing key in the clear, so the role in the DSN is the signing authority for this holder's ingress bearers** — protect both as you would a private key file; see [DEPLOYMENT.md](DEPLOYMENT.md), "The signing key's trust boundary". |
| `SHN_STORE_MAX_CONNS` | Maximum connections in the shared-state pool (default `8`; a `MinConns` floor of 2 is kept warm). Four consumers share this one pool — claim state, one-time-use records, ingress signing key, exchange correlation — and an exchange append holds a connection for its transaction, so the default is sized for concurrency rather than for the host's CPU count. Size it against your database's connection limit divided by the number of gateways sharing the DSN: the fleet's ceiling is this value multiplied by that count, and it has to stay under the limit. Count replicas, not deployments: the draw is replicas × gateways sharing the DSN × this value, and that product is what has to stay under the limit (each replica also holds the warm floor open whether or not it is serving). Overrides any `pool_max_conns` in the DSN. Must be an integer in `[1, 2147483647]` (the pool field's own width); anything else is a boot error naming the variable. |
| `EXCHANGE_TTL` | Lifetime of an exchange correlation record as a Go duration (default `168h`). Applies to exchanges begun after the change — existing records keep the expiry they were written with. Must be positive; an unparsable or non-positive value is a boot error naming the variable. |

## Accept Da Vinci requests from a provider EHR (provider, optional)

See [INTEGRATION.md](INTEGRATION.md#native-da-vinci-ingress) for when to use this
instead of `provider-data` origination.

Set `PROVIDER_DAVINCI_INGRESS=1` to mount the provider-side Da Vinci ingress: the
gateway accepts a provider EHR / reference-implementation's **native Da Vinci
requests** — CDS Hooks `order-sign`, `order-select` and `order-dispatch` (Coverage
Requirements Discovery), `Questionnaire/$questionnaire-package` (DTR), and `Claim/$submit`
(PAS) — and forwards them through to the Hub. A CDS Hooks request is forwarded as your EHR
sent it, with `fhirServer` and `fhirAuthorization` removed and any prefetch value it left
out added from **your own system of record** (see [CDS Hooks prefetch](#cds-hooks-prefetch));
there is never a callback into your systems.

A `$questionnaire-package` request is forwarded as your EHR sent it: every parameter, in
order and repeated as sent, a questionnaire canonical's `|version`, `context`, `meta` and any
parameter the gateway does not know. Every resource in it (each `coverage`, each `order`,
everything in `referenced` and in parameter parts) must be about one patient, the one its
coverages and orders name (a request naming none is refused with 422, one naming another
patient anywhere with 403), and all of its coverages must name one payer (422 otherwise).
Name the patient with a relative reference (`Patient/<member id>`): a questionnaire request
has no `fhirServer`, so an absolute Patient reference cannot be read as your EHR's and is
refused (403). A `coverage` parameter is always your EHR's own: the gateway never adds one
beside it, and a `coverage` parameter that carries no resource (only a reference, or
nothing) is refused with `400 coverage parameter carries no resource`.
When the request carries no `coverage` parameter, the gateway appends one parameter holding
the patient's Coverage exactly as **your own system of record** returns it for a Coverage
search (recorded as a `prefetch.obtained` event with `operation` `questionnaire-package`,
also when the search finds nothing or cannot run). When that system holds several Coverages
that name one payer, the first is appended. It refuses instead when that system names the
patient by another id (422), holds no Coverage (422), holds Coverages naming different payers
(422), cannot search (422) or is unavailable (503). The appended Coverage is sent alone, not
with the payor Organization it may reference (see
[Payer backend identity mapping](#payer-backend-identity-mapping) for what that means for a
payer that maps its identity). The request is sent to the payer's gateway naming the operation, which a payer
gateway accepts only when it declares the framed-operation capability (`v1op`); a payer that
does not is refused before anything is sent with 502 `payer gateway does not support framed
DTR operations (upgrade required)`. The payer's answer is returned to your EHR exactly.

`GET /cds-services` lists one CDS service per hook: `shn-order-sign` (`order-sign`),
`shn-order-select` (`order-select`) and `shn-order-dispatch` (`order-dispatch`). Post each
request to the service for its hook (`POST /cds-services/shn-order-sign`): an id that is not
listed is `404`, and a request whose `hook` is not the service's hook is `400`. The hook is
never changed on the way to the payer. The payer's answer is returned to your EHR exactly as
the payer sent it once it meets the CDS Hooks response rules (see
[CDS Hooks answers](#cds-hooks-answers)); `cards` may be empty, with the coverage information
in `systemActions`. A payer that offers no service for your hook answers `422` with the hooks
it offers.

What the gateway changes on these requests is only the callback removal and the prefetch and
coverage additions above. A signature inside a message (`Bundle.signature`,
`Provenance.signature`, a `Signature` element) travels untouched. HTTP-level signatures
(signed header fields, a detached JWS) are not carried: each gateway terminates HTTP, and the
network carries only the message's media type, contract line and operation name with it. A
participant that needs an end-to-end signature signs inside the payload. The answer's body
reaches your EHR exactly; its media type is the gateway's own (`application/json` for CDS
Hooks, `application/fhir+json` for DTR and PAS).

Inbound requests authenticate via **SMART Backend Services**: the gateway hosts its
own `POST /oauth/token` and `GET /.well-known/smart-configuration`, verifies a
registered client's signed JWT assertion (`private_key_jwt`, ES384/RS384), issues a
short-lived bearer, and verifies it on every ingress call. The client-side procedure
(key pair, registration entry, assertion claims, `curl`) is in
[INTEGRATION.md → Calling the ingress from your EHR](INTEGRATION.md#calling-the-ingress-from-your-ehr).

| Env var | Description |
|---|---|
| `PROVIDER_DAVINCI_INGRESS` | Set to `1` to mount the ingress on the provider gateway. |
| `PROVIDER_DAVINCI_INGRESS_BASE_URL` | The gateway's public base URL — the SMART audience the gateway pins and the token endpoint it advertises. **Required** when the ingress is enabled. |
| `INGRESS_CLIENTS_FILE` | Path to a JSON array of registered inbound clients: `[{"client_id":"…","alg":"ES384","public_key_pem":"-----BEGIN PUBLIC KEY-----…","scopes":["system/Davinci.write"]}]`. **Required** (≥1 client) when the ingress is enabled. |

Enabling the ingress without a base URL or at least one valid registered client is a
hard startup error.

**This ingress is a private, within-boundary surface, not a public endpoint.** The
gateway's only public-internet leg is the gateway↔Hub connection; every connection to
your own systems — including this ingress — is private/within-boundary. Your EHR or
reference implementation calls it from inside your own network, authenticating as one
of the clients pre-registered in `INGRESS_CLIENTS_FILE`. There is no plan to expose
this ingress on the public internet. Dynamic client registration (as opposed to the
static file above) remains a tracked enhancement.

### CDS Hooks prefetch

The gateway advertises six prefetch keys: `patient` (`Patient/{{context.patientId}}`),
`coverage` (`Coverage?patient=Patient/{{context.patientId}}&_include=Coverage:payor`: the payer
resolves each Coverage's payor from the request, since it has no route into your system),
`deviceHistory` (`DeviceRequest?patient=Patient/{{context.patientId}}&_include=DeviceRequest:performer`:
the payer resolves a dispatched order's supplier the same way), and `serviceHistory`,
`medicationHistory` and `questionnaireResponses` (each
`<type>?patient=Patient/{{context.patientId}}`). For each:

- **Your EHR sent it** (a resource, a Bundle, or `null`): it is sent unchanged.
- **Your EHR left it out:** the gateway obtains it from your system of record, for the
  Patient your connector names for the member, and adds it to the request's `prefetch`
  (nothing else in the request changes):
  - `patient` is read (`Patient/<id>`) and sent exactly as your server returned it. No such
    Patient is a `422 patient not found in system of record`. (See the member id limitation
    below for when nothing is obtained.)
  - `coverage` and the history keys are searched (`<type>?patient=Patient/<id>`, the coverage
    search with `_include=Coverage:payor` and the device search with
    `_include=DeviceRequest:performer`, within the connector's search bounds). The value
    sent is a searchset the gateway writes: each matching record, and each record the search
    included (a Coverage's payor Organization, an order's performer, as `search.mode`
    `include`), exactly as your
    server returned it, under an entry `fullUrl` the gateway assigns (`urn:uuid:`). Nothing
    else of your server's answer is sent — not its links, its entry addresses or its messages
    about the search (`OperationOutcome` entries) — so the payer is never given an address in
    your system. (A record that refers to another record by your server's absolute URL keeps
    that reference; the payer cannot resolve it within the request.) FHIR defines no
    resolution of a relative reference against entries whose `fullUrl` is a `urn:uuid:`, so
    a record's relative reference to another record in the searchset (a Coverage's
    `payor`, an order's `performer`) resolves only for a payer that matches entries by
    their resource type and id. A search with no match sends `null`.
  - A search your system of record cannot answer (unavailable, not supported, not a
    searchset, or over the bounds) leaves the key out. Without a coverage the request cannot
    be routed, so it is refused: `503 coverage unavailable from system of record` when your
    server is unavailable, otherwise a `422`. A missing history key is left for the payer to
    decide on. A connector that does not implement search (the built-in FHIR connector
    does) can obtain only `patient`.
- **Every value must be about the request's patient.** Each resource is checked against
  the patient by its FHIR Patient-compartment reference (for example
  `Coverage.beneficiary`, `ServiceRequest.subject`); a value holding another patient's
  record is refused — `403` for a value your EHR sent, `502 system of record returned
  another patient's resource` for one your system of record returned — and nothing is
  sent. Absolute references on your EHR's `fhirServer` are read like relative ones, both here
  and when the request's patient references (`context.patientId`, each draft order's subject,
  each prefetch resource's patient) are bound to one patient.
- **No opaque content.** A `Binary` resource (a value, a Bundle entry or a contained
  resource) is never sent in a prefetch value: `403` when your EHR sent it, `502 system of
  record returned a Binary resource` when your system of record returned it.
- **The payer is found from the request.** A coverage your EHR sent that references its payor
  Organization is resolved against every prefetch value the request carries (a resource, or
  the entries of a Bundle value), whatever its key; your system of record is read for it only
  when the coverage itself came from there.
- **Signed content is never edited.** A value your EHR sent, signed or not, is sent byte
  for byte; added keys sit beside it.

Each key the gateway tried to obtain is recorded: a log line, and, when an observer is
configured, a `prefetch.obtained` event carrying the key, the search it ran, its outcome,
the match and page counts and the time — never the value. Nothing about where a value came
from is added to the request.

**Member id limitation.** `context.patientId` must be the patient's network member id, and
the request's own patient references (the order's subject, the coverage's beneficiary) must
use it. Values from your system of record name the patient by your server's own Patient id,
so the gateway obtains values only when that id is the member id. When your server (as your
connector reports it) names the patient differently:

- a request that leaves out `patient` or `coverage` is refused before anything is read or
  sent: `422 system of record names the patient differently from context.patientId; supply
  patient and coverage prefetch in the request`;
- a history key the request leaves out is left out; nothing is searched, and the
  `prefetch.obtained` event records the outcome `not-run` with the reason `patient named
  differently in the system of record`. The request is sent.

Values your EHR sends are not affected. To have the gateway fill in prefetch, identify the
patient on your server by the member id; otherwise send `patient` and `coverage` (and any
history your EHR has) in the request.

**Requests the gateway originates (`ORIGINATION_PROFILE`).** The order such a request carries
is your system of record's order, with its own status — the gateway never changes it. The
`order-select` coverage check (`provider-data`) carries your member's one **draft** order
(`DeviceRequest` or `ServiceRequest` with `status` `draft`, found with the patient search
above): no draft order is `502 no draft order for member in system of record`, several are
`422 several draft orders for member in system of record`, and a connector that cannot search
is `422`. `order-sign` (a signed order going to prior authorization) and `order-dispatch`
carry your member's **active** order as your system holds it. CRD 2.1 and 2.2 accept an active
order on those hooks; the CRD 2.0 profiles for them require `draft`, so a CRD 2.0 payer that
enforces that profile refuses such a request (a known limitation).

A CDS Hooks request the gateway
builds for your own workflow names the patient by the member id too. When your server names
the patient differently, the gateway — as the author of that request — names the patient by
the member id in the records it carries, and changes nothing else: the Patient's `id`, and the
relative Patient reference on each record's patient path (the order's `subject`, the Coverage's
`beneficiary`), in the record and in its contained resources (a contained Patient's local `id`
stays). Every other byte of those records is your server's, and that is checked byte
for byte before the request is sent; other references to the patient (for example a
Coverage's `subscriber`) are left as your server holds them. History values are left out of
such a request, as above.

After the payer's CDS Hooks answer, the gateway's own `$questionnaire-package` request carries
your system of record's Coverage search matches (each record exactly), the order as the payer
returned it, the payer's `coverage-assertion-id` as `context` when the answer gives one, and
**one** questionnaire: the first the payer's answer names, exactly as stated (a `|version`
kept). A payer answer naming several questionnaires for the order has only the first
requested (a known limitation). At DTR 2.2, where `coverage` is 1..1, a system of record
holding several Coverages for the member refuses the request (422).

## Advanced overrides (rarely needed)

Each network endpoint and trust-anchor key URL is resolved from discovery by
default; set the matching variable only to override (e.g. when the gateway runs
inside the SHN-operated network itself): `AUTHZ_URL`, `HUB_URL`, `CONSENT_URL`,
`AUDIT_URL`, `PHG_URL`, `REGISTRAR_URL`, `FHIR_VALIDATE_URL`, `AUTHZ_PUBKEY_URL`,
`HUB_TRANSPORT_KEY_URL`. Explicit env always wins over discovery.

`NPI` is the ordering clinician's NPI for requests a provider gateway originates
(`ORIGINATION_PROFILE`); it defaults to a synthetic placeholder. A CDS Hooks request's `userId`
is your order's `requester` when it names a `Practitioner` or `PractitionerRole`, and
`Practitioner/<NPI>` otherwise. `NPI` also signs attestations that name no clinician and names
the provider of eligibility requests. (A gateway built on the engine with no NPI refuses a
CDS Hooks request whose order names no requester — `422 order names no requester; configure
NPI` — and an attestation with no clinician NPI, unless the order's requester `Practitioner`
carries one, and its eligibility requests name no provider.)

## Native-forward payer mode (`PAYER_DAVINCI_*`)

See [INTEGRATION.md](INTEGRATION.md#native-forward-payer-mode) for what native-forward
mode does and when to use it, and
[Authenticating to your backend](INTEGRATION.md#authenticating-to-your-backend-smart-backend-services)
for how to set up the SMART Backend Services credentials (`private_key_jwt`,
ES384/RS384 — preferred — or `client_secret_post` for servers that only issue
shared secrets).

| Env var | Description |
|---|---|
| `PAYER_DAVINCI_BASE_URL` | Base URL of the payer's own Da Vinci endpoint (e.g. `https://api.payer.example/davinci`). **Required for `ROLE=payer`.** Every Da Vinci leg — CRD, DTR and PAS — is answered there; the gateway has no in-process payer of its own, so a `role=payer` gateway without this refuses to boot with an error naming it. |
| `PAYER_DAVINCI_CDS_BASE_URL` | Base URL for the partner's CDS Hooks (CRD) posts when they are **not** co-located with the FHIR base — e.g. a payer that serves `/cds-services` at the root but FHIR ops under `/fhir`. Empty ⇒ CDS uses `PAYER_DAVINCI_BASE_URL`. |
| `PAYER_DAVINCI_DTR_BASE_URL` | Base URL for the partner's DTR operations (`/Questionnaire/$questionnaire-package`, `/Questionnaire/$next-question`) when they are **not** co-located with the PAS base — e.g. a payer that serves DTR under `/dtr` and PAS under `/pas`. Empty ⇒ DTR uses `PAYER_DAVINCI_BASE_URL`. Requires `PAYER_DAVINCI_BASE_URL`. |
| `PAYER_DAVINCI_PAS_BASE_URL` | Base URL for the partner's PAS operations (`/Claim/$submit` for submit and update) when they are **not** co-located with the DTR base. Empty ⇒ PAS uses `PAYER_DAVINCI_BASE_URL`. Requires `PAYER_DAVINCI_BASE_URL`. |

**Where each operation goes.** For every forward the gateway resolves the URL in this order: the endpoint your partner publishes for that contract line in its `.well-known/davinci-configuration` (the partner's own published rule, honored only when it is same-origin with the base that contract uses), then the per-operation base if you set one, then `PAYER_DAVINCI_BASE_URL`. The per-operation bases exist for partners that split DTR and PAS across bases and publish no `.well-known/davinci-configuration`; where the partner publishes one, its endpoints win and the bases are only the fallback.

| Env var | Description |
|---|---|
| `PAYER_DAVINCI_TOKEN_URL` | SMART Backend Services token endpoint for the partner. Required if the partner requires authentication. |
| `PAYER_DAVINCI_CLIENT_ID` | SMART client id for the partner. Required when `PAYER_DAVINCI_TOKEN_URL` is set. |
| `PAYER_DAVINCI_CLIENT_KEY` | Path to the SMART client's private-key PEM file (the value is a path, not the key text — mount the file into the container). Required for `private_key_jwt` mode (i.e. when `PAYER_DAVINCI_CLIENT_SECRET` is unset). |
| `PAYER_DAVINCI_CLIENT_ALG` | `ES384` or `RS384`. Required for `private_key_jwt` mode (i.e. when `PAYER_DAVINCI_CLIENT_SECRET` is unset). |
| `PAYER_DAVINCI_SCOPE` | Requested scope the gateway asks your token endpoint for. Default `system/*.read` (covers the read-only legs). Must be a scope your authorization server grants this client; it must cover `/Claim/$submit` as well as the read-only legs, since every leg forwards. |
| `PAYER_DAVINCI_CLIENT_KID` | Key id for the client assertion JWK, if the partner requires it. |
| `PAYER_DAVINCI_CLIENT_SECRET` | OAuth2 client secret for the `client_secret_post` `client_credentials` grant — for authorization servers that cannot issue asymmetric credentials. The value is the secret **itself, not a path** (unlike `PAYER_DAVINCI_CLIENT_KEY`). Mutually exclusive with `PAYER_DAVINCI_CLIENT_KEY`/`_ALG`/`_KID`; prefer `private_key_jwt` when your server supports it. |
| `PAYER_DAVINCI_PAS_NATIVE` | **No longer a switch.** PAS submit/update always forward to the payer's `/Claim/$submit` along with every other leg; there is no in-process PAS fallback to select. Setting it `false` logs a notice at boot and changes nothing. |
| `PAYER_DAVINCI_CRD_SERVICE_ID` | Optional: names your CDS service for `order-select` and `order-sign` requests. Empty (the default) ⇒ each request goes to the one service your CDS service listing offers for the request's hook (see [CDS service selection](#cds-service-selection)). The named service must be in your listing and answer the request's hook; any other request is refused. |
| `PAYER_DAVINCI_DISPATCH_SERVICE_ID` | Optional: names your CDS service for `order-dispatch` requests, with the same rules. Empty ⇒ the service your listing offers for `order-dispatch`. |
| `PAYER_DAVINCI_CONTRACT_VERSIONS` | Declared Da Vinci contract versions for the partner payer, comma-separated `<contract>@<line>` tokens (e.g. `pa.pas@2.0, pa.crd@2.0`). Two things read this: the connectivity checks verify the partner's published capability against it (`version-drift` on disagreement, FR-G46), and native-forward routing refuses (before forwarding) any leg whose contract shares no line with it (FR-G48). Requires `PAYER_DAVINCI_BASE_URL`. Unset ⇒ native-forward legs are unfiltered (today's default) and the checks skip the drift comparison. |
| `PAYER_DAVINCI_STRICT_EXTENSIONS` | `true` to reserve the per-peer gated overlay (FR-G52) for this partner — a peer flagged this way would refuse a cross-version transform chain carrying or dropping its extensions instead of forwarding stripped or lossy content. **Currently DORMANT: setting it has no routing effect on this deployment.** The strict *consult* itself is already live where transforms are actually selected (route-layer chain selection, exercised by test-only seams), but the one peer this flag targets — the foreign Da Vinci partner reached through native-forward mode — is filtered through arm-1-only forwarding this slice (never through the chain-selection path), so the flag has nothing to gate yet. It goes live together with transform-at-the-native-forward-edge (not yet shipped; re-labeling another gateway's build product as a translated payload needs its own stamp/Provenance semantics worked out first). Default `false`. |

**Removed settings.** `PAYER_DAVINCI_CRD_HOOK`, `PAYER_DAVINCI_DISPATCH_HOOK` and
`PAYER_DAVINCI_CRD_COVERAGE_BUNDLE` no longer exist. The request's hook is never changed, and
the CDS Hooks request reaches your system as the provider's system sent it (a `coverage`
prefetch template that is a search is answered with a searchset `Bundle`, by the provider's
system or by its gateway). A gateway that still sets one of them refuses to start, naming it.

### CDS service selection

Your gateway reads your CDS service listing (`GET {PAYER_DAVINCI_CDS_BASE_URL}/cds-services`)
and sends each CDS Hooks request to the service whose `hook` is the request's hook. It never
changes the hook. The listing is read at startup (to log which hook each configured service
answers; an unreadable listing is a warning, not a startup failure) and again when the last
reading is more than five minutes old. One read runs at a time; requests arriving meanwhile use
the listing already read, or wait for the read when there is none. When the listing cannot be
read again, requests keep using the last listing read for up to one hour after it was read (the
gateway logs each failed read); a failed read is not repeated for five seconds. Nothing is sent to
your system when:

- your listing offers no service for the request's hook — 422
  `{"error":"payer offers no CDS service for hook order-select","offered":["order-sign","order-dispatch"]}`,
  naming the hooks you do offer; a configured service that is not in your listing is refused
  the same way (`payer offers no CDS service <id>`);
- a configured service is listed for another hook — 422 `payer CDS service <id> is for hook
  <hook>, not <request hook>`, with `"offered"` naming the service's hook;
- your listing offers several services for the hook and none is configured — 422
  `payer offers several CDS services for hook <hook>`;
- your listing cannot be read and no listing read in the last hour is held — 502
  `payer CDS service listing unavailable`;
- the request names no hook, or a hook its leg does not carry (`order-select` and
  `order-sign` travel on one leg, `order-dispatch` on another) — 400.

### CDS Hooks answers

Your CDS Hooks answer is relayed to the provider exactly as your system sent it once it meets the CDS Hooks 2.0 response rules and, at a CRD line, the CRD card
rules (for example: `cards` is an array and may be empty; every card has a `summary` under 140
characters, an `indicator`, and a `source` with a `label` and, at a CRD line, a `topic`; every
action has a `type` and a `description`). An answer that breaks one is refused with 502
`payer CRD response is not a valid CDS Hooks response: <rule> at <path>` rather than repaired,
and the provider's gateway applies the same rules. Your gateway carries the answer with the
media type your system sent (`application/json` when it sent none); the provider's gateway
returns it to the EHR as `application/json`, the CDS Hooks media type. Coverage information belongs in a system
action that updates the order; each FHIR resource your answer embeds is also validated at the
routed CRD line, but only to record the outcome (the `crd.embedded.validated` observation):
it never changes or refuses your answer. A non-2xx answer is relayed as your system's error.

**Exactly-one-mode rule:** if `PAYER_DAVINCI_TOKEN_URL` is set, then
`PAYER_DAVINCI_CLIENT_ID` must also be set, plus exactly one credential mode —
`PAYER_DAVINCI_CLIENT_KEY` + `PAYER_DAVINCI_CLIENT_ALG` (`private_key_jwt`,
preferred) or `PAYER_DAVINCI_CLIENT_SECRET` (`client_secret_post`). A partial or
mixed credential block is a hard startup error (a likely misconfig). Setting
`PAYER_DAVINCI_BASE_URL` alone (no token URL) is valid and forwards to the
partner **unauthenticated** — the gateway logs a warning on startup to make this
mode visible.

### Payer backend identity mapping

A common shape for a payer operator: the network-facing identity your gateway is
registered under is not the same identifier your own backend (a utilization-management
or CRD engine you run or buy off the shelf) knows itself by. Some engines select their
coverage-determination logic by the payer identifier carried on the inbound `Coverage`
resource, and will reject a request whose identifier they don't recognize — even though
it is genuinely theirs to answer, just addressed under your network identity instead of
your engine's own.

| Env var | Description |
|---|---|
| `PAYER_DAVINCI_PAYOR_OWN` | `"system\|value"` of this deployment's registered payer identity — the identity your gateway is known by on the network. |
| `PAYER_DAVINCI_PAYOR_BACKEND` | `"system\|value"` your own backend expects instead — the identity to re-stamp onto outbound requests before they reach it. |

When both are set, every CDS Hooks (`order-select`, `order-sign` and `order-dispatch`), DTR,
and PAS request this gateway forwards to your system has the payor identifier of **every**
`Coverage` it carries re-stamped from `PAYER_DAVINCI_PAYOR_OWN` to
`PAYER_DAVINCI_PAYOR_BACKEND` — but **only** when the inbound identifier is genuinely your
own. The identifier is the `Coverage.payor` identifier itself, or the first identifier
with a system and value on the payer `Organization` it references (contained in the
Coverage, or another resource of the same Bundle or prefetch). Each Coverage is read by its
first `payor`, the one the network routes on. A PAS Claim's `insurer` is resolved the same
way and re-stamped too when it names your identity; an insurer that names you under
another identifier (for example your NPI) is left as sent.

A request is refused with a clear error, never silently forwarded, when:

- a Coverage names a different payer identifier, or no resolvable payor identifier at all
  (400) — this is an ownership check, not a blind rewrite: forwarding a misdirected
  request under your own backend's identity would have your engine adjudicate someone
  else's request;
- its Coverages name more than one payer, or a Coverage payor or Claim insurer reference
  resolves to no resource or to more than one (422, naming the reference). References are
  matched exactly (a `fullUrl`, `Type/id`, or `#id` for a contained resource); a
  version-specific (`_history`) reference is not resolved and is refused;
- the identifier to re-stamp is covered by a signature inside the message (a
  `Bundle.signature`, a `Provenance` signature, or a `Signature` element) — 422
  `signed content cannot be edited (E-03, <signature>)`. The gateway never strips a
  signature and never forwards a stale one.

Only the identifier's `system` and `value` strings are changed. For a PAS Bundle and a CDS
Hooks request, every other byte of the request — the payer organization's name, entry
members, layout, numbers — is forwarded exactly as it arrived, and so is a DTR
`$questionnaire-package` or `$next-question` input a requester sends naming the operation.
(A questionnaire request a requester sends in the older envelope, without naming the
operation, is still assembled by the gateway from the carried canonical, order and Coverage.) When your backend identity equals your network identity nothing is
changed at all. The mapping never touches your answers: CDS Hooks and questionnaire answers
are relayed back exactly as your system sent them.

A known limitation of the mapping: a questionnaire request can carry a Coverage that names its
payer only by a reference to an Organization the request does not carry — the Coverage a
provider's gateway adds from its system of record when the EHR sent none, and the
questionnaire requests a provider's gateway builds for its own workflow, carry the Coverage
alone. With the mapping set, such a request is refused (422, the payor reference resolves to
no resource), because the identity cannot be read from the request. Requests that carry the
payor identifier on the Coverage, or the Organization beside it, are mapped as described. A PAS submission your system pends is
currently followed up by the gateway, which returns an answer assembled from your system's
later decision instead of the pended response.

Both env vars are **all-or-nothing**: setting one without the other is a startup error.
Leaving both unset (the default) disables this mapping entirely — requests forward with
their payor identifier untouched, the behavior for every deployment that doesn't need it.

## Provider DTR population (`PROVIDER_DTR_*`)

See [INTEGRATION.md](INTEGRATION.md#provider-dtr-population) for the managed-vs-native
tradeoff.

| Env var | Description |
|---|---|
| `PROVIDER_DTR_NATIVE` | `true` to forward DTR population to an SDC `$populate` engine instead of the managed populator. Default `false`. |
| `PROVIDER_DTR_POPULATE_URL` | The SDC `Questionnaire/$populate` endpoint. Required when `PROVIDER_DTR_NATIVE=true`, and for `ORIGINATION_PROFILE=demo` / `provider-data` (the operated engine those lanes require). |
| `PROVIDER_DTR_POPULATE_TOKEN_URL` | SMART Backend Services token endpoint the gateway authenticates to the `$populate` engine with. Sets the connector's **own** credential block — the engine may be your SoR under the same registration, or a separate server with its own; either way the connector mints its own bearer and never reuses the `FHIR_*` client's. Requires `PROVIDER_DTR_POPULATE_URL`. |
| `PROVIDER_DTR_POPULATE_CLIENT_ID` | Client id the token endpoint knows the connector by. Required with `PROVIDER_DTR_POPULATE_TOKEN_URL`. |
| `PROVIDER_DTR_POPULATE_CLIENT_KEY` | Path to the PEM private key for `private_key_jwt`. |
| `PROVIDER_DTR_POPULATE_CLIENT_ALG` | `ES384` or `RS384`; must match the key. |
| `PROVIDER_DTR_POPULATE_CLIENT_KID` | Optional `kid` header for the client assertion. |
| `PROVIDER_DTR_POPULATE_CLIENT_SECRET` | Client secret for `client_secret_post` (the value, not a path). |
| `PROVIDER_DTR_POPULATE_SCOPE` | Scope requested from the token endpoint. Default `system/*.read`. |

**Exactly-one-mode rule:** if `PROVIDER_DTR_POPULATE_TOKEN_URL` is set, then
`PROVIDER_DTR_POPULATE_URL` and `PROVIDER_DTR_POPULATE_CLIENT_ID` must also be set,
plus exactly one credential mode — `PROVIDER_DTR_POPULATE_CLIENT_KEY` +
`PROVIDER_DTR_POPULATE_CLIENT_ALG` (`private_key_jwt`, preferred) or
`PROVIDER_DTR_POPULATE_CLIENT_SECRET` (`client_secret_post`). A partial or mixed
credential block is a hard startup error (a likely misconfig). Setting
`PROVIDER_DTR_POPULATE_URL` alone (no token URL) is valid and posts to the engine
**unauthenticated** — the gateway logs a warning on startup to make this mode
visible. A token refusal fails the DTR leg closed (the request that needed the
population answers `502`); the gateway never retries without credentials. The
token endpoint also joins `/internal/checks` as a credential check, so a rotated
or revoked credential is reported there (`credential-rejected`, with the HTTP
status) rather than discovered on the next prior authorization.

## Sealed message frames (v1)

When your gateway is the **recipient** of an exchange (it answers a request routed
through the Hub — the payer/responder side), an application-level failure from the far
end — e.g. the payer's real `502` + `OperationOutcome`, or a `422` amendment rejection —
used to be collapsed into a generic `502 {"error":"hub routing failed"}` at the
requester's edge, because a non-`2xx` answer to the Hub was treated as a routing failure.

As of v0.28.0, a capable pair of gateways instead exchanges a **sealed message
frame**: the responder's real answer — its actual status, an allowlisted `Content-Type`
header, and its body, success or not — travels *inside* the sealed response leg and is
surfaced to the requester **verbatim**. The Hub stays payload-blind throughout: it still
only ever sees an opaque ciphertext and records the leg as `answered` over its hash, never
the status or body inside it. A true **transport fault** (the far end is unreachable, or
the gateway's own build/dial/read fails) is not an application answer and still surfaces
as `"hub routing failed"` — only a response the far end actually produced is relayed.

**Negotiation, not configuration.** There is no environment variable to set. Whether an
exchange frames is decided per pair of holders from what each side has advertised to the
registry: every gateway (and SDK-based participant) on a codec-capable build
self-declares message-frame support automatically the moment it registers or rotates its
credentials — no app-level opt-in. A response frames only when **both** the requester and
the responder have advertised support; if either side is still on an older, pre-frame
build, the exchange falls back byte-for-byte to the legacy contract: a bare payload on
success (implicit `200`), and a non-`2xx` application answer collapsing to the Hub's
generic `"hub routing failed"` on failure, exactly as before. Upgrading one gateway in a
mesh is always safe — older peers simply keep the legacy contract until they, too,
re-register from an upgraded build.

**The `RESPONDER_RELAY_ERRORS` environment variable no longer exists.** It gated an
interim, JSON-wrapper-based version of this same idea shipped in v0.27.0; that wrapper,
its flag, and the response sniff it relied on have all been removed and replaced by the
negotiated message frame described above. Deployments that still set
`RESPONDER_RELAY_ERRORS` can drop the variable — it is inert.

**Request frames are a separate, independently negotiated capability.** As of v0.38.0 the
same v1 codec also frames the **request** leg of a version-mapped exchange, carrying the
contract line the originator built the request at. It negotiates off its own registry
capability (`requestFrames`), not the response direction's — so the two roll out
independently — and, like message frames, there is **nothing to configure**: a
codec-capable build self-declares it at registration/rotation. A peer that has not
declared it keeps receiving byte-identical bare requests. A gateway that *has* declared it
accepts **both** framed and bare inbound requests; declaring the capability commits only
to being able to decode a frame, never to requiring one. `coverage-eligibility` is
version-neutral and is never framed.

## Exchange contract lines (`SHN_CONTRACT_VERSIONS`)

The gateway BUILDS every prior-authorization contract at three Da Vinci generations —
CRD/DTR/PAS at `2.0.x`, `2.1.x`, and `2.2.x` (plus PDex `2.1.x`). That is its **native**
capability. What it **declares** to the network is a separate, operator-chosen subset, and
the declared set is the starting point for routing; qualified native lanes also
support the native-reach and inbound-honor rules below.

| Env var | Description |
|---|---|
| `SHN_CONTRACT_VERSIONS` | This gateway's own **declared** exchange-contract versions: comma-separated `<contract>@<line>` tokens, e.g. `pa.crd@2.2, pa.dtr@2.2, pa.pas@2.2`. Drives leg selection, the published `CapabilityStatement`s and `.well-known/davinci-configuration`, and the declaration peers route against. Must be a **subset of the native set** (`pa.crd@{2.0,2.1,2.2}`, `pa.dtr@{2.0,2.1,2.2}`, `pa.pas@{2.0,2.1,2.2}`, `pa.pdex@2.1`) — a token outside it is a boot error, not a routing outcome. Unset ⇒ the build default, the canonical `2.0` line (`pa.crd@2.0`, `pa.dtr@2.0`, `pa.pas@2.0`, `pa.pdex@2.1`). |
| `FHIR_VALIDATE_URL_2_1` | Optional **2.1** `$validate` address override. Compose default: `http://shn-validator-2-1:8080/fhir`. |
| `FHIR_VALIDATE_URL_2_2` | Optional **2.2** `$validate` address override. Compose default: `http://shn-validator-2-2:8080/fhir`. |

**One validator per line — this is not optional.** A FHIR server loads exactly **one**
version of a given IG package, so a single HAPI cannot host CRD 2.0.1 and CRD 2.2.1 at the
same time; a 2.2 payload validated against a 2.0-loaded server is not validated, it is
mis-validated. Each declared non-canonical line therefore needs its own `$validate` lane.
`FHIR_VALIDATE_URL` (the base variable, above) remains the canonical `2.0` lane.
The gateway resolves explicit per-line override first, then the existing canonical
endpoint, then the Compose default. Kit child ports and hosted service addresses
continue to come from their existing launcher wiring; they need not use Compose DNS.
Malformed override URLs refuse startup. Explicit endpoints keep their existing
startup behavior; setting an override does not trigger synthetic qualification.

A newly defaulted declared CRD, DTR or PAS line must pass the complete finite
synthetic qualification before the gateway serves traffic, even when it is the
only declared line. Metadata availability only permits that corpus to begin;
a metadata 200 alone is insufficient. Failure returns a boot error naming the
line, endpoint and qualification reason. The total startup bound is 600 seconds.
The image's `/healthcheck` is an executable observing a qualification marker,
not an HTTP readiness endpoint.
The supplied validator image withholds public metadata and validation until its own
finite worker succeeds, then each gateway performs the corpus above. Its external
`8080/fhir` address stays the same; no extra readiness flag or operator sequence is
required. A terminal image qualification failure remains unavailable until restart
and is reported by the image's `/healthcheck`.

Undeclared defaults qualify in the background without delaying configured `2.0`
startup. Until qualification succeeds they cannot satisfy routing or inbound
frame admission. Each default receives one finite attempt per gateway lifecycle;
a complete failed attempt is terminal until restart. Shutdown cancels and joins
workers. Embedders using `Handler` or `HandlerWithClock` must close the returned
`io.Closer` after stopping HTTP service; tests can use `HandlerForTest` or
`HandlerForTestWithClock` and invoke their cleanup before releasing dependencies.
Ordinary validation verdicts do not change synthetic readiness.

PDex has one native line and retains its canonical/configured compatibility.
That canonical alias cannot satisfy a CRD, DTR or PAS `2.1` request. A qualified
`2.1` default can serve those contracts while PDex still uses its canonical endpoint.

### Opting a line in

1. **Stand up the line's validator** — an IG-loaded `$validate` for that line's package
   set. The shipped sidecar image builds per line:
   `docker build --build-arg SHN_IG_LINE=2.2 deploy/validator/`. Use its default
   Compose address, or point `FHIR_VALIDATE_URL_2_2` at a different address.
2. **Widen `SHN_CONTRACT_VERSIONS`** to include the new tokens *alongside* the ones you
   already declare (see the grow-only rule below), and restart the gateway. A default
   endpoint must complete synthetic qualification before boot succeeds. An explicit
   well-formed URL retains URL-only startup and can boot while unreachable; verify
   that endpoint operationally before advertising its line.
3. **Re-register or rotate** (`shn rotate`) so the new declaration reaches the registrar.
   Declaration tracks the current build/config, and it is published at
   registration/rotation — not continuously.
4. **Peers converge on their next registry poll.** Until they do, they are still selecting
   against your previous declaration.

**The mismatch window is benign, by design.** Between your rotation and a peer's next poll,
that peer routes legs at your *old* line. Those legs still complete: a gateway **honors**
an inbound request's declared line whenever it can both natively build and validate at
it — a wider predicate than its own declared set — so in-flight and stale-routed legs are
answered correctly rather than refused. Configured explicit lanes and successfully
qualified default lanes can cover undeclared native lines, widening this honor window.
An unavailable default never grants admission, and the PDex canonical compatibility
alias cannot satisfy a CRD, DTR or PAS request at the same numeric line.

**Declared-set changes must grow, never swap or shrink.** Adding a line is safe in either
rollout order. **Removing** one is a breaking operation: a pended prior-authorization pins
its contract line at origination and must resume on that exact line, so dropping a line can
strand pends that can no longer be resumed. If you must remove a line, drain outstanding
pends first — treat it as a migration, not a config edit. The same rule holds for
*swapping* (`2.0` → `2.2` in one step): that is a shrink and a grow at once, and the shrink
half strands pends.

### Configuring a validator lane without declaring the line (opt-in, cross-version translation)

You can point `FHIR_VALIDATE_URL_2_1`/`FHIR_VALIDATE_URL_2_2` at a line's validator **without**
adding that line's tokens to `SHN_CONTRACT_VERSIONS`. A configured-but-undeclared lane, for any
line this build natively speaks, enters the lane map. A default lane enters after
qualification. Both make the line available in these two directions:

1. **Egress:** a peer that declares that line becomes reachable by **native reach** (routing
   arm 2 — this build constructs a genuine native payload at that line, zero transform loss)
   even though this deployment never advertises the line itself. Without an explicit or qualified default lane,
   the same peer would only be reachable, if at all, through a transform chain (arm 3) or a
   legible refusal.
2. **Inbound:** the SAME lane map backs what this gateway **honors** on an inbound request-frame
   `contractVersion` claim (`docs/PARTICIPANT_PROTOCOL.md` §8.6's *native ∩ laned* rule, wider
   than the declared set by design) — so configuring an undeclared lane widens what you'll
   silently accept from a stale-routed peer too, not only what you can build for one. This is
   the bidirectional lane-admission rule, read by both routing directions.

Lanes obey the same grow-only discipline as declared lines, for the identical pend-stranding
reason: a resumed pended exchange needs its pinned line's lane to remain configured for as long
as the pend can still be amended. Do not remove a `FHIR_VALIDATE_URL_<line>` env var while any
pend may still be pinned to that line, declared or not. A single-line contract (`pa.pdex`) has
nothing to opt into — it has only one native line and always rides the canonical lane.

### CLOSED — DTR at line 2.2 on the built-in payer responder

Previously recorded here: the in-process payer responder that used to ship with this repo
(since removed — every `role=payer` deployment now forwards to a real Da Vinci payer
endpoint, `PAYER_DAVINCI_BASE_URL`) could not answer `dtr-questionnaire-fetch` at line 2.2.
DTR 2.2's `DTR-QPackageBundle` profile requires a `QuestionnaireResponse` entry in the
returned package, and its `QuestionnaireResponse` profile in turn requires a Coverage
reference — but that old responder's DTR request carried only the questionnaire canonical,
and it had no way to resolve a member from it. It held no honest source for either value,
and fabricating clinical or coverage attribution on the payer side is exactly what
per-message validation exists to prevent. A deployment that declared `pa.dtr@2.2` while
running that old in-process responder therefore failed that leg closed
(`TestDTRAt22_UnansweredGap`, now deleted).

This is now closed **honestly**, on both sides of the wire:

- **Request side:** DTR's `$questionnaire-package` input profile makes `coverage` **1..1**
  at every line (min=1 everywhere; 2.2 additionally tightens `max` from `*` to `1` —
  verified live against the pinned 2.2.0 package). `DTRDef.QuestionnairePackageCoverageRequired`
  is `true` only at 2.2. The questionnaire requests the gateway originates are built with
  the SDK's `BuildQuestionnairePackageParameters` at the selected line: they always carry
  the Coverage records your system of record returns for the patient (at every line), and
  at 2.2 a system of record holding more than one Coverage for the patient is refused
  **before the wire** (422) — a legible local error naming the line and the cardinality,
  replacing what would otherwise be a real partner's opaque 400.
- **Responder side:** the payer's answer is its OWN Da Vinci endpoint's
  `$questionnaire-package`, relayed verbatim — the gateway builds no package itself. (It
  used to, for the in-process payer that has since been removed; a 2.2 package's mandatory
  `QuestionnaireResponse` entry is now the payer's to produce.) The requester's Coverage is
  carried on the questionnaire request at every line, which is what lets a conformant payer
  derive that entry's subject and coverage reference from a real resource instead of inventing them.
  Whatever shell comes back is discarded by the consumer — `extractQuestionnaireFromPackage`
  only ever reads the bare `Questionnaire` entry, and the auto-filled/authored
  `QuestionnaireResponse` that crosses into the PAS submission is built separately.

Verified live against the pinned DTR 2.2.0 package (`shn-hapi-validate-ig22`, 2026-08-12):
a hand-built shell of this exact shape validates against `dtr-questionnaireresponse|2.2.0`
with zero error-severity issues. `pa.dtr@2.2` is now included in the whole-line 2.2 mesh
(`test/conformance/per_line_uc_test.go`'s `perLineMeshes`) alongside `pa.crd@2.2` and
`pa.pas@2.2`.

A **separate, pre-existing, unrelated** gap surfaced during this verification and is
recorded here rather than fixed (out of scope for that fix — it concerns the Questionnaire a
worked-example payer responder *asks*, not the QuestionnaireResponse it *answers with*): the
worked-example lumbar Questionnaire (content unchanged; the SDK's public `DemoLumbarQuestionnaire`
export was retired — register open-items §8, 2026-08-25 — and this fixture now lives in
`internal/dtr`/`gateway/fhirseed`) does not itself conform to DTR 2.2's
`dtr-base-questionnaire` profile (`Questionnaire.subjectType` min=1 unmet, an unmet
`sdc-2` versionAlgorithm constraint, and item-extension slices this build's pinned package
set cannot resolve). This was already true of the existing `testdata/golden/2.2/
questionnaire-package-pa-lumbar-mri.json` golden before this task and is not newly
introduced by it; `make validate`'s current gate does not catch it (that golden is not in
`pinnedProfiles`, so it validates against base R4 only, never against
`DTR-QPackageBundle`). Tracked as a follow-up, not a blocker for this closure.

### CLOSED — authored DTR answers now build at the selected line

Previously recorded here: the provider-side paths that build a `QuestionnaireResponse`
from **authored** answers — clinician manual entry, attestation, and the pended-resume
amendment — and the managed populator's auto-fill were **not line-parameterized**. They
emitted the 2.0-line shape regardless of the line the leg routed to, and the mismatch was
not uniformly a loud failure: the two-RI evidence against the real 2.2 reference
implementation found the attestation-bearing scenarios failed closed with a 422
egress-validation error, while the pure auto-fill scenario's wrong-line QR bytes went out
and were accepted — a **silent** wrong-line pass (the UC-03 gap).

This build threads the leg-selected DTR line (`crdDtrResult.dtrLine`, resolved once at the
`dtr-questionnaire-fetch` select-before-build site) through all three build sites: the
managed populator's auto-fill (`managedPopulator.Populate` → `shnsdk.FillQuestionnaireAtLine`),
and both authored-answer refills, `handleUC04`'s attestation and `scenarioToPend`'s UC-06
attestation (both → `shnsdk.FillQuestionnaireFromAnswersAtLine`) — and their paired
egress-`$validate` calls now check against that same line, not the canonical (2.0) lane. A
deployment declaring only `2.0` is unaffected (byte-identical output, regression-fenced).
Auto-fill through a **native** SDC `$populate` engine (`PROVIDER_DTR_NATIVE=true`) was
never affected — the engine, not the gateway, shapes that response.

Verified by `TestManagedPopulatorBuildsAtLine` (`gateway/engine/populator_test.go`) at the
populator seam, and `TestAuthoredQRBuiltAtSelectedLine`
(`gateway/engine/authoredqr_line_test.go`) end-to-end against the actual submitted PAS
bundle's embedded `QuestionnaireResponse` for both attestation sites — both assert the DTR
2.2 wire markers (the only line with an observable delta from 2.0/2.1: the `qr-coverage`
extension and the `intendedUse` code-system rename), since 2.0 and 2.1 are byte-identical
on every marker this build can check.

## Demo-only egress narrowing (`SHN_DEMO_EGRESS_NATIVE_LINES`)

| Env var | Description |
|---|---|
| `SHN_DEMO_EGRESS_NATIVE_LINES` | Comma-separated contract lines (e.g. `2.0`) that narrow **this gateway's own view of which lines it can reach natively on egress** (arm 2 — see [Configuring a validator lane without declaring the line](#configuring-a-validator-lane-without-declaring-the-line-opt-in-cross-version-translation) above for the arm numbering). Empty (unset — the default) means the full native set, unrestricted. Every token must be a line this build natively speaks; an unknown token is a boot error, and so is a set-but-lineless value (e.g. `","`) — the knob never degrades to a silent no-op. |

**Narrows arm-2 routing only — arm 1 is unaffected.** This knob changes which lines a
narrowed gateway can reach through **native-reach selection** (arm 2: building a leg at a
line it natively speaks against a peer's declared set) — it has no effect on **arm 1**
(a leg answered at a line both peers already share/declare): an arm-1 leg routes exactly
as it would with the knob unset. Narrowing arm 2 makes a peer that only declares a newer
line unreachable there, so a transform chain (arm 3) fires instead, or the leg refuses if
no chain resolves — which is the whole point of the knob: simulating "a build that
predates the newer contract lines" without running an older build.

**Narrowing gates routing, not authoring.** A narrowed gateway still *builds* fully-shaped
content at any line it natively speaks whenever that content rides an unaffected leg — for
example, a DTR questionnaire package authored at 2.2 that then rides an arm-1 PAS submit.
The knob changes what a gateway **routes as**, never what it can **author**.

**Never set this in a shipped deployment config.** It exists for exactly one consumer: the
**SHN Kit's bridging demo** (see `kit/README.md`), which restarts its own supervised
gateway child with it set to demonstrate cross-version bridging against skewed peers. It is
loud wherever it's set — in the boot log:

```
gateway: demo: egress-native lines narrowed to [2.0] — arm-2 native reach restricted;
transform chains may fire (SHN_DEMO_EGRESS_NATIVE_LINES)
```

— and in the name itself: a `SHN_DEMO_*` variable set anywhere but a demo is a
misconfiguration, not a supported deployment posture.

## Demo-only pre-seal edge capture (`SHN_DEMO_EDGE_CAPTURE`)

| Env var | Description |
|---|---|
| `SHN_DEMO_EDGE_CAPTURE` | `"true"` turns on a bounded, in-memory store of each transformed egress leg's own pre-seal before/after payload pair, readable back at `GET /demo/capture/{correlationId}` on the observer loopback listener. **Requires `OBSERVER_ADDR` to also be set** — that endpoint is the only way a capture is ever read back, so with `OBSERVER_ADDR` unset the flag is gated off at config load (a quiet downgrade, not a boot refusal — the boot log names the reason). Any other value, or unset (the default), leaves it off: no store is built, the capture hook never runs, and the endpoint answers `404`. Bounded to the 32 most recently transformed legs; an entry whose combined before/after payload exceeds 2 MiB is not stored. |

This is a local inspection surface over a gateway's own outgoing traffic, never a second
wire path: the capture happens at the same internal point the leg's payload is already
being built for sending, after loss reporting but before the leg leaves — nothing about
what actually goes out changes, on or off. With the knob off (the production default) no
store is ever allocated and the capture site is skipped entirely, so the leg's bytes are
byte-identical to a run with the knob on. Captured pairs are never written to the wire,
never added to the audit record, and never checked by any conformance surface — they exist
only to be read back by the participant that produced them, from their own loopback
listener.

**Never set this in a shipped deployment config.** Like `SHN_DEMO_EGRESS_NATIVE_LINES`
above, it exists for exactly one consumer: the SHN Kit's bridging demonstration, which
turns this on alongside the egress-narrowing knob (and sets `OBSERVER_ADDR`) so a
transformed leg's own edge capture is available for the demonstration's before/after view.
It is loud wherever it's set — in the boot log, when `OBSERVER_ADDR` is also set:

```
gateway: demo: edge capture enabled — bounded in-memory inspection of this gateway's own
pre-seal egress payloads (SHN_DEMO_EDGE_CAPTURE)
```

— or, when `OBSERVER_ADDR` is NOT set (the flag is gated off rather than silently running
into a store nothing can read):

```
gateway: demo: edge capture requested but OBSERVER_ADDR is unset — capture disabled
(nothing could read it) (SHN_DEMO_EDGE_CAPTURE)
```

— and in the name itself: a `SHN_DEMO_*` variable set anywhere but a demo is a
misconfiguration, not a supported deployment posture.
