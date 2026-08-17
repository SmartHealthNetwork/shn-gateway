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
at `POST /substrate/inbound` and reply to whoever the Hub delivers from. A payer
adjudicates with a built-in default decision policy out of the box — plug in your
own via [INTEGRATION.md](INTEGRATION.md#payer-decisioning).

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
FHIR `$metadata` fetch; `FHIR_TOKEN_URL` and `PAYER_DAVINCI_TOKEN_URL` are
checked with a live credential fetch against your configured client; every
other endpoint URL you've set — including the [advanced
overrides](#advanced-overrides-rarely-needed) — is checked with a plain
reachability request. `PAYER_DIRECTORY` is a local file path, not a network
endpoint, and is never probed.

## Connect your system of record

See [INTEGRATION.md](INTEGRATION.md) for how these fit together.

| Env var | Description |
|---|---|
| `ORIGINATION_PROFILE` | provider. Set to `provider-data` to originate every prior-auth UC off your seeded FHIR system of record and drive real payer verdicts — the config-only provider lane, no custom code. When set to `provider-data`, `PROVIDER_DTR_POPULATE_URL` is required (validated at boot). |
| `FHIR_DATA_URL` | FHIR R4 base URL for your system of record. **Omit to use the built-in synthetic stub** (seeded with example personas, so you can run end to end with no backend). |
| `FHIR_TOKEN_URL` | SMART Backend Services token endpoint, if your FHIR server requires authenticated access. Requires the client credential block below. |
| `FHIR_CLIENT_ID` | SMART client id. |
| `FHIR_CLIENT_KEY` | Path to the SMART client's private-key PEM file (the value is a path, not the key text — mount the file into the container). Required for `private_key_jwt` mode (i.e. when `FHIR_CLIENT_SECRET` is unset). |
| `FHIR_CLIENT_ALG` | `ES384` or `RS384`. Required for `private_key_jwt` mode (i.e. when `FHIR_CLIENT_SECRET` is unset). |
| `FHIR_CLIENT_SCOPE` | Requested scope. Default `system/*.read` — must be a scope your server grants this client. |
| `FHIR_CLIENT_KID` | Key id for the client assertion JWK, if your server requires it. |
| `FHIR_CLIENT_SECRET` | OAuth2 client secret for the `client_secret_post` `client_credentials` grant — for authorization servers that cannot issue asymmetric credentials. The value is the secret **itself, not a path** (unlike `FHIR_CLIENT_KEY`). Mutually exclusive with `FHIR_CLIENT_KEY`/`_ALG`/`_KID`; prefer `private_key_jwt` when your server supports it. |
| `SHN_STORE_DATABASE_URL` | Postgres DSN for durable claim-state storage. Omit for in-memory (non-durable across restarts). |

## Accept Da Vinci requests from a provider EHR (provider, optional)

See [INTEGRATION.md](INTEGRATION.md#native-da-vinci-ingress) for when to use this
instead of `provider-data` origination.

Set `PROVIDER_DAVINCI_INGRESS=1` to mount the provider-side Da Vinci ingress: the
gateway accepts a provider EHR / reference-implementation's **native Da Vinci
requests** — CDS Hooks order-select (Coverage Requirements Discovery),
`Questionnaire/$questionnaire-package` (DTR), and `Claim/$submit` (PAS) — resolves
and inlines the payer's prefetch from **your own system of record** (non-aggregating;
no callback to the provider), and forwards conformant requests through to the Hub.

Inbound requests authenticate via **SMART Backend Services**: the gateway hosts its
own `POST /oauth/token` and `GET /.well-known/smart-configuration`, verifies a
registered client's signed JWT assertion (`private_key_jwt`, ES384/RS384), issues a
short-lived bearer, and verifies it on every ingress call.

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

## Advanced overrides (rarely needed)

Each network endpoint and trust-anchor key URL is resolved from discovery by
default; set the matching variable only to override (e.g. when the gateway runs
inside the SHN-operated network itself): `AUTHZ_URL`, `HUB_URL`, `CONSENT_URL`,
`AUDIT_URL`, `PHG_URL`, `REGISTRAR_URL`, `FHIR_VALIDATE_URL`, `AUTHZ_PUBKEY_URL`,
`HUB_TRANSPORT_KEY_URL`. Explicit env always wins over discovery. `NPI` overrides
the organization NPI stamped into a provider's originated requests (defaults to a
synthetic placeholder).

## Native-forward payer mode (`PAYER_DAVINCI_*`)

See [INTEGRATION.md](INTEGRATION.md#native-forward-payer-mode) for what native-forward
mode does and when to use it, and
[Authenticating to your backend](INTEGRATION.md#authenticating-to-your-backend-smart-backend-services)
for how to set up the SMART Backend Services credentials (`private_key_jwt`,
ES384/RS384 — preferred — or `client_secret_post` for servers that only issue
shared secrets).

| Env var | Description |
|---|---|
| `PAYER_DAVINCI_BASE_URL` | Base URL of the partner Da Vinci payer (e.g. `https://api.payer.example/davinci`). Setting this enables native-forward mode. |
| `PAYER_DAVINCI_CDS_BASE_URL` | Base URL for the partner's CDS Hooks (CRD) posts when they are **not** co-located with the FHIR base — e.g. a payer that serves `/cds-services` at the root but FHIR ops under `/fhir`. Empty ⇒ CDS uses `PAYER_DAVINCI_BASE_URL`. |
| `PAYER_DAVINCI_TOKEN_URL` | SMART Backend Services token endpoint for the partner. Required if the partner requires authentication. |
| `PAYER_DAVINCI_CLIENT_ID` | SMART client id for the partner. Required when `PAYER_DAVINCI_TOKEN_URL` is set. |
| `PAYER_DAVINCI_CLIENT_KEY` | Path to the SMART client's private-key PEM file (the value is a path, not the key text — mount the file into the container). Required for `private_key_jwt` mode (i.e. when `PAYER_DAVINCI_CLIENT_SECRET` is unset). |
| `PAYER_DAVINCI_CLIENT_ALG` | `ES384` or `RS384`. Required for `private_key_jwt` mode (i.e. when `PAYER_DAVINCI_CLIENT_SECRET` is unset). |
| `PAYER_DAVINCI_SCOPE` | Requested scope the gateway asks your token endpoint for. Default `system/*.read` (covers the read-only legs). Must be a scope your authorization server grants this client; widen it if you enable `PAYER_DAVINCI_PAS_NATIVE`. |
| `PAYER_DAVINCI_CLIENT_KID` | Key id for the client assertion JWK, if the partner requires it. |
| `PAYER_DAVINCI_CLIENT_SECRET` | OAuth2 client secret for the `client_secret_post` `client_credentials` grant — for authorization servers that cannot issue asymmetric credentials. The value is the secret **itself, not a path** (unlike `PAYER_DAVINCI_CLIENT_KEY`). Mutually exclusive with `PAYER_DAVINCI_CLIENT_KEY`/`_ALG`/`_KID`; prefer `private_key_jwt` when your server supports it. |
| `PAYER_DAVINCI_PAS_NATIVE` | `true` to forward PAS submit/update legs to the partner's `/Claim/$submit`. Default `false` (built-in PAS fallback). Requires a payer Store. |
| `PAYER_DAVINCI_CRD_SERVICE_ID` | Escape-hatch override for the partner's order-select CDS service id. Empty ⇒ the gateway fetches `{base}/cds-services` at boot and auto-selects the single order-select service (fails closed if none, or ambiguous). Set it when the partner's CRD service isn't uniquely discoverable. |
| `PAYER_DAVINCI_CRD_HOOK` | CDS Hooks hook value to stamp on the CRD request before forwarding (e.g. a partner whose service expects `order-sign`). Empty ⇒ forward the originator's hook verbatim. |
| `PAYER_DAVINCI_DISPATCH_SERVICE_ID` | The partner's CDS service id for the `crd-order-dispatch` leg. **Empty ⇒ the dispatch leg fails closed (502)** — set it if your flow uses order-dispatch. |
| `PAYER_DAVINCI_DISPATCH_HOOK` | CDS Hooks hook value to stamp on the order-dispatch request before forwarding. Empty ⇒ forward the originator's hook verbatim. |
| `PAYER_DAVINCI_CRD_COVERAGE_BUNDLE` | `true` to wrap the CRD request's bare `prefetch.coverage` in a searchset `Bundle` on egress — for a partner whose `order-sign` `coverage` prefetch is a search template that requires a Bundle (a bare `Coverage` returns 412). Default off ⇒ forwarded verbatim. |
| `PAYER_DAVINCI_CONTRACT_VERSIONS` | Declared Da Vinci contract versions for the partner payer, comma-separated `<contract>@<line>` tokens (e.g. `pa.pas@2.0, pa.crd@2.0`). Two things read this: the connectivity checks verify the partner's published capability against it (`version-drift` on disagreement, FR-G46), and native-forward routing refuses (before forwarding) any leg whose contract shares no line with it (FR-G48). Requires `PAYER_DAVINCI_BASE_URL`. Unset ⇒ native-forward legs are unfiltered (today's default) and the checks skip the drift comparison. |
| `PAYER_DAVINCI_STRICT_EXTENSIONS` | `true` to reserve the per-peer gated overlay (FR-G52) for this partner — a peer flagged this way would refuse a cross-version transform chain carrying or dropping its extensions instead of forwarding stripped or lossy content. **Currently DORMANT: setting it has no routing effect on this deployment.** The strict *consult* itself is already live where transforms are actually selected (route-layer chain selection, exercised by test-only seams), but the one peer this flag targets — the foreign Da Vinci partner reached through native-forward mode — is filtered through arm-1-only forwarding this slice (never through the chain-selection path), so the flag has nothing to gate yet. It goes live together with transform-at-the-native-forward-edge (not yet shipped; re-labeling another gateway's build product as a translated payload needs its own stamp/Provenance semantics worked out first). Default `false`. |

**Exactly-one-mode rule:** if `PAYER_DAVINCI_TOKEN_URL` is set, then
`PAYER_DAVINCI_CLIENT_ID` must also be set, plus exactly one credential mode —
`PAYER_DAVINCI_CLIENT_KEY` + `PAYER_DAVINCI_CLIENT_ALG` (`private_key_jwt`,
preferred) or `PAYER_DAVINCI_CLIENT_SECRET` (`client_secret_post`). A partial or
mixed credential block is a hard startup error (a likely misconfig). Setting
`PAYER_DAVINCI_BASE_URL` alone (no token URL) is valid and forwards to the
partner **unauthenticated** — the gateway logs a warning on startup to make this
mode visible.

## Provider DTR population (`PROVIDER_DTR_*`)

See [INTEGRATION.md](INTEGRATION.md#provider-dtr-population) for the managed-vs-native
tradeoff.

| Env var | Description |
|---|---|
| `PROVIDER_DTR_NATIVE` | `true` to forward DTR population to an SDC `$populate` engine instead of the managed populator. Default `false`. |
| `PROVIDER_DTR_POPULATE_URL` | The SDC `Questionnaire/$populate` endpoint. Required when `PROVIDER_DTR_NATIVE=true`. |

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
only the declared set routes.

| Env var | Description |
|---|---|
| `SHN_CONTRACT_VERSIONS` | This gateway's own **declared** exchange-contract versions: comma-separated `<contract>@<line>` tokens, e.g. `pa.crd@2.2, pa.dtr@2.2, pa.pas@2.2`. Drives leg selection, the published `CapabilityStatement`s and `.well-known/davinci-configuration`, and the declaration peers route against. Must be a **subset of the native set** (`pa.crd@{2.0,2.1,2.2}`, `pa.dtr@{2.0,2.1,2.2}`, `pa.pas@{2.0,2.1,2.2}`, `pa.pdex@2.1`) — a token outside it is a boot error, not a routing outcome. Unset ⇒ the build default, the canonical `2.0` line (`pa.crd@2.0`, `pa.dtr@2.0`, `pa.pas@2.0`, `pa.pdex@2.1`). |
| `FHIR_VALIDATE_URL_2_1` | The `$validate` endpoint hosting the **2.1** line's IG packages. **Required** whenever `SHN_CONTRACT_VERSIONS` declares any `@2.1` line. |
| `FHIR_VALIDATE_URL_2_2` | The `$validate` endpoint hosting the **2.2** line's IG packages. **Required** whenever `SHN_CONTRACT_VERSIONS` declares any `@2.2` line. |

**One validator per line — this is not optional.** A FHIR server loads exactly **one**
version of a given IG package, so a single HAPI cannot host CRD 2.0.1 and CRD 2.2.1 at the
same time; a 2.2 payload validated against a 2.0-loaded server is not validated, it is
mis-validated. Each declared non-canonical line therefore needs its own `$validate` lane.
`FHIR_VALIDATE_URL` (the base variable, above) remains the `2.0` lane. A gateway that
declares a line with no lane configured for it **refuses to start**:

```
gateway: SHN_CONTRACT_VERSIONS declares pa.pas@2.2 but no FHIR validator lane is
configured for line 2.2: set FHIR_VALIDATE_URL_2_2 to a $validate endpoint hosting
that line's IG packages (one HAPI hosts exactly one version of an IG) — refusing to
declare a line this gateway cannot validate (FR-36/FR-G29)
```

That is deliberate: advertising a line you cannot certify would put unvalidated payloads
on the wire (FR-36).

### Opting a line in

1. **Stand up the line's validator** — an IG-loaded `$validate` for that line's package
   set. The shipped sidecar image builds per line:
   `docker build --build-arg SHN_IG_LINE=2.2 deploy/validator/`. Point
   `FHIR_VALIDATE_URL_2_2` at it.
2. **Widen `SHN_CONTRACT_VERSIONS`** to include the new tokens *alongside* the ones you
   already declare (see the grow-only rule below), and restart the gateway. It will refuse
   to boot if step 1 is incomplete — that check is your safety net.
3. **Re-register or rotate** (`shn rotate`) so the new declaration reaches the registrar.
   Declaration tracks the current build/config, and it is published at
   registration/rotation — not continuously.
4. **Peers converge on their next registry poll.** Until they do, they are still selecting
   against your previous declaration.

**The mismatch window is benign, by design.** Between your rotation and a peer's next poll,
that peer routes legs at your *old* line. Those legs still complete: a gateway **honors**
an inbound request's declared line whenever it can both natively build and validate at
it — a wider predicate than its own declared set — so in-flight and stale-routed legs are
answered correctly rather than refused. That predicate is narrower than it sounds on a real
deployment, though: your validator lanes are themselves built from your declared set, so
`laned` ⊆ declared and the honor window collapses to ≈ your declared set — it buys no
cross-line width against a stale-routed peer, only cross-contract width within a line you
already declare, which matters most mid-roll across a multi-instance fleet.

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
line this build natively speaks, still enters the lane map — this is deliberate opt-in headroom
for cross-version translation (FR-G52), and it has **two consequences, both grow-only, both
worth understanding before you flip it**:

1. **Egress:** a peer that declares that line becomes reachable by **native reach** (routing
   arm 2 — this build constructs a genuine native payload at that line, zero transform loss)
   even though this deployment never advertises the line itself. Without the lane configured,
   the same peer would only be reachable, if at all, through a transform chain (arm 3) or a
   legible refusal.
2. **Inbound:** the SAME lane map backs what this gateway **honors** on an inbound request-frame
   `contractVersion` claim (`docs/PARTICIPANT_PROTOCOL.md` §8.6's *native ∩ laned* rule, wider
   than the declared set by design) — so configuring an undeclared lane widens what you'll
   silently accept from a stale-routed peer too, not only what you can build for one. This is
   the bidirectional nature of the opt-in: there is one lane map, read by both routing
   directions.

Lanes obey the same grow-only discipline as declared lines, for the identical pend-stranding
reason: a resumed pended exchange needs its pinned line's lane to remain configured for as long
as the pend can still be amended. Do not remove a `FHIR_VALIDATE_URL_<line>` env var while any
pend may still be pinned to that line, declared or not. A single-line contract (`pa.pdex`) has
nothing to opt into — it has only one native line and always rides the canonical lane.

### CLOSED — DTR at line 2.2 on the built-in sandbox responder

Previously recorded here: the sandbox payer responder that ships with this repo could not
answer `dtr-questionnaire-fetch` at line 2.2. DTR 2.2's `DTR-QPackageBundle` profile
requires a `QuestionnaireResponse` entry in the returned package, and its
`QuestionnaireResponse` profile in turn requires a Coverage reference — but the sandbox DTR
request carried only the questionnaire canonical, and the sandbox responder had no way to
resolve a member from it. It held no honest source for either value, and fabricating
clinical or coverage attribution on the payer side is exactly what per-message validation
exists to prevent. A deployment that declared `pa.dtr@2.2` while running the sandbox
responder therefore failed that leg closed (`TestDTRAt22_UnansweredGap`, now deleted).

This is now closed **honestly**, on both sides of the wire:

- **Request side:** DTR's `$questionnaire-package` input profile makes `coverage` **1..1**
  at every line (min=1 everywhere; 2.2 additionally tightens `max` from `*` to `1` —
  verified live against the pinned 2.2.0 package). `DTRDef.QuestionnairePackageCoverageRequired`
  is `true` only at 2.2: `buildQuestionnairePackageRequestAtLine` /
  `buildQuestionnairePackageOrderRequestAtLine` refuse an empty coverage **before the wire**
  at that line — a legible local error naming the line and the cardinality, replacing what
  would otherwise be a real partner's opaque 400. The provider-side fetch (`originate.go`)
  now always attaches the requester's own (SoR-derived) Coverage at 2.2, not only on the
  br-payer-targeting profile as before.
- **Responder side:** the sandbox responder answers a 2.2 package with an **honest,
  in-progress, zero-answer** `QuestionnaireResponse` shell (`buildDTRPackageQRShellAtLine`).
  It never fabricates a subject or coverage reference — both are read straight off the
  Coverage resource the requester just sent on the same leg
  (`dtrPackageCoverageSubject`: `Coverage.beneficiary` for the patient, `Coverage.id` for
  the coverage reference — the requester's own logical id, not a private identifier
  system). The responder **fails the shell closed** if the requester's Coverage carries
  no `id`, rather than inventing one. The shell carries zero `item` answers; the
  auto-filled/authored `QuestionnaireResponse` that actually crosses into the PAS
  submission is built separately and is unaffected by this entry (the consumer discards
  it — `extractQuestionnaireFromPackage` only ever reads the bare `Questionnaire` entry).

Verified live against the pinned DTR 2.2.0 package (`shn-hapi-validate-ig22`, 2026-08-12):
a hand-built shell of this exact shape validates against `dtr-questionnaireresponse|2.2.0`
with zero error-severity issues. `pa.dtr@2.2` is now included in the whole-line 2.2 mesh
(`test/conformance/per_line_uc_test.go`'s `perLineMeshes`) alongside `pa.crd@2.2` and
`pa.pas@2.2`.

A **separate, pre-existing, unrelated** gap surfaced during this verification and is
recorded here rather than fixed (out of scope for that fix — it concerns the Questionnaire the
sandbox payer *asks*, not the QuestionnaireResponse it *answers with*): the sandbox lumbar
Questionnaire (`shnsdk.SandboxLumbarQuestionnaire`) does not itself conform to DTR 2.2's
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
