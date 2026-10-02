# Configuration reference

The gateway is configured entirely by environment variables. `SHN_DISCOVERY_URL`
resolves almost everything else; a typical deployment sets only a handful of
variables. This document is the complete reference; for a task-oriented walk
through wiring your own systems, see [INTEGRATION.md](INTEGRATION.md).

- [Required (every role)](#required-every-role)
- [Validation (required to boot — FR-36) and conformance enforcement](#validation-required-to-boot--fr-36-and-conformance-enforcement)
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

## Validation (required to boot — FR-36) and conformance enforcement

The gateway **refuses to start without a FHIR validator**, at every enforcement
level. The published discovery descriptor does not advertise a validator, so you
must supply one. Whether your gateway validates each resource at its own edge, and
what it does with an invalid result, is your choice (`CONFORMANCE_ENFORCEMENT`
below):

| Env var | Description |
|---|---|
| `FHIR_VALIDATE_URL` | A FHIR `$validate` endpoint (a HAPI server with the Da Vinci CRD/DTR/PAS + US Core IGs loaded). The production path. |
| `SHN_FAKE_VALIDATOR` | Set to `1` to use a no-op validator. **Dev only** — skips real profile validation. Use for a first wiring smoke test; never in production. |
| `CDS_ADVERTISE_HOOKS` | Optional. A comma-separated list of the CDS Hooks your provider ingress advertises and dispatches, from `order-sign`, `order-select`, `order-dispatch`. Unset advertises every hook the network carries. Set it only to narrow: when no payer your gateway routes to carries a hook's leg, advertising it promises a service that can only fail at routing; a request to a service id you do not advertise is refused with `404` and the ids you offer. A hook the network does not carry refuses to boot. This is an interim override — the network does not yet carry a declaration of the hooks each payer offers, and the listing will derive from routing once it does. |
| `CONFORMANCE_ENFORCEMENT` | `none`, `observe` (the default when unset), `structural` or `strict`. A level applies only to what each leg's check covers (a resource's declared profiles, else its base FHIR R4 definition; a carried PAS request is not `$validate`d): see [INTEGRATION.md](INTEGRATION.md#conformance-enforcement). At `none` no payload conformance check runs (no FHIR profile validation, no CDS Hooks response rules, no content checks) and no finding is recorded. At `observe` every check runs, each defect is recorded as a finding in your gateway's log and observer stream, and the message is carried as sent, apart from the gateway's registered edits (the callback removed, payer identity mapping, and, when the provider opts in with `ENRICH_NATIVE_REQUESTS=true`, prefetch and coverage obtained; participant protocol §7a.4); a validator that cannot be reached is recorded the same way. From v0.60.0, at `observe` a check that can only record does not hold the message: it is queued, and its finding is written when the validator answers. The queue is bounded: a check that finds it full is dropped, and the gateway logs `gateway: observe check dropped` (on the 1st, 2nd, 4th, 8th… drop). Two kinds of check still wait: the check of a payload this gateway itself translated between IG lines, which refuses at every level, and a payer gateway's checks of the decision ExplanationOfBenefits it builds from its system's answer, because whether each decision is written depends on them. At `observe` all of one exchange's decision checks share at most 2 seconds, a fixed bound; a check that bound does not reach is recorded as unavailable and its decision is written. At `structural` every check runs, a message whose structure is broken (a missing required element, an element the resource does not define, a value of the wrong JSON type or one that cannot be read, a CDS Hooks answer that cannot be read or lacks a required member, a request or answer the gateway cannot read, or a validator result the gateway cannot classify) is refused as at `strict`, and every other defect is recorded as at `observe`. Of FHIR profile issues, only invariants and the validator's recognized issues for a code outside its code list (licensed code systems included) are recorded. From v0.55.0 that includes a code system the validator does not know (`Terminology_TX_System_Unknown`, which it reports as an error only for a system in the HL7 FHIR namespace, a misspelled HL7 system URL included): an unknown code system is recorded and the message relayed when nothing else in it refuses at this level, where earlier releases refused it. Any other terminology issue refuses, and every other FHIR profile issue and every fatal issue refuses; an unreachable validator is recorded. At `strict` a defect refuses the message, and the refusal names the rule and the issues it was based on; an unreachable validator refuses with `500`. A gateway at `observe` waits up to 10 seconds on stop for its queued checks, after its listener's 5-second shutdown and up to 2 seconds for the handlers it cuts, so give its container a stop timeout above about 17 seconds (Docker's default is 10); a gateway stopped sooner loses the findings of the checks still queued. Network rules refuse at every level: authentication, authority (including a token presented with a request other than the one it was issued for), consent, the patient binding (each gateway identifies the member the request names by its own system), routing, replay, a repeated member name in any body, the contract line stamped on an answer's frame, and the check of a payload this gateway itself translated between IG lines. Any other value refuses to boot. |

If neither `FHIR_VALIDATE_URL` nor `SHN_FAKE_VALIDATOR` is set (and discovery
advertises none), the gateway exits with `refusing to run without per-message
validation (FR-36)` — at every `CONFORMANCE_ENFORCEMENT` level.

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

**A PAS Bundle's payor.** On `Claim/$submit` and `Claim/$inquire`, the gateway routes by the first
Coverage's first `payor`: an identifier it carries routes first; otherwise its reference resolves
among the Bundle's own entries, and the gateway never reads the payor from outside the Bundle. An
absolute reference (a URL or a `urn:uuid`) names the entry whose `fullUrl` equals it; a relative
`Organization/<id>` names the entry with that type and id, whatever its `fullUrl`; a `#<id>` names
an Organization the Coverage contains. A reference several entries answer routes only when every
one is an Organization naming the same payer identifier. When the payor names no payer identifier
routing can read, the request is refused `422` `no payer identifier on member coverage`, followed,
when the gateway can tell, by `: ` and the reason, which echoes nothing the request carried. (A
`$submit` Bundle with no Coverage is refused earlier, `400 PAS bundle missing
Coverage.beneficiary`, so the first reason below is seen on `$inquire`.)

  - `the Bundle carries no Coverage`
  - `the Coverage names no payor`
  - `Coverage.payor carries neither a reference nor an identifier with both a system and a value`
  - `Coverage.payor names a contained resource the Coverage does not contain`
  - `Coverage.payor matches more than one entry of the Bundle, and they do not name one payer`
  - `Coverage.payor is an absolute reference that is no entry's fullUrl: the payor Organization must be an entry of the Bundle`
  - `Coverage.payor is a relative reference that matches no entry of the Bundle: the payor Organization must be an entry of the Bundle`
  - `Coverage.payor references a resource that is not an Organization`
  - `the contained payor Organization carries no identifier with both a system and a value, such as a NAIC code or payer id`
  - `the payor Organization in the Bundle carries no identifier with both a system and a value, such as a NAIC code or payer id`

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

## Members your system of record does not hold (every role)

A Da Vinci CRD, DTR or PAS request can name a member your system of record does not hold,
a patient from the other participant's own environment, for example. By default your
gateway carries it; set `REQUIRE_KNOWN_MEMBERS=true` to have it refused instead.

| Env var | Description |
|---|---|
| `REQUIRE_KNOWN_MEMBERS` | `true` or `false`; unset means `false`. By default a CRD, DTR or PAS subject your system of record does not hold (and, on a payer that declares its own eligibility endpoint with `PAYER_ELIGIBILITY_URL`, a coverage-eligibility subject) is carried, identified by the member id and the Patient the request carries for it. Send the same Patient, unchanged, on every leg of one exchange: for a member the payer does not hold, a later leg that carries a different Patient, or none, is not matched to what an earlier leg recorded (an amendment still reaches the payer and its answer is relayed, but it binds and records nothing; an inquiry's decision is relayed but not recorded). A receiving gateway handles the member the request names as it would directly, whether one side holds the member, both do, or neither does, and files what it records about the exchange under its own identification of that member. That identification, for a member your system of record does not hold, is never the identifier of a member it does hold, so the exchange is never recorded under the wrong member; the bytes exchanged are the same. Nothing is added to a request on the member's behalf unless the provider opts in with `ENRICH_NATIVE_REQUESTS=true` (see [Accept Da Vinci requests from a provider EHR](#accept-da-vinci-requests-from-a-provider-ehr-provider-optional)); with it, a CRD request for a member your system does not hold leaves out history prefetch it cannot read, with the reason recorded. Set `true` to check members against your own system of record instead: such a subject is refused (`400 unknown member`; `403 request patient does not resolve` on a provider's `$questionnaire-package` request). `true` needs a system of record: a native-forward payer that keeps none (`FHIR_DATA_URL` unset) refuses to boot with it. Any other value refuses to boot. |
| `SHN_ACCEPT_UNKNOWN_MEMBERS` | Deprecated; removed in a later release. Carrying members your system of record does not hold is the default, so the gateway only logs a warning when it is set. Set to anything but `0` or `false` together with `REQUIRE_KNOWN_MEMBERS=true`, it contradicts it and the gateway refuses to boot. |

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

## Access lines (always on)

A gateway writes one line to its log for every call it answers on a Da Vinci
ingress route (a call from your own system) or for a leg it receives from the
network, at every conformance level. There is nothing to configure:

`gateway: access: {"time":…,"correlationId":…,"direction":"ingress"|"inbound","route":…,"exchange":…,"outcome":…,"status":…,"latencyMs":…,…}`

- `correlationId` is the leg's id, the `X-SHN-Leg-Id` your caller is answered
  with; `trace` is the caller's own `X-Correlation-Id` when it differs. A leg
  id a message chose (a PAS Claim's own `urn:shn:correlation`) that is not
  one bounded token is recorded as `sha256:<digest>`.
- `exchange` is the leg (`crd-order-select`, `pas-claim`, …); `contractLine`
  the IG line it ran at; `sender` and `recipient` the two holders.
- `outcome` is `answered`, `refused`, `unreachable`, `upstream-error` or
  `other`. A refusal names who refused (`refusal.by`) and the network rule
  (`refusal.rule`: authentication, authority, consent, routing, replay,
  integrity, audit, fidelity, conformance for a check you opted into, limit,
  or other when no rule is named).
- On a leg from the network, `backend` is your own system's answer: its
  `status`, `latencyMs`, `errorClass` and how many `calls` were made, reads
  and searches of your system of record included (they have status 0). It
  describes the call the answer came from: the operation forwarded to your
  system, or, where there was none, the last read (of your system of record,
  or of your CDS service listing); a read that checks the answer afterwards
  is counted in `calls` only. The error class is timeout, connect, tls, auth
  (a token that could not be obtained, or your system answering an operation
  or the listing with 401 or 403: it rejected this gateway's credentials or
  access), http-3xx, http-4xx, http-5xx, read, malformed, cancelled, or other
  (your system of record reported itself unavailable, a search of it failed,
  your connector cancelled a read itself, or a call to your system was
  abandoned while the request it served was still live).
- `timeout` on a forwarded operation is your system not answering within
  this gateway's deadline for it (`PAYER_DAVINCI_BACKEND_TIMEOUT`, 25 s by
  default, under the requester's 30 s leg budget). The deadline counts from
  the leg's arrival, so this gateway's own work before the call (validation,
  the member's lookup) counts against it: a `timeout` whose `latencyMs` is
  well under the deadline means that work used most of it. The requester is
  answered this gateway's own `504` while it is still waiting, and the
  exchange is your system's upstream error. If that work used the whole
  deadline, the operation is not sent to your system: the access line
  records the exchange as `other` and no call of the forwarded operation (if
  a read ran before it, such as the member's lookup or the CDS service
  listing, the last one is still its `backend`). The `504` says your gateway
  ran out of time.
- `cancelled` is a call cut short because the request it served ended before
  that deadline: the requester stopped waiting, or went away. The exchange
  is recorded as `other`, not as your system's error.
- `malformed` is an answer this gateway could not read, whether it refused it
  or relayed it. A readable answer a conformance check found fault with is
  not malformed. At `none`, where no conformance check runs, an answer is
  malformed only when the gateway itself could not read it:
  - one repeating a member name (the network's own rule);
  - your CDS service listing;
  - a system-of-record read your connector reports invalid, or fails without
    saying why;
  - a search page;
  - a facility's records out of shape: another patient's record, no Patient,
    a record without an id, or the same record twice.

  A read of your system of record that a `strict` check needed and could not
  make refuses as that check's conformance refusal, counted in `calls`.
- From shn-gateway v0.61.0, on a leg from the network, `stages` says where the
  leg's time went, in milliseconds:
  - `unwrapMs`: verifying the leg and opening its envelope, up to its
    dispatch (for a leg refused before it, the call's time in no other stage);
  - `readsMs`: every read of your own system (your system of record and your
    CDS service listing);
  - `forwardMs`: every call forwarding the operation to your system;
  - `validateMs`: the `$validate` checks the answer waits for (at `observe`
    a check that only records runs after the answer: only queueing it is
    counted, and its own time is its finding's `validatorMs`);
  - `sealMs`: sealing the answer and authorizing it with the Authorization
    Framework, the tokens for the other patients the answer involves and the
    answer's diagnostic capture included;
  - `ledgerMs`: the pend ledger's work: its lookups, the claim an update
    takes (and its release when the update is not recorded), an inquiry's
    effect derived from the payer's answer, and its writes;
  - `writeMs`: handing the answer to the HTTP server (a small answer reaches
    the Hub after the call is recorded).

  The stages do not overlap, and their sum is at most `latencyMs`; the rest
  is the gateway's own handling between them. Durations only.
- From shn-gateway v0.61.0, when the answer to a leg (your system's, or this
  gateway's own refusal) could not be authorized, the requester is still
  answered `502 authorization failed`, and the access line's `answerError`
  says why: `cancelled` (the request ended first: the requester had gone),
  `timeout` (the authorize call ran out of time), `auth` (the Authorization
  Framework denied it), `unreachable` (it never answered) or `other`. The
  gateway also logs `gateway: <exchange> answer not authorized: <class>
  (correlation <leg id>)`.
- `findings` counts the conformance findings the call's checks recorded; each
  finding keeps its own `gateway: conformance:` line. From v0.60.0, at `observe`
  the line carries `"deferred": true` instead of a count (`count` reads 0 and
  `kinds` is left out): the checks do not hold the call, so a count taken when it
  ends would be partial. `refused` is still carried, because a check that refuses
  is judged before the answer at every level. Read the call's findings from its
  `conformance:` lines, by its correlation id.
  From v0.61.0 a finding from a check that called the validator carries
  `validatorMs`: the time its `$validate` calls took, every line tried
  together (an outage's included, up to its failure), and each entry of its
  `lines` carries `ms`, that line's own call's time, so a line's lane is timed
  apart from the others. Each is rounded down to whole milliseconds on its
  own and left out when that is 0, so the lines' `ms` need not add up to
  `validatorMs`.
  `validatorMs` is always a finding's whole check; `ms` is always one
  `$validate` call, here and on a `crd.embedded.validated` observation.
  At `observe` this is the check's time after the answer, which the leg's
  `stages` do not hold.
- `requestCiphertextHash` and `responseCiphertextHash` are the hashes the
  network's audit records carry for the same leg.
- `relayEdits` (from v0.61.0) names the registered edits this gateway made to
  the bytes it sent on the leg, by id (`E-01` …), in the order made, each once:
  on a call from your own system, the request as sent to the network (for
  example `["E-01","E-07"]`: `fhirServer` and `fhirAuthorization` removed, and
  the coverage read through `fhirServer` carried); on a leg from the network,
  the request as forwarded to your system (`["E-03"]`: your payer identity
  mapped). Only the ids: never a value an edit removed or wrote. The ids are
  named once the request has left the gateway: the receiver answered (with
  anything, a refusal included), or the connection failed after the request
  was written. A request that was never sent names none: one refused before it
  was sent (by routing, or by the network's authorization), one whose
  connection could not be made, or one whose bearer token could not be
  obtained. A leg whose message was carried exactly has no `relayEdits` key.
- With diagnostic collection configured, the captured request names the ids it
  was built with, as its `Detail`, before it is sent: `leg.sealed` once the
  request is sealed, before the network's authorization is asked, and
  `native.request` just before the forward to your system. So a captured
  request can name edits its access line does not: a leg the network's
  authorization denies, or a forward that never reached your system.
- `hubDelivered` (from v0.61.0), on a call from your own system whose leg the
  Hub answered with an error, is the Hub's own word on whether the
  recipient's gateway received the leg, from the `X-SHN-Delivered` header on
  that answer: `no` (the Hub refused before forwarding, or the recipient's
  gateway was not reached or refused the leg at its edge), `yes` (the
  recipient's gateway answered, and its answer was lost on the way back), or
  `unknown` (it may have received the leg: it answered 5xx, the connection
  failed after the request was sent, or it did not answer in time). The key
  is absent when the leg drew no error answer from the Hub itself: for
  example, it was answered, it never reached the Hub, no answer came back
  from the Hub, or the error came from something in front of the Hub. This
  gateway never infers it. When a call makes more than one leg, it is the
  last leg's; a leg from the network never carries it.

A line never carries a message body or a patient identifier; of the header
values it saw, it carries only your caller's own `trace`, on the provider
test endpoint its verified call id, and, from v0.61.0, the Hub's
`X-SHN-Delivered` as `hubDelivered` (only as `no`, `yes` or `unknown`).
The full field list is `diagnostics.AccessLine` (see [STABILITY.md](../STABILITY.md),
"Access lines"). With optional diagnostic collection configured (below), each
line is also published there.

## Exchange metrics (optional — CloudWatch EMF)

The gateway can emit CloudWatch EMF metrics (counts and latencies — no payloads,
no PHI): `LegOutcome`/`LegError` for each leg it originates and, from v0.58.0, one
set for every call it answers (the `METRICS_SERVICE` row). Off unless configured;
the published binary defaults OFF.

| Env var | Description |
|---|---|
| `METRICS_SERVICE` | Names this gateway service for the EMF `Service` dimension (e.g. `provider-data-gw`). Empty (default) disables metric emission entirely. When set, the gateway reports every call it answers, in both directions: `Exchange` (`direction`, `exchange`, `outcome`) and `ExchangeLatency` (`direction`, `exchange`); and, when it called your own system, `BackendCall` (`exchange`, `class`: `ok` or the error class), `BackendLatency` (`exchange`, when the call's duration is known) and `BackendError`. Those describe the access line's `backend` call: the operation forwarded to your system, or, where there was none, the last read. `BackendError` is one count per such call your system gave no usable answer to: a timeout, a connection or TLS failure, `auth` (a token that could not be obtained, or a 401 or 403 from your system), a 5xx, an unreadable or malformed answer, or a failure the gateway could not classify. That holds for an exchange whose operation was never sent because the gateway's own work spent its deadline (`PAYER_DAVINCI_BACKEND_TIMEOUT`): its `backend` is the last read, so a CDS service listing whose re-read failed in one of those ways (the gateway then uses the listing it last read) counts, and a usable read, or a re-read answered with a 3xx or a 4xx other than 401 or 403, does not. A `cancelled` call is not counted. Every dimension value comes from a fixed list, so the number of series is bounded; no identifier, party or payload ever becomes a dimension. |
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

When a base the gateway calls with a credential is configured with one
(`PAYER_DAVINCI_BASE_URL` with `PAYER_DAVINCI_TOKEN_URL`, `FHIR_DATA_URL` with
`FHIR_TOKEN_URL`), its `$metadata` check authenticates the same way the
gateway's own calls to it do, with the same credential settings and headers
(through a client of its own, which keeps its own token), and passes or fails
on that read. The check then repeats the read without the credential and
records the answer as an informational `anonymous` object (`ok`, `detail`, and
on a failed read `failure`) on the result; it never fails the check, and it
follows the same redaction rule as `detail`. A base that refuses the anonymous
read with `401` or `403` passes with a detail such as `CapabilityStatement
(FHIR 4.0.1); the base answers /metadata only when authenticated (anonymous:
HTTP 403)`; any other anonymous failure is named as `anonymous read: …`. The
result's `latencyMs` covers both reads. A credential the gateway cannot obtain
fails the check as `credential-rejected`; while it does, each run asks the
token endpoint for it twice, once for the credential check and once for this
read, and runs are held to the 30-second cooldown above. A base configured without a
credential is read anonymously, as the gateway calls it, and carries no
`anonymous` object.

## Connect your system of record

See [INTEGRATION.md](INTEGRATION.md) for how these fit together.

| Env var | Description |
|---|---|
| `ORIGINATION_PROFILE` | provider. Set to `provider-data` to originate every prior-auth UC off your seeded FHIR system of record and drive real payer verdicts — the config-only provider lane, no custom code. `demo` originates the shipped demo order set instead. Both lanes answer a REAL payer's questionnaire, so both require `PROVIDER_DTR_POPULATE_URL` (the operated `$populate` endpoint, validated at boot). Unset means `demo`. From shn-gateway v0.57.0 any other value refuses to boot, on every role, with an error naming the accepted values; that includes `demo` or `provider-data` in another case or with surrounding whitespace, since the value is matched exactly. Earlier releases accepted an unknown value and built the requests the gateway originates on no known lane. |
| `FHIR_DATA_URL` | FHIR R4 base URL for your system of record. **Required on every role but a native-forward payer.** The gateway reads its members, coverage and clinical facts from your own FHIR server; there is no built-in persona stub any more, so an unset value is a boot error naming this variable. From shn-gateway v0.58.0 (earlier releases refuse to boot without it on every role), a payer whose own system answers every exchange (`PAYER_DAVINCI_BASE_URL`, below) may leave it unset: its gateway then keeps no system of record. It binds each member from the request that names it, exactly as it binds a member a system of record does not hold (see [Members your system of record does not hold](#members-your-system-of-record-does-not-hold-every-role)), and answers coverage eligibility only through `PAYER_ELIGIBILITY_URL`: without that endpoint, an eligibility request is answered `501` "coverage eligibility is not offered by this payer". It logs `gateway: no system of record (FHIR_DATA_URL unset)` at boot. `REQUIRE_KNOWN_MEMBERS=true` needs a system of record, and refuses to boot without one. Patient Access returns no explanations of benefit on such a gateway: they are filed under its binding of each member, which is never the identifier a patient-access token names (as for any member a system of record does not hold). |
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

### How long a prior authorization stays answerable

A payer gateway keeps a ledger row for every prior authorization it pends or
decides, so a later `Claim/$inquire` or amendment about that authorization resolves
to the right one. The row holds decision metadata only — the state, the outcome, the
date the payer gave it, the requester, and the identifiers the authorization can be
named by. No clinical content is stored.

**The ledger records; it never decides what reaches your system.** Every amendment is
sent to your system and your answer is relayed, whatever the ledger holds for the
authorization — no pend recorded here, a decision already recorded, or another
amendment still with your system. The ledger then records your answer by the rules
below. When an amendment does not match a pend this gateway recorded, the gateway
notes `pend.amendment-unbound:<reason>`, the reason being `not-pended` (no pend here),
`already-decided`, `update-in-progress` (another amendment is still with your system),
`ledger-unavailable`, `no-prior-claim` (the update names no prior claim) or
`other-requester`. Such an
amendment's answer is recorded only onto the authorization this gateway already holds
for that patient and that requester (or for no requester); it never creates one, and an
amendment of another requester's authorization records nothing (`other-requester`). An amendment holds its
authorization while your system answers it; a hold its gateway never released (it
stopped mid-leg) lapses after five minutes. If a record cannot be written after your
system answered, your answer still reaches the requester, and the gateway emits
`pa.local-write-failed` naming the kind of failure. Your system's own `409` (for
example a version conflict while it resolves the same claim) is relayed as its answer;
the gateway does not re-send an amendment for the requester.

- **Retention is six months from the last change to the authorization**, and it is
  not configurable: Prior Authorization requires a pended authorization to stay
  answerable for at least that long, so the period is fixed rather than left to an
  operator to shorten. Expired rows are removed in the background, a bounded number
  at a time, so the sweep never competes with live traffic.
- **The payer's later word wins.** Once the payer has approved or denied an
  authorization, the ledger keeps that decision; an amendment of it still reaches your
  system, which answers it. If your system later pends that authorization again, dated
  after the decision, your newer answer wins and the authorization reopens. A re-pend
  that arrives while an amendment of the same authorization is still with your system
  is recorded without reopening that amendment: only the amendment's own answer moves
  it.
- **A correlation id belongs to one patient.** A submission for another patient under a
  correlation id an authorization already holds, in any state (pended, in progress or
  decided, with or without its ExplanationOfBenefit), is refused (`409`) before the payer is
  asked; this check does not refuse the same patient's submission under it, so a
  retried submission lands on the same authorization. (The Hub refuses only a
  byte-identical envelope; a retry sealed afresh reaches this check.)
- **An answer the gateway could not read writes no row.** Below enforcement `strict`,
  a payer answer your gateway cannot read, whose patient linkage is inconsistent, or
  whose decision cannot be stated on an ExplanationOfBenefit is relayed to the requester
  exactly as your system sent it, and nothing is recorded from it: no pend, no decision,
  no ExplanationOfBenefit (the gateway emits `pa.local-write-skipped`). An amendment
  whose prior authorization is pended in the ledger stays pended. A later amendment of a
  submission whose answer was relayed this way finds no pended authorization here and
  is sent to your system all the same; a later `Claim/$inquire` about it is relayed to
  your system and its answer to the requester, and records no decision. At `strict`
  such an answer is refused instead.
- **An inquiry's answer decides only the authorization it names.** From v0.60.0, a
  ClaimResponse in your system's `Claim/$inquire` answer is matched to an authorization
  recorded here (pended, or decided on its submission) by an identifier your system issued
  for that authorization alone: its
  `ClaimResponse.identifier` or its `preAuthRef`. The requester's request identifier and
  item trace numbers can confirm that match but never make one, because reused example
  claims share them. The authorization found must also agree with every identifier the
  ClaimResponse states, of each kind it holds; when your `preAuthRef` names it, a new
  `ClaimResponse.identifier` for the decision need not match the pend's. Once a decision is
  recorded for an authorization pended here first, its authorization number names the
  authorization too. A ClaimResponse that names no
  authorization, names more than one, or contradicts the one it names records nothing,
  and the inquiry emits one `pend.inquiry-unmatched` event counting them
  (`no-strong-key`, `none`, `ambiguous`, `disagrees`, `requester-key-only`). Your answer reaches the requester
  unchanged either way. So:
  - if your system's pended ClaimResponse carries neither a `ClaimResponse.identifier`
    nor a `preAuthRef`, a decision learned by inquiry is never recorded here;
  - if your system echoes the submitted Claim's identifier value as its
    `ClaimResponse.identifier` (under any system), that identifier is the requester's, not
    yours, and claims built from one body share it: it names no authorization on its own
    (`requester-key-only`). A decision learned by inquiry is then recorded only when your
    `preAuthRef` names the authorization.
- **A decision learned by inquiry gets its ExplanationOfBenefit only when the inquiry was
  about that authorization.** The EOB states the product coding of the inquiry's own
  lines, so it is built only when the inquiry's item trace numbers and authorization
  numbers are held by that authorization and by no other authorization of the same
  requester, counting claims your system decided on submission too. Otherwise the decision
  is still recorded, without an EOB, and the inquiry emits `pend.inquiry-eob-withheld`
  counting those that have none yet. An inquiry with a line that states neither a trace
  number nor an authorization number is never about one authorization. The
  cost: when your system answers an inquiry with several authorizations, a member does not
  see an ExplanationOfBenefit in Patient Access for a decision the inquiry was not about,
  nor for any decision of claims that reuse one trace number, until an inquiry about that
  authorization alone restates it. A decision your system gives on the submission itself is
  written with its EOB as before; like any recorded decision, a later restatement can
  replace that EOB and a later decision with another outcome removes it (below).
- **Patient Access never shows a decision the ledger no longer keeps.** An EOB is written
  only for the decision the ledger keeps (the same outcome, dated no earlier), so an
  earlier or losing answer never replaces it. When a later decision with another outcome
  arrives without an EOB of its own (from an inquiry about another claim, or an amendment's
  answer), or a later re-pend reopens the authorization, the old decision's EOB is removed
  and the gateway emits `pend.eob-removed`: the member sees no EOB for it until a decision
  arrives with one.
- **Without `SHN_STORE_DATABASE_URL` the ledger is in memory**, so it does not
  survive a restart: after one, a follow-up about an authorization pended before the
  restart cannot be resolved, and the requester submits again. Set the DSN for any
  deployment that needs authorizations to outlive a restart or to be shared between
  replicas.

### Continuing a decision the payer has not made yet

A payer answers a submission with its own determination, and "pended" is one of
those answers: the payer has not decided yet. Nothing polls for a later decision —
a later decision comes only from an explicit `Claim/$inquire`, which the requester
performs when it chooses to.

Who keeps what is needed to ask again depends on who is asking.

- **A participant's own system keeps its own record.** It submitted the request, it
  has the patient, the coverage and the order, and it inquires through its own
  gateway's `POST /Claim/$inquire`. No state on this gateway is involved, and
  nothing here can be lost.
- **This gateway's own originator flows** — the operator console, the `/scenario/*`
  routes and the headless provider-data runs — have no such system behind them, so
  the gateway keeps a **continuation** for them: an opaque id, and the metadata an
  inquiry is built from. It stores identity, routing, the member, the identifiers
  the payer answered with, and the sequence, product code and service date of each
  submitted line. It stores **no clinical content**: at inquiry time the order is
  re-read from your own system of record, and a request that has since become a
  different one is reported (`409 order changed since submission`) rather than
  followed.

The continuation id **is** the capability, and it is bound to this holder. An id
this gateway did not mint answers `404` and says nothing further.

- **Retention is six months from the last change**, the same period the ledger
  keeps the authorization itself, and for the same reason: a capability that
  outlived the authorization it names would resolve to nothing.
- **Without `SHN_STORE_DATABASE_URL` continuations are in memory**, so a restart
  loses them. A continuation minted by a gateway in that shape, presented after
  the restart, answers
  `410 continuation lost (gateway restarted; submit again or inquire from your own
  system)` — it says the record is gone from a store that could not have kept it,
  rather than reporting the id as unknown. (It establishes that the id came from a
  non-durable store other than the one answering; an in-memory gateway keeps no
  register of its own past runs, so it cannot distinguish its own earlier restart
  from another such gateway's id. The answer and the way forward are the same
  either way.) Set the DSN for any deployment where an operator must be able to
  continue a decision after a restart, or from whichever replica the next request
  reaches.
- **What survives what, by deployment shape.** The answer states it per pend
  (`continuationDurable`), so a surface never has to infer it:

  | Shape | Continuations survive a restart | Continuations shared between replicas |
  |---|---|---|
  | One gateway, no `SHN_STORE_DATABASE_URL` (the desktop and single-container installs) | no — the id is refused `410` afterwards | n/a |
  | One or more gateways with `SHN_STORE_DATABASE_URL` | yes, for the retention period below | yes — any replica on that DSN continues it |

  Within a durable shape the retention above applies. Neither shape changes what a
  participant's OWN system can do: it holds its own record of the request and can
  inquire through `POST /Claim/$inquire` at any time, with no state on this gateway
  involved.
- **The originator routes accept a bounded wait, and take none by default.** A route
  answers with the payer's own answer — a pend included — unless its caller asks it to
  follow the decision: `?wait=<seconds>` states how long that caller is willing to
  hold its own request open, and the answer comes back as soon as the payer decides.
  The maximum is 30 seconds, which is the inquiry schedule's own reach rather than a
  round number: the first inquiry falls due 2 seconds after the pend, later ones back
  off to 5-second steps, at most six are made, and the sixth falls due at 26 seconds —
  so a longer bound would hold the connection open with no inquiry left to make.
  Reaching the bound is not an error: the answer is the
  pend and its continuation, which
  `POST /scenario/pa/inquire {"continuation": "…"}` continues later (with an optional
  `"waitSeconds"` of its own, under the same bounds).
- **Waiting is an opt-in stand-in, not the mechanism the guide names.** Prior
  Authorization makes Subscription the way a requester learns a decision made later
  and states it as a `SHALL`; this gateway does not offer Subscription, and the
  bounded wait stands in for it. The inquiry itself is the manual status check the
  guide permits. For a payer that decides in seconds the wait is a convenience; for
  one that decides in hours or days, keep the continuation and continue it when you
  are ready.
- **`/scenario/*` is an unauthenticated local operator surface and is never
  publicly routed.** The public ingress and the hosted door admit the participant's
  own `POST /Claim/$inquire` and refuse everything under `/scenario/`. Do not give
  that surface a public host.

## Accept Da Vinci requests from a provider EHR (provider, optional)

See [INTEGRATION.md](INTEGRATION.md#native-da-vinci-ingress) for when to use this
instead of `provider-data` origination.

Set `PROVIDER_DAVINCI_INGRESS=1` to mount the provider-side Da Vinci ingress: the
gateway accepts a provider EHR / reference-implementation's **native Da Vinci
requests** — CDS Hooks `order-sign`, `order-select` and `order-dispatch` (Coverage
Requirements Discovery), `Questionnaire/$questionnaire-package` (DTR), `Claim/$submit`
(PAS) and `Claim/$inquire` (the follow-up that asks a payer for the decision on an
authorization it pended) — and forwards them through to the Hub. A CDS Hooks request is forwarded as your EHR
sent it, with `fhirServer` and `fhirAuthorization` removed; the payer never gets a callback
into your systems. By default the gateway itself reads your EHR's FHIR server through
`fhirServer` in two cases only, both to choose the payer: a request with no coverage to route
by, for a member your system of record does not hold (a Coverage search, and at most one read
of its payor Organization; from v0.61.0 the records the payer was chosen by are then carried
as the request's `prefetch.coverage`, since the payer cannot read them once `fhirServer` is
removed; see [Reading the coverage through `fhirServer`](#reading-the-coverage-through-fhirserver)),
and a coverage your EHR sent that names its payor Organization by reference alone, which the
request does not resolve, for any member: unless that `fhirServer` is your system of record's
own FHIR base and your system resolves it (one read of that Organization, only to choose the
payer and never carried; see [CDS Hooks prefetch](#cds-hooks-prefetch)). Set
`CDS_FHIR_SERVER_READ=off` to turn both off; then nothing is read or carried. Apart from that
carried coverage, nothing is added to a request your EHR sends by default: set
`ENRICH_NATIVE_REQUESTS=true` to have the gateway add what a request leaves out from
**your own system of record** (see [CDS Hooks prefetch](#cds-hooks-prefetch) and the
`$questionnaire-package` Coverage and Patient below).

A `$questionnaire-package` request is forwarded as your EHR sent it: every parameter, in
order and repeated as sent, a questionnaire canonical's `|version`, `context`, `meta` and any
parameter the gateway does not know. Every resource in it (each `coverage`, each `order`,
everything in `referenced` and in parameter parts) must be about one patient, the one its
coverages and orders name (a request naming none is refused with 422; at enforcement
`strict`, one naming another patient anywhere is refused with 403), and all of its coverages
must name one payer (422 otherwise). A `coverage` parameter that names its payor Organization
by reference alone (`Organization/<id>`), with no such Organization in the request, has it
read from your system of record when that system names the patient by the request's member
id, only to choose the payer: nothing is added to or changed in the request. A questionnaire
request has no `fhirServer` to read it through, so otherwise (or when your system holds no
such Organization) it is refused `422 no payer identifier on member coverage: Coverage.payor
is a reference to an Organization the gateway could not read; send the payor Organization with
the coverage, or a payor identifier`. A reference written absolute, versioned, with a fragment,
a leading slash or a dot segment is never read, with or without the opt-in. An absolute one that a Bundle entry's `fullUrl`
can be (no `/_history/`, query, fragment or dot segment) is resolved by an entry with that
`fullUrl` of a Bundle parameter, and otherwise refused `422 no payer identifier on member
coverage: Coverage.payor is a reference to an Organization the gateway could not resolve; send
the payor Organization as a Bundle entry whose fullUrl is that reference, or a payor
identifier`; any other is refused `422 no payer identifier on member coverage: Coverage.payor
is a reference to an Organization the gateway does not read; send a payor identifier with the
coverage`. Name the patient with a relative reference
(`Patient/<member id>`): a questionnaire request has no `fhirServer`, so an absolute Patient
reference cannot be read as your EHR's and, at `strict`, is refused (403). Below `strict` a
resource naming another patient is carried as sent (recorded as a finding at `observe`). A `coverage` parameter is always your EHR's own: the gateway never adds one
beside it, and a `coverage` parameter that carries no resource (only a reference, or
nothing) is refused with `400 coverage parameter carries no resource`.
When the request carries no `coverage` parameter, the gateway reads the patient's Coverage
from **your own system of record** with a Coverage search (recorded as a `prefetch.obtained`
event with `operation` `questionnaire-package`, also when the search finds nothing or cannot
run) and routes the request to the payer it names. It routes on the active Coverages when
any is active, otherwise on the others; when several name one payer, the first is used. A
payor Organization the Coverage names by reference is found among the records the search
included, then in your system of record. It refuses instead when that system does not hold the
patient (`422 no coverage to route by: send the coverage parameter (this gateway's system of
record names no patient for this member)`), holds no Coverage
(422), when the Coverages it chooses (the active ones, else the others) name different payers
(422), cannot search (422) or is unavailable (503). A Coverage that references its payor
Organization is resolved among the request's own resources and the records the search
included, then by one read from your system of record, with or without the opt-in below;
records that answer the reference without naming one payer are refused (422), as for the CDS
Hooks prefetch. By default the Coverage is only routed by: it is searched (every Coverage, no
status filter) under your system's own id for the patient, and the request is sent as your
EHR sent it. The payer then receives a request without a coverage, as it would from your EHR
directly; a payer's gateway that runs `strict` refuses it (`400`), and its answer is relayed
to your EHR. With
`ENRICH_NATIVE_REQUESTS=true` the search is the coverage template's (`status=active`), and
the Coverage the request is routed by is also appended to the request as one `coverage`
parameter, exactly as your system returned it, sent alone, not with the payor Organization it
may reference. When that search finds no Coverage (a member with no active coverage), or your
system of record cannot answer it (unavailable, not supported, not a searchset, or over the
bounds), nothing is appended, and the request is routed by the read that chooses the payer
above (every Coverage, no status filter, recorded as its own `prefetch.obtained` event after
the template's search), as without the opt-in; it is refused as above only when that read
fails too (see
[Payer backend identity mapping](#payer-backend-identity-mapping) for what that means for a
payer that maps its identity). An appended Coverage must name the patient the request names,
so when your system names the patient by another id nothing is appended: from v0.61.0 the
request is routed by the read that chooses the payer, under your system's own id, exactly as
without the opt-in (before v0.61.0 it was refused, 422). Also
under the opt-in, when the request carries no Patient for the patient anywhere in its
parameters and your system of record holds the patient under the id the request names, the
gateway appends that Patient, exactly as your system returned it,
as one `referenced` parameter (a Patient about another patient is refused with 502 at every
level; one your system cannot supply is refused at `strict` and left out below it). The request is sent to the payer's gateway naming the operation, which a payer
gateway accepts only when it declares the framed-operation capability (`v1op`); a payer that
does not is refused before anything is sent with 502 `payer gateway does not support framed
DTR operations (upgrade required)`. The payer's answer is returned to your EHR exactly.

`GET /cds-services` lists one CDS service per hook: `shn-order-sign` (`order-sign`),
`shn-order-select` (`order-select`) and `shn-order-dispatch` (`order-dispatch`). Post each
request to the service for its hook (`POST /cds-services/shn-order-sign`): an id that is not
listed is `404`, and at enforcement `strict` a request whose `hook` is not the service's hook
is `400`. Below `strict` it is carried as sent on the exchange of the service your EHR
addressed, and the payer's gateway picks the payer's service by the request's own `hook`: it
refuses, at every level, a hook that exchange does not carry. `shn-order-select` and
`shn-order-sign` carry `order-select` and `order-sign`; `shn-order-dispatch` carries only
`order-dispatch`. The hook is never changed on the way to the payer. The payer's answer is returned to your EHR exactly as
the payer sent it once it meets the CDS Hooks response rules (see
[CDS Hooks answers](#cds-hooks-answers)); `cards` may be empty, with the coverage information
in `systemActions`. A payer that offers no service for your hook answers `422` with the hooks
it offers.

`POST /Claim/$inquire` asks the payer for the decision on an authorization it pended.
The inquiry is carried to the payer as your EHR sent it, bound to the one member every
patient reference in it names (at enforcement `strict` a Bundle naming two members is
refused with 403; below `strict` it is bound to its Claim's patient) and
routed by the Coverage it carries (a Coverage naming no payer the gateway can resolve
is refused with 422, never defaulted). The payer's answer reaches your EHR exactly, in
whichever shape the payer's prior-authorization line defines: the response Bundle
itself, or a `Parameters` whose `return` parameters are those Bundles. The gateway
holds no state for this: the inquiry names the authorization, so your EHR is the only
thing that has to remember it.

The gateway does not profile-validate your inquiry, and does not profile-validate
the payer's answer — your bytes and the payer's are carried as written. The payer's
own system certifies the inquiry it receives, so a request that does not meet the
prior-authorization profile for the line it is routed at comes back as that payer's
own refusal rather than the gateway's. Note that the earliest line requires the
inquiry to name at least one item and the later ones do not, so an inquiry by
authorization number alone is accepted here and answered by the payer.

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

Every answer from the ingress carries `X-Correlation-Id` and `X-SHN-Leg-Id`, on refusals
as on successes, so your EHR can quote one value when something needs looking into. Send
your own `X-Correlation-Id` (up to 64 characters: letters, digits, `.`, `_`, `-`) and it is
the call's **trace** value: it comes back on the answer, and the gateway logs it beside
the exchange (`ingress … trace <yours> → leg <id>`). Anything else is ignored. The
exchange itself runs under its own id, freshly minted for each call and returned as
`X-SHN-Leg-Id`; that is the id the gateway's `certify:` and `leg.failed` lines carry. So
reusing a trace value, or retrying under it, is never refused and never lands on another
call's record at the payer.

A `$submit` or update whose `Claim.identifier` names its correlation
(`urn:shn:correlation`) is different: that value is the exchange's id and the payer's key
for the authorization, and both headers report it. So is an `X-Correlation-Id` equal to
one of the Claim's own identifiers, so an amendment naming it in `Claim.related` binds. A
resend of the same submission lands on the same authorization. A different patient's submission under a Claim correlation
another patient's authorization already holds is refused with `409` before the payer is
asked, so give each authorization its own Claim correlation.

| Env var | Description |
|---|---|
| `PROVIDER_DAVINCI_INGRESS` | Set to `1` to mount the ingress on the provider gateway. |
| `PROVIDER_DAVINCI_INGRESS_BASE_URL` | The gateway's public base URL — the SMART audience the gateway pins and the token endpoint it advertises. **Required** when the ingress is enabled. |
| `INGRESS_CLIENTS_FILE` | Path to a JSON array of registered inbound clients: `[{"client_id":"…","alg":"ES384","public_key_pem":"-----BEGIN PUBLIC KEY-----…","scopes":["system/Davinci.write"]}]`. **Required** (≥1 client) when the ingress is enabled. |
| `CDS_FHIR_SERVER_READ` | `private`, `public` or `off`; unset means `private`. How the gateway reads, to choose the payer, through a CDS Hooks request's own `fhirServer`: the Coverage when the request carries none and your system of record does not hold the member (from v0.61.0 what that read routed by is carried as the request's `prefetch.coverage`; see [Reading the coverage through `fhirServer`](#reading-the-coverage-through-fhirserver)), and the payor Organization a coverage your EHR sent names by reference alone when the request does not resolve it, for any member, unless the request's `fhirServer` is your system of record's own FHIR base and your system resolves it (see [CDS Hooks prefetch](#cds-hooks-prefetch)). `private` (the default) reads an `https` server in your own network or on the internet, never a loopback, link-local, metadata or reserved address; `public` reads only an `https` server on port 443 at a public address; `off` never calls `fhirServer`, and carries nothing. Any other value refuses to boot, and so does a value other than `off` set without `PROVIDER_DAVINCI_INGRESS` (unset needs no ingress). A gateway SHN hosts runs `public`, or `off` if you set it (see the section). |
| `ENRICH_NATIVE_REQUESTS` | `true` or `false`; unset means `false`. By default a Da Vinci request your EHR sends is carried as sent (apart from the CDS Hooks callback, which is always removed, and, from v0.61.0, the coverage read through that callback's `fhirServer` to route by, which is then carried): nothing else is added to it. Set `true` to have the gateway add what a request leaves out, read from your own system of record: each advertised CDS Hooks prefetch value (see [CDS Hooks prefetch](#cds-hooks-prefetch)), and the patient's Coverage and Patient on a `$questionnaire-package` request that carries none. Either way, a coverage a request leaves out is read from your system of record to choose the payer (or, unless `CDS_FHIR_SERVER_READ=off`, through the request's own `fhirServer` for a member that system does not hold). Requests the gateway builds itself (`ORIGINATION_PROFILE`) are not affected. Any other value refuses to boot. |

Enabling the ingress without a base URL or at least one valid registered client is a
hard startup error.

**This ingress is a private, within-boundary surface, not a public endpoint.** The
gateway's only public-internet leg is the gateway↔Hub connection (and, when a request names a
`fhirServer` on the internet, the Coverage or payor Organization read through it, unless you set
`CDS_FHIR_SERVER_READ=off`; see
[Reading the coverage through `fhirServer`](#reading-the-coverage-through-fhirserver)); every connection to
your own systems — including this ingress — is private/within-boundary. Your EHR or
reference implementation calls it from inside your own network, authenticating as one
of the clients pre-registered in `INGRESS_CLIENTS_FILE`. There is no plan to expose
this ingress on the public internet. Dynamic client registration (as opposed to the
static file above) remains a tracked enhancement.

### CDS Hooks prefetch

The gateway advertises six prefetch keys, on every service it lists at `/cds-services`
(from shn-gateway v0.60.0; the Da Vinci reference payer's order-sign templates, within
the query features CDS Hooks 2.0 asks a client to support):

| Key | Template |
|---|---|
| `patient` | `Patient/{{context.patientId}}` |
| `coverage` | `Coverage?patient={{context.patientId}}&status=active` |
| `serviceHistory` | `ServiceRequest?patient={{context.patientId}}&status=active,completed` |
| `deviceHistory` | `DeviceRequest?patient={{context.patientId}}&status=active,on-hold,completed` |
| `medicationHistory` | `MedicationRequest?patient={{context.patientId}}&status=active,completed` |
| `questionnaireResponses` | `QuestionnaireResponse?patient={{context.patientId}}&status=completed` |

Before v0.60.0 the templates named the patient `Patient/{{context.patientId}}`, had no
status filter, and asked for `_include=Coverage:payor` and `_include=DeviceRequest:performer`,
which CDS Hooks does not list among the query features a client supports.

The gateway's own search of your system of record for the same value (below) uses the
same status filter as the template, read from one table, but keeps the includes: a
coverage search also asks for `_include=Coverage:payor` and a device search for
`_include=DeviceRequest:performer`. A value the gateway reads is sent to the payer, which
has no route into your system (`fhirServer` is removed before the network), so without
the include the payor Organization and a dispatched order's supplier would be lost. What
your EHR is asked for and what the gateway reads from your system therefore differ only
in the include (and in naming the patient `Patient/<id>`).

One read differs on the filter too: the coverage the gateway reads only to choose the
payer, for a request that carries none (by default, below). It is not sent to the payer,
so it asks for every Coverage (`Coverage?patient=Patient/<id>&_include=Coverage:payor`)
and routes on the active ones when any is active, so a stale cancelled coverage naming
another payer never makes routing ambiguous; when none is active, it routes on the
others, provided they name one payer, so a member whose coverage is no longer in force
still reaches the payer that answers "not covered". The same read chooses the payer of a
`$questionnaire-package` request that carries no coverage, and of a CRD request the gateway
originates for its own workflow (`ORIGINATION_PROFILE`). That request is the gateway's own,
not your EHR's message with something filled in, so it carries the Coverages it is routed
by, with their payors, and its `$questionnaire-package` and PAS legs, and the inquiry about a
pended prior authorization, name the same coverage. With `ENRICH_NATIVE_REQUESTS=true` the
payer is still chosen from every Coverage, active first: the coverage the gateway adds is only
the template's search (below), and the request is routed on it when it finds a Coverage, or by
this read when it finds none or your system of record cannot answer it. A member with no
active coverage is therefore routed as without the opt-in, and its request carries `null`
coverage.
For each key:

- **Your EHR sent it** (a resource, a Bundle, or `null`): it is sent unchanged.
- **Your EHR left it out, by default:** it is left out of what the payer receives; nothing
  is added. The one value the gateway still reads is the coverage, when your EHR left it out:
  it is searched in your system of record (every Coverage, routed on the active ones first,
  as above; under the Patient id your system names for the member, recorded as a
  `prefetch.obtained` event) only to find the payer to route the request to, and is not
  added to the request. Coverages naming more than one payer (among the active ones, or,
  when none is active, among all of them) are refused with `422 ambiguous coverage for
  routing`. A request whose coverage your system of
  record cannot supply is refused as below (`422`, or `503` when your server is unavailable);
  one whose `prefetch.coverage` is `null`, or for whose member your system holds no Coverage,
  is refused `412 no coverage in request or system of record`. One that leaves out the coverage
  of a member your system does not hold has nothing there to be routed by: by default its
  coverage is read through the request's own `fhirServer` instead, to route by, and from
  v0.61.0 carried as `prefetch.coverage` once the request is routed (see
  [Reading the coverage through `fhirServer`](#reading-the-coverage-through-fhirserver)), and a
  request that names no `fhirServer` is refused with `412 no coverage to route by: send
  prefetch.coverage or fhirServer (this gateway's system of record names no patient for this
  member)`. A coverage read through `fhirServer` is carried to the payer from v0.61.0 (see
  that section); a coverage read from your system of record is not. Otherwise the payer
  receives a request without the
  coverage, as it would from your EHR directly; a payer's gateway that runs `strict` refuses
  such a request (`400`), and its answer is relayed to your EHR. So send every prefetch value
  you want the payer to see.
- **Your EHR left it out, with `ENRICH_NATIVE_REQUESTS=true`:** the gateway obtains it from
  your system of record, for the Patient your connector names for the member, and adds it to
  the request's `prefetch` (nothing else in the request changes):
  - `patient` is read (`Patient/<id>`) and sent exactly as your server returned it. No such
    Patient is a `422 patient not found in system of record` at enforcement `strict`; below
    `strict` the key is left out (recorded as a finding at `observe`) and every other key,
    `coverage` included, is still obtained. (See the member id limitation below for when
    nothing is obtained.)
  - `coverage` and the history keys are searched (`<type>?patient=Patient/<id>&status=<the
    template's codes>`, the coverage search with `_include=Coverage:payor` and the device
    search with `_include=DeviceRequest:performer`, within the connector's search bounds;
    from v0.60.0 with the status filter). The value
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
    their resource type and id. A search with no match sends `null`. For `coverage` the
    request is still routed: on the active Coverages the search returned (the others only
    when none is active), or, when it found none, by the read that chooses the payer above
    (every Coverage, active first, not added), so a member with no active coverage reaches
    its payer as without the opt-in and the request carries `"coverage": null`.
  - A search your system of record cannot answer (unavailable, not supported, not a
    searchset, or over the bounds) leaves the key out. For `coverage` the request is still
    routed by the read that chooses the payer (every Coverage, active first, not added), as
    without the opt-in; only when that read fails too is it refused, at every level: `503
    coverage unavailable from system of record` when your server is unavailable, otherwise a
    `422`. A missing history key is left for the payer to decide on. When your system of
    record cannot answer for the patient at all, a request that carries no coverage is
    refused with that failure at every level; one that carries its own coverage is refused at
    `strict` and, below `strict`, sent with its own values and nothing obtained. A connector
    that does not implement search (the built-in FHIR connector does) can obtain only
    `patient`.
- **Every value must be about the request's patient.** Each resource is checked against
  the patient by its FHIR Patient-compartment reference (for example
  `Coverage.beneficiary`, `ServiceRequest.subject`); a value holding another patient's
  record is refused — at enforcement `strict`, `403` for a value your EHR sent (below
  `strict` it is sent as your EHR sent it, recorded as a finding at `observe`); at every
  level, `502 system of record returned another patient's resource` for one your system of
  record returned, and nothing is sent. Absolute references on your EHR's `fhirServer` are read like relative ones, both here
  and when the request's patient references (`context.patientId`, each draft order's subject,
  each prefetch resource's patient) are bound to one patient. From shn-gateway v0.61.0 one
  other person's record passes this check: a dependent's Coverage may name the parent, its
  `subscriber` or `policyHolder`, as a contained `Patient` (carrying an MRN, or the parent's
  own member identifier). It is part of the Coverage and is carried with it byte for byte; the
  Coverage's `beneficiary` must still be the request's patient, the contained Patient must be
  referenced by those two slots' own references only, and it may contain nothing. It is never
  read as the patient: a gateway deriving the identity of a member its system of record does not
  hold reads the member's own Patient.
- **No opaque content.** A `Binary` resource (a value, a Bundle entry or a contained
  resource) is never added to a prefetch value: `502 system of record returned a Binary
  resource` at every level when your system of record returned it. One your EHR sent is
  refused with `403` at enforcement `strict`, and sent as your EHR sent it below `strict`.
- **The payer is found from the request.** A coverage your EHR sent that references its payor
  Organization is resolved against every prefetch value the request carries (a resource, or
  the entries of a Bundle value), whatever its key: an absolute reference (a URL, or a
  `urn:uuid`) names the Bundle entry whose `fullUrl` equals it, and `Organization/<id>` the
  resource with that type and id. Several that answer must each be an Organization naming
  the same payer identifier; otherwise the request is refused, at every level, with `422 no
  payer identifier on member coverage: Coverage.payor matches more than one resource of the
  request, and they do not name one payer` (for a coverage your system of record supplied,
  `… of the request and the system of record's Coverage search …`: its search's records are
  among the values), and the payor is then never read from your system of record or
  `fhirServer`. An EHR that fulfils the coverage template exactly sends no Organization (the
  template asks for no `_include`), so a payor the request does not resolve is read by the
  gateway, only to choose the payer: nothing is added to or changed in what is carried. `Organization/<id>` is an id on your EHR's server, so where it is
  read depends on the request's `fhirServer`:

  | The request's `fhirServer` | Where the payor Organization is read |
  |---|---|
  | none (absent, `null` or empty) | your system of record, when your connector names the patient by `context.patientId` |
  | your system of record's own FHIR base | your system of record, when your connector names the patient by `context.patientId`; otherwise, or when your system holds no such Organization, once through `fhirServer` |
  | any other base | only once through `fhirServer`, never your system of record (its ids are another server's), even for a patient id it shares |

  The bases are compared with the scheme and host lowercased, the default port (`443` for
  `https`) dropped and a trailing slash trimmed; your system of record's base is
  `FHIR_DATA_URL` (a connector built on the engine names it with
  `engine.SystemOfRecordFHIRBase`; one that does not is never the same base). The read through
  `fhirServer` is `GET {fhirServer}/Organization/<id>` with the request's `fhirAuthorization`
  token, within the checks and the time budget of [Reading the coverage through
  `fhirServer`](#reading-the-coverage-through-fhirserver), not with
  `CDS_FHIR_SERVER_READ=off`. It is recorded as a `prefetch.obtained` event with key
  `coverage`, `source` `fhirServer` and `reason` `payor Organization of the coverage the EHR
  sent` (followed by the refusal's reason when refused), and logged as that read, not as a
  coverage. Its refusals keep that section's statuses, with the reason after `no payer
  identifier on member coverage: ` (the request carried a coverage): a `404` or `410` is `412
  no payer identifier on member coverage: fhirServer holds no Organization for the coverage's
  payor`, a refused token `412 no payer identifier on member coverage: fhirServer refused the
  fhirAuthorization token`, an answer that is not that Organization `502 no payer identifier
  on member coverage: fhirServer's answer is not the payor Organization`. A failed read of
  your system of record is its failure status (`503 system of record unavailable`, or `502`
  for an answer it cannot read), and `fhirServer` is then not read. Coverages naming the same
  Organization share the read; two different Organizations named by reference alone are
  refused `422 no payer identifier on member coverage: the request's coverages name more than
  one payor Organization by reference alone`. A reference to another server, a versioned one,
  or one with a fragment, a leading slash or a dot segment is never read (nor, with
  `CDS_FHIR_SERVER_READ=off`, one absolute on `fhirServer` itself, except in your system of
  record at the same base as above). When nothing resolves the payor the request is refused
  before the network, at every level, with the reason and then the remedy that resolves that
  reference:
  - `Organization/<id>`: `422 no payer identifier on member coverage: Coverage.payor is a
    reference to an Organization the gateway could not read; send the payor Organization with
    the coverage, or a payor identifier`;
  - an absolute reference that a Bundle entry's `fullUrl` can be (no `/_history/`, query,
    fragment or dot segment): `422 no payer identifier on member coverage: Coverage.payor is a
    reference to an Organization the gateway could not resolve; send the payor Organization as
    a Bundle entry whose fullUrl is that reference, or a payor identifier`. The Organization as
    an entry, with that `fullUrl`, of any Bundle value the request carries (the coverage
    value's own, or another prefetch value) routes it; one sent as a prefetch value by itself
    does not, since that resolves `Organization/<id>` only;
  - any other reference to an Organization (versioned, with a fragment, a leading slash or a
    dot segment): `422 no payer identifier on member coverage: Coverage.payor is a reference to
    an Organization the gateway does not read; send a payor identifier with the coverage`;
  - from shn-gateway v0.61.0, a `urn:uuid:` or `urn:oid:` reference: `422 no payer identifier
    on member coverage: Coverage.payor is a urn reference no Bundle entry's fullUrl matches;
    send the payor Organization as a Bundle entry whose fullUrl is that reference, or a payor
    identifier`.

  A payor of another kind that nothing resolves (a `RelatedPerson`, a `Patient`) keeps the
  bare `422 no payer identifier on member coverage`. From shn-gateway v0.61.0 a payor
  reference that resolved, wherever it was read, to a resource naming no payer identifier is
  refused with its reason: `the payor Organization carries no identifier with both a system
  and a value, such as a NAIC code or payer id`, or `Coverage.payor references a resource
  that is not an Organization` (v0.60.0 answers the bare text). Every one of these refusals is
  `no payer identifier on member coverage`, `: ` and the reason, as a PAS Bundle's is ("A PAS
  Bundle's payor", under [Per-role](#per-role)); only the references nothing resolved add a
  remedy, after the reason.
  Under the earlier templates `_include=Coverage:payor`
  brought the Organization with the coverage, so such a request was routed with no read. Now
  it is routed only when your system of record resolves the payor as above or the `fhirServer`
  read succeeds, and is otherwise refused with the reason: for example a member your system
  does not hold, or holds without the Organization, and no `fhirServer` (`422`);
  `CDS_FHIR_SERVER_READ=off` with a `fhirServer` at another base (`422`); or a token that does
  not allow the Organization read (`412`). A payor identifier avoids all of these, and so
  does the payor Organization sent with the coverage where its reference resolves: contained
  (referenced as `#<id>`), as another prefetch value for `Organization/<id>`, or as an entry
  of a Bundle value whose `fullUrl` is the absolute reference. For a
  coverage the gateway itself read, your system of record (or the request's `fhirServer`) is
  read for the payor only when the coverage came from there and no prefetch value answers
  the reference.
- **Signed content is never edited.** A value your EHR sent, signed or not, is sent byte
  for byte; added keys sit beside it.

Each key the gateway tried to obtain (with `ENRICH_NATIVE_REQUESTS=true`; by default, only
the coverage read to route by) is recorded: a log line, and, when an observer is
configured, a `prefetch.obtained` event carrying the key, where it was read (`source`:
`system-of-record`, or `fhirServer` for the coverage read through the request's own server and
the payor Organization read for a coverage your EHR sent),
the search it ran, its outcome, the match and page counts and the time — never the value.
With `ENRICH_NATIVE_REQUESTS=true`, `coverage` can record two events: the template's search,
then, when that search finds no Coverage or cannot be answered, the read that chooses the
payer, whose value is not carried. Nothing about where a value came from is added to the
request.

**Member id limitation (with `ENRICH_NATIVE_REQUESTS=true`).** `context.patientId` must be
the patient's network member id, and the request's own patient references (the order's
subject, the coverage's beneficiary) must use it. Values from your system of record name the
patient by your server's own Patient id, so the gateway adds values only when that id is the
member id. (By default nothing is added, and the coverage read to route by is searched under
your server's own Patient id, so this limitation does not apply.) The opt-in never leaves a
member with less to route by than the default; a prefetch value it cannot fill is decided by
`strict` (below). When your server (as your connector reports it)
names the patient differently:

- a request that leaves out `coverage` is routed as without the opt-in (from v0.61.0; before,
  it was refused `422`): the coverage is searched under your server's own Patient id, only to
  choose the payer, and is not added to the request. A Coverage that search returns about
  another patient (one naming `Patient/<member id>`, which on your server is another patient)
  is refused `502`, as without the opt-in;
- a request that leaves out `patient` is refused before anything is read or sent at
  enforcement `strict`: `422 system of record names the patient differently from
  context.patientId; supply the patient prefetch in the request` (before v0.61.0 the text
  ended `supply patient and coverage prefetch in the request`). Below `strict` it is sent
  without it, and its coverage, if it leaves that out too, is read to route by as above;
- a history key the request leaves out is left out; nothing is searched, and the
  `prefetch.obtained` event records the outcome `not-run` with the reason `patient named
  differently in the system of record`. The request is sent.

Values your EHR sends are not affected. To have the gateway fill in prefetch, set
`ENRICH_NATIVE_REQUESTS=true` and identify the patient on your server by the member id;
otherwise send `patient` and `coverage` (and any history your EHR has) in the request.

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
the Coverages it chose to route by (the active ones your system of record holds for the
member, else the others; each record exactly), the order as the payer returned it, the
payer's `coverage-assertion-id` as `context` when the answer gives one, and **one**
questionnaire: the first the payer's answer names, exactly as stated (a `|version` kept). A
payer answer naming several questionnaires for the order has only the first requested (a
known limitation). At DTR 2.2, where `coverage` is 1..1, a choice of several Coverages (several
active ones, or, when none is active, several others) refuses the request (422).

### Reading the coverage through `fhirServer`

A CDS Hooks request that carries no `prefetch.coverage`, for a member your system of record
does not hold, has nothing in your system to route by. By default the gateway then reads that
member's coverage through the EHR FHIR server the request names (`fhirServer`, with its
`fhirAuthorization` token), as a CRD service your EHR called directly would, to choose the
payer. Because `fhirServer` and `fhirAuthorization` are removed before the network, the payer
cannot read that coverage itself, so from v0.61.0 the gateway carries the records the payer was
chosen by as the request's `prefetch.coverage` (registered edit E-07; before v0.61.0 nothing it
read was carried, and a payer that needs the coverage, such as the Da Vinci reference payer,
answered `400`). `CDS_FHIR_SERVER_READ` sets where it may read, or turns the read off, and then
nothing is read or carried:

| Value | What the gateway reads |
|---|---|
| `private` (the default; unset is the same) | An `https` server in your own network or on the internet: on any port at a private address (RFC 1918, IPv6 unique-local, carrier-grade NAT and the like), on port 443 at a public one. Never an address no mode reads: loopback, link-local (the cloud metadata and credential endpoints `169.254.169.254` and `169.254.170.2` included), `fd00:ec2::254`, `fd00:ec2::23`, `fd20:ce::254`, `100.100.100.200`, unspecified, multicast, broadcast, `0.0.0.0/8`, `240.0.0.0/4`, `::/96`, a zoned address, or a NAT64 or 6to4 address embedding any of these. |
| `public` | Only an `https` server on port 443 at a public address: never a private, carrier-grade NAT, documentation or benchmarking address, nor any of the above. For a gateway whose own network is not its participant's: every gateway SHN hosts or runs uses it. |
| `off` | Nothing: the gateway never calls `fhirServer`, carries nothing, and such a request is refused `412 no coverage to route by: send prefetch.coverage (this gateway's system of record names no patient for this member, and it does not read fhirServer)`. |

Any other value refuses to boot, and so does `private` or `public` set without
`PROVIDER_DAVINCI_INGRESS` (left unset, the key needs no ingress). With the read on, a request
that names no `fhirServer` (or a `null` or empty one) has nothing to read and is refused `412 no
coverage to route by: send prefetch.coverage or fhirServer (this gateway's system of record
names no patient for this member)`; with it `off`, every such request is refused `412 no
coverage to route by: send prefetch.coverage (this gateway's system of record names no patient
for this member, and it does not read fhirServer)`.

**A gateway SHN hosts.** A hosted gateway's own network is SHN's, not yours, so SHN sets
`CDS_FHIR_SERVER_READ=public` on every hosted provider tenant with the ingress on. You may set
the tenant's key to `off`, never to `private`. Gateway releases before v0.60.0 ignore the key and
never read `fhirServer`.

- **A Coverage search, to route by.** `GET {fhirServer}/Coverage?patient={context.patientId}`
  (no status filter, and no `_include`: CDS Hooks does not ask a client to support it; one page) with
  `Accept: application/fhir+json`, and `Authorization: Bearer <access_token>` when the
  request carries `fhirAuthorization` (its `token_type` must be `Bearer`, its `access_token`
  non-empty, at most 8 KiB and without whitespace; a `null` one sends no token). The read
  presents nothing of the gateway's own: no other credential, no client certificate. A `null`
  or empty `fhirServer` names no server. When any Coverage in the answer is
  `active`, the request is routed on the active ones only, so a stale cancelled Coverage naming
  another payer never makes routing ambiguous; when none is, it is routed on the others,
  provided they name one payer, the party to answer that the member is not covered. The payer
  is chosen from each chosen Coverage's payor: its own `identifier`, or the identifier of the
  Organization it references when the answer resolves it (an Organization entry or a contained
  one). Chosen Coverages naming two payers are refused as ambiguous (`422 ambiguous coverage
  for routing: …`).
- **At most one more read, of the payor Organization.** When a Coverage names its payor only
  by reference (`Organization/<id>`, or the same written absolute on the `fhirServer` base) and
  nothing in the answer resolves it, the gateway reads `GET {fhirServer}/Organization/<id>`
  once, with the same token and the same checks. Coverages naming the same Organization share
  that read; two different Organizations named by reference alone are refused (below). A
  reference to another server, or a versioned one, is never read, and such a Coverage is
  refused as any coverage without a payer (`422 no payer identifier on member coverage`), as
  is an Organization that carries no payer identifier. (The same single read resolves the
  payor of a coverage your EHR sent; see [CDS Hooks prefetch](#cds-hooks-prefetch).) So your FHIR server must allow a
  Coverage search and an Organization read with the token.
- **What routing used is carried (from v0.61.0).** Once the request is routed, the gateway
  adds `prefetch.coverage` (creating `prefetch` when the request has none): a `searchset` it
  writes, with a `match` entry for each Coverage the payer was chosen by (the active ones, else
  all of them, in your server's order) and an `include` entry for the payor Organization the
  payer was chosen by for each of them (the first payor, as routing reads it; one with a payor
  identifier of its own needs none), whether your server's search returned it or the gateway
  read it. Each
  resource is exactly the bytes your server returned; each entry's `fullUrl` is a `urn:uuid:`
  the gateway assigns, with one exception: an included Organization that a carried Coverage
  names by an absolute reference on the request's `fhirServer` base (compared with the scheme
  and host lowercased, the default port dropped and a trailing slash trimmed) has that
  reference, exactly as the Coverage writes it, as its `fullUrl`, so the payer resolves it
  in the `searchset` (the reference is already in the Coverage's own bytes). A record has at
  most one such reference: a search entry answers an absolute reference only when its `fullUrl`
  is that reference, and the Organization read only the reference written on the `fhirServer`
  base itself, so two chosen Coverages that write the Organization's address differently each
  resolve their own record, and both are carried, each under the reference that resolved it.
  If your server's entry addresses use another base than the `fhirServer` URL it hands out, a
  Coverage whose absolute payor reference is on that other base has its Organization carried
  under a `urn:uuid:`, which that reference does not resolve: the payer may answer `400` (or
  `422` from a payer gateway that maps its identity), where v0.60.0 carried no coverage. `total`
  is the number of Coverages. The records are byte for byte, so any reference a record holds (an absolute
  one on your server included) is carried as written. Nothing else of your server's answer
  is carried: not its links, its entry addresses (`fullUrl`s on your server), its Bundle `id` or
  `meta`, nor its `OperationOutcome` entries, nor a Coverage that was not chosen or an
  Organization only such a Coverage names. Everything carried passes the same patient check as
  the read: another patient's record is never carried. The rest of the request is your EHR's,
  byte for byte, with `fhirServer` and `fhirAuthorization` still removed. A request refused
  while routing carries nothing and is not sent. A request that carries a `coverage` prefetch
  key, even `null`, is never given another, and a coverage your system of record supplied, or
  the payor Organization read for a coverage your EHR sent, is never carried this way. Before
  v0.61.0 nothing read was carried.
- **Checked in every mode.** An `https` base URL with no query, fragment or credentials, and
  a port from 1 to 65535 when it names one; the mode's address rules applied to an address
  literal, to every address the host resolves to, and again to the address the connection is
  made to, which is only one that was checked. At most two reads a request. Redirects are never
  followed, no proxy is used, TLS (1.2 or later) is verified, each connection has 2 s to
  connect and 2 s for TLS, both reads share one 4 s budget, and each answer may be at most
  512 KiB, with its headers at most 64 KiB (an answer whose headers are larger reads as not
  reached). Each read asks for `application/fhir+json` and accepts an answer whose
  `Content-Type` is `application/fhir+json` or `application/json` (parameters such as
  `charset` aside); any other, or none, is refused as not a Coverage searchset, or on the
  Organization read as not the payor Organization.
- **Recorded.** Each read is its own `prefetch.obtained` event with key `coverage`, `source`
  `fhirServer` and the read's path and query (never the host); the gateway's log names the
  server's host only, never the token or the path. The read of the payor of a coverage your EHR
  sent (see [CDS Hooks prefetch](#cds-hooks-prefetch)) has the `reason` `payor Organization of
  the coverage the EHR sent`. Its outcome is a system-of-record search's:
  `ok`, `zero` (no Coverage, or no such Organization), `unavailable` (not reached, not in time,
  or an error status, a refused token included), `bound` (over 512 KiB), `malformed` (not a
  Coverage searchset or the Organization asked for, a redirect, or another patient's coverage),
  or `not-run` (refused by the request's own values or the address rules before any request).

A read that cannot give a coverage to route by refuses the request at every enforcement level
(the Organization read's other refusals are the search's). Each is CDS Hooks' `412` (the
service could not obtain the data it needs) unless the table says otherwise: routing ambiguity
is `422`, and an answer that contradicts the request is `502`. The `412`s and the `422` are the
network's own routing refusal (there is nothing to route by), not a judgment of your EHR's
request, and the exchange record names your gateway as the refusing party, rule `routing`. The
`502`s are your EHR's server answering wrongly, recorded as `other`. Each begins `no coverage to
route by: `; on the read of the payor of a coverage your EHR sent (see
[CDS Hooks prefetch](#cds-hooks-prefetch)), which carried a coverage, the same reason and status
begin `no payer identifier on member coverage: ` instead (two Organizations by reference alone:
`422 no payer identifier on member coverage: the request's coverages name more than one payor
Organization by reference alone`):

| Refusal | Status |
|---|---|
| `fhirServer is not an absolute base URL (no query or fragment)` (a port outside 1–65535 included) | 412 |
| `fhirServer must be https` | 412 |
| `fhirServer must not carry credentials` | 412 |
| `fhirAuthorization is not a bearer token` | 412 |
| `fhirServer's address is one a gateway never reads (loopback, link-local, metadata or reserved)` (`private`) | 412 |
| `fhirServer at a public address must use port 443` (`private`) | 412 |
| `fhirServer must use port 443` (`public`) | 412 |
| `fhirServer's address is not public` (`public`) | 412 |
| `fhirServer's host did not resolve` | 412 |
| `fhirServer redirected; redirects are not followed` | 412 |
| `fhirServer's TLS could not be verified` | 412 |
| `fhirServer could not be reached` (including an answer whose headers exceed 64 KiB) | 412 |
| `fhirServer did not answer in time` | 412 |
| `fhirServer's answer exceeds 512 KiB` | 412 |
| `fhirServer refused the fhirAuthorization token` (a `401` or `403`) | 412 |
| `fhirServer answered with an error` (any other status but `200`, a `404` on the Coverage search included) | 412 |
| `fhirServer's answer is not a Coverage searchset` (not JSON, not a `searchset` Bundle, an entry that is not a Coverage, Organization or OperationOutcome, or one the patient check cannot use, such as a repeated `fullUrl`, a contained resource with no `id`, or a patient it cannot identify as the request's; from shn-gateway v0.61.0 a dependent's Coverage naming the parent as a contained `Patient` in its `subscriber` or `policyHolder` only is used) | 412 |
| `fhirServer holds no Coverage for the patient` | 412 |
| `fhirServer holds no Organization for the coverage's payor` (a `404` or `410` on the Organization read) | 412 |
| `fhirServer returned another patient's coverage` (a Coverage whose patient reference names another patient: another `Patient` id, or the member identifier with another member's value) | 502 |
| `fhirServer's answer is not the payor Organization` (not JSON, or not the Organization asked for) | 502 |
| `fhirServer's coverages name more than one payor Organization by reference alone` | 422 |

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

Each operation the gateway forwards to your system (the CRD service call,
the DTR, PAS and eligibility operations) carries `X-Correlation-Id` with the
leg's id — the `correlationId` of the gateway's access line, and the
`X-SHN-Leg-Id` the provider's system was answered with — so your own logs name
the exchange the network knows it by. The CDS service listing, connectivity
probes and token requests do not carry it. It is sent when the id is one
token of letters, digits, `.`, `_` or `-`, up to 64 characters (a PAS Claim
may name its own id; one outside that shape is not sent), and it is never a
value you configure: `PAYER_DAVINCI_BACKEND_HEADERS` cannot set it. The
message bytes are unchanged: a system that ignores the header receives
exactly what it did before. If your system validates `X-Correlation-Id` its
own way, set `PAYER_DAVINCI_BACKEND_CORRELATION=off` and it is not sent.

| Env var | Description |
|---|---|
| `PAYER_DAVINCI_BASE_URL` | Base URL of the payer's own Da Vinci endpoint (e.g. `https://api.payer.example/davinci`). **Required for `ROLE=payer`.** Every Da Vinci leg — CRD, DTR and PAS — is answered there; the gateway has no in-process payer of its own, so a `role=payer` gateway without this refuses to boot with an error naming it. |
| `PAYER_DAVINCI_CDS_BASE_URL` | Base URL for the partner's CDS Hooks (CRD) posts when they are **not** co-located with the FHIR base — e.g. a payer that serves `/cds-services` at the root but FHIR ops under `/fhir`. Empty ⇒ CDS uses `PAYER_DAVINCI_BASE_URL`. |
| `PAYER_DAVINCI_DTR_BASE_URL` | Base URL for the partner's DTR operations (`/Questionnaire/$questionnaire-package`, `/Questionnaire/$next-question`) when they are **not** co-located with the PAS base — e.g. a payer that serves DTR under `/dtr` and PAS under `/pas`. Empty ⇒ DTR uses `PAYER_DAVINCI_BASE_URL`. Requires `PAYER_DAVINCI_BASE_URL`. |
| `PAYER_DAVINCI_PAS_BASE_URL` | Base URL for the partner's PAS operations (`/Claim/$submit` for submit and update) when they are **not** co-located with the DTR base. Empty ⇒ PAS uses `PAYER_DAVINCI_BASE_URL`. Requires `PAYER_DAVINCI_BASE_URL`. |
| `PAYER_ELIGIBILITY_URL` | Your system's own coverage-eligibility endpoint, when it has one: the absolute `http(s)` URL it takes a `POST` of a `CoverageEligibilityRequest` on (FHIR R4 defines no standard eligibility operation, so give the exact URL). Unset (the default): the gateway answers eligibility from your system of record's Coverage, or, when it keeps none (`FHIR_DATA_URL` unset), answers `501` "coverage eligibility is not offered by this payer". Set: the request is `$validate`d at the conformance level (an invalid one is refused `422` at `structural`, for a structural issue, and at `strict`, and your system is not called), then carried there exactly, with the same authentication (`PAYER_DAVINCI_TOKEN_URL` and the client settings) and `PAYER_DAVINCI_BACKEND_HEADERS` as your other operations, and your answer is relayed as your system sent it; an error answer is relayed as your error. If your system gives no answer (unreachable, timed out), the requester is told so (`502`, saying whether your system received the request), as on your other operations; no answer is built from your records in its place. An answer about a patient other than the request's is relayed at `none`, `observe` and `structural` (recorded at `observe` and `structural`) and refused at `strict`; an answer that names its patient otherwise than by reference (an identifier only) is treated the same way; an answer that cannot be read as a `CoverageEligibilityResponse`, or names no patient at all, is relayed at `none` and `observe` and refused (`502`) at `structural` and `strict`; an answer naming the patient by your own Patient id for the member the request names is the same patient, and is relayed at every level. Telling the two apart reads your system of record only when the answer names a patient other than the request's, and never at `none`; with no system of record, the patient is one it does not hold; if that read fails, the answer is relayed at `observe` and `structural` (recorded as unavailable) and refused with the system-of-record failure at `strict`. Unlike the relayed PAS and questionnaire answers, your eligibility answer is `$validate`d at the conformance level: not at all at `none`, recorded at `observe`, refused (`502`) at `strict`, and at `structural` refused for a structural issue and recorded for a deeper one; a validator your gateway cannot reach refuses with `500` at `strict` and is recorded at `observe` and `structural`. A token naming another patient is not refused: the member the request names is bound by your records, as on the prior-authorization legs. Requires `PAYER_DAVINCI_BASE_URL`; a provider gateway refuses to boot with it. |

**Where each operation goes.** For every forward the gateway resolves the URL in this order: the endpoint your partner publishes for that contract line in its `.well-known/davinci-configuration` (the partner's own published rule, honored only when it is same-origin with the base that contract uses), then the per-operation base if you set one, then `PAYER_DAVINCI_BASE_URL`. The per-operation bases exist for partners that split DTR and PAS across bases and publish no `.well-known/davinci-configuration`; where the partner publishes one, its endpoints win and the bases are only the fallback.

| Env var | Description |
|---|---|
| `PAYER_DAVINCI_TOKEN_URL` | SMART Backend Services token endpoint for the partner. Required if the partner requires authentication. |
| `PAYER_DAVINCI_CLIENT_ID` | SMART client id for the partner. Required when `PAYER_DAVINCI_TOKEN_URL` is set. |
| `PAYER_DAVINCI_CLIENT_KEY` | Path to the SMART client's private-key PEM file (the value is a path, not the key text — mount the file into the container). Required for `private_key_jwt` mode (i.e. when `PAYER_DAVINCI_CLIENT_SECRET` is unset). |
| `PAYER_DAVINCI_CLIENT_ALG` | `ES384` or `RS384`. Required for `private_key_jwt` mode (i.e. when `PAYER_DAVINCI_CLIENT_SECRET` is unset). |
| `PAYER_DAVINCI_SCOPE` | Requested scope the gateway asks your token endpoint for. Default `system/*.read` (covers the read-only legs). Must be a scope your authorization server grants this client; it must cover `/Claim/$submit` and `/Claim/$inquire` as well as the read-only legs, since every leg forwards. |
| `PAYER_DAVINCI_CLIENT_KID` | Key id for the client assertion JWK, if the partner requires it. |
| `PAYER_DAVINCI_CLIENT_SECRET` | OAuth2 client secret for the `client_secret_post` `client_credentials` grant — for authorization servers that cannot issue asymmetric credentials. The value is the secret **itself, not a path** (unlike `PAYER_DAVINCI_CLIENT_KEY`). Mutually exclusive with `PAYER_DAVINCI_CLIENT_KEY`/`_ALG`/`_KID`; prefer `private_key_jwt` when your server supports it. |
| `PAYER_DAVINCI_PAS_NATIVE` | **No longer a switch.** PAS submit/update always forward to the payer's `/Claim/$submit` along with every other leg; there is no in-process PAS fallback to select. Setting it to any value changes nothing. From shn-gateway v0.60.0, a gateway that sets it, to any value, logs `gateway: PAYER_DAVINCI_PAS_NATIVE is set but is no longer a switch` at boot, and one that leaves it unset logs nothing about it; remove it. Releases v0.39.0 through v0.59.x log a similar notice whenever it is not `true`, unset included. |
| `PAYER_DAVINCI_CRD_SERVICE_ID` | Optional: names your CDS service for `order-select` and `order-sign` requests. Empty (the default) ⇒ each request goes to the one service your CDS service listing offers for the request's hook (see [CDS service selection](#cds-service-selection)). The named service must be in your listing and answer the request's hook; any other request is refused. |
| `PAYER_DAVINCI_DISPATCH_SERVICE_ID` | Optional: names your CDS service for `order-dispatch` requests, with the same rules. Empty ⇒ the service your listing offers for `order-dispatch`. |
| `PAYER_DAVINCI_CONTRACT_VERSIONS` | Declared Da Vinci contract versions for the partner payer, comma-separated `<contract>@<line>` tokens (e.g. `pa.pas@2.0, pa.crd@2.0`). Two things read this: the connectivity checks verify the partner's published capability against it (`version-drift` on disagreement, FR-G46), and native-forward routing refuses (before forwarding) any leg whose contract shares no line with it (FR-G48). Requires `PAYER_DAVINCI_BASE_URL`. Unset ⇒ native-forward legs are unfiltered (today's default) and the checks skip the drift comparison. From shn-gateway v0.59.0 it is also this gateway's own declaration when `SHN_CONTRACT_VERSIONS` is unset (see [Exchange contract lines](#exchange-contract-lines-shn_contract_versions)); a value that names no `pa.crd`, `pa.dtr` or `pa.pas` line, or a `pa.crd`, `pa.dtr` or `pa.pas` line this build cannot exchange, then refuses boot. Editing it then changes the declaration, so the grow-only rule below applies to it too. |
| `PAYER_DAVINCI_BACKEND_HEADERS` | Fixed request headers for a partner system that routes on one (a tenant or plan key its API gateway reads before any payload), as comma-separated `Name: value` pairs, e.g. `X-Route-Key: plan-7`. Sent on every request to the partner's bases — the CDS service listing read, each CRD post, the DTR and PAS operations (`$submit` and `$inquire`) and the connectivity probes — and **never** to `PAYER_DAVINCI_TOKEN_URL`. The message bytes are untouched: this is addressing for your own system, not a change to what the request asserts. Refused at boot: a pair that is not `name: value`, an empty name or value, a name or value that is not a valid HTTP field, a repeated name, and the names this gateway sets itself (`Authorization`, `Content-Type`, `Accept`, `Host`, `Content-Length`, `X-Correlation-Id`) or hop-by-hop names (`Connection`, `Transfer-Encoding`, `Upgrade`, …). A value cannot contain a comma. Requires `PAYER_DAVINCI_BASE_URL`. |
| `PAYER_DAVINCI_BACKEND_TIMEOUT` | How long this gateway waits for your system on each operation it forwards, counted from the leg's arrival at this gateway, as a Go duration from `1s` to `28s` (from v0.59.0). Unset: `25s`, under the requester's 30 s leg budget by enough that a system slower than it is answered as its own timeout, this gateway's framed `504`, while the requester is still waiting, and recorded as your system's `timeout`. Any other value refuses the boot, as does setting it without `PAYER_DAVINCI_BASE_URL`. |
| `PAYER_DAVINCI_BACKEND_CORRELATION` | Whether each operation forwarded to your system carries `X-Correlation-Id` with the leg's id. Unset (or `on`, the default): it does. `off`: no `X-Correlation-Id` is sent, for a system that validates that header its own way; nothing else changes. Any other value refuses the boot, as does `off` without `PAYER_DAVINCI_BASE_URL`. `PAYER_DAVINCI_BACKEND_HEADERS` can never name `X-Correlation-Id`, whatever this is set to. |
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
action has a `type` and a `description`). At enforcement `strict` an answer that breaks one
is refused with 502 `payer CRD response is not a valid CDS Hooks response: <rule> at <path>`
rather than repaired, and the provider's gateway applies the same rules; below `strict` it is
relayed exactly as your system sent it (recorded as a finding at `observe`). An answer that
repeats a member name is refused at every level. Your gateway carries the answer with the
media type your system sent (`application/json` when it sent none); the provider's gateway
returns it to the EHR as `application/json`, the CDS Hooks media type. Coverage information belongs in a system
action that updates the order; each FHIR resource your answer embeds is also validated at the
routed CRD line, but only to record the outcome (the `crd.embedded.validated` observation,
at `observe` and `strict`; from v0.61.0 it carries `ms`, the time the `$validate` call took): it
never changes or refuses your answer. A non-2xx answer is relayed as your system's error.

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
| `PAYER_DAVINCI_PAYOR_OWN` | `"system\|value"` of a payer identity this deployment answers for. Setting it (together with `PAYER_DAVINCI_PAYOR_BACKEND`) is what **switches the mapping on**; the feed then adds your other published identities to it. See "which identities count as yours" below. |
| `PAYER_DAVINCI_PAYOR_BACKEND` | `"system\|value"` your own backend expects instead — the identity to re-stamp onto outbound requests before they reach it. |

When both are set, every CDS Hooks (`order-select`, `order-sign` and `order-dispatch`), DTR,
and PAS request this gateway forwards to your system has the payor identifier of **every**
`Coverage` it carries re-stamped to
`PAYER_DAVINCI_PAYOR_BACKEND` — but **only** when the inbound identifier is genuinely your
own. The identifier is the `Coverage.payor` identifier itself, or the first identifier
with a system and value on the payer `Organization` it references (contained in the
Coverage, or another resource of the same Bundle or prefetch). Each Coverage is read by its
first `payor`, the one the network routes on. A PAS Claim's `insurer` is resolved the same
way and re-stamped too when it names your identity; an insurer that names you under
another identifier (for example your NPI) is left as sent.

**Which identities count as yours.** The pair above is all-or-nothing: setting
`PAYER_DAVINCI_PAYOR_OWN` and `PAYER_DAVINCI_PAYOR_BACKEND` together turns the mapping on
(setting one alone is a startup error), and `PAYER_DAVINCI_PAYOR_OWN` is always one of the
identities you answer for. What the feed does is **add to that set**, never replace it.

A payer may publish several identities on the network feed, and the routing directory is
many-to-many (see "How a payer publishes its identity into the feed" above), so a requester
may address you under any of them. Once the mapping is on, the identities this gateway
answers for are therefore **`PAYER_DAVINCI_PAYOR_OWN` plus every identity your own holder
publishes on the feed** — each of them is re-stamped to `PAYER_DAVINCI_PAYOR_BACKEND` on the
way to your system. So publishing a second identity is enough on its own: no configuration
change is needed to answer on it, and the set is re-read per request, so a newly published
identity takes effect without a restart. Nothing is invented — the feed carries your own
attested claims, and an identity another holder publishes is never yours.

`PAYER_DAVINCI_PAYOR_OWN` decides **alone** whenever this gateway cannot see its own entry
among the holders it has converged: you run no registrar (`SHN_REGISTRAR_URL` unset), your
registration has not propagated to the feed yet, or your entry was skipped because its
published encryption key would not decode. (An unreachable registrar at startup is a boot
failure, not this case, and a feed outage after startup leaves the last converged snapshot
in place.) The fallback is fail-closed — it narrows what this gateway will answer for,
never widens it — and the gateway says so once:

```
gateway: payer backend identity mapping: this gateway cannot see its own payer identities on
the network feed (holder "…"); the configured identity alone decides which requests it owns
```

A request is refused with a clear error, never silently forwarded, when:

- a Coverage names a payer identifier that is none of yours, or carries no resolvable payor
  identifier at all (400) — this is an ownership check, not a blind rewrite: forwarding a misdirected
  request under your own backend's identity would have your engine adjudicate someone
  else's request. The refusal names the identifier that arrived and the identities this
  gateway answers for (up to five, then a count of the rest), so a mismatch is diagnosable
  from the error alone;
- its Coverages name more than one payer, or a Coverage payor reference resolves to no
  resource or to more than one (422, naming the reference). References are matched exactly
  (a `fullUrl`, `Type/id`, or `#id` for a contained resource); a version-specific
  (`_history`) reference is not resolved and is refused;
- at `strict`, a PAS Claim `insurer` reference resolves to no resource or to more than one
  (the same 422, naming the reference). Which payer answers is decided on the Coverages, so
  below `strict` that insurer is left as sent (recorded as a finding at `observe`) and the
  Coverages are re-stamped as usual;
- a PAS submission or amendment carries no `Coverage` at all (400 `PAS bundle missing
  Coverage.beneficiary`), at every level: there is no payor to re-stamp. Without the
  mapping, such a Bundle is refused only at `strict`;
- the identifier to re-stamp is covered by a signature inside the message (a
  `Bundle.signature`, a `Provenance` signature, or a `Signature` element) — 422
  `signed content cannot be edited (E-03, <signature>)`. The gateway never strips a
  signature and never forwards a stale one.

A CDS Hooks request that carries no Coverage at all has nothing to map, and is forwarded to your
system exactly as it arrived; your own system answers it. The network has already routed it
to you. A CDS Hooks request carries no Coverage only when both hold: its `prefetch`, or
`prefetch.coverage`, is absent or `null`, or `prefetch.coverage` is a Bundle whose `entry` is
absent, `null`, or holds only entries whose `resource` is an `OperationOutcome` (a search
that found none); and no `"resourceType": "Coverage"` object appears anywhere else in the
request (another prefetch member, `context`, a nested Bundle, a contained resource). The
`resourceType` member is matched without regard to case in its name and its value, and a
`prefetch`, `coverage` or `entry` member named in another case counts as present.
Anything else is read for its Coverage as before: a `prefetch` or `prefetch.coverage` that is
present but not a JSON object, a coverage Bundle with any other entry, and a request whose
only Coverage is outside `prefetch.coverage` are refused with the 400 above. Releases
before v0.60.0 refused such a CDS Hooks request with that 400
(`inbound Coverage carries no resolvable payor identifier`). A requester's gateway sends one
when it chose your gateway by a Coverage it read only to route (its own system of record, or,
from v0.60.0 and before v0.61.0, the EHR's `fhirServer`) and carried nothing it read. From
v0.61.0 a coverage the requester's gateway read through the EHR's `fhirServer` is carried as a
`searchset` with `urn:uuid:` entries (an included Organization that a Coverage names by an
absolute reference has that reference as its `fullUrl`), which your gateway maps like any other. A DTR request with no
coverage parameter is forwarded as it arrived too. Your conformance level still applies:
an `order-select` or `order-sign` request whose coverage beneficiary cannot be read is the
request's own shape, refused at `structural` and `strict` and carried below.

Only the identifier's `system` and `value` strings are changed. For a PAS Bundle and a CDS
Hooks request, every other byte of the request — the payer organization's name, entry
members, layout, numbers — is forwarded exactly as it arrived, and so is a DTR
`$questionnaire-package` or `$next-question` input a requester sends naming the operation.
(A questionnaire request a requester sends in the older envelope, without naming the
operation, is still assembled by the gateway from the carried canonical, order and Coverage.) When the request already carries your backend identity nothing is
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
the status or body inside it. A **transport fault** on the requester's own side (its
gateway cannot reach the Hub, or its own build or dial fails) is not an application answer
and surfaces as `"hub routing failed"`; the responding gateway's own failures are its
answer (below). From v0.60.0, an Authorization Framework that gives the requester's
gateway no answer (the connection refused, reset or closed before any response, no answer
within the gateway's HTTP client timeout, or a `503` or `504` from a proxy or load
balancer in front of it; the Framework never answers those itself) is answered
`503 {"error":"the authorization service could not be reached, so this leg was not sent to
the payer"}` (an `OperationOutcome` with issue code `transient` on the FHIR operation
routes), and the leg is recorded `unreachable`; before, it was
`502 {"error":"authorization failed"}`. The leg is not sent to the Hub: a Da Vinci ingress
call is one leg, so its caller can resend it. The gateway makes that call once more, with a fresh holder assertion, only when
the first attempt failed with a refused, reset or closed connection before its request was
written: after the write the Framework may already have decided and recorded its decision.
A `403` denial is `403 {"error":"authorization denied"}`, and any other answer from the
Framework, or one the gateway cannot read, stays `502 {"error":"authorization failed"}`;
neither is retried.

Two timeouts are named rather than left generic. One is a Hub leg that produces **no answer
within the wait your gateway gives it**. The originating gateway posts each exchange to the
Hub with an HTTP client whose timeout is the leg's whole budget — Hub, counterpart gateway
and the counterpart's own system together — and the published gateway's client waits
**30 seconds**. When that budget runs out the caller receives `504
{"error":"no answer on the hub leg within 30s (hub leg timeout)"}` (an
`OperationOutcome` with issue code `timeout` on the FHIR operation routes), and the gateway
logs one line naming the leg, the counterpart and the correlation id. On the other side, a
payer gateway that is still waiting on its participant's system when the request it serves
ends (the requester stopped waiting, or the payer gateway is shutting down) logs its own
line: `gateway: upstream payer <leg label> call abandoned after <s>s: the request it serves
ended (<cause>) (host <host>, leg <leg>, correlation <id>, request written: yes|no)`. The
cause is usually `context canceled`, since the requester's side closes the call. It names the
upstream host only, never the path, query, headers or body. It is distinct from `upstream
payer … unreachable`, which is a call that failed while the request was still live. The
other timeout is a payer gateway's own, from v0.59.0: its participant's system did not
answer within its deadline (`PAYER_DAVINCI_BACKEND_TIMEOUT`, 25 s by default, counted from
the leg's arrival), so it answers the requester its own framed `504` while the requester is
still waiting (`{"error":"the payer's system received this request but did not answer in
time; it may have acted on it: check its outcome before resending"}`, or `… could not be
reached in time` when the request was not sent), and logs `gateway: upstream payer <leg label>
call timed out after <deadline>, this gateway's deadline for its system: answered 504 (host
<host>, leg <leg>, correlation <id>, request written: yes|no)`. When the gateway's own work
before the call used the whole deadline, it does not send the operation to its system and
answers `504 {"error":"the payer's gateway ran out of time before it could send this
request to the payer's system; the payer's system did not receive it"}`, logging `gateway:
upstream payer <leg label> call not sent: this gateway's own work used its <deadline>
deadline for its system: answered 504 (leg <leg>, correlation <id>)`. When the request had
already been sent to the Hub, the text adds `; the recipient may have received this request:
check its outcome before resending`, since the Hub may have forwarded it. An embedding that
supplies its own `engine.Config.Client` sets the budget with that client's `Timeout`, which
the gateway applies as its own deadline on the leg. The number is claimed only when that
deadline is what ended the wait: a caller whose own request deadline ends the wait first
reads `504 {"error":"hub leg timed out"}` with no number, because no budget of the
gateway's was what ended it. The leg's outcome stays `unreachable` (the leg did not
complete), so the operators' `LegError` count is unchanged. A Hub that cannot be reached
— including a connection or TLS handshake that gives up before the Hub is reached — is the
generic `502 {"error":"hub routing failed"}`: the gateway will not call something a leg
timeout unless its own leg deadline, or the caller's, ended the wait. A connection that
fails after the request was sent to the Hub, or a Hub `200` that cannot be read as an
envelope, may have reached the recipient: the `502` says so (below). A Hub that answers with a refusal is reported with
`{"error":"hub refused the exchange: <reason>"}`: a `409` (a replayed envelope) keeps
its status; any other Hub `4xx` concerns this gateway's standing with the Hub (its
registration, token or clock), not the caller's request, and is a `502`. A recipient
gateway that refused the forward at its edge is `502 {"error":"the recipient's gateway
refused the exchange (<status>)"}`. An
Authorization Framework denial is `403 {"error":"authorization denied"}`. A leg the
recipient's gateway was reached for, whose answer was lost (the Hub's `X-SHN-Delivered`
marker, or an answer that fails this gateway's own verification), is a `502` that says
the recipient received the request, or may have, and may have acted on it.

The recipient gateway's **own verdict about a request** travels the same way. Once the
leg is authenticated, any `4xx` the recipient writes about the request — a member it does
not hold when it requires known members (`400 unknown member`), a request it cannot read, no order to decide on, an eligibility
subject that does not match the token (`403`), a consent it cannot confirm, an ingress validation
failure at enforcement `strict` (`422 ingress validation failed`, issues echoed) — is its
answer about the request, and a frame-capable requester receives it with that status and
body. So is any `4xx` it writes about **its own participant's answer** after that system
answered: an answer that repeats a member name (`403`, at every level), and at enforcement
`strict` a PAS response whose patient linkage is inconsistent or that names another
patient (`403`; from v0.61.0 the parent a dependent's Coverage entry names as a contained
`Patient` in its `subscriber` or `policyHolder` is part of that Coverage, not another
patient), a questionnaire package that carries a subject (`403`), or an answer that
fails validation (`422`) — the recipient's gateway will not relay it, and the requester
reads why. Below `strict` those answers are relayed as sent (recorded as a finding at
`observe`). As of v0.54.0 so are the recipient gateway's **own failures** once the leg is
authenticated (`5xx`): its participant's system that could not be reached
(`502 {"error":"the payer's system could not be reached"}`) or that received the request
and gave no answer it could carry (`502` saying the payer's system may have acted on it
and to check the outcome before resending), a system that did not answer within that
gateway's deadline for it (`504`, saying the same when the request was sent, or
`504 {"error":"the payer's system could not be reached in time"}` when it was not), a
deadline its own work used up before it could send the request to its system (`504` saying
the payer's system did not receive the request), a system-of-record read that failed
(`502`, or `503` while it is unavailable), a validator it cannot reach. (A record it cannot write
after your system answered withholds nothing: the answer is relayed, and the gateway emits
`pa.local-write-failed`.) On a PAS submit or update, any refusal the gateway makes after its payer's
system answered (a failure of its own, or its refusal of the payer's answer) says so
too, so the requester checks before resending. Before v0.54.0 these went bare and the
requester read `502 hub routing failed`. Only exchange machinery stays a bare non-`2xx`:
the checks that happen before any handler runs (a bad hop assertion, an envelope that
fails to decode, a token that fails verification, a replay, an unknown transaction type)
and a response leg the recipient could not build. The Hub reports those as a failed
forward that names the recipient's status, never its body.

**Negotiation, not configuration.** There is no environment variable to set. Whether an
exchange frames is decided per pair of holders from what each side has advertised to the
registry: every gateway (and SDK-based participant) on a codec-capable build
self-declares message-frame support automatically the moment it registers or rotates its
credentials — no app-level opt-in. A response frames only when **both** the requester and
the responder have advertised support; if either side is still on an older, pre-frame
build, the exchange falls back byte-for-byte to the legacy contract: a bare payload on
success (implicit `200`), and a non-`2xx` application answer reaching the requester as
the Hub's failed forward, which names the recipient's status but not its body. Upgrading one gateway in a
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
to being able to decode a frame, never to requiring one. The one leg that requires a frame
is the questionnaire leg: a `dtr-questionnaire-fetch` request must name its operation
(`questionnaire-package` or `next-question`) in the frame's operation header, and one that
names none — the older questionnaire request — is refused with `400` naming what to send.
`coverage-eligibility` is version-neutral and is never framed.

## Exchange contract lines (`SHN_CONTRACT_VERSIONS`)

The gateway BUILDS every prior-authorization contract at three Da Vinci generations —
CRD/DTR/PAS at `2.0.x`, `2.1.x`, and `2.2.x` (plus PDex `2.1.x`). That is its **native**
capability. What it **declares** to the network is a separate, operator-chosen subset, and
the declared set is the starting point for routing; qualified native lanes also
support the native-reach and inbound-honor rules below.

| Env var | Description |
|---|---|
| `SHN_CONTRACT_VERSIONS` | This gateway's own **declared** exchange-contract versions: comma-separated `<contract>@<line>` tokens, e.g. `pa.crd@2.2, pa.dtr@2.2, pa.pas@2.2`. Drives leg selection, the published `CapabilityStatement`s and `.well-known/davinci-configuration`, and the declaration peers route against. Must be a **subset of the native set** (`pa.crd@{2.0,2.1,2.2}`, `pa.dtr@{2.0,2.1,2.2}`, `pa.pas@{2.0,2.1,2.2}`, `pa.pdex@2.1`) — a token outside it is a boot error, not a routing outcome. Unset ⇒ the build default, the canonical `2.0` line (`pa.crd@2.0`, `pa.dtr@2.0`, `pa.pas@2.0`, `pa.pdex@2.1`). From shn-gateway v0.59.0, a payer that forwards to its own system (`PAYER_DAVINCI_BASE_URL`) and declares that system's versions (`PAYER_DAVINCI_CONTRACT_VERSIONS`) derives this set instead when it is unset: the system's `pa.crd`, `pa.dtr` and `pa.pas` lines, plus `pa.pdex@2.1`, which the gateway answers itself. The gateway logs `gateway: declaring <set>, derived from PAYER_DAVINCI_CONTRACT_VERSIONS` at boot; the registry entry peers select it against must declare the same set (see [Opting a line in](#opting-a-line-in), step 3). A derived line needs its validator lane exactly as a declared one does, and a system line this build cannot exchange refuses boot, naming it. Earlier releases declare the build default, which advertises lines the forward then refuses. From shn-gateway v0.59.0, any payer that forwards to its own system also refuses to boot when its declaration, set or derived, has no `pa.crd`, `pa.dtr` or `pa.pas` line: no provider could reach it for prior authorization. |
| `FHIR_VALIDATE_URL_2_1` | Optional **2.1** `$validate` address override. Compose default: `http://shn-validator-2-1:8080/fhir`. |
| `FHIR_VALIDATE_URL_2_2` | Optional **2.2** `$validate` address override. Compose default: `http://shn-validator-2-2:8080/fhir`. |
| `FHIR_CERTIFY_URL_2_1` | Optional **2.1** `$validate` address for the certification evidence only. Never a routing lane. |
| `FHIR_CERTIFY_URL_2_2` | Optional **2.2** `$validate` address for the certification evidence only. Never a routing lane. |
| `FHIR_DEFAULT_VALIDATOR_LANES` | Unset, or `none`. `none` states that this gateway's network has no Compose default validator services, so the gateway never probes their names. A CRD, DTR or PAS line other than 2.0 then validates only through its own `FHIR_VALIDATE_URL_<line>` (routing) or `FHIR_CERTIFY_URL_<line>` (evidence), and a declared line without its own `FHIR_VALIDATE_URL_<line>` refuses boot, naming the key (a `FHIR_CERTIFY_URL_<line>` does not count). A single-line contract (`pa.pdex@2.1`) keeps riding the canonical validator, as it does with the default lanes. Any other value is a boot error. |

A `FHIR_VALIDATE_URL_<line>` is a routing lane: setting it makes the line reachable in both
directions (an inbound frame at that line is honoured, origination may target it). A
`FHIR_CERTIFY_URL_<line>` is not: it gives the certification evidence (`certify:` lines) an
address for that line and changes nothing about routing, so a gateway can record 2.2
verdicts while it stays a 2.0 gateway. The evidence uses, in order, `FHIR_CERTIFY_URL_<line>`,
then `FHIR_VALIDATE_URL_<line>`, then the Compose default once it has qualified — by
routing's attempt at boot, or by the evidence's own: the evidence tries the default in a
background loop of its own, started only where certification evidence is collected (never
at `CONFORMANCE_ENFORCEMENT=none`; first attempt 15 s after boot, stopping at the first
success from either side), so a validator that comes up after boot is certified against
within one interval of coming up, and routing's lanes never change because of it. What that
loop sends a validator that is not up: each attempt lasts up to 60 s and, until the
validator's `GET /metadata` answers `200`, repeats that request about once a second (each
request waits up to 4 s, then 1 s passes before the next), so an address that refuses the
connection, does not resolve or answers another status gets about one request per second,
and one that accepts the connection but never answers about one every 5 s. Between attempts
the loop pauses 15 s during the first hour after it starts, then 5 min. So for the first hour
the validator gets requests during about 60 s of every 75 s, and afterwards during 60 s of
about every 6 min. A metadata `200` ends the probing: when it is a readable R4
CapabilityStatement of at most 4 MiB the attempt then posts the readiness corpus once, and
passes or fails; any other `200` fails the attempt. No exchange waits on that: a line whose default has not
qualified records its verdict as `certification validator unavailable: FHIR_CERTIFY_URL_<line>
and FHIR_VALIDATE_URL_<line> are not configured; default lane qualification pending` (or
`… failed, retrying` after a failed attempt) and dials nothing for that exchange; routing's boot
probe logs `validator_qualification` lines, and the evidence's loop logs a
`certification_lane_qualification` line when an attempt's outcome changes (the first
failure, then success). Each line carries the lane's line, `base` address, the `host` it
dials and its `state`. A `failed` line also carries a `reason` naming why, in the gateway's
own words: `name does not resolve`, `name lookup failed`, `connection refused`,
`metadata answered HTTP <status>`, `metadata is not an R4 CapabilityStatement`,
`qualification corpus did not pass`, `no answer within the qualification budget`,
`qualification stopped` or `qualification did not pass`. Never a validator's answer.
From shn-gateway v0.57.0 an address set by either key for 2.1 or 2.2 is gated the same way,
on its own qualification by the same corpus: a validator still warming answers errors about
itself (on the 2.2 line, `SLICING_CANNOT_BE_EVALUATED` the first time it meets a PAS answer),
not about the message. Its loop starts at boot where certification evidence is collected
(never at `none`, where the address is never dialed), with no 15 s wait, and sends the same
traffic on the same schedule afterwards (as described above); until an attempt passes, the line records `certification validator unavailable:
lane not qualified; qualification pending` (or `…; qualification failed, retrying`) and dials
nothing for that exchange, and the loop logs `certification_lane_qualification` lines as
above. The gate re-qualifies after a lane stops answering: once a 2.1 or 2.2 lane (a default or an
address) has qualified, a certification it fails at the connection (the lane refuses, resets or
closes the connection, cannot be reached, or does not answer within the certification's 2 s
while the collection is still waiting) records `… lane not qualified; re-qualifying`, or for a
default lane `…; default lane re-qualifying`. A certification the lane did not answer in time is
itself recorded `expired`, since the certification client's own limits are 2.5 s and the
certification's 2 s always runs out first. Either way the lane is qualified again, routing's own
qualification of a default lane no longer counting for the evidence; until that passes, the
line records the same and dials nothing. A re-qualification starts at once, or 30 s after the
previous one started if that was more recent. Only its start is limited: while the lane stays
broken, its attempts follow the qualification's own schedule above, and that schedule starts
over with each re-qualification: attempts of up to 60 s, probing metadata as described above, 15 s apart for the first hour after the
re-qualification starts, then 5 min apart. An attempt whose metadata answers `200` with a readable R4
CapabilityStatement runs the readiness corpus instead, so a lane that answers its metadata but fails the corpus is sent
the corpus every 15 s or so for that hour. An answer, whatever it is, does not clear the qualification: a verdict,
valid or invalid, is recorded as such, and a 5xx or an answer that is not an OperationOutcome
marks only that certification `unavailable`. Nor does a certification abandoned because the
collection's own time ran out (its 6 s, or its 30 s in the queue) or the gateway is shutting
down: that certification is `expired`. A lane restarted between two certifications, with no failure in
between, is not detected. The validator at such an address must pass the readiness corpus the packaged validator
image passes; one that never does keeps the line `unavailable`. The qualification never makes
a certify-only address a routing lane. The 2.0 line certifies on the gateway's own validator
(`FHIR_VALIDATE_URL` or discovery) without this qualification.
Where the default name does not resolve (any deployment that is not the Compose stack), set
one of the two keys for each line to get a verdict. Set `FHIR_DEFAULT_VALIDATOR_LANES=none`
so the gateway never probes the default names at all. A line with neither key then records
`certification validator unavailable: FHIR_CERTIFY_URL_<line> and FHIR_VALIDATE_URL_<line>
are not configured, and this network has no default validator lanes`, and dials nothing.

From shn-gateway v0.56.0, wherever certification evidence is collected (any
`CONFORMANCE_ENFORCEMENT` but `none`), the gateway process sends each certification address it
was given (`FHIR_CERTIFY_URL_<line>` or `FHIR_VALIDATE_URL_<line>`, and the 2.0 validator,
whether from `FHIR_VALIDATE_URL` or discovery) one PAS request bundle of that line at boot,
against the profile a certification of it names. This runs beside the listener, never before it,
so that the process's first request to a validator is not a real certification's. It uses a
client of its own, stops at the first answer, records no evidence, and sends at most three: a
failed request is sent again right away, so an address that is not yet listening gets three
immediate refusals. It logs one `certification_warm` line per address with its `line`, the `host` it
dials, `state` (`answered`, `unanswered` or `stopped`), `attempts` and `duration_ms`, never a
validator's answer. A Compose default lane is not sent one: its qualification already sends it
the readiness corpus. From shn-gateway v0.57.0 only the 2.0 validator is sent one: a 2.1 or 2.2
address is gated on its own qualification, which already sends it the readiness corpus.

A line's verdict in a `certify:` line is `valid`, `invalid`, `unavailable` or `expired`; only a
`valid` line is listed in `certified`, and `sourceLine` is chosen from those alone. From
shn-gateway v0.57.0, when every error a validator reports is terminology it could not check (a
code system it does not hold, such as the licensed X12 code systems no validator lane loads:
the passed-through "CodeSystem is unknown and can't be validated" and "Unable to expand
ValueSet because CodeSystem could not be found"; the required-binding miss on that same
element when that element also says the value set could not be expanded for want of the code
system of each code the miss names; on any line, a Bundle entry's no-match summaries
(`BUNDLE_BUNDLE_ENTRY_MULTIPLE_PROFILES_NO_MATCH`, `Validation_VAL_Profile_NoMatch`) when every
other error on that entry is one of these; and on the 2.1 and 2.2 lines only, in a Bundle checked
against or declaring the PAS request-bundle profile, on a Claim entry whose `meta.profile` names
at most one of the two PAS Claim profiles and agrees with `Claim.related` (present on an
update, absent on a submit), the Claim's match of neither Claim profile: the no-match summaries
naming exactly those two profiles and the other profile's own cardinality minimums and
maximums (on any element), attributed to it alone. In every case the entry must itself carry terminology the
validator could not check), the verdict is `unavailable` with `error` `terminology unavailable: <code system>`,
not `invalid`: the validator reached no verdict on the message. Any other error keeps it
`invalid`: a structural error, a failed invariant, a code the validator checked and did not
find (`Unknown code`), a required-binding miss on a code system it holds or under a value set
it could expand (a code from a system it does not know, where the value set is drawn from
systems it does), or a fatal issue. A
code system is named as written only when it is an X12 code system canonical
(`https://codesystem.x12.org/<version>/<list>`); any other is named by its size and SHA-256.

**One validator per line — this is not optional.** A FHIR server loads exactly **one**
version of a given IG package, so a single HAPI cannot host CRD 2.0.1 and CRD 2.2.1 at the
same time; a 2.2 payload validated against a 2.0-loaded server is not validated, it is
mis-validated. Each declared non-canonical line therefore needs its own `$validate` lane.
`FHIR_VALIDATE_URL` (the base variable, above) remains the canonical `2.0` lane.
The gateway resolves explicit per-line override first, then the existing canonical
endpoint, then the Compose default. Kit child ports and hosted service addresses
continue to come from their existing launcher wiring; they need not use Compose DNS.
Malformed override URLs refuse startup. Explicit endpoints keep their existing
startup behavior for routing; setting an override does not trigger synthetic qualification of
the routing lane (the certification evidence qualifies a 2.1 or 2.2 address on its own, above).

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
3. **Re-register or rotate** so the new declaration reaches the registrar.
   Declaration tracks the current build/config, and it is published at
   registration/rotation — not continuously. The registration or rotation must carry the
   declared set: `shn register` and `shn rotate` send this build's default declaration
   and do not yet take a declared set, so a gateway that declares any other set builds its
   registration with the SDK's `Identity.RegistrationWithDeclared`. A gateway SHN hosts for you needs no
   rotation: SHN's hosting service republishes a changed declaration itself, withdrawing a
   dropped line before the new configuration rolls out and publishing a new line once the
   gateway serving it is running.
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

## Optional diagnostic collection for test deployments

Capture is disabled by default. Configure these settings only on test participant
instances whose traffic may be retained, including credentials and raw bodies.
This is independent of the loopback observer and does not change authorization,
routing, readiness, or clinical responses.

| Setting | Meaning |
|---|---|
| `DIAGNOSTIC_SINK_URL` | HTTP(S) observation endpoint. Health is posted to `health` in the same parent path, and batches go to `batch` beneath this path (`<sink>/batch`). Redirects are not followed. |
| `DIAGNOSTIC_SOURCE` | Collector-configured source identity for this test participant. |
| `DIAGNOSTIC_KEY_FILE` | Raw shared publication key, a nonempty regular file of at most 4096 bytes. |
| `DIAGNOSTIC_TRACE_KEY_FILE` | Optional provider-only raw key for private ingress attribution proofs (also applies when `ROLE` is unset and defaults to `provider`). It does not grant exchange authority. |

The first three settings must be present together. Invalid optional settings leave
capture disabled and emit a fixed diagnostic warning; a missing trace key leaves
call attribution unavailable. Set these only on the specifically configured test
participants. Other gateway deployments remain off by default.

From v0.60.0 each conformance finding is also captured as its own
`conformance.finding` observation: metadata only (the finding as its
`conformance:` log line has it, never a body), bound to its call like the
gateway's other observations. At `observe` each exchange is then closed by one
`conformance.result` observation carrying its finding count, sent once its checks
have run. At `observe` an exchange captures at most 32 findings on their own, and
its result still counts them all (`"truncated": true`); a check dropped from a
full queue makes the result `"incomplete": true`.

Request handlers enqueue to a bounded queue (4,096 observations, 64 MiB retained,
8 MiB maximum retained body per event). Each publication attempt has a bounded
30-second ownership window so evidence waiting for another source's prerequisite
can arrive without holding a request open. HTTP capture separately limits concurrent
buffering and marks unread, aborted, limited or failed bodies partial. A body the
capture limits left out or cut says so in the observation's `detail` (`body not
kept: capture budget`, `body capture partial: capture budget`; see STABILITY.md). Publishing
runs in the background with bounded retries; overload and collector failures can
leave gaps, and each heartbeat counts how every observation left the queue
(acknowledged, discarded, or dropped by reason).

- **Batches.** The first heartbeat is sent at start. Once the collector answers a
  heartbeat `2xx` with `X-SHN-Evidence-Batch`, observations are posted to
  `<sink>/batch`, up to 256 per signed request, bounded by 1 MiB of the queue's
  retained-cost estimate (one larger observation goes alone, so a request can be
  larger), and the collector answers each one. A collector that never sends the
  header gets one observation per request. A batch answered `404` or `405`, or a
  `2xx` without one result per observation in order, falls back to one observation
  per request until a heartbeat's answer carries the header again.
- **Withheld kinds.** A kind the collector says it never admits from this gateway
  (`kind_not_admitted`) is withheld for 10 minutes: its observations are counted as
  discarded and `suppressed`, not sent. Then they are sent again and the answer
  decides afresh.
- **At a stop.** The gateway's listener shuts down (5 s), requests still running are
  cut and their handlers get up to 2 s to return, and the gateway closes. The
  publisher then delivers what is queued for up to 10 s and sends a last heartbeat,
  counting anything left as `stopped`. With collection configured, stopping can take
  up to about 11 s longer: up to about 28 s in all, since the gateway's close can
  take up to 10 s to flush its conformance checks. Give the container a stop timeout
  above that (Docker's default is 10 s).

The collector's retention and staff access policy governs persisted traffic. This is
application-visible evidence, not a packet capture or a complete record of all
attempted calls.

Private ingress proofs bind method and request URI to a call identity. The live
body fingerprint and actual sealed-request ciphertext hash supply separate byte
bindings. Reusing a correlation or patient identifier does not establish a call
link. Invalid or absent proof affects attribution only. Recipient stage links are
created after independent transport and authority checks; the Hub remains blind
to payload content. SMART operation HTTP capture runs beneath bearer injection,
and token acquisition is observed separately. Original request objects and the
SMART token cache retain their existing behavior.

Use an independent publication key per configured test service and a separate
provider attribution-proof key. Replicas may share a service publication key; their
process incarnations and sequence numbers remain distinct. A hosted operator can
supply these settings through its existing secret materialization path, without
changing clinical routing or requiring participant configuration. Enrollment and
retirement belong to the collector; retiring capture must preserve existing evidence.
