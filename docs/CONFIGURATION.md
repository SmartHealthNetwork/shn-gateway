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
- [Exchange stats (`/internal/stats`)](#exchange-stats-internalstats)
- [The audit outbox](#the-audit-outbox)
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
| `SHN_DISCOVERY_URL` | The single anchor — resolves the network's trust-plane endpoints (Hub, Authorization Framework, registrar, consent, audit, PHG, …) and trust anchors. Example: `https://accounts.shn-preview.org/discovery`. The gateway starts only when this descriptor accepts wire protocol `2.0.0`, the version it sends (below). |
| `ROLE` | `provider`, `payer`, `facility`, or `phg`. Must match the role you registered. |
| `SHN_SECRETS` | Path to the bundle directory written by `shn register -out`. |

**The start check.** The gateway sends wire protocol `2.0.0`'s leg names, so at start it reads the
descriptor once and starts only when `wireProtocolVersionsAccepted` contains `2.0.0` (with the
field absent, when `wireProtocolVersion` is `2.0.0`). Otherwise it does not start, and says so:
`gateway: the network at <SHN_DISCOVERY_URL> does not accept wire protocol 2.0.0 (it accepts
["1.1.0"]); … Point it at a network whose discovery accepts 2.0.0, or run a gateway before
v1.0.0`. A descriptor it cannot fetch or read stops the start too, since it cannot confirm what
the network accepts. There is no setting that skips the check.

## Validation (required to boot — FR-36) and conformance enforcement

The gateway **refuses to start without a FHIR validator** at every enforcement
level that checks a payload: `observe`, `structural` and `strict`. From v1.0.0 a
gateway that leaves `CONFORMANCE_ENFORCEMENT` unset is at `none` (the default) and
checks no payload; before v1.0.0 the default was `observe`.
The published discovery descriptor does not advertise a validator, so at those
levels you must supply one. From v0.63.0 a gateway at `none`, which checks no
payload, starts without one ([below](#running-at-none-without-a-validator)).
Whether your gateway validates each resource at its own edge, and what it does
with an invalid result, is your choice (`CONFORMANCE_ENFORCEMENT` below):

| Env var | Description |
|---|---|
| `FHIR_VALIDATE_URL` | A FHIR `$validate` endpoint (a HAPI server with the Da Vinci CRD/DTR/PAS + US Core IGs loaded). The production path. |
| `SHN_FAKE_VALIDATOR` | **Not supported.** From v0.63.0 a released gateway refuses to boot when it is set to any value, at every `CONFORMANCE_ENFORCEMENT` level: it selected a stand-in validator that accepted almost everything, and it remains only in SHN's own test builds. For a first wiring test, run [`deploy/eval`](../deploy/eval/README.md), which pairs the gateway with a real validator; to run your own, use the sidecar below; or, from v0.63.0, run at `CONFORMANCE_ENFORCEMENT=none`, which needs no validator ([below](#running-at-none-without-a-validator)). |
| `CDS_ADVERTISE_HOOKS` | Optional. A comma-separated list of the CDS Hooks your provider ingress advertises and dispatches, from `order-sign`, `order-select`, `order-dispatch`. Unset advertises every hook the network carries. Set it only to narrow: when no payer your gateway routes to carries a hook's leg, advertising it promises a service that can only fail at routing; a request to a service id you do not advertise is refused with `404` and the ids you offer. A hook the network does not carry refuses to boot. This is an interim override — the network does not yet carry a declaration of the hooks each payer offers, and the listing will derive from routing once it does. |
| `CONFORMANCE_ENFORCEMENT` | `none` (the default when unset, from v1.0.0), `observe` (the default when unset before v1.0.0), `structural` or `strict`. Payload conformance is your opt-in: left unset, a gateway from v1.0.0 runs no payload check, records no finding and needs no validator; its boot line says `conformance enforcement=none (CONFORMANCE_ENFORCEMENT unset)`. A level applies only to what each leg's check covers (a resource's declared profiles, else its base FHIR R4 definition; a carried PAS request is not `$validate`d, and from v0.63.0 the payer's answer to one is; a carried DTR questionnaire package request is not, and from v1.0.0 the payer's answer to one is, against the operation's output Parameters profile at the lines whose profile constrains the parameter names it uses): see [INTEGRATION.md](INTEGRATION.md#conformance-enforcement). At `none` no payload conformance check runs (no FHIR profile validation, no CDS Hooks response rules, no content checks) and no finding is recorded. At `observe` every check runs, each defect is recorded as a finding in your gateway's log and observer stream, and the message is carried as sent, apart from the gateway's registered edits (the callback removed, payer identity mapping, and, when the provider opts in with `ENRICH_NATIVE_REQUESTS=true`, prefetch and coverage obtained; participant protocol §7a.4); a validator that cannot be reached is recorded the same way. From v0.60.0, at `observe` a check that can only record does not hold the message: it is queued, and its finding is written when the validator answers. The queue is bounded: a check that finds it full is dropped, and the gateway logs `gateway: observe check dropped` (on the 1st, 2nd, 4th, 8th… drop). One kind of check still waits: the check of a payload this gateway itself translated between IG lines, which refuses at every level. (Before v0.63.0 a payer gateway's checks of the decision ExplanationOfBenefits it built also waited, sharing at most 2 seconds per exchange; from v0.63.0 it builds none.) At `structural` every check runs, a message whose structure is broken (a missing required element, an element the resource does not define, a value of the wrong JSON type or one that cannot be read, a CDS Hooks answer that cannot be read or lacks a required member, a request or answer the gateway cannot read, or a validator result the gateway cannot classify) is refused as at `strict`, and every other defect is recorded as at `observe`. Of FHIR profile issues, only invariants and the validator's recognized issues for a code outside its code list (licensed code systems included) are recorded. From v0.55.0 that includes a code system the validator does not know (`Terminology_TX_System_Unknown`, which it reports as an error only for a system in the HL7 FHIR namespace, a misspelled HL7 system URL included): an unknown code system is recorded and the message relayed when nothing else in it refuses at this level, where earlier releases refused it. Any other terminology issue refuses, and every other FHIR profile issue and every fatal issue refuses; an unreachable validator is recorded, and so is a regex check the validator could not run (`TYPE_SPECIFIC_CHECKS_DT_PRIMITIVE_REGEX_EXCEPTION`, which a validator gives when a regex overruns its time limit, or, from v1.2.0, `REGEX_MATCH_TIMED_OUT`, for a regex a profile sets), and, from v1.8.0, a profile the payload itself declares in `meta.profile` that the validator does not hold (`VALIDATION_VAL_PROFILE_UNKNOWN_NOT_POLICY` naming that element and quoting its profile is read so on its own; a `Validation_VAL_Profile_Unknown` quoting the same profile beside it is read with it; the finding names it in `unresolvedProfiles`), unless an issue that refuses at this level is beside it. Such a profile is no defect a no-match summary follows from: an entry's no-match summary (`BUNDLE_BUNDLE_ENTRY_MULTIPLE_PROFILES_NO_MATCH`, `Validation_VAL_Profile_NoMatch`) reads as it would without such a profile beside it, so one nothing else excuses still refuses at this level. Where the payload is read at several lines (a payer's answer at the candidate lines the check consults), such a profile is a check that could not run only when no line the check consulted holds it: one another of those lines holds (another IG line's own profile, say) was checked there, and the line that lacks it reads it as a structural issue. A known limit: the lines consulted are those the check reads the payload at (for a questionnaire package answer, its own name family's: 2.0 and 2.1, or 2.2), so a profile only a lane outside them holds reads as unchecked: carried, recorded at `observe`, never a pass. So does a profile only a lane that did not answer, or stopped without a verdict, would have held: that lane is an outage, recorded and carried at `observe` and `structural` and refused with `500` at `strict`, as any outage is. The observer's `validate.result` reports each lane alone, so where another line held the profile, the lane that lacks it says `validator unavailable` there while the check's finding reads it as structural. Each is a check that could not run, never a pass: at `observe` it is recorded the same way, and at `strict` it refuses with the gateway's own `500`, as an unreachable validator does; for a declared profile the refusal says `profile could not be checked: <profiles>`, naming them as the finding's `unresolvedProfiles` does, where an unreachable validator's says `validator unavailable`. A profile the gateway asks the validator for explicitly is not the payload's declaration, and a validator that does not hold it is a structural issue. Nor is a profile on a questionnaire response the gateway's own engine filled for module 1 (its managed `$populate` fill or its automatic fill, not a response your own `PROVIDER_DTR_POPULATE_URL` engine returned): every profile such a response declares, the line's DTR profile the gateway adds included, is the gateway's own and asked for at its DTR check, so a validator that does not hold one refuses it there as a structural issue. At `strict` a defect refuses the message, and the refusal names the rule and the issues it was based on; an unreachable validator refuses with `500`. A gateway at `observe` waits up to 10 seconds on stop for its queued checks, one step of its stop sequence, so give its container the 45-second stop timeout [DEPLOYMENT.md](DEPLOYMENT.md#stopping-the-gateway) gives (Docker's default is 10); a gateway stopped sooner loses the findings of the checks still queued. From v0.62.0, a payer's PAS `$submit` or amendment answer the gateway cannot read (`answer.shape`) is read by the graph rule of its IG line (PAS 2.0.1: every reference resolves, the request Claim's included; 2.1.0: only that `ClaimResponse.request` names the request Claim; 2.2.1: every reference resolves except one to the request Claim, which `request` must name), at the answer's candidate lines, in the order its FHIR profile is certified at, at the routed line and the lines the gateway has a validator lane for (a validator serving several lines judges an answer once, at the first of them): valid on any line is valid, and an answer read at a line after the first is recorded at that line. So a gateway with a 2.1 validator lane carries any PAS answer that names its request Claim, however incomplete for 2.0.1 or 2.2.1, and a rule only one line states (PAS 2.2.1's inquiry-answer rule that a ClaimResponse name its Claim or give a Data Absent Reason) refuses only where every candidate line carries it (gateway CHANGELOG.md, "A PAS answer is read at its IG line"). Network rules refuse at every level: authentication, authority (including a token presented with a request other than the one it was issued for), consent, the patient binding (each gateway identifies the member the request names by its own system), routing, replay, a repeated member name in any body, a coverage eligibility batch of more requests than one exchange is recorded under (`413`, from v0.63.0), and the check of a payload this gateway itself translated between IG lines. From v0.62.0 an answer whose frame states another contract line than its leg was routed at is relayed, and refused only under `REQUIRE_SHARED_LINE=true`. Any other value refuses to boot. |
| `CONFORMANCE_TOLERATE_PEERS` | Optional, unset by default. From shn-gateway v1.0.0. The payers whose answers your gateway tolerates: payer identities as `<system>\|<value>` (the identity a member's Coverage names, which the exchange is routed by), comma-separated, for example `urn:oid:2.16.840.1.113883.6.300\|00001`. At `structural` or `strict`, a defect the FHIR profile check of a listed payer's DTR or PAS answer would refuse (an answer to a request your gateway sends, or to one it carries for your system through its Da Vinci ingress) is recorded instead and the answer carried; the finding is marked `tolerated: true`. Only that check is tolerated: the content rules on an answer (its IG line's answer graph, `answer.shape`, and its patient linkage, `patient.answer`) and the CDS Hooks answer check refuse as ever, as do network rules and the gateway's own edits; a check that could not run is never tolerated (`500` at `strict`). An answer from an exchange routed by several payer identities is tolerated only when every one is listed. Unset, or empty, tolerates no one: no payer, the network's reference payers included, is exempt by default. The list is logged at boot (`gateway: conformance tolerated peers=…`, `none` when unset); an entry without both parts, or listed twice, refuses to boot. At `none` and `observe` nothing is refused, so the list changes nothing. A hosted gateway cannot set it yet. |

If `FHIR_VALIDATE_URL` is not set (and discovery advertises none), the gateway exits
with `refusing to run without per-message validation (FR-36)` at `observe`,
`structural` and `strict`, and with `CONFORMANCE_ENFORCEMENT` unset; from v0.63.0
the message goes on to name both ways forward, a validator or
`CONFORMANCE_ENFORCEMENT=none`. Before v0.63.0 it exits at `none` too.

### Running at `none` without a validator

From v0.63.0 a gateway at `CONFORMANCE_ENFORCEMENT=none` starts with no validator at
all, and logs `gateway: no FHIR validator: …` at boot. It needs no `FHIR_VALIDATE_URL`,
no `FHIR_VALIDATE_URL_<line>` for a line it declares, and no default validator lane. It
checks nothing, exactly as at `none` with a validator, and it exchanges as follows:

- It builds and answers at every line it declares, honors a request that states one
  of those lines, and resumes a pended prior authorization (amendment and inquiry) at
  the line it was pinned to.
- It never translates a message between IG lines, since that needs the validator of
  the line it translates to. A peer that shares no declared line with it is carried as
  sent at this gateway's own line, and recorded; with `REQUIRE_SHARED_LINE=true` it is
  refused as `routing`. Reaching a peer at a line you do not declare also needs that
  line's validator, as at every level.
- A validator you do configure is used as at any other level. A Compose default lane
  for a line you declare is qualified in the background instead of before traffic, and
  the line is served meanwhile. A translation to a line whose validator you configured
  but cannot reach is refused (`500`) at every level, `none` included.

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
| `PATIENT_ACCESS_API` | any (payer in v0.62.0) | v0.62.0 only: `off` left the Patient Access API unmounted. From v0.63.0 no gateway serves that API: a value still set is read by nothing, and the gateway, of any role, logs one boot warning saying so. The gateway's liveness is `GET /health` on every role. |
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

**A request your gateway builds names your payer record.** When your gateway builds a
prior-authorization request for you, the Claim names the payer's own Organization record, read
from your system through the Coverage's `payor` reference, and a Coverage that names its payer by
identifier alone leads to no such record. The gateway still routes on that identifier, and carries
the coverage check and any questionnaire, so the no-PA coverage check, and a route that stops at the
payer's not-covered answer, answer as usual. When an order goes on to prior authorization, the
gateway sends no `pas-claim` leg and refuses `422` with `the member's Coverage names its payer by
identifier alone, so your system of record supplies no payer Organization for a prior-authorization
request to name`, recorded as `fidelity` at stage `build`, at every conformance level. It never
mints the record. Point the Coverage's `payor` at an Organization your system holds.

**Where a request your gateway builds is refused.** A request your gateway builds for you
(`ORIGINATION_PROFILE`, the `/scenario/*` routes) is refused at the first leg that cannot be built,
never earlier, so a request that can still succeed without a later leg is carried: a payer's
not-covered answer, the no-PA coverage check, and UC-05 when the disclosure is denied all
answer as usual, whatever a later leg would have needed. From v1.4.0:
- **Before the first leg**, only what no leg can be sent without:
  - the request to your gateway malformed: an unreadable body (`400`), a `wait` that is not a
    whole number of seconds (`400`), a sent `npi` that is no NPI (`400`);
  - a leg that cannot be addressed, recorded as `routing`: a member your system does not hold
    (`400 unknown member`), no Coverage on file or several that name different payers, no payer
    router, a Coverage naming no payer identifier, or no participant registering the payer (`422`);
  - a record the first leg carries that your system cannot supply: no Patient, no open or draft
    order (or several drafts), an open order about another product than the route originates
    for, a dispatched order with no id or performer, a connector that
    cannot search for coverage, or a record the gateway cannot name the member in (`422`,
    recorded as `fidelity` at stage `build`), and, behind a connector whose coverage read is not
    the Coverage search, a coverage search your system answers past the gateway's bounds or
    says definitely that it cannot run (the same);
  - a record your system returns about another patient (`422`, recorded as `fidelity` at stage
    `verify`).
- **At the questionnaire leg:** a payer that declares no framed DTR operations (`422`, recorded
  as `routing`).
- **At UC-07's patient questionnaire exchange:** no patient health gateway registered (`422`,
  recorded as `routing`), and, on a lane whose patient answers a score, an answer that is no
  score (`400`).
- **At the PAS claim, after the questionnaire is filled** (from v1.5.0): a filled QuestionnaireResponse about
  another patient than the request's or naming none, whether your operated `$populate` engine
  returned it or the gateway's own check finds it, or for another questionnaire than the one
  fetched or naming none (`422`, recorded as `fidelity` at stage `verify`): the fill is the
  gateway's own edit, and it failed the gateway's own check. The claim is not sent.
- **At the leg that consumes it**, everything only a later leg carries, refused `422` and recorded
  as `fidelity` at stage `build`:
  - at the PAS claim:
    - the member and the order the request states its claim facts for;
    - the claim facts themselves;
    - the payer's Organization record;
    - the records the order's requester leads to;
    - the requesting provider;
    - a dispatched order's supplier record;
    - the record of a person the member's Coverage names as its `subscriber` or `policyHolder`
      by a reference to another `Patient` record (a dependent's parent): the claim carries it
      as a `Patient` entry of its own, and refuses a Coverage naming a person your system does
      not hold or whose record cannot be carried as it is;
    - an order with no product coding the claim's item can name;
    - UC-03's oxygen attestation clinician;
  - at the amendment: its `certificationType` and `reportType`, its attesting clinician and
    UC-06's answer, and, on `/scenario/uc06/complete` and `/scenario/uc07/complete`, the member
    and order the request names;
  - on `/scenario/uc05`, the facility's report the amendment carries as evidence, and the
    `Provenance` that attributes it, when either is about another patient: refused once the
    report's `federated-query` leg is answered, before the next leg is sent;
  - at the patient's questionnaire exchange: UC-07's answer.

  `/scenario/uc07/complete` sends the patient's questionnaire exchange before it checks the
  amendment's member, order and facts.
- **Your system failing to answer** a read of a record, or your operated `$populate` engine
  failing to answer, is a backend failure, never a refusal: `503` or `502`, recorded as one, at
  the first leg that needs the record. The coverage is read
  before any leg, to route by, and is first carried by the coverage check. Behind the built-in
  FHIR connector both are one Coverage search, so any failure of that search fails before any
  leg: `503` when your system does not answer, and `502` for anything else, including an answer
  past the gateway's bounds and an answer saying your system does not support the search (that
  read reports only whether your system answered). An answer that repeats a member name is
  recorded as `integrity`. The payer Organization record is first needed at the questionnaire request when
  the coverage check asks for a questionnaire, and otherwise at the PAS claim. The record of a
  dependent's parent is first needed at the PAS claim.

Each of the refusals and backend failures above that comes at or after a leg's build, and each
routing refusal of an unknown member, ends with a sentence saying which legs the request sent:
`The crd-order-select and dtr-questionnaire-fetch legs this request sent were carried and stand.`,
or `No leg of this request was sent.` From v1.5.0, every other answer your gateway writes itself after a leg
of the request was carried ends with the same sentence, with its status and what it is recorded
as unchanged: a questionnaire package with no Questionnaire, or one for another questionnaire
than the coverage card names, a payer answer the gateway cannot read or that is not the pend the
route expects, a payer answer your conformance level refuses, or your store failing to keep the
payer's authorization number. On `/scenario/uc06/complete` and `/scenario/uc07/complete` the
sentence names the legs that call sent. An answer written before any leg is unchanged, and an
answer relayed from a leg or the network is carried as it came. The sentence is said once. The
payer holds whatever those legs asked of it: a submission refused at its amendment leaves the
payer's pend in place, for a corrected `/complete` or an inquiry to continue.

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

**What a gateway declares at each start.** From shn-gateway v1.1.0, a gateway with a registrar
declares the legs it compiles for its `ROLE` (`PUT /register/{id}/capabilities`, signed with its
own key), so the network refuses a leg it does not take before anything reaches it. From v1.8.0
a payer's gateway also declares the request frames it serves; a provider's declares none. You set
nothing for this. A failure is logged, `gateway: capability declaration not stored`, and the
gateway keeps running and sends the declaration again until the registrar has it. A `409` is
logged once and not resent: the registrar holds a declaration issued no earlier, which the next
start declares over, or your holder was founded with the network and the network's registrar is
older than the one that applies such a holder's declaration. If your holder was founded with the
network, a current registrar applies only the request frames your gateway declares; your legs and
attachment limit stay those it was founded with.
A `404` or `405` is logged once after four tries and then tried hourly in silence: your holder
is not registered, or the network's registrar is older than the declaration. Your gateway runs on
as one that declared nothing, and declares once the registrar takes it, with no restart. The
`capability-declaration` check on `GET /health` says where it stands.

Responder roles (`payer`/`facility`/`phg`) need no `PAYER_DIRECTORY`: they receive
at `POST /substrate/inbound` and reply to whoever the Hub delivers from. A payer has no
built-in decision policy — it answers Da Vinci legs by native-forwarding to your own
Da Vinci endpoint (`PAYER_DAVINCI_BASE_URL`, required); see
[INTEGRATION.md](INTEGRATION.md#payer-decisioning) for the config and the (advanced,
Go-only) in-process alternative.

## Members your system of record does not hold (every role)

A Da Vinci CRD, DTR or PAS request, a coverage eligibility request, a CDex request or a
federated query can name a member your system of record does not hold, a patient from the
other participant's own environment, for example. By default your gateway carries it; set
`REQUIRE_KNOWN_MEMBERS=true` to have it refused instead. The refusal is a check you opted
into, so its access line records `refusal.rule` `conformance` (from v1.7.0 on every route
that binds a member; earlier releases recorded it as `other` on some ingress routes).

A provider's and a payer's gateway read the setting wherever they bind a patient a request
names: the legs the table below lists, and also the federated query at a provider's ingress and
the CDex attachment request on both sides. A facility's gateway answers only the federated
query, and a PHG's only the patient's questionnaire package; on that leg each refuses a patient
its system of record does not hold (`403`), at every level, whatever the setting: it changes
nothing there. On every role, a value other than `true` or `false` refuses to boot.

| Env var | Description |
|---|---|
| `REQUIRE_KNOWN_MEMBERS` | `true` or `false`; unset means `false`. By default a CRD, DTR, PAS, CDex or federated query subject your system of record does not hold (and, on a payer that declares its own eligibility endpoint with `PAYER_ELIGIBILITY_URL`, a coverage-eligibility subject) is carried, identified by the member id and the Patient the request carries for it. A Patient that a coverage names as its subscriber, policyholder or payor, by reference or by an identifier, is another person and is never taken for the member's Patient, even when it carries the member's number; from v0.63.0 a request that carries no Patient for the member with a birth date and name, and carries such a person under the member's number, is refused `400 cannot identify the patient: …` (recorded as `authority`): the number is shared, so the member id alone identifies no one patient. A request that carries no Patient for the member with a birth date and a family name, or several that disagree, is identified by the member id alone, so for a payer that issues family-level member numbers (a dependent sharing the subscriber's number) such requests from different members of one family share one identity; send the member's own Patient on every leg to avoid it. Send the same Patient, unchanged, on every leg of one exchange: for a member the payer does not hold, a later leg that carries a different Patient, or none, is not matched to what an earlier leg recorded (an amendment still reaches the payer and its answer is relayed, but it binds and records nothing; an inquiry's decision is relayed but not recorded). A receiving gateway handles the member the request names as it would directly, whether one side holds the member, both do, or neither does, and files what it records about the exchange under its own identification of that member. That identification, for a member your system of record does not hold, is never the identifier of a member it does hold, so the exchange is never recorded under the wrong member; the bytes exchanged are the same. Nothing is added to a request on the member's behalf unless the provider opts in with `ENRICH_NATIVE_REQUESTS=true` (see [Accept Da Vinci requests from a provider EHR](#accept-da-vinci-requests-from-a-provider-ehr-provider-optional)); with it, a CRD request for a member your system does not hold leaves out history prefetch it cannot read, with the reason recorded. Set `true` to check members against your own system of record instead: such a subject is refused (`400 unknown member`; `403 request patient does not resolve` on a provider's `$questionnaire-package` request, and on a provider's `$next-question` rounds from v1.8.0). `true` needs a system of record: a native-forward payer that keeps none (`FHIR_DATA_URL` unset) refuses to boot with it. Any other value refuses to boot. |
| `SHN_ACCEPT_UNKNOWN_MEMBERS` | **No longer read** (from shn-gateway v1.0.0). Carrying members your system of record does not hold is the default, so a gateway that sets it, to any value, logs `gateway: WARNING: SHN_ACCEPT_UNKNOWN_MEMBERS is set but no longer read` at boot and starts; remove it. Beside `REQUIRE_KNOWN_MEMBERS=true` it no longer refuses to boot: `REQUIRE_KNOWN_MEMBERS` alone decides. Releases before v1.0.0 logged a deprecation warning, and refused to boot when it was set to anything but `0` or `false` together with `REQUIRE_KNOWN_MEMBERS=true`. |

## Documentation attachment size (every role)

From shn-gateway v1.1.0. A CDex documentation attachment submission (`$submit-attachment`) has its own size bounds, above the 8 MiB the network carries on every other leg. The network's ceiling is 16 MiB of body, the bound the gateway's strict JSON reading reads a body to; its sealed envelope may be up to 22 MiB. Both are published in the SDK (`MaxAttachmentBytes`, `MaxRequestBytesFor`). A payer may also declare the largest single file its system accepts, and a provider's gateway refuses a file over it before sending. The Hub gives an envelope over 8 MiB 30 seconds to arrive, so a full 22 MiB envelope needs about 6 Mbit/s from your gateway to the Hub; a slower link should set a lower ceiling. When the Hub has no room for a large envelope, your caller gets `503` with `Retry-After`: send it again after that wait.

| Env var | Description |
|---|---|
| `ATTACHMENT_CEILING_BYTES` | The largest documentation attachment submission body, in bytes, your gateway sends or accepts. Unset (the default): the network's ceiling, 16777216 (16 MiB). Set a lower positive integer to bound it further. A provider's gateway reads a submission from its system up to it and refuses one over it `413` (`request body too large`, recorded as `routing`) before reading anything in it; a payer's gateway refuses a submission over it the same way before your system is called. The boot log states the value when it is below the network's ceiling. A value above the network's ceiling, zero, a negative number or anything but an integer refuses to boot. |
| `ATTACHMENT_INFLIGHT_BYTES` | The bytes of documentation attachments and large envelopes your gateway holds at once. Unset (the default): a tenth of the task's memory, and never below `ATTACHMENT_CEILING_BYTES`. The figure comes from the container's cgroup (`memory.max`) when that states a limit, and otherwise, on Amazon ECS, from the task metadata endpoint (`Limits.Memory`): an ECS Fargate container whose task alone has a memory limit has no limit in its cgroup. When neither states it, the budget is 64 MiB (67108864). You set nothing for this. The boot log states the value and where it came from. A provider's gateway counts each submission's stated length, or the whole ceiling when it states none, such as a gzip-coded one. A payer's gateway counts envelopes over 8 MiB, up to the largest envelope any leg may carry; an envelope at or under 8 MiB is never counted. One that states no length is read to 8 MiB and, past that, counted at that largest envelope. A request that does not fit is refused `503` with `Retry-After`, recorded as `routing`, before your system is called: the refusal is transient, so send it again after that wait. A submission it holds must arrive within 30 seconds, which is about 5 Mbit/s for a full one, or it is refused `400`. A value below the ceiling, or anything but an integer, refuses to boot. |
| `PAYER_ATTACHMENT_MAX_BYTES` | Payer gateways only. The largest single inline attachment (file), in bytes after base64 decoding, your system accepts in one documentation submission. Unset (the default): your gateway declares no limit, which the network reads as unknown, never as zero, and a provider's gateway applies only its own ceiling. Set, it is the value your gateway's capability declaration states (`attachmentMaxBytes`), exactly as you set it, and a provider's gateway sending to you refuses, before sending, a submission carrying any inline file over the smaller of your value and its own ceiling (`413`, recorded as `routing`). Only inline files are measured, each on its own, wherever the submission carries them (a `DocumentReference`'s content, a `QuestionnaireResponse`'s answer): an attachment given by `url`, and content that carries no inline file (a `QuestionnaireResponse` with no attachment answer), are carried; so is a file whose `data` is not well-formed base64, bounded by the ceiling on the whole body. Each start declares what is set, so a start with it unset clears a limit declared before, and a provider's gateway goes back to its own ceiling. A release before v1.1.0 declares nothing, so a limit stays through a rollback to one, until a later start on v1.1.0 or newer replaces it. A holder founded with the network declares no limit: the registrar keeps the record it was founded with, which has none, whatever your gateway sets. A positive integer up to 9007199254740991 (2^53-1); zero, a negative number or anything but an integer refuses to boot, and a provider gateway refuses to boot with it. |

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
ingress route (a call from your own system) or on one of module 1's origination
routes, and for a leg it receives from the network, at every conformance level.
There is nothing to configure:

`gateway: access: {"time":…,"correlationId":…,"direction":"ingress"|"inbound","route":…,"exchange":…,"outcome":…,"status":…,"latencyMs":…,…}`

- `correlationId` is the leg's id, the `X-SHN-Leg-Id` your caller is answered
  with; `trace` is the caller's own `X-Correlation-Id` when it differs. A leg
  id a message chose (a PAS Claim's own `urn:shn:correlation`) that is not
  one bounded token is recorded as `sha256:<digest>`.
- `route` is the route the call arrived on: an ingress route (`crd`, `pas`,
  …), the network's own route for a leg from the network, or, for a call on one of
  module 1's `/scenario` routes that originates (builds and sends a request of
  the gateway's own), `scenario-*`, the route's path joined by `-`
  (`scenario-uc07-complete` for `POST /scenario/uc07/complete`), with
  `direction` `ingress`. The local `/scenario` routes (`uc06/cancel`,
  `uc07/cancel`, `uc07/pending`, `reset`) send nothing and write no line.
- `exchange` is the leg (`crd-order-select`, `pas-claim`, …); `contractLine`
  the IG line it ran at; `sender` and `recipient` the two holders. On an
  origination route, which can send several legs, `exchange` and
  `correlationId` are the first leg's, while a refusal the line records may
  belong to a later leg of the same call.
- `outcome` is `answered`, `refused`, `unreachable`, `upstream-error` or
  `other`. A refusal names who refused (`refusal.by`) and the network rule
  (`refusal.rule`: authentication, authority, consent, routing, replay,
  integrity, audit, fidelity, conformance for a check you opted into, limit,
  or other when no rule is named). An exchange whose audit could not be
  recorded is refused on `audit`: by the Hub, or, from v1.1.0, by the
  Authorization Framework when it could not record its
  decision, as its answer's `rule` states. Your caller's answer is unchanged
  either way. A fidelity refusal also names its stage
  (`refusal.stage`): `build`, the gateway refused to build the message the
  refusal is about because of what the caller declared or did not state (an
  earlier leg of the same call may already have been sent); `verify`, an edit
  or message the gateway built failed its own verification. Absent for every
  other rule.
- From v1.7.0, a refusal this gateway makes of your own system's call before
  the request it refuses reaches the Hub also names its reason
  (`refusal.reason`); on a module's origination route, an earlier leg of the
  same call may have been sent. This
  covers refusals for authentication or scope, a body that reads two ways or
  in an unsupported coding, authority read from the body (no patient it can
  identify, not the resource the route takes), routing with no recipient,
  and size.
  - The reason is the gateway's own fixed sentence: the answer your caller was
    given, or the fixed sentence in it when the answer adds a detail (an
    identifier, a member name, a count, the contract lines). It never carries a value from the
    request, a credential or patient data, and a value the gateway has not
    registered is written as `other`.
  - Read it to tell such refusals apart, but don't match on its text: a minor
    release may reword a sentence or add one. `refusal.rule` is the field to
    decide by.
  - These refusals reach no Hub record, so this line and the exchange record
    are where they are kept.
  - Also from v1.7.0, three refusals that recorded rule `other` name their
    rule. An origination route's body over the network's limit (`413`, on the
    routes that read their body against it) is `routing`, the rule of every
    size refusal, with its reason. The DTR
    package route's refusal of a payer that has not declared framed DTR
    operations (`502`) is `routing`, with its reason, as module 1's
    origination records it. A member your system of record does not hold,
    refused because you set `REQUIRE_KNOWN_MEMBERS=true`, is `conformance`
    on every route: a check you opted into, so it carries no reason.
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
- From shn-gateway v1.0.0, on a call from your own system, `network` says how
  long it waited on the network: `latencyMs` runs from handing each sealed
  request to the transport for the Hub to reading the Hub's answer (or the
  connection failing), the Hub's and the recipient's time together, and
  `calls` counts the requests handed to the transport. Requests outstanding together are timed
  once, so it is at most the call's `latencyMs`, and the difference is this
  gateway's own time. It is absent on a leg from the network and on a call
  that handed nothing to the transport for the Hub. A duration and a count
  only.
- From shn-gateway v0.61.0, when the answer to a leg (your system's, or this
  gateway's own refusal) could not be authorized, the access line's
  `answerError` says why: `cancelled` (the request ended first: the requester
  had gone), `timeout` (the authorize call ran out of time), `auth` (the
  Authorization Framework denied it), `unreachable` (it never answered) or
  `other`. The gateway also logs `gateway: <exchange> answer not authorized:
  <class> (correlation <leg id>)`. From v1.1.0, once your system has answered
  the forwarded operation, the gateway says so: the Hub is answered a raw `502`
  naming the reason and its rule (`authority`, or `audit` for the Framework's
  audit refusal), marked `X-SHN-Delivered: yes`, and the requester learns its
  request was received and answered. Otherwise (the gateway's own refusal
  before your system was asked to act, an answer from your records, a forward
  that got no answer, or a requester that had gone) the answer is the plain
  `502 authorization failed`.
- From v1.1.0, when no authority could be obtained for a leg this gateway was
  sending (the Authorization Framework answered no usable token, one that does
  not verify for the leg, or an error, or it could not be reached), the leg is
  not sent and is recorded as this gateway's refusal on rule `authority`, and
  the access line's `authorizeError` says why, with the classes `answerError`
  uses. A denial, and the Framework's audit refusal, stay the Framework's. The
  caller's answer is unchanged: `503` when the Framework could not be reached,
  else `502 authorization failed`. A request that ended first has
  `authorizeError` `cancelled` and is no refusal.
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
  From v1.8.0 a finding carries `unresolvedProfiles` when the payload declares
  in `meta.profile` a profile the validator does not hold, so the payload was
  not checked against it: on the `unavailable` finding such a profile made
  unavailable, and on an invalid finding beside the defect that decided it. Each
  profile is named as `profile bytes=<n> sha256=<hex>`, never as written, since
  it is quoted from the payload: `<n>` is the length in bytes of the profile URL exactly as the
  payload declares it, `|version` included, and `<hex>` the SHA-256 of those
  bytes, so you match one to your own profiles by hashing their URLs (for
  example `printf %s '<url>' | shasum -a 256`). At most eight are named,
  sorted, the rest counted (`and N more`).
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
  mapped); and, from v0.62.0, on a call from your own system, the answer as
  given to your system (`["E-08"]`: a questionnaire package answer's parameter
  names renamed to your DTR line's); and, from v0.63.0, on a prior-authorization
  submission or update this gateway built for your system and carried to the
  payer's PAS line (`["E-09"]`: the registered transform); and, from v1.7.0, on
  a prior-authorization submission or update this gateway built that carries a
  record from your system it re-pointed to the member (`["E-06"]`: the evidence
  a provider-data request carries, or the supplemental report an update
  carries); and, from v1.8.0, on a prior-authorization update this gateway
  built that carries a facility's report whose `subject.reference` it
  re-pointed to the Claim's patient (`["E-14"]`, or `["E-14","E-06"]` with a
  record from your system E-06 re-pointed). Only the ids: never a value an edit
  removed or wrote. The ids are
  named once the request has left the gateway: the receiver answered (with
  anything, a refusal included), or the connection failed after the request
  was written. A request that was never sent names none: one refused before it
  was sent (by routing, or by the network's authorization), one whose
  connection could not be made, or one whose bearer token could not be
  obtained. A leg whose message was carried exactly has no `relayEdits` key.
- `ownership` and `builder` (from v0.63.0) say whose message the request this
  gateway sent on the leg was: on a call from your own system, the request as
  sent to the network; on a leg from the network, the request as forwarded to
  your system.
  - `ownership` is `relayed` (your system's, byte for byte), `edited` (yours,
    with registered edits) or `authored` (the gateway's own, as a module
    ingress client's request is). From v0.63.0 a request the gateway built and
    carried to the payer's PAS line through the registered transform is `edited`
    with its `builder` and `relayEdits` `["E-09"]`: the builder's message, then
    that edit. From v1.7.0 an `authored` request names in `relayEdits` a
    registered edit made to a record from your system that its build carries
    (`E-06`, made before the build): the request is still the builder's own,
    and through the registered transform it names `["E-06","E-09"]`. From
    v1.8.0 it so names an edit made to a facility's report an update carries
    (`E-14`), before any `E-06`;
  - `builder` names the registered builder of a request the gateway built:
    `authored`, or `edited` by the registered transform.

  They say nothing of the answer, though `relayEdits` may also name an edit
  to it (`E-08`). Both are absent when no request was sent, and `builder`
  unless the request was the gateway's own. Names only. Both keys are Evolving.
- `answerOwnership` and `answerBuilder` (from v0.63.0) say the same of the
  answer this gateway wrote: on a call from your own system, the answer it
  gave your system; on a leg from the network, the answer it sent back.
  `relayed` is the answering system's answer byte for byte (the recipient's
  on a call from your own system, yours on a leg from the network), `edited` is
  that answer with registered edits, and `authored` is the gateway's own, as
  every refusal it writes is (`answerBuilder` `gateway-refusal`), a refusal
  before any leg was opened included. Both are absent when no answer was
  written, as when the gateway's own refusal could not be written and it
  answered a bare `500`.
- `requestEdits` and `answerEdits` (from v0.63.0) split `relayEdits` by body:
  the edits to the request `ownership` describes, and those to the answer
  (`["E-08"]`). From v1.7.0 `requestEdits` also names an edit made to a record
  from your system that a request the gateway built carries (`["E-06"]`),
  and from v1.8.0 one made to a facility's report an update the gateway built
  carries (`["E-14"]`).
  Each is absent when that body names no edit. All four keys are Evolving.
- `editsNotMade` (from v1.1.0) names each edit the gateway would make
  that it could not make on the call, and why, so the bytes it would have
  changed were carried as sent: `E-10:no-structural-payer-url` (a pend's
  `payer-url` that is not a string `valueUrl`), `E-10:unreadable-answer` (an
  answer that could not be read as JSON, so whether it named a `payer-url` is
  unknown), `E-10:payer-does-not-take` (a payer not known to take the
  documentation submission) and, from v1.2.0, `E-08:check-could-not-run` (the
  check of a questionnaire package answer at its own line could not run or that
  line's lane is still qualifying, or, at `none`, the lane for your own line is
  still qualifying, so whether to rename it is unknown). Also from v1.2.0,
  `amendment:report-type-not-carried`: a request to an amending route stated a
  `reportType` that the amendment this gateway built does not carry, because
  its line has no place for one, its build is bridged to another line, or the
  pend ties the answer to no item. That one is your own statement, not an edit.
  From v1.5.0, on a
  payer's attachment-request Task a provider's gateway carries to its own system (below),
  `E-11:no-structural-payer-url` (a `payer-url` that is not a non-empty `valueUrl`),
  `E-11:unreadable-payer-url` (several `payer-url` inputs, or an input that cannot be read, so
  which address to re-key is unknown) and `E-11:payer-does-not-take` (a payer not known to take
  the documentation submission). From v1.7.0, on a request a payer's gateway forwards to its
  own system (see [Adaptive questionnaire addresses](#adaptive-questionnaire-addresses-the-payers-tags-and-the-restore-payer)),
  for an address in the form a provider's gateway re-keys to that names this payer:
  `E-13:unclean-remainder` (its remainder breaks the clean-remainder rule), `E-13:not-verified`
  (its tag does not verify) and `E-13:unreadable-request` (it cannot be rewritten). From v1.8.0,
  on a DTR answer a provider's gateway relays to its own DTR client (see
  [Adaptive questionnaire rounds](#adaptive-questionnaire-rounds-the-re-key-and-the-routes-provider)),
  for an adaptive address it does not re-key: `E-12:payer-gateway-older`,
  `E-12:not-the-rounds-address`, `E-12:contained-in-package-response`,
  `E-12:request-carried-a-questionnaire`, `E-12:not-under-base`, `E-12:escaped-address`,
  `E-12:base-spelled-differently`, `E-12:unreadable-tag`, `E-12:unclean-remainder`,
  `E-12:unreadable-address`, `E-12:unreadable-answer` and `E-12:address-not-writable`, each
  described there. Absent when empty. Evolving.
- `answerHeadersNotCarried` (from v1.3.0) names each `ETag`, `Last-Modified` or
  `Retry-After` of a participant's 2xx answer that was not carried, and, from v1.7.0, each
  `operationBase` or `operationAddressTags` a payer's gateway states on a DTR answer that was not
  carried (see
  [Adaptive questionnaire addresses](#adaptive-questionnaire-addresses-the-payers-tags-and-the-restore-payer)),
  and why, never its value: `<header>:value-not-carriable` (not an HTTP field value, or not UTF-8, which a
  frame cannot carry exactly), `<header>:over-frame-limit` (no room in the message frame's
  64 KiB header segment), `<header>:over-answer-limit` (with it, the answer
  would leave less than the 64 KiB its authorization takes under the network's limit, so
  it is sealed without its headers before that is minted) or `ETag:decoded-from-gzip` (a strong `ETag` on an answer the transport
  decoded from gzip). The answer goes through without it. A value that is merely odd, such
  as an unquoted `ETag`, is carried as sent. Absent when empty. Evolving.
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

From v1.0.0, with `METRICS_SERVICE` set, the gateway also reports its validator lanes:
the `ValidatorLane` gauge, every minute, 1 for a lane that has qualified and 0 for one
that has not, by `line` and `lane` (`routing`, a Compose default lane routing admits;
`certification`, a lane the certification evidence qualifies itself, reported at every
level but `none`, where certification never starts); and `ValidatorLaneAttempt`, one
count per qualification attempt of a default lane once the gateway is serving (the
attempts a lane boot waits for are not counted), by `line`, `lane` and `outcome`
(`ready` or `failed`). A gauge that stays 0 is a lane left unqualified.

With `METRICS_SERVICE` set, or an OpenTelemetry exporter on, the gateway also reports its
shared state and the patients a leg leaves out:

- `StoreError` (`store`): one count per failure of the shared-state store, by store
  (`exchange`, `replay`, `ingresskey` or `pended`). It is how a database outage shows:
  a `replay` or `ingresskey` failure on a request is answered `503`, and a failed
  background reload of the ingress key is counted as `ingresskey` too, though it answers
  nothing. An `exchange` or `pended` failure leaves the request itself unaffected, and only
  the record is skipped (for `pended`, the payer's answer still reaches the requester),
  apart from `POST /scenario/reset`, which answers `503` when it cannot clear the exchange
  records.
- `StorePool` (`stat`), with `SHN_STORE_DATABASE_URL` set, every 30 seconds: the store's
  connection pool, `TotalConns` (open), `AcquiredConns` (in use) and `IdleConns` (open and
  free), and `EmptyAcquireCount`, the acquires since start that waited for a connection.
  It is cumulative for the process, so alarm on its rate: a rising rate is a pool
  saturating.
- `InvolvedOmitted` (`reason`): one count per patient a leg's involved list left out,
  by reason: `overflow`, `system-of-record-unavailable`, `unknown-member`,
  `authorization-refused`, `authorization-failed`, `involvement-not-carried` or
  `budget`. `authorization-failed` covers a token that does not verify for the leg
  and patient, from v1.1.0. The exchange has no record under a patient left out.
  The access line's `involvedOmitted` counts the same, by reason, on any gateway,
  with or without metrics.

With `METRICS_SERVICE` set, or an OpenTelemetry exporter on, the gateway also reports
its operational state every minute. A value it does not know is left out, never
reported as 0.

- `GatewayInfo` is 1 for each IG line the declared contract versions name, by `line`,
  `wireProtocol` (the version this gateway sends) and `level` (`CONFORMANCE_ENFORCEMENT`).
- `CertificateExpiry` is the seconds until a certificate expires, the soonest of each
  `kind` and `role`, with `line` for the per-line `validator` and `certify` kinds and
  `none` for the others. `role` is one of:
  - `server`, what an endpoint presented on its last response, by the setting that
    names it (`hub`, `payer-pas`, `fhir-token` and so on);
  - `self`, the gateway's own certificates, read once when loaded: `ingress` (from
    `TLS_CERT_FILE`), `otlp` (the OTLP client certificate,
    `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` or its `_METRICS_` form) and `metrics-pull`
    (the pull listener's, `METRICS_PULL_TLS_CERT_FILE`);
  - `trust-root`, the soonest root of a bundle you added through `SSL_CERT_FILE` or
    `SSL_CERT_DIR`, as one series; the image's and the system's own bundle files and
    directories are not read, but a file of your own that combines the system's roots
    with yours reports the soonest of all of them.

  A server certificate's reading is left out once its endpoint has not answered for
  24 hours, apart from the endpoints read once at start (`discovery`, `hub-key`,
  `authorization-key`). `CertificateExpiryMin` is the soonest of the `server` and
  `self` certificates, one stream to alarm on; a bundle can carry roots nothing uses,
  so `trust-root` is left out of it.
- `IngressKeyRemaining` is the seconds until the ingress signing key expires, and
  `IngressKeyAge` the age of the newest key, with `SHN_STORE_DATABASE_URL` set. A
  remaining time that keeps falling while the age grows past a day is a rotation that
  has stalled.
- `DependencyAge` and `DependencyAgeMax` are the seconds since the registrar feed was
  last read successfully, by `dependency` (`registrar-feed`); the age keeps growing
  while reads fail. `PeersKnown` is the holders the last successful read found.
- `TokenAcquisition` counts each Authorization Framework token acquisition, its one retry
  included, by `outcome`:
  `ok`; `denied` (401 or 403); `unavailable` (no answer, a timeout, an answer cut off,
  or 429, 502, 503 or 504); or `failed` (any other answer, a token that does not
  verify among them). `TokenLatency` is its
  duration, its one retry included.

From v1.0.0, with `METRICS_SERVICE` set, the gateway also writes these EMF
metrics, each beside the ones above (which are unchanged), every dimension
from a fixed list:

- `ExchangeRefused` (`direction`, `rule`, `by`) for every refused call, and
  `ExchangeRefusedByRule` (`rule`), the same count by rule alone, for an alarm
  per rule. It leaves out the exchange, since CloudWatch bills each
  combination of dimensions; the OpenTelemetry metric carries it.
  ExchangeRefusedByRule{rule=fidelity} counts fidelity refusals at stage verify; build-stage refusals count in ExchangeRefused and the OTel refusals instrument with shn.refusal.stage=build.
- `ExchangeStageLatency` (`direction`, `exchange`, `stage`): where a call's
  time went. A leg from the network reports its seven stages (the access
  line's `stages`) and `rest`, the time in none of them. A call from your
  own system reports `network`, its wait on the Hub and the recipient (the
  access line's `network.latencyMs`), and `self`, the rest. A call's stages
  add up to its latency; each line is in whole milliseconds, truncated, as
  `ExchangeLatency` is, so their sum may fall short of it by a millisecond a
  stage. A stage the call did not enter is not reported.
- `RequestBytes` and `ResponseBytes` (`direction`, `exchange`): the size of
  the call's request body, when it was read to its end, and of the answer
  written.
- `ActiveRequests` (`direction`): the most calls in flight at once in the
  minute.
- `ConformanceFinding` (`level`, `kind`, `action`, `tolerated`) for each
  conformance finding recorded, and `ObserveCheckDropped` for each observe
  check dropped because its queue was full or closed: these appear only where
  conformance checks run. `ConformanceFinding` leaves out the exchange, as
  `ExchangeRefused` does; the OpenTelemetry metric carries it. `ValidatorLatency` (`line`, `lane`) is
  each `$validate` call the gateway makes, its conformance checks' and the
  DTR answer check SHN's own line edit relies on, which runs at every level
  where a validator lane serves the line. A lane that is not configured, or
  has not qualified, makes no call and records none.

They add lines to a call's EMF output: for a leg from the network, up to 10
more when answered (its stages, `rest` and its two sizes) and 12 when
refused; for a call from your own system, up to 4 more when answered
(`network`, `self` and its two sizes) and 6 when refused. Each `$validate`
call adds a `ValidatorLatency` line and each finding a `ConformanceFinding`
line, at any level where they happen. Beside these, each Authorization
Framework token a call acquires writes a `TokenLatency` and a
`TokenAcquisition` line, and a call from your own system writes a
`LegOutcome` line for each step of its leg (routed, then its outcome), and
a `LegError` line when the leg failed or was unreachable.
[`MONITORING.md`](MONITORING.md), "What EMF costs", gives the lines each kind
of exchange writes in all, and what they cost.

## OpenTelemetry metrics export (optional)

From v1.0.0 the gateway can push the same metrics over OTLP, or serve them for a
Prometheus server to scrape, or both (every instrument and how to read it:
[MONITORING.md](MONITORING.md)), configured by a closed subset of the
OpenTelemetry SDK's environment settings. It is independent of the
CloudWatch metrics above: each, both or neither. The metrics carry no payload, no
peer's identity and no identifier; the gateway's own holder id appears only on the
resource. Recording never waits on an export, and a vendor that is slow, down or
refusing changes no exchange.

| Env var | Description |
|---|---|
| `OTEL_CONFIG_FILE` | The path of an OpenTelemetry configuration file (`file_format` 1.0). When set, the file governs and every other `OTEL_*` setting in this table is ignored, `OTEL_SDK_DISABLED` included; see "The configuration file" below. |
| `OTEL_METRICS_EXPORTER` | `otlp` turns OTLP push on, `prometheus` the Prometheus pull listener, `otlp,prometheus` both. Unset, or `none`, exports nothing. |
| `OTEL_SDK_DISABLED` | `true` exports nothing, whatever else is set. |
| `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL` | `http/protobuf` (default) or `grpc`. |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | The collector or vendor URL, `http://` or `https://`. For `http/protobuf`, `/v1/metrics` is appended to the general form; the metrics form is used as given. Default `http://localhost:4318/v1/metrics`, or `http://localhost:4317` for `grpc`. A plain `http://` endpoint on another host is allowed, and logged at boot. |
| `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_METRICS_HEADERS` | Comma-separated `key=value` headers sent with every export, values percent-encoded (for example `authorization=Bearer%20<token>`). Values are never logged. |
| `OTEL_EXPORTER_OTLP_CERTIFICATE`, `OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE`, `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE`, `OTEL_EXPORTER_OTLP_METRICS_CLIENT_CERTIFICATE`, `OTEL_EXPORTER_OTLP_CLIENT_KEY`, `OTEL_EXPORTER_OTLP_METRICS_CLIENT_KEY` | PEM files: the CA certificates to trust, and a client certificate and key for mTLS. They need an `https://` endpoint. |
| `OTEL_EXPORTER_OTLP_TIMEOUT`, `OTEL_EXPORTER_OTLP_METRICS_TIMEOUT`, `OTEL_EXPORTER_OTLP_COMPRESSION`, `OTEL_EXPORTER_OTLP_METRICS_COMPRESSION` | One export request's timeout in milliseconds (default 10000), and `gzip` or `none` (default). |
| `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_METRIC_EXPORT_TIMEOUT` | Milliseconds between exports (default 60000) and the bound on one export (default 30000). |
| `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` | `cumulative` (default), `delta` or `lowmemory`. OTLP push only: a scrape is always cumulative. |
| `OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION` | `explicit_bucket_histogram` (default; SHN's buckets from 5 ms to 60 s) or `base2_exponential_bucket_histogram`. OTLP push only: a scrape always has SHN's buckets. |
| `OTEL_EXPORTER_PROMETHEUS_HOST`, `OTEL_EXPORTER_PROMETHEUS_PORT` | The pull listener's bind (default `localhost` and `9464`), its own, never the gateway's listener: it serves `GET /metrics` (and `HEAD`) and nothing else. A loopback bind (`localhost`, or a loopback address such as `127.0.0.1` or `::1`; any other host name counts as another host, and `localhost` must bind a loopback address or nothing is served) needs nothing more. Any other host, `0.0.0.0` included, is served only over TLS and only with `METRICS_PULL_TOKEN`, `METRICS_PULL_CLIENT_CA_FILE` or both; without them nothing is served there and `telemetry-config` says so. A bind that fails (the port taken) turns pull off the same way; the gateway runs on. |
| `METRICS_PULL_TLS_CERT_FILE`, `METRICS_PULL_TLS_KEY_FILE` | PEM certificate and key the pull listener serves TLS with, read once at start (as is the client CA file): a renewed certificate takes a restart. |
| `METRICS_PULL_TOKEN` | A bearer token every scrape must send (`Authorization: Bearer <token>`); a scrape without it, or with another, gets `401`. Letters, digits and `-._~+/`, optionally ending in `=`. Separate from `CHECKS_TOKEN`; never logged. |
| `METRICS_PULL_CLIENT_CA_FILE` | PEM CA certificates: every scrape must present a client certificate they sign (mTLS). Needs the TLS certificate and key. |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | The resource's `service.name` (else `OTEL_RESOURCE_ATTRIBUTES`' `service.name`, else `METRICS_SERVICE`, else `shn-gateway`) and further resource attributes. The resource also carries `service.version`, `service.instance.id` (stable for a host), `deployment.environment.name` (from `METRICS_ENV`, when it is set), `shn.holder_id` and `shn.role`; `OTEL_RESOURCE_ATTRIBUTES` cannot set `service.version` or any `shn.*` attribute. |

The gateway follows the OpenTelemetry specification for these settings except in two
documented ways:

- **Off by default.** `OTEL_METRICS_EXPORTER` unset means `none`, where the
  specification's default is `otlp`: a gateway exports nothing unless you ask.
- **A setting that is not valid never stops the gateway, and never sends metrics to a
  guessed place.** The gateway always boots and logs a line naming the setting (never
  its value) and the fix, and the `telemetry-config` check fails. A setting that only
  tunes export (the interval, the timeouts, the temporality, the histogram aggregation,
  the compression) falls back to its default and export continues. A metrics form that
  is not valid is treated as unset, so a valid general form applies. A setting that
  decides where metrics go or how they authenticate (the endpoint, the protocol, the
  headers, the certificates) turns OTLP export off when no valid form of it remains:
  it never falls back to the built-in localhost default in place of a value you set. An
  entry of `OTEL_METRICS_EXPORTER` the gateway does not recognize is dropped and named,
  and the entries it does recognize still run. A pull setting that is not valid turns
  the pull listener off, and OTLP push, when on, runs on.

Whenever an exporter is on, push or pull, it also carries the Go runtime's metrics
(`go.memory.used`, `go.memory.limit`, `go.memory.allocated`, `go.memory.allocations`,
`go.memory.gc.goal`, `go.goroutine.count`, `go.processor.limit` and `go.config.gogc`)
and the process's: `process.cpu.time` by `cpu.mode` (`user`,
`system`), `process.memory.usage` (the resident memory, on Linux; left out elsewhere) and
`process.uptime`. They follow the OpenTelemetry semantic conventions' names and units, and
are not written to CloudWatch.

Trace, log and propagator settings, and any other `OTEL_*` setting the gateway does not
read, are named in one boot warning and ignored, and so are the pull settings when
`OTEL_METRICS_EXPORTER` does not name `prometheus`. `OTEL_GO_X_DEPRECATED_RUNTIME_METRICS`
has no effect: the deprecated `process.runtime.go.*` and `runtime.uptime` metrics it would
add are never exported, and a boot warning says so. `/internal/checks` reports `telemetry-config`
whenever a monitoring setting, or any other `OTEL_*` setting, is set, and `telemetry-export` (the newest export's
outcome, read without contacting the vendor) while OTLP export is on; both are
`advisory`, so neither ever reads as the gateway not being ready. At stop, the final export runs after the audit outbox drains and the
observe checks flush, within 5 s: one step of the stop
([DEPLOYMENT.md](DEPLOYMENT.md#stopping-the-gateway), "Stopping the gateway").

**The configuration file.** With `OTEL_CONFIG_FILE` set, the gateway reads that file as an
OpenTelemetry configuration file (`file_format` 1.0), with `${VAR}` references replaced from
its environment, and ignores the other `OTEL_*` settings. The file builds the same export
as the settings above, with the same checks, defaults and boot lines:

- `disabled: true` exports nothing. A file with no `meter_provider`, or no reader the
  gateway supports, exports nothing.
- One `periodic` reader with an `otlp_http` or `otlp_grpc` exporter is the OTLP export: its
  `endpoint` (used as given), `headers` and `headers_list` (`headers` win), `timeout`,
  `compression`, `temporality_preference`, `default_histogram_aggregation` and `tls`
  files, and the reader's `interval`, `timeout` and `cardinality_limits`. A cardinality
  limit can only lower the gateway's own limit for an instrument kind, never raise it,
  and only a reader that exports sets one. A timeout or interval of 0 (no limit) is not
  supported: the default applies, and it is reported. A header whose value is null (an
  unset or empty `${VAR}`) is not sent, and reported.
- One `pull` reader with a `prometheus/development` exporter is the Prometheus pull
  listener: its `host` and `port` (defaults `localhost` and `9464`), protected exactly as
  in the table above by `METRICS_PULL_TOKEN`, `METRICS_PULL_TLS_CERT_FILE`,
  `METRICS_PULL_TLS_KEY_FILE` and `METRICS_PULL_CLIENT_CA_FILE`, which the file has no
  place for: a host other than this machine's without TLS and a token or client CA
  serves nothing (the four settings, set beside a file with no pull reader, are named in a
  boot warning). The series are named as in environment mode, so
  `without_scope_info: false`, `without_target_info: true`, a `translation_strategy`
  other than `underscore_escaping_with_suffixes`, `with_resource_constant_labels`, any
  other key, the reader's `producers` and its `cardinality_limits` are ignored and
  reported. The file's views apply to what it serves too, except that a view's
  `base2_exponential_bucket_histogram` is served on the metric's own buckets (the text
  format has no exponential histogram), and reported.
- `resource` `attributes` and `attributes_list` set the resource, `service.name` among
  them; the gateway's own attributes (`service.version` and `shn.*`) cannot be set there.
- `views` apply over the gateway's own: a view may rename a metric, change its
  description or aggregation, or drop attributes (`attribute_keys` `included` or
  `excluded`); it cannot add an attribute. The first view that matches an instrument
  applies, so an instrument is never exported twice. `explicit_bucket_histogram` without
  `boundaries` gives the instrument's own explicit buckets, even where the reader's
  default aggregation is exponential. Ignored and reported: a view's
  `aggregation_cardinality_limit` (set limits by instrument kind on the reader instead),
  `record_min_max`, and an exponential histogram's `max_scale` and `max_size` (the
  gateway's is 160 buckets, scale up to 20). A view that selects nothing (an empty string selects nothing), renames every
  instrument a wildcard selects, or lists boundaries out of order is reported and left
  out.
- `tracer_provider`, `logger_provider` and `propagator` are named in one boot warning and
  ignored: the gateway exports metrics only.
- Reported through `telemetry-config` and turned off, each alone: a second `periodic`
  reader (send to several destinations through an OpenTelemetry Collector), a second
  `pull` reader, a `console` or other exporter, `encoding: json`, and any
  setting that is not valid, named by its place in the file (for example
  `meter_provider.readers[0].periodic.exporter.otlp_http.endpoint`). Ignored and reported:
  `producers`, an `exemplar_filter` other than `always_off` (exemplars stay off),
  `tls.insecure` (the endpoint's scheme decides), `attribute_limits`, `resource`
  `schema_url`, and a `file_format` other than 1.0 (read as 1.0). A client certificate
  without its key, or the reverse, is named by the half that is missing.
- A file that cannot be read or does not parse exports nothing, reported as
  `OTEL_CONFIG_FILE`; the gateway never falls back to the other settings in its place.
  The report never quotes the file, which can hold header values.

## Operational checks (optional)

The gateway can probe its own outbound dependencies — your FHIR system of
record, a partner Da Vinci payer, and every other endpoint URL you've
configured — and report what it found, both automatically at startup and on
demand. This is an operator diagnostic only: a failing probe never affects
`/health` or the request-serving path.

| Env var | Description |
|---|---|
| `CHECKS_TOKEN` | Bearer token that gates `GET`/`POST /internal/checks`. When set, a request must present `Authorization: Bearer <token>` exactly, or it gets `401`. When unset (the default), the endpoint instead accepts only requests that reach it directly from the gateway's own host (loopback), returning `403` for anything else — **note that behind any reverse proxy or load balancer, the connecting address the gateway sees is the proxy's, not the original caller's**, so leaving `CHECKS_TOKEN` unset behind a proxy means only that proxy's own host can reach it, not any external caller. Set a token to allow probing from off-host operator tooling. |
| `SHN_VERSION` | Overrides the version string `GET /health` reports as `version`. Unset, it reports the build's own version (below). SHN's own hosted deployments may set it; a partner's gateway need not. When it differs from the build's version, the boot line names both. |

**The build's own version.** Each gateway binary knows its version, and logs it at boot
(`gateway: build version v1.0.0 (stamp)`). A released image is stamped with its release tag
when it is built: `docker build --build-arg GATEWAY_VERSION=v1.0.0 .` in a clone of the
release. A build without the argument uses the version Go records for the module: the
release tag for `go install github.com/SmartHealthNetwork/shn-gateway/cmd/gateway@v1.0.0`, or
for a build from a clone at that tag. Otherwise it reads as `v0.0.0-unstamped`, which no
check counts as a release. `gateway -version` prints it and where it came from
(`v1.0.0 stamp`) and exits, before it reads any setting: `docker run --rm
ghcr.io/smarthealthnetwork/shn-gateway:v1.0.0 -version`.

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

A result that carries `"advisory": true` reports on the gateway's monitoring
(`telemetry-config` and `telemetry-export`): a failing advisory result means
some monitoring is off or not delivered, never that the gateway is not ready to
carry exchanges. Every other result omits the field.

A result that ran no probe carries `"noProbe": true` and has no latency (`latencyMs`
is 0): the two monitoring checks and each `validator-lane-<line>` (read from the
gateway's own state), an endpoint the network's discovery does not name
(`endpoint-unknown`), and a check the run deadline left unprobed (`not-checked`). A
result whose probe ran omits the field.

A failing result also carries a machine-readable `failure` object: `code` is one of
`unreachable` (the endpoint never answered), `http-status` (it answered with a failing
status), `invalid-capability-statement` (a 2xx answer that is not a valid
CapabilityStatement), `credential-rejected` (the credential check failed), `not-checked`
(the run deadline left this probe unprobed, or no metrics export has run yet), `internal` (a gateway-side bug, not a
network condition), `version-drift` (for a contract both you and the endpoint name, it
publishes none of the versions you declared for it), `invalid-davinci-configuration` (the
endpoint published a `.well-known/davinci-configuration` that could not be parsed;
publishing none is not a failure), `endpoint-unknown` (the network's discovery names no
endpoint for the check, so nothing was probed), `lane-unqualified` (a default validator lane has not
qualified yet, below), or `invalid-telemetry-configuration` (a monitoring setting is not
valid: one that tunes export falls back to its default and export continues, while one
that decides where metrics go or how they authenticate turns OTLP export off; either
way the gateway serves as before, and `hint` names the settings, never their
values); `hint` carries the redaction-safe specifics (for example `HTTP 401`)
and is omitted when the code says it all. Passing results carry no `failure` key. The
same redaction rule applies to `hint` as to `target` and `detail`.

From v1.0.0 the results also include one `validator-lane-<line>` per Compose default
validator lane (a line with no `FHIR_VALIDATE_URL_<line>`). It reads the lane's
qualification from the gateway's own state, dials nothing and starts no attempt, and a
`GET` reads it afresh rather than from the last run. It
passes once the lane has qualified; until then it fails with `lane-unqualified`, whose
`hint` says whether the lane is on its first attempt (`qualifying (attempt 1)`) or
retrying after failures (`failed, retrying (<n> attempts): <reason>`, the reason in
the words of the `validator_qualification` line, never a validator's answer).

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

One check is not a setting: `AUDIT_ENDPOINT` is the audit endpoint the network's
discovery names (`endpoints.audit`), where the gateway sends its audit records (see
[The audit outbox](#the-audit-outbox)). It is checked with a plain reachability request
and, when discovery names none, fails `endpoint-unknown` without probing anything: the
gateway's audit records then wait in its outbox.

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

## Exchange stats (`/internal/stats`)

From v1.0.0 every gateway answers `GET /internal/stats` with a summary of the
exchanges it answered: per direction and exchange kind, how many there were, by outcome,
and their latency at P50, P95 and P99. A call on one of module 1's origination routes
counts with its outcome but adds no latency, as it adds none to `ExchangeLatency` and
`ExchangeStageLatency`: it is a flow of legs, not one exchange's answer. It is always on, with no setting, and does not
depend on the CloudWatch or OpenTelemetry metrics above. It carries no payload, no peer's
identity and no identifier. Each replica keeps its own counts, in memory, from when it
started.

Access is `/internal/checks`' rule: with `CHECKS_TOKEN` set, a request must present
`Authorization: Bearer <token>` (else `401`); without it, only a request from the
gateway's own host is answered (else `403`). Any method but `GET` gets `405`.

The answer is JSON (fields may be added in a minor release; decode tolerantly):

- `replica`: this replica's `service.instance.id`, the value on its OpenTelemetry
  resource. `since`: when it started counting. `generatedAt`: when the answer was read.
- `sketch`: the latency sketch's parameters, `relativeError` (0.02), `minSeconds`
  (0.0001) and `maxSeconds` (300).
- `windows`: `1m`, `5m` and `1h`, each the last 1, 5 or 60 complete minutes plus the
  current one, from `start` (never before `since`) to `generatedAt`. Each window
  lists, for every series with any traffic in it:
  - `exchanges`: `direction`, `exchange`, `count`, `rate` (exchanges per second over
    the window), `outcomes` (counts by outcome, the values of the `Exchange` metric's
    `outcome`) and `latency`, from arrival to answer;
  - `backends`: the exchanges that called your own system, by `exchange` only (the
    backend metrics carry no direction), as the `BackendCall` metric counts them:
    `count` (one per such exchange, whatever the call's class) and `latency` (that
    call's duration, where it is known).

  A `latency` gives `p50`, `p95`, `p99` and `max` in seconds, absent when no duration
  was recorded, and the `sketch` they were read from. Each quantile is within the
  sketch's relative error for durations from `minSeconds` to `maxSeconds`; a shorter
  one reads as 0, and a longer one as the top bucket's value (about 299 s), while
  `max` states the longest exactly.

**Merging replicas.** The quantiles of several replicas cannot be averaged, but their
sketches merge exactly. A sketch is `zero` (values at or below `minSeconds`), `buckets`
(each `[index, count]`, ascending; an index can be negative) and `max`. When two answers
carry equal `sketch` parameters, add their `zero` counts and the counts of equal indexes,
and take the larger `max`. Answers whose `sketch` parameters differ (another release's
bucketing) must not be merged: their indexes name different buckets. To read a quantile `q`
from `n` values, take the first bucket, zero bucket first and then in index order, at which
the running count reaches `q·n` (the nearest rank, the bucket Prometheus'
`histogram_quantile` reads from for a classic histogram; for a native one at `q` ≥ 0.5 it
can read the next value up, which it does where `(1−q)·n` comes out a whole number, as it
always does at the median). It reads 0 for the zero bucket, else `2·γ^i/(γ+1)` for bucket
`i`, where `γ = (1+relativeError)/(1−relativeError)`, and never more than `max`.

`shn gateway stats`, in the `shn` CLI from shn-sdk v1.0.0, does this for you: give it
each replica's base URL (`--url`, repeatable) and the `CHECKS_TOKEN` in a file
(`--token-file`), and it prints one table by exchange for the window you name
(`--window`, `5m` by default; `--json` for JSON). Rates add across replicas. It
refuses answers with different `sketch` parameters, a malformed answer, and one replica
given twice, naming the URL.

## The audit outbox

The gateway writes audit records about its own work and sends them to the network's
Audit Plane. As of v1.0.0 it writes none yet; the outbox that will carry them is in
place. There is no setting for it.

- **Signed by your gateway.** Each record names your gateway's holder as its sender
  and is signed with the holder signing key in your `SHN_SECRETS` bundle. Before it is
  queued, a record is checked against the shape the Audit Plane accepts from a gateway,
  bounded more tightly where the Plane leaves a field open; one that fails is refused,
  counted, and never stored.
- **Signed when it is sent.** The outbox keeps each record unsigned, and the gateway
  signs it with the key it is running with each time it sends it. To rotate your
  holder's signing key (`shn rotate`), stop the gateway, rotate, and start it with the
  new bundle: records still in a durable outbox go out under the new key.
- **Sent to the endpoint discovery names.** The gateway reads `endpoints.audit` from the
  network's discovery once, at start, as it reads the network's other endpoints; no
  setting overrides it (`AUDIT_URL` is not read). When discovery names none, records are
  queued and nothing is sent: the `AUDIT_ENDPOINT` check fails `endpoint-unknown` and the
  gateway logs it at start.
- **Where it waits.** With `SHN_STORE_DATABASE_URL` the outbox is the table
  `gw_audit_outbox` in that database: audit records waiting to be signed and
  sent, metadata only (no clinical content), each deleted once the Audit Plane
  accepts or refuses it. It survives a restart, and replicas sharing the database
  share it, never sending one record twice at once. Without the DSN it is in memory:
  at a stop the gateway sends what it can for up to 10 seconds, and counts what is
  left as lost. Before it starts, it reports how many records it holds as
  `AuditQueueDepth` and, when discovery names an audit endpoint, logs the number, so a
  gateway killed during the drain still leaves that count
  ([DEPLOYMENT.md](DEPLOYMENT.md#stopping-the-gateway), "Stopping the gateway").
- **Bounded.** It holds at most 10,000 records for your holder. One more drops the
  oldest, counted, so it cannot grow without limit while the Audit Plane is out of reach.
- **Retried.** A record the Audit Plane refuses (400, 403, 413, a 401 other than "unknown
  sender", or a 409 for a record id it already holds) is dropped, counted, and not sent
  again. Any other answer short of acceptance (none at all, a 5xx, a 408 or 429, or a 401
  "unknown sender" while the Plane does not know your holder yet) is sent again, waiting
  1 second after the first failure and doubling to at most 5 minutes.
- **Never in the way.** Queuing a record never fails or holds up an exchange: it waits at
  most 200 ms for the outbox, and a record that cannot be queued in time is counted and
  dropped while the exchange goes on as before.

With `METRICS_SERVICE` set the outbox reports `AuditAppendSent`, `AuditAppendRetried`
(`reason`), `AuditAppendDroppedBy` (`reason`: `overflow`, `invalid`, `store`, `refused`,
`conflict`, `stopped`), the gauge `AuditQueueDepth` each minute, and, while discovery
names no audit endpoint, the gauge `AuditEndpointUnknown` with the queue's depth.
`AuditAppendDropped` (`Service` only, for a single-metric alarm) counts every record that
left the outbox without the Audit Plane's acceptance, whatever the reason.

## Connect your system of record

See [INTEGRATION.md](INTEGRATION.md) for how these fit together.

| Env var | Description |
|---|---|
| `ORIGINATION_PROFILE` | provider. Set to `provider-data` to originate every prior-auth UC off your seeded FHIR system of record and drive real payer verdicts — the config-only provider lane, no custom code. `demo` originates the shipped demo order set instead. Both lanes answer a REAL payer's questionnaire, so both require `PROVIDER_DTR_POPULATE_URL` (the operated `$populate` endpoint, validated at boot). Setting either is your declaration that your system cannot speak Da Vinci itself, so the gateway runs module 1, the `/scenario/*` origination API, for it. Unset, module 1 is off: the gateway serves its Da Vinci ingress and relays, mounts no `/scenario` route (each answers `404`), and needs no module 1 setting; `SCENARIO_CALLERS_FILE` then refuses to boot, and a set `NPI`, `PROVIDER_DTR_NATIVE`, `PROVIDER_DTR_POPULATE_URL` or `PROVIDER_DTR_POPULATE_TOKEN_URL` is unused and named in a boot warning, though still checked at boot (an invalid `NPI`, or an incomplete `PROVIDER_DTR_*` group, refuses boot). Before shn-gateway v1.0.0, unset meant `demo`. From shn-gateway v0.57.0 any other value refuses to boot, on every role, with an error naming the accepted values; that includes `demo` or `provider-data` in another case or with surrounding whitespace, since the value is matched exactly. Earlier releases accepted an unknown value and built the requests the gateway originates on no known lane. |
| `FHIR_DATA_URL` | FHIR R4 base URL for your system of record. **Required on every role but a native-forward payer.** The gateway reads its members, coverage and clinical facts from your own FHIR server; there is no built-in persona stub any more, so an unset value is a boot error naming this variable. From shn-gateway v0.58.0 (earlier releases refuse to boot without it on every role), a payer whose own system answers every exchange (`PAYER_DAVINCI_BASE_URL`, below) may leave it unset: its gateway then keeps no system of record. It binds each member from the request that names it, exactly as it binds a member a system of record does not hold (see [Members your system of record does not hold](#members-your-system-of-record-does-not-hold-every-role)), and answers coverage eligibility only through `PAYER_ELIGIBILITY_URL`: without that endpoint, an eligibility request is answered `501` "coverage eligibility is not offered by this payer". It logs `gateway: no system of record (FHIR_DATA_URL unset)` at boot. `REQUIRE_KNOWN_MEMBERS=true` needs a system of record, and refuses to boot without one. |
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

### System-of-record search errors

When your FHIR server answers one of the gateway's searches (the coverage read to route by,
the prefetch values, an originated request's draft order, a federated query's records) with an
error status, the built-in FHIR connector tells two answers apart, by the status and the
answer's `OperationOutcome` alone (`diagnostics` and other text are never read):

- **Your server cannot run the search:** `501 Not Implemented`, or a `4xx` other than `401`,
  `403` and `429` whose `OperationOutcome` has at least one issue of severity `error` or `fatal`,
  every one of them with the code `not-supported`. Your system, as connected, cannot supply
  the value, as for a connector that cannot search: a request that needs it is refused `422`,
  a prefetch key is left out, and a federated query reads the connector's own records
  (`FacilityRecordsContext`).
- **Any other error answer** (`400`, `404` or `422` without that `OperationOutcome`, or one
  that pairs `not-supported` with an error of another code): what your system holds is
  unknown, so the gateway never reads it as nothing. A request that needs the value fails
  `502`, a backend failure, never a refusal; a prefetch key is left out, as for any search
  your server cannot answer.

`401`, `403`, `429` and `5xx` (other than `501`) are your server not answering (`503`).

One read does not yet tell the two apart: a request your gateway originates
(`ORIGINATION_PROFILE`) first reads the coverage to route by, and behind the built-in FHIR
connector that read answers `502` for both, as for an answer past the gateway's bounds.

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
notes `pend.amendment-unbound` with the reason as its `detail`: `not-pended` (no pend here),
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
- **An authorization is keyed by its patient and its correlation id.** The ledger files
  each authorization under the payer's own binding of the member the request names as
  well as the correlation id, so a submission for another patient under a correlation id
  an authorization already holds is carried to the payer and answered like any other, and
  its authorization is that patient's own. Neither patient's later amendment or inquiry
  reaches the other's: an amendment binds only its own patient's pend, and an inquiry
  answer naming another patient's authorization records nothing (`pend.other-subject`).
  The same patient's submission under the same correlation id lands on the same
  authorization, so a retry is answered too. (The Hub refuses only a byte-identical
  envelope; a retry sealed afresh reaches the payer.) Before v0.63.0 another patient's
  submission was refused (`409 correlation id already names another patient's
  authorization`, or `502 holder read failed` when the store could not be read) before
  the payer was asked.
- **An answer the gateway could not read writes no row.** Below enforcement `strict`,
  a payer answer your gateway cannot read, or whose patient linkage is inconsistent, is
  relayed to the requester exactly as your system sent it, and nothing is recorded from
  it: no pend and no decision (the gateway emits `pa.local-write-skipped`). An amendment
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
- **No ExplanationOfBenefit is built from your decision.** From v0.63.0 the gateway records
  your decision on the ledger only (decided, with your outcome and date) and writes no
  ExplanationOfBenefit, on submission or from an inquiry's answer. Before v0.63.0 it also
  wrote a base FHIR R4 decision ExplanationOfBenefit as its own record and reported what
  it could not write (`pa.decision-eob-not-written`, `pend.inquiry-eob-withheld`,
  `pend.eob-removed`); those events are gone. The store no longer reads or writes its
  ExplanationOfBenefit table: a row an earlier release wrote stays, read by nothing, until
  a later release drops the table on first boot.
- **Your decision is never refused over this gateway's record of it.** A decision that
  reads as a denial yet states a top-level `preAuthRef` may be a partial certification,
  which the ledger's reading cannot hold: it is relayed exactly at every level and nothing
  is written from it (`pend.decision-not-recorded`). Before v0.62.0 strict answered it
  with 502. No event or log line about it echoes a value you stated.
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
- **This gateway's own originator flows** — the `/scenario/*` routes and the
  headless provider-data runs — have no such system behind them, so
  the gateway keeps a **continuation** for them: an opaque id, and the metadata an
  inquiry is built from. It stores identity, routing, the member, the identifiers
  the payer answered with, and the sequence, product code and service date of each
  submitted line. It stores **no clinical content**: at inquiry time the order is
  re-read from your own system of record, and a request that has since become a
  different one is reported (`409 order changed since submission`) rather than
  followed. The patient, the coverage, the requesting provider and the payer
  organization are read again too, and, from v1.6.0, so is the record of a person the
  member's Coverage names as its `subscriber` or `policyHolder` by a reference to
  another `Patient` record (a dependent's parent), by the submission's own rule: the
  inquiry carries it as a `Patient` entry of its own, as the submission did. A person
  your system does not hold, or whose record cannot ride the inquiry, refuses the
  inquiry on `POST /scenario/pa/inquire` (`422`, recorded as `fidelity` at stage
  `build`); your system failing to answer that read is a backend failure (`503`), and
  so is its answering with another record than the person's (`502`). On a route's own
  wait each keeps the pend: no inquiry is sent, and the observer event
  `pa.inquiry-failed` names why.

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
- **`/scenario/*` is a local operator surface and is never publicly routed.** The
  public ingress and the hosted door admit the participant's own
  `POST /Claim/$inquire` and refuse everything under `/scenario/`. Do not give that
  surface a public host.
- **`/scenario/*` authenticates its caller when `SCENARIO_CALLERS_FILE` names them
  (provider, new in v0.63.0).** The file is a JSON array of the callers the routes
  admit: `[{"label":"monitor","public_key":"<base64 Ed25519 public key>"}]`. Each
  request must then carry its caller's `X-Holder-Assertion`, signed with that
  caller's own key for this gateway (package `scenarioauth`; `scenarioauth.Client`
  signs for you). The assertion's audience is `scenario:` and the gateway's own
  holder id, from its bundle, so an assertion for another gateway is refused; a
  caller using `scenarioauth.ClientFromEnv` names that holder id in
  `SCENARIO_GATEWAY`. Each assertion is admitted once (recorded in the gateway's
  one-time-use store; if that store is unavailable the request is refused `503`,
  never admitted). Any other request is refused `401 {"error":"unauthorized
  caller"}` before the route runs, at every conformance level, and sends nothing.
  An empty array admits no caller. A missing, unreadable or malformed file, or the
  variable on another role, refuses to boot. Unset, the routes do not authenticate
  their caller, as before, and the gateway logs a warning at boot saying so; a later
  release refuses every `/scenario` call when no callers file is set.

  To name a caller, run `shn scenario-key --bundle <your bundle>` (shn-sdk v0.63.0
  or later), set `SCENARIO_CALLERS_FILE` to the `scenario-callers.json` it writes in
  the bundle, and restart the gateway. It writes the caller's key outside the bundle,
  readable by you only. `shn send-test --scenario-key <key> --bundle <your bundle>`
  then drives the scenarios signed as that caller, and `shn scenario-sign` prints
  the header that signs one request, for any HTTP client.

## Accept Da Vinci requests from a provider EHR (provider, optional)

See [INTEGRATION.md](INTEGRATION.md#native-da-vinci-ingress) for when to use this
instead of `provider-data` origination.

Set `PROVIDER_DAVINCI_INGRESS=1` to mount the provider-side Da Vinci ingress: the
gateway accepts a provider EHR / reference-implementation's **native Da Vinci
requests** — CDS Hooks `order-sign`, `order-select` and `order-dispatch` (Coverage
Requirements Discovery), `Questionnaire/$questionnaire-package` (DTR), `Claim/$submit`
(PAS), `Claim/$inquire` (the follow-up that asks a payer for the decision on an
authorization it pended) and, from shn-gateway v0.63.0, `CoverageEligibilityRequest/$submit`
(coverage eligibility) and `POST /Task` (a consented federated query, the Da Vinci CDex
task-based data request) — and forwards them through to the Hub. A CDS Hooks request is forwarded as your EHR
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

**A coverage eligibility request** (from shn-gateway v0.63.0). `POST
/CoverageEligibilityRequest/$submit` is the base FHIR R4 operation, which IHE FAIS profiles as
ITI-124. Send any of the operation's inputs as the body:
- the `CoverageEligibilityRequest`;
- a `Bundle` carrying one request with the resources it references (IHE FAIS's
  CoverageEligibilityRequest Bundle: the request and its Patient, Coverage, Organization and
  other entries);
- a `Bundle` that is a batch of at most 17 requests, each one alone or in such a Bundle of its
  own;
- a `Parameters` whose one `resource` parameter holds any of these (the operation's
  invocation).

Whichever you send is carried to the payer exactly as sent, in the form you sent it, as one
exchange, and the payer's answer is relayed exactly. A batch carries at most 17 requests: the
exchange's patient and the 16 other patients the network can record one exchange under (the
Hub's involved list). A batch of more, in any form, could not be recorded under every patient it
names, so it is refused before anything is read from it, at every level, by your gateway and by
the payer's: `413 the batch carries <n> coverage eligibility requests; a gateway reads at most 17 in one batch, so send them in batches of at most 17`. Send a larger set of requests as several batches. A `Bundle` that is neither form (two
requests beside other resources, or a batch entry that is a Bundle of more or fewer than one
request) is refused `400`, as is one whose entries cannot be read one way only: an entry's
`resource`, `fullUrl`, or its resource's `resourceType` or `id` written in another case or with
another JSON type, or an entry with no typed resource (`400 … the Bundle's entries cannot be read
one way only`). Every input, a request sent alone included, is read by its exact member names,
as the payer's gateway reads it. Each gateway refuses these requests with the same text, at
every level:
- a request that repeats a member name, exactly or in another case, anywhere in the body, can
  be read as naming two patients: `400 eligibility request repeats the JSON member "<name>" in
  <where>: it can be read two ways, so it is not read` (message integrity);
- a request that names only in another case a member its patient is read through (its
  `resourceType`, a `Parameters`' `parameter`, the `name` of any of its parameters or the
  `resource` of its `resource` parameter, its `patient`, the patient's `reference`, or, for a
  `#id` patient reference, its `contained`) names no patient,
  since member names are case-sensitive: `400 eligibility request names "<member>" for
  "<name>": member names are case-sensitive, so the request names no patient to bind` (the
  patient the exchange is for cannot be identified).

A request's `contained` resources are read for its patient only through a `#id` reference. The
provider's gateway also reads them to route the request, and refuses a request whose
`contained` it cannot read one way only (`400 parse eligibility request failed`).

None of these texts carries a value from the request. A Bundle entry whose own members are named
in another case is refused as above (`… the Bundle's entries cannot be read one way only`).
- **Patient.** Each request's `patient` is read in any reference form, as on the payer side, and
  bound to a patient from your system of record (else from the member id and the Patient carried
  with the request). In a Bundle, a reference resolves to the Patient entry it names: that
  entry's id (so send the member id as the Patient's id, as for `Patient/<id>`), or with no id
  its member identifier. A request in a Bundle (either Bundle form) is read by its exact member
  names: a patient reference that is empty, written with a member name in another case, or is
  neither a `Patient/<id>` nor a `#id` reference and names no Patient entry (a mistyped
  `urn:uuid`, or a `urn:uuid` on a request alone in a batch, which carries no entries) names no
  patient, and is refused `400`. The exchange's patient is the first request's,
  in document order; every other request's patient is bound too and named on the exchange's
  involved list, so the network records the exchange under each of them. With
  `REQUIRE_KNOWN_MEMBERS=true`, a request for a member your system does not hold is refused,
  naming which request of the batch.
- **Routing.** Each request is routed by its `insurer`, the request's recipient: its
  identifier, or an Organization the request carries; failing that, by the first payor of each
  Coverage it carries and names in `insurance` (an identifier, or an Organization it carries).
  Every coverage it names must name a payer, and all of them the same one: a coverage naming
  none is not passed over for another. A reference is resolved among the resources the
  request carries: a `#id` among the referring resource's contained resources, and, in a
  Bundle, any other reference among the Bundle's entries (by `fullUrl`, or type and id); every
  member is read by its exact name. A relative `Organization/<id>` insurer or payor, or
  `Coverage/<id>` coverage, that nothing the request carries answers is read from **your own
  system of record**, only to route and never carried: only when that system names the
  request's patient by its member id (`Patient/<member id>`), since its records are then the
  request's. A Coverage read so is read for its payor, which resolves among its own contained
  resources, then the same way. A reference written absolute, versioned, or with a fragment is
  never read; a read that fails is your system's failure (`502`, or `503` when it is
  unavailable), as is an answer that is not the resource asked for. The
  requests of a batch must all name one payer: one exchange has one recipient, so a batch whose
  requests name different payers is refused `422 the eligibility requests name more than one
  payer: …`, naming the requests and payers; send each payer's requests separately. A request of
  a batch that cannot be routed is refused as below, naming which request. No default: when the insurer names no payer
  identifier and the request names no coverage, it is refused `422 no payer to route the
  eligibility request by: …`; when the insurer names none and a coverage names none, `422 …: its
  insurer names no payer identifier, and a coverage it names in insurance names none, …:
  insurance[i].coverage "<reference>"`. A reference of the expected type that nothing resolves
  is refused naming itself and what resolves a reference of its form (`422 …: its insurer is a
  reference to an Organization the gateway could not resolve, …; <remedy>: insurer
  "<reference>"`, `422 …: a coverage it names in insurance is a reference to a Coverage the
  gateway could not resolve; <remedy>: insurance[i].coverage "<reference>"`, and `422 …: the
  payor of a coverage it names in insurance is a reference to an Organization the gateway could
  not resolve; <remedy>: insurance[i].coverage "<reference>", payor "<reference>"`):
  - a relative `Organization/<id>` or `Coverage/<id>`: send the resource with the request, name
    the payer identifier, or hold the resource in your system of record with the request's
    patient;
  - a `#id`: send a contained resource with that id;
  - a `urn`, or an absolute reference: send a Bundle entry whose `fullUrl` it is (an absolute
    reference is not read from your system);
  - any other form naming the type (versioned, a leading slash): send the resource with the
    request, since only a relative reference is read from your system;
  - the payor of a Coverage read from your system: only that Coverage's payor identifier or
    contained Organization, or a relative `Organization/<id>` your system holds, resolves it.

  Every form can also name the payer identifier instead. A reference to another type than the
  one expected names no payer and gets the refusal of a request naming none;
  coverages naming different payers, whether carried or read from your system, `422 the eligibility
  request names more than one payer: …`; a contained id it is routed by that more than one
  contained resource has, or a reference more than one Bundle entry answers (unless they are
  Organizations naming one payer), `422 ambiguous reference for routing: …`; an unregistered
  payer, `422 no registered payer for identifier …`.
- **The payer's answer** is relayed exactly. One that repeats a member name is refused `502`
  at every level. One that is not a `CoverageEligibilityResponse` naming its patient, or a
  `Bundle` of them (each with the resources it references), is relayed
  at `none` and `observe` (recorded at `observe`) and refused `502` at `structural` and
  `strict`. It is `$validate`d at your conformance level, as the answer to an eligibility
  request your gateway builds itself is: recorded at `observe`, refused `502` at `structural`
  for a structural issue and at `strict`; a validator your gateway cannot reach refuses `500`
  at `strict` and is recorded at `observe` and `structural`. Which patient it names is checked by the payer's
  gateway, at that gateway's own conformance level, since only it knows the payer's own Patient
  ids (see `PAYER_ELIGIBILITY_URL`).
- **Your request** is carried as sent and not `$validate`d by your gateway; the payer's gateway
  validates each request at its level. A `Parameters`' other parameters are carried as sent,
  and no check reads any of their members but `name`, at any level.
- **A payer that does not read the `Parameters` or `Bundle` forms** (a payer gateway before
  v0.63.0, or a payer answering through an shn-sdk Responder before shn-sdk v0.63.0)
  answers them `400 parse member failed`, which is relayed to your system. Send the request
  itself to reach it.
- **Not certified.** Like every coverage-eligibility leg, this route records no certification
  evidence. If it ever does, it certifies the request as `$validate` reads it: the
  `CoverageEligibilityRequest` itself, unwrapped from a `Parameters`, never the bare
  `Parameters`, which a FHIR validator cannot read as an operation input.
- **A payer gateway before v0.63.0** read a request sent alone, or in a `Parameters`, by a
  reading that ignores the case of member names and takes the last of a repeated one, so it
  answered about the member that reading found. From v0.63.0 a payer gateway refuses such a
  request as above.
- **A batch on a payer answering from its records.** A payer gateway that answers eligibility
  from the payer's records answers one request at a time: it answers a batch `501 this payer
  answers coverage eligibility from its records one request at a time, so it does not answer a
  batch of requests: this is the payer's answer, not a network refusal`,
  its own answer, which is relayed to your system. A payer that declares its own endpoint
  (`PAYER_ELIGIBILITY_URL`) receives the batch as you sent it.
- **Advertised from shn-gateway v1.0.0.** The ingress `CapabilityStatement` (`GET /metadata`)
  lists this operation, `CoverageEligibilityRequest/$submit`. shn-gateway v0.63.0 serves it
  without listing it.

**A consented federated query** (from shn-gateway v0.63.0). `POST /Task` takes the Da Vinci CDex
task-based data request a data consumer sends a data source
(<https://hl7.org/fhir/us/davinci-cdex/task-based-approach.html>): a `cdex-task-data-request`
Task with one `data-query` input, for one patient's named documents (one Task per document
type). It is carried to the facility exactly as sent, and the facility's answer (a facility's
gateway answers with your Task fulfilled with the records) is relayed exactly.
- **Routing.** The Task's `owner` names the facility by holder identifier
  (`http://smarthealth.network/ids/holder`), as an SDK-built CDex Task does. No default: an owner
  that names no holder identifier (a value of another type among them) is refused `422 no
  facility to route the federated query to:
  the Task's owner names no holder identifier …`; one naming a holder the network does not
  register, or a holder that is not a facility, `422 no facility to route the federated query to:
  the Task's owner "<id>" …`, saying which.
- **Patient.** `Task.for` is read as the facility's gateway reads it, in any reference form, and
  bound to a patient from your system of record (else from the member id and the Patient the
  Task carries). A Task that is not a data request for one patient's named documents, or that a
  FHIR reader could read another way (a member repeated, or named only in another case, the
  owner's among them), is refused `400 federated query rejected: …`.
- **Consent.** The Authorization Framework checks consent for that patient with the owner as the
  custodian: without it the query is refused `403 authorization denied` and nothing is sent. The
  facility's gateway then checks consent again, with its own backstop, and that the patient it
  is asked about is the consented one; its refusal is relayed exactly.
- **What the network guarantees.** Consent is enforced where the disclosure is made: the
  facility's gateway checks it with its own backstop, binds the queried patient to the
  consented one, and builds the answer it serves from its system of record from that patient's
  records alone, at every level. The network does not guarantee that a facility's own system,
  answering through its gateway, discloses records about one patient only.
- **The facility's answer** is relayed exactly. One that repeats a member name is refused `502`
  at every level. One that reads as about a patient other than the queried one (the answer
  Task's own patient, or any resource it contains) is relayed at `none`, relayed and recorded
  at `observe` and `structural`, and refused `502` at `strict`; this is a best-effort content
  check, and free text and encoded data are not judged. One that is not
  a Task fulfilled with a records Bundle (a data-query output naming a Bundle it contains; it is
  not compared with your Task) is relayed at `none` and `observe` (recorded at
  `observe`) and refused `502` at `structural` and `strict`. It is `$validate`d at your
  conformance level: recorded at `observe`, refused `502` at `structural` for a structural issue
  and at `strict`; a validator your gateway cannot reach, or has no lane for, refuses `500` at
  `strict`.
- **Your Task** is carried as sent and not `$validate`d by your gateway.
- **Not advertised yet.** The ingress `CapabilityStatement` (`GET /metadata`) does not list this
  interaction until a later shn-sdk release. The gateway serves it before then.

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
patient (`422 no coverage to route by: send the coverage parameter (this gateway's records
name no patient for this member)`), holds no Coverage
(422), when the Coverages it chooses (the active ones, else the others) name different payers
(422), cannot search, including a server that says definitely that it cannot run the search
(422), answers the search with any other error (502, see [System-of-record search
errors](#system-of-record-search-errors)) or is unavailable (503). A Coverage that references its payor
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
DTR operations (upgrade required)`. The payer's answer is returned to your EHR exactly, with
one exception from v0.62.0: when `SHN_CONTRACT_VERSIONS` declares exactly one DTR line and
the payer answers in the other DTR line's parameter names, the gateway renames them to your
line's (registered edit E-08): `PackageBundle` (DTR 2.0.1 and 2.1.0) and `packagebundle`
(2.2.0) for the package, and toward 2.2.0, `operationOutcome` to `outcome`, and carries
the package's QuestionnaireResponse across the lines through the registered DTR steps (the
Coverage context's place and the intended-use code system). Every other byte is the payer's,
and from v1.3.0 the payer's `ETag` is left out of the answer so changed, since it named the
payer's bytes. It does so only when the answer is valid at its own line; otherwise the answer is
returned as sent (and judged as a payer's answer at your conformance level). From v1.2.0, when
that check could not run, or the lane for that line is still qualifying (or, at `none`, the lane for
your own line), whether to rename is unknown, so the answer is returned as sent and the access line's `editsNotMade` says
`E-08:check-could-not-run`: it is never renamed on a guess, even if another line of its name
family validates. At `structural` and `strict` the gateway's own check of the answer runs first and
can refuse it, and then `editsNotMade` stays empty. An answer valid at its own line that
cannot be valid at your line (a 2.0.1 package with no QuestionnaireResponse, which 2.2.0
requires, several outcomes toward 2.2.0, or a Questionnaire written for another line) is
refused at every level, the
refusal saying the payer answered: the gateway never fills. Declaring several DTR lines
keeps every answer exact, since the gateway cannot know which names your EHR reads. From
v0.63.0, a gateway at `none` with no validator for its own DTR line, which it boots with only
at `none`, never renames: it returns the answer exactly.

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
is refused with 422, never defaulted). The payer's answer reaches your EHR exactly (apart
from a pend's `payer-url`, below), in whichever shape the payer's prior-authorization line defines: the response Bundle
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

`POST /$submit-attachment` sends documentation for a prior authorization to a payer: a
CDex 2.1.0 `$submit-attachment` (a `Parameters` with its `TrackingId`, `AttachTo`,
`MemberId`, `PayerId`, `OrganizationId` or `ProviderId`, and one or more `Attachment`). From
v1.1.0, it is in the evolving tier (see `STABILITY.md`). The submission is
carried to the payer as your EHR sent it, and the payer's answer (its `OperationOutcome`,
informational or an error) reaches your EHR exactly; an answer that repeats a member name,
so it can be read two ways, is refused with 502 instead (message integrity). It is routed by its `PayerId` to the
registered payer that identifier names; a submission naming none, two that differ, or a
payer the network does not register, is refused with 422 before it is sent. Each registered payer also has a
**return path** on this gateway, `{PROVIDER_DAVINCI_INGRESS_BASE_URL}/cdex/payer/{payer}`:
a submission posted there, or to that path with `/$submit-attachment` appended, goes to that
payer whatever its `PayerId` says (a path naming no registered payer is refused with 422): a submission with a `PayerId` that names another payer,
or one the network does not register, is carried and noted once as the observer event
`cdex.payer-id-differs`. `GET /cdex/payers` lists the registered payers
that take the submission, each with its participant id, the payer identifiers it registered and its return path:
`{"payers":[{"participant":"…","payerIdentifiers":[{"system":"…","value":"…"}],"returnBase":"…"}]}`.
The submission's patient is bound from its `MemberId`, as the other legs bind theirs: a
member your system of record does not hold is carried (unless
`REQUIRE_KNOWN_MEMBERS=true`), and a submission that is not a `Parameters`, or has no
readable `MemberId` or two that differ, is refused with 400, since the network cannot
identify the patient; one that repeats a member name is refused with 400 too, since it can
be read two ways. A body larger than the attachment ceiling is
refused with 413 before it is read further, and a payer that does not take the leg is the Hub's
`422`, with its reason as it stands. The submission rides no declared contract line,
so `SHN_CONTRACT_VERSIONS` and `REQUIRE_SHARED_LINE` do not apply to it. This gateway's
`.well-known/davinci-configuration` names the ingress base as its CDex endpoint
(`davinci_cdex_attachsubmit_endpoint#2.1`).

**A pend's documentation return address.** From v1.1.0, by default, the gateway re-keys the
`payer-url` of each Task in a payer's answer to `$submit`, an amendment or `$inquire`
before it reaches your EHR (registered edit E-10, `pas-pend-return-rekey`): the Task's
`payer-url` `valueUrl` becomes your gateway's documentation return path for the payer that
answered, `PROVIDER_DAVINCI_INGRESS_BASE_URL` + `/cdex/payer/` + that payer's participant
id. Documentation your EHR sends there with `$submit-attachment` reaches that payer through
the network. Nothing else in the answer's body changes, and from v1.3.0 the payer's `ETag`
is left out of that answer, since it named the payer's bytes. A `payer-url` in another form than a
`valueUrl` is carried as the payer sent it, and the exchange record's `EditsNotMade` says
so (`E-10:no-structural-payer-url`). So is the `payer-url` of a payer not known to take the
documentation submission (`E-10:payer-does-not-take`): one whose registry entry lacks the
leg, one that declares no list, as a payer on an earlier release does, or one your gateway
cannot read. The Hub refuses that leg to a payer that does not take it. The re-key is made even where a signature covers the
`payer-url`, so that signature no longer verifies in your copy. The answer then carries
`X-SHN-Edited: pas-pend-return-rekey`, with `; signed="<signatures>"` added when a
signature covered the address.

What the gateway changes on these requests is only the callback removal and the prefetch and
coverage additions above. A signature inside a message (`Bundle.signature`,
`Provenance.signature`, a `Signature` element) travels untouched, apart from the `payer-url`
re-key above and, from v1.5.0, the re-key of a payer's
attachment-request Task (below). HTTP-level signatures
(signed header fields, a detached JWS) are not carried: each gateway terminates HTTP, and the
network carries only the message's media type, contract line and operation name with it. A
participant that needs an end-to-end signature signs inside the payload. The answer's body
reaches your EHR exactly, apart from a pend's `payer-url`; its media type is the gateway's own (`application/json` for CDS
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
resend of the same submission lands on the same authorization. A different patient's
submission under a Claim correlation another patient's authorization already holds is
carried to the payer and answered (before v0.63.0 it was refused with `409` before the
payer was asked): the payer gateway keys each authorization by its patient as well.

| Env var | Description |
|---|---|
| `PROVIDER_DAVINCI_INGRESS` | Set to `1` to mount the ingress on the provider gateway. |
| `PROVIDER_DAVINCI_INGRESS_BASE_URL` | The gateway's public base URL — the SMART audience the gateway pins and the token endpoint it advertises, and the base of its documentation return paths (`/cdex/payer/<payer>`). **Required** when the ingress is enabled. From v1.1.0 it must be an absolute `http` or `https` URL with a host, no `?` or `#` and no user information, or the gateway refuses to start. |
| `INGRESS_CLIENTS_FILE` | Path to a JSON array of registered inbound clients: `[{"client_id":"…","alg":"ES384","public_key_pem":"-----BEGIN PUBLIC KEY-----…","scopes":["system/Davinci.write"]}]`. **Required** (≥1 client) when the ingress is enabled. From v0.63.0 an entry may add `"module": "on-behalf"` to register that client as a module ingress client ([below](#a-module-ingress-client)); an unknown module, or a `builders` key on any entry, refuses to boot. |
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

### A module ingress client

From v0.63.0. A module ingress client builds Da Vinci requests on your behalf: it plays the Da
Vinci system your own system cannot be, and sends its requests to this ingress as your EHR does.
Its requests are the gateway's own messages, not your system's, so the gateway treats them
differently:

- **Registering it.** Mark one of your registered clients with `"module": "on-behalf"` in
  `INGRESS_CLIENTS_FILE`. `on-behalf` is the only module this build has. The builders such a
  client may name are the module's: `sdk-crd-request`, `sdk-dtr-package`, `sdk-pas-submit`,
  `sdk-pas-update`, `sdk-pas-inquiry`, `sdk-eligibility` and `sdk-federated-query`. They are not
  configurable, and an entry that lists `builders` refuses to boot.
- **Naming the builder.** Each request names its builder in the `SHN-Builder` header, the one
  that builds that leg's request (for example `sdk-pas-submit` on `POST /Claim/$submit`).
- **How it is sent.** The gateway sends the module's bytes exactly as built. It makes none of the
  edits it makes to your system's requests (removing a CDS Hooks callback, filling prefetch or a
  questionnaire request's coverage or patient), so the module builds each request whole. The
  exchange record names it `ownership: authored` with its `builder`.
- **What is refused.** The gateway refuses, `422`, at every conformance level and before any leg
  is sent:
  - a request from your own system (a client not marked as a module) that sends `SHN-Builder`:
    a native client must not send it;
  - a module client's request that names no builder;
  - a module client's request that repeats `SHN-Builder`;
  - a builder that is not the module's, or that does not build this leg's request;
  - a module request the gateway would have to edit (a CDS Hooks request that carries
    `fhirServer` or `fhirAuthorization` is refused before anything is read through it).

  Each is recorded as a `fidelity` refusal: the gateway does not seal a message as its own by a
  builder it cannot vouch for, or pass one off as your system's.
- **Authentication.** The header is honoured only from the client the gateway authenticated, by
  its own token or its direct bearer, never from anything else the request states.

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

A `Patient` your system includes in a coverage search the gateway runs, other than the one
a Coverage's `beneficiary` names, is dropped from what the gateway reads: it is neither
checked against the patient nor sent to the payer. The usual one is the parent a dependent's
Coverage names as its `subscriber` or `policyHolder`; R4 also lets a Coverage's `payor` name a
Patient. The Patient a `beneficiary` names is kept: it must be the patient, and it is carried
as before. Each Coverage is still checked against the patient by its `beneficiary`. Earlier
gateways refused a search that included another person's Patient (`502`, at every level): the
routing read, the coverage `ENRICH_NATIVE_REQUESTS` adds, and the coverage of a CRD request the
gateway originates. The `fhirServer` read does the same with a `Patient` entry your EHR's
server returns: one a Coverage's `beneficiary` names is checked to be the patient, any other is
dropped, and neither is sent to the payer
([Reading the coverage through `fhirServer`](#reading-the-coverage-through-fhirserver)).

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
  record cannot supply is refused as below (`422`; `502` when your server answers the search
  with an error that does not say it cannot run it, see [System-of-record search
  errors](#system-of-record-search-errors); or `503` when your server is unavailable);
  one whose `prefetch.coverage` is `null`, or for whose member your system holds no Coverage,
  is refused `412 no coverage in request or system of record`. One that leaves out the coverage
  of a member your system does not hold has nothing there to be routed by: by default its
  coverage is read through the request's own `fhirServer` instead, to route by, and from
  v0.61.0 carried as `prefetch.coverage` once the request is routed (see
  [Reading the coverage through `fhirServer`](#reading-the-coverage-through-fhirserver)), and a
  request that names no `fhirServer` is refused with `412 no coverage to route by: send
  prefetch.coverage or fhirServer (this gateway's records name no patient for this member)`. A coverage
  read through `fhirServer` is carried to the payer from v0.61.0 (see that section); a
  coverage read from your system of record is not. Otherwise the payer
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
    coverage unavailable from system of record` when your server is unavailable, `502 the
    system of record answered the coverage search with an error` when it answers with an error
    that does not say it cannot run the search (see [System-of-record search
    errors](#system-of-record-search-errors)), otherwise a `422`. A missing history key is left for the payer to decide on. When your system of
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
above): no draft order is `422 no draft order for member in system of record`, several are
`422 several draft orders for member in system of record`, and a connector that cannot search,
or a system that says definitely that it does not support the search, is `422`. A search your
system answers with any other error is `502` (see [System-of-record search
errors](#system-of-record-search-errors)). `order-sign` (a signed order going to prior authorization) and `order-dispatch`
carry your member's **active** order as your system holds it, at every CRD line; the network
never changes its status. CRD 2.1.0 and 2.2.1's order profiles fix no status. CRD 2.0.1's
`profile-devicerequest` and `profile-servicerequest` fix `status` to `draft`, so at CRD 2.0
an active order does not conform to them, and a CRD 2.0 payer that enforces those profiles
may refuse the request. That refusal is the payer's, returned to you as it answers. Your
gateway's conformance check does not check the routed line's CRD order profile today. At
`observe`, `structural` and `strict`, it checks the order it originates for `order-sign` and
`order-select` against base FHIR R4 and the profiles the order declares in its own
`meta.profile` (at `none` it checks nothing). So an order that declares US Core, or no
profile, draws no finding for its status at `observe`; an order that itself declares one of
those CRD profiles is checked against it at the CRD version your gateway's canonical
validator (`FHIR_VALIDATE_URL`) loads,
whatever line the request is routed at. The `order-dispatch` order is not checked before the
`order-dispatch` request is sent.

When your order's `requester` names the clinician who placed it, every prior-authorization
request the gateway builds about that order (the submission and each amendment) carries that
clinician's record from your system of record, so the payer resolves the order's requester
inside the request. It carries the record the `requester` names (a `Practitioner`,
`PractitionerRole` or `Organization`) and the records those reference, up to two references
away from the requester, when they are a `Practitioner`, `PractitionerRole`, `Organization` or
`Location`: for example a Practitioner's `qualification.issuer`, or a PractitionerRole's
`practitioner`, `organization` and `location`. Each record is carried once, with the content
your system holds (as JSON, its values unchanged); on the reference payers' lane, the references the gateway resolves this way
are written as the entries' absolute `fullUrl`s, as every other reference to an entry is. The
records are read once, before the gateway sends anything for the order. When your system cannot
supply them, the gateway refuses the request before sending anything (`422`, naming the
reference and the reason): a record the requester leads to is not in your system of record, is
of another type, is more than two references away, is not a `Type/id` reference into your
system, or there are more than 16. A contained requester (`#id`) travels inside the order, and
an order that names no requester is sent exactly as before.

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
questionnaire: the first the payer's answer names, exactly as stated (a `|version` kept).
When a Coverage it carries names its payer (its first `payor` reference, the one routing
reads) by a reference to a record outside itself, the request also carries that payer
Organization as a `referenced` parameter, each record your system supplied once, exactly as
your system of record holds it: the record your Coverage search included for a relative
reference (`Organization/<id>`), else the record your system supplied, for the same reference,
when the gateway read the payer of the Coverage it routes by. A payer
that resolves the Coverage's payor from the request can then do so. A Coverage that contains
its payer Organization, or names its payer by identifier alone, adds nothing. A reference
that no record your system supplied answers, or that several different Organizations answer,
adds nothing either, and refuses nothing. Once the request is built, it is recorded, at every
conformance level, as a `dtr.referenced` event with outcome `left-out`, reason
`none-answers` or `several-answer`, and the reference's resource type only. Nothing is minted. A
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
| `off` | Nothing: the gateway never calls `fhirServer`, carries nothing, and such a request is refused `412 no coverage to route by: send prefetch.coverage (this gateway's records name no patient for this member, and it does not read fhirServer)`. |

Any other value refuses to boot, and so does `private` or `public` set without
`PROVIDER_DAVINCI_INGRESS` (left unset, the key needs no ingress). With the read on, a request
that names no `fhirServer` (or a `null` or empty one) has nothing to read and is refused `412 no
coverage to route by: send prefetch.coverage or fhirServer (this gateway's records name no
patient for this member)`; with it `off`, every such request is refused `412 no coverage to route by: send
prefetch.coverage (this gateway's records name no patient for this member, and it does not read
fhirServer)`.

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
  Organization only such a Coverage names, nor any `Patient` entry. A `Patient` entry a
  Coverage's `beneficiary` names is checked with the Coverages and must be the patient; any
  other `Patient` entry your server returns, such as the parent a dependent's Coverage names as
  its `subscriber`, is dropped before the check (earlier gateways refused the answer, `412`).
  Everything carried passes the same patient check as the read: another patient's record is
  never carried. The rest of the request is your EHR's,
  byte for byte, with `fhirServer` and `fhirAuthorization` still removed. A request refused
  while routing carries nothing and is not sent. A request that carries a `coverage` prefetch
  key, even `null`, is never given another, and a coverage your system of record supplied, or
  the payor Organization read for a coverage your EHR sent, is never carried this way. Before
  v0.61.0 nothing read was carried.
- **Checked in every mode.** An `https` base URL with no query, fragment or credentials, and
  a port from 1 to 65535 when it names one; the mode's address rules applied to an address
  literal, to every address the host resolves to, and again to the address the connection is
  made to, which is only one that was checked. At most two reads a request. Redirects are never
  followed, no proxy is used, TLS (1.2 or later) is verified against the system's trusted
  certificate authorities. An EHR server whose certificate a private CA signs is trusted by
  adding that CA to them: on Linux (the gateway's image), its certificate in a directory named
  in `SSL_CERT_DIR` beside the system's own, for example
  `SSL_CERT_DIR=/etc/ssl/certs:/etc/ehr-ca`, or a PEM bundle file in `SSL_CERT_FILE`, which
  the Go runtime reads in place of the system's default bundle file (the system's certificate
  directories are still read unless `SSL_CERT_DIR` names others); on macOS or Windows, the
  system's trust store.
  Every TLS connection the gateway makes then trusts it, and verification cannot be turned
  off. Each connection has 2 s to
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
| `fhirServer's answer is not a Coverage searchset` (not JSON, not a `searchset` Bundle, an entry that is not a Coverage, Organization, Patient or OperationOutcome, or one the patient check cannot use, such as a repeated `fullUrl`, a contained resource with no `id`, or a patient it cannot identify as the request's; from shn-gateway v0.61.0 a dependent's Coverage naming the parent as a contained `Patient` in its `subscriber` or `policyHolder` only is used; a `Patient` entry no Coverage's `beneficiary` names, such as the parent returned as an entry of its own, is dropped and not checked, and a repeated `fullUrl` is refused across every entry) | 412 |
| `fhirServer holds no Coverage for the patient` | 412 |
| `fhirServer holds no Organization for the coverage's payor` (a `404` or `410` on the Organization read) | 412 |
| `fhirServer returned another patient's coverage` (a Coverage whose patient reference names another patient: another `Patient` id, or the member identifier with another member's value) | 502 |
| `fhirServer's answer is not the payor Organization` (not JSON, or not the Organization asked for) | 502 |
| `fhirServer's coverages name more than one payor Organization by reference alone` | 422 |

## Take a payer's request for documentation: the attachment-request Task (provider, optional)

From v1.5.0 a payer's system can ask your system for documentation for a prior authorization,
through its own gateway and the network, by creating a CDex 2.1.0 Task Attachment Request
(`cdex-task-attachment-request`; the payer's side is
[below](#ask-a-provider-for-documentation-the-payers-attachment-request-task-payer-optional)). The
leg is `cdex-attachment-request`, in the evolving tier (see `STABILITY.md`). The network delivers
it only to a provider's gateway, and only to one whose registry entry lists the leg.

The request asks either for documents by code (`Task.code`
`PASTempCodes#attachment-request-code`) or, from v1.7.0, for a questionnaire to be
completed (`PASTempCodes#attachment-request-questionnaire`). A questionnaire request names its
questionnaire by canonical in a `questionnaire-context` input. Your system completes it with DTR
and answers on the documentation return path, as for a request by code. The canonical is the
payer's identifier for the questionnaire, not an address, and it reaches your system as the payer
wrote it. Fetch the questionnaire through your gateway's `$questionnaire-package`, which carries
the fetch on the network. A plain `GET` of the canonical URL goes around the network. For an
adaptive questionnaire, your DTR client calls `$next-question` at the address the questionnaire's
`questionnaireAdaptive` extension names. In v1.7.0 that is the payer's own system as the payer
wrote it, and those rounds go to the payer directly, outside the network. From v1.8.0 your
gateway re-keys that address to its own ingress wherever the payer's gateway allows it, so the
rounds stay on the network (see
[Adaptive questionnaire rounds](#adaptive-questionnaire-rounds-the-re-key-and-the-routes-provider)).

A provider's gateway takes the request only once you set your system's Task endpoint:

| Env var | Description |
|---|---|
| `PROVIDER_CDEX_TASK_URL` | The full URL your system takes the Task at (the gateway posts the Task there). Setting it makes your gateway declare the leg and serve the route the Hub delivers it on. It requires the settings below, and `PROVIDER_DAVINCI_INGRESS` with its `PROVIDER_DAVINCI_INGRESS_BASE_URL` ([above](#accept-da-vinci-requests-from-a-provider-ehr-provider-optional)), under which the Task's documentation return path is built: without them the gateway refuses to start. |
| `PROVIDER_CDEX_TOKEN_URL` | SMART Backend Services token endpoint of your system's authorization server. **Required** with `PROVIDER_CDEX_TASK_URL`. The client assertion's `aud` is this URL exactly as you set it. |
| `PROVIDER_CDEX_CLIENT_ID` | SMART client id the gateway authenticates as (the client assertion's `iss` and `sub`). **Required** with `PROVIDER_CDEX_TASK_URL`. |
| `PROVIDER_CDEX_SCOPE` | Scope the gateway asks your token endpoint for, sent as you set it, e.g. `system/Task.c`. **Required** with `PROVIDER_CDEX_TASK_URL`, with no default; a blank value refuses to start. It must let the gateway create the Task. |
| `PROVIDER_CDEX_CLIENT_KEY` | Path to the client's private-key PEM file (the value is a path, not the key text — mount the file into the container). Required for `private_key_jwt` mode (when `PROVIDER_CDEX_CLIENT_SECRET` is unset). The client assertion carries no `x5c` certificate chain, so a token endpoint that requires one in the assertion, as the UDAP Security IG has it, refuses it. |
| `PROVIDER_CDEX_CLIENT_ALG` | `ES384`, `RS384` or `RS256`. Required for `private_key_jwt` mode. `RS256` is taken on this setting only. |
| `PROVIDER_CDEX_CLIENT_KID` | Key id for the client assertion's `kid` header, if your authorization server requires one. Unset, the assertion carries no `kid`. |
| `PROVIDER_CDEX_CLIENT_SECRET` | OAuth2 client secret for the `client_secret_post` `client_credentials` grant, for an authorization server that cannot issue asymmetric credentials. The value is the secret itself, not a path. Use it or `PROVIDER_CDEX_CLIENT_KEY`, not both. |

A gateway of any other role refuses to start with any of these set, and so does one with any of
them set but `PROVIDER_CDEX_TASK_URL`.

**When your gateway takes it.** From v1.5.0, with `PROVIDER_CDEX_TASK_URL` set, your gateway's
signed capability declaration lists the leg, it serves the route the Hub delivers the leg on
(and needs the Hub's transport key, as every gateway that takes a leg does), and payers'
gateways list your gateway's base. Without it, your gateway does not declare the leg or serve
the route the Hub delivers it on, so the Hub refuses a payer's Task to it `422` (`the recipient
does not take …`), recorded as refused by the Hub on `routing`, and payers' listings of
providers leave it out. A hosted provider tenant takes it the same way on SHN Cloud, with the
same settings, authenticating with a client secret or with a key SHN Cloud mints for it
(`PROVIDER_CDEX_CLIENT_ALG`). Unless you set `PROVIDER_CDEX_CLIENT_KID`, an `ES384` or `RS384`
key's id is its RFC 7638 thumbprint and an `RS256` key has none, as the tenant's JWKS, published
by SHN Cloud, states; for a UDAP registration (`RS256`), SHN Cloud also publishes a certificate
over the key, for your registration only: the gateway's client assertion does not carry it. Its
hosted edge then serves that route to it. A provider the network's provisioning registers itself
(a founding holder) cannot take the leg yet, whatever it sets: the registrar does not apply the
legs it declares.

**What reaches your system.** From v1.5.0 the Task is carried to `PROVIDER_CDEX_TASK_URL` as
the payer sent it, apart from its `payer-url` (below), with the payer's FHIR JSON media type.
The forward names the payer the network authenticated, outside the
body: `X-SHN-Requester: <payer participant id>; identifiers="<system>|<value> …"`, the payer
identities the network's registry attests for that payer, space-separated (the `identifiers`
part is left out when it attests none). Your gateway sets it on the forward alone and never reads
one from a caller. A Task whose `requester` identifier is not one of those identities, or
cannot be read, is carried, and noted as the observer event `cdex.requester-differs`, which
carries no identifier; it is never refused. A Task naming its requester by a reference alone
is carried with no event.

Before anything is sent, your gateway decides on the Task as the payer's gateway does, at every
conformance level, and answers the payer with the refusal:

| Answer | When |
|---|---|
| `400 the Task can be read two ways: it repeats a member name, or names one in another case` | The body repeats a member name, or names one in another case (`integrity`). |
| `403 not a CDex Task Attachment Request: the body is not a Task this leg carries` | The body is not JSON, or not a Task (the leg's `authority`). |
| `403 the Task's code is not one this leg carries: …` | `Task.code` holds neither a `PASTempCodes#attachment-request-code` nor a `PASTempCodes#attachment-request-questionnaire` coding, a CDex data request among them (the leg's `authority`). |
| `422 cannot bind the request to a patient: Task.for names no one member number (an identifier typed v2-0203#MB)` | `Task.for`, on the reference or on the Patient it contains, names no member number, or two that differ (the leg's `authority`). |

Your gateway binds the member number against your system of record, as on every leg the network
delivers: a member your system holds is that patient, and one it does not hold is identified from
the number alone and carried, unless you require known members (`REQUIRE_KNOWN_MEMBERS`, then
`400 unknown member`). A system of record that fails the lookup gives the usual `502` or `503`.

**The answer.** Your system's answer (`201` with the Task, or an error) reaches the payer's
system as your system sent it. The forward, its token request included, has the deadline a payer's
gateway gives every call to its own system by default: 25 seconds, the network's 30-second leg
budget less five, so your gateway's answer reaches the payer inside the payer's budget. When your
system fails, the payer gets the answers every forward to a participant's own system gives:
`502 the provider's system could not be reached` (a token endpoint that does not answer in 10
seconds reads this way), `504 the provider's system could not be reached in time`, or, once the
Task was sent, `502` or `504` saying your system received it and may have acted on it. When your
gateway's own work before the call (binding the member against your system of record) uses the
whole deadline, the answer is `504 the provider's gateway ran out of time before it could send
this request to the provider's system; the provider's system did not receive it`.

**The Task's documentation return address.** From v1.5.0, before your gateway carries a
payer's Task to your system, it re-keys the Task's `payer-url` (registered edit E-11,
`cdex-task-return-rekey`): the `valueUrl` of the Task's one `payer-url` input (typed
`PASTempCodes#payer-url`) becomes your gateway's documentation return path for the payer that
sent the Task, `PROVIDER_DAVINCI_INGRESS_BASE_URL` + `/cdex/payer/` + that payer's participant
id, rebuilt from the authenticated payer and never read from the Task. Documentation your system
sends there with `$submit-attachment` reaches that payer through the network. Nothing else in the
Task changes. The re-key is made even where a signature covers the `payer-url`, so that signature
no longer verifies in your copy; the forward then carries `X-SHN-Edited: cdex-task-return-rekey`,
with `; signed="<signatures>"` added when a signature covered the address.

The Task is carried exactly as the payer sent it, and the exchange record's `editsNotMade` says
why, when the gateway cannot tell which address to re-key, or the payer does not take the
documentation submission:
- a `payer-url` that is not a non-empty `valueUrl` (`E-11:no-structural-payer-url`);
- several `payer-url` inputs, or an input that cannot be read (`E-11:unreadable-payer-url`);
- a payer not known to take the documentation submission (`E-11:payer-does-not-take`): one
  whose registry entry lacks the leg, one that declares no list, or one your gateway cannot
  read. The Hub refuses that leg to a payer that does not take it.

A Task with no `payer-url` needs no edit and is carried exactly. A re-key that cannot be made,
as for a payer whose participant id cannot form a return path, is refused `422 the gateway
cannot re-key the Task's documentation return address to the network (fidelity of SHN's own
edit): <reason>`, recorded as `fidelity` at stage `build` when there is no return path to write
and `verify` otherwise; the Task is not sent.

## Advanced overrides (rarely needed)

Each network endpoint and trust-anchor key URL is resolved from discovery by
default; set the matching variable only to override (e.g. when the gateway runs
inside the SHN-operated network itself): `AUTHZ_URL`, `HUB_URL`, `CONSENT_URL`,
`REGISTRAR_URL`, `FHIR_VALIDATE_URL`, `AUTHZ_PUBKEY_URL`, `HUB_TRANSPORT_KEY_URL`. Explicit env always wins over discovery.
Before v0.63.0 the list also had `AUDIT_URL` and `PHG_URL`; later releases read neither, and a gateway with either still set logs one boot warning for it.

`NPI` is your clinician's NPI for the requests a provider gateway originates
(`ORIGINATION_PROFILE`), used where your own records name none. It has no default: unset, the
gateway states no clinician of its own. Set, it must be a ten-digit NPI with a valid check digit,
or the gateway refuses to boot. The ordering clinician (a CDS Hooks request's `userId`) and the
attesting clinician (an attestation's NPI) come from the first of these that names one:

1. an `npi` the request sends, when scenario caller authentication (`SCENARIO_CALLERS_FILE`)
   verified its caller;
2. your order's `requester` in your system of record: `userId` names it, and an attestation
   uses its NPI (a `Practitioner`'s `us-npi` identifier, or a `PractitionerRole`'s
   practitioner's);
3. `NPI`;
4. an `npi` the request sends without a verified caller.

A sent `npi` that is not a valid NPI is refused (`400 npi must be a ten-digit NPI with a valid check
digit`). With no source, the gateway does not build the request, and answers `422`:
- a CDS Hooks request: `build order-sign request failed: order <ref> names no ordering
  clinician: …` (or `order-select`), before any leg;
- an attestation: `attestation names no clinician NPI: …`, at the leg that carries it, after the
  legs before it, which stand: UC-03's oxygen attestation at its PAS claim, and UC-06's at its
  amendment (`/scenario/uc06` after the submission the payer pended, `/scenario/uc06/complete`
  before the amendment is sent), where the pend stays parked for a corrected request.

An eligibility request names `NPI` as its provider when it is set, and no provider otherwise.
The answer a UC-06 or UC-07 attestation states is the request's own: a request that states none
is refused (`422 answer is required: …`) rather than built with an answer the gateway chose, at
the leg that carries it (UC-06's amendment, UC-07's patient questionnaire exchange).

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
| `PAYER_ELIGIBILITY_URL` | Your system's own coverage-eligibility endpoint, when it has one: the absolute `http(s)` URL it takes the request's `POST` on (give the exact URL). The body is the request as the provider's system sent it: a `CoverageEligibilityRequest`, or, from shn-gateway v0.63.0, any other input of the FHIR R4 operation `CoverageEligibilityRequest/$submit`: a `Bundle` carrying one request with the resources it references, a `Bundle` that is a batch of requests, or a `Parameters` whose `resource` parameter holds one of these. Unset (the default): the gateway answers eligibility from your system of record's Coverage, one request at a time (from shn-gateway v0.63.0 a batch is answered `501`, saying so; a batch of more than 17 requests is refused `413` first, as below), or, when it keeps none (`FHIR_DATA_URL` unset), answers `501` "coverage eligibility is not offered by this payer". A batch of more than 17 requests, in any form, is refused `413` before anything is read from it ("a gateway reads at most 17 in one batch"), whether or not this is set: one exchange is recorded under at most 17 patients. Set: each request's member is bound by your records (the first request's is the exchange's patient; every other request's is named on the answer's involved list), and each request (the one a `Bundle` or `Parameters` holds, each of a batch) is `$validate`d at the conformance level (an invalid one is refused `422` at `structural`, for a structural issue, and at `strict`, and your system is not called); the input is then carried there exactly, with the same authentication (`PAYER_DAVINCI_TOKEN_URL` and the client settings) and `PAYER_DAVINCI_BACKEND_HEADERS` as your other operations, and your answer is relayed as your system sent it; an error answer is relayed as your error. If your system gives no answer (unreachable, timed out), the requester is told so (`502`, saying whether your system received the request), as on your other operations; no answer is built from your records in its place. An answer about a patient other than the requests' is relayed at `none`, `observe` and `structural` (recorded at `observe` and `structural`) and refused at `strict`; an answer that names its patient otherwise than by reference (an identifier only) is treated the same way; an answer that cannot be read as a `CoverageEligibilityResponse`, or names no patient at all, is relayed at `none` and `observe` and refused (`502`) at `structural` and `strict`; an answer that is a `Bundle` is read as its `CoverageEligibilityResponse` entries (and those of the Bundles among its entries, one level deep; a Bundle nested deeper is treated as an answer that cannot be read), each held to these rules against the patients of all the requests (a response naming its patient by a reference to a Patient entry of the Bundle carrying it names that entry's patient), and one holding none, or whose entries cannot be read one way only (a member named in another case, a resource with no type or a null one), is treated as an answer that cannot be read; an entry carrying no resource at all (a batch-response entry carrying only its outcome) is no response; an answer naming the patient by your own Patient id for the member the request names is the same patient, and is relayed at every level. Telling the two apart reads your system of record only when the answer names a patient other than the request's, and never at `none`; with no system of record, the patient is one it does not hold; if that read fails, the answer is relayed at `observe` and `structural` (recorded as unavailable) and refused with the system-of-record failure at `strict`. Unlike the relayed PAS and questionnaire answers, your eligibility answer is `$validate`d at the conformance level: not at all at `none`, recorded at `observe`, refused (`502`) at `strict`, and at `structural` refused for a structural issue and recorded for a deeper one; a validator your gateway cannot reach refuses with `500` at `strict` and is recorded at `observe` and `structural`. A token naming another patient is not refused: the member the request names is bound by your records, as on the prior-authorization legs. Requires `PAYER_DAVINCI_BASE_URL`; a provider gateway refuses to boot with it. |

**Where each operation goes.** For every PAS, DTR and CDex forward the gateway resolves the URL in this order: under the base your partner publishes for that contract line in its `.well-known/davinci-configuration` (the partner's own published rule, honored only when it is same-origin with the base that contract uses), then under the per-operation base if you set one, then under `PAYER_DAVINCI_BASE_URL`. HRex 1.2.0 defines each published value as the base an operation is invoked on, not the operation's URL, so the gateway always adds the operation's path to it: `davinci_pas_submission_endpoint#<line>` is the base for `/Claim/$submit` (submit and update) and `/Claim/$inquire`, `davinci_dtr_qpackage_endpoint#<line>` the base for `/Questionnaire/$questionnaire-package` (the gateway sends `/Questionnaire/$next-question` under the same base), and `davinci_cdex_attachsubmit_endpoint#2.1` the base for `/$submit-attachment`. The operation's path is added where the published base's path ends, before any query or fragment: the slashes that end the published path are dropped first, so none is doubled, and the rest of the URL the gateway builds is the published value as written, sent as any URL the gateway calls is. Nothing is read from how a published path ends: a value that already names an operation is still a base, so the operation's path is added to it and your system answers that URL; publish the base. The per-operation bases exist for partners that split DTR and PAS across bases and publish no `.well-known/davinci-configuration`; where the partner publishes one, its bases win and the configured bases are only the fallback. The published CDex base is honored only when it is same-origin with `PAYER_DAVINCI_BASE_URL`, and there is no per-operation base for it. The submission reaches your system exactly as the provider's system sent it, and your answer reaches the provider's system exactly.

| Env var | Description |
|---|---|
| `PAYER_DAVINCI_TOKEN_URL` | SMART Backend Services token endpoint for the partner. Required if the partner requires authentication. |
| `PAYER_DAVINCI_CLIENT_ID` | SMART client id for the partner. Required when `PAYER_DAVINCI_TOKEN_URL` is set. |
| `PAYER_DAVINCI_CLIENT_KEY` | Path to the SMART client's private-key PEM file (the value is a path, not the key text — mount the file into the container). Required for `private_key_jwt` mode (i.e. when `PAYER_DAVINCI_CLIENT_SECRET` is unset). The client assertion signed with it carries no `x5c` certificate chain, only a `kid` when `PAYER_DAVINCI_CLIENT_KID` is set, so a token endpoint that requires the chain in the assertion, as the UDAP Security IG has it, refuses it. |
| `PAYER_DAVINCI_CLIENT_ALG` | `ES384` or `RS384`. Required for `private_key_jwt` mode (i.e. when `PAYER_DAVINCI_CLIENT_SECRET` is unset). |
| `PAYER_DAVINCI_SCOPE` | Requested scope the gateway asks your token endpoint for. Default `system/*.read` (covers the read-only legs). Must be a scope your authorization server grants this client; it must cover `/Claim/$submit` and `/Claim/$inquire` as well as the read-only legs, since every leg forwards. |
| `PAYER_DAVINCI_CLIENT_KID` | Key id for the client assertion JWK, if the partner requires it. |
| `PAYER_DAVINCI_CLIENT_SECRET` | OAuth2 client secret for the `client_secret_post` `client_credentials` grant — for authorization servers that cannot issue asymmetric credentials. The value is the secret **itself, not a path** (unlike `PAYER_DAVINCI_CLIENT_KEY`). Mutually exclusive with `PAYER_DAVINCI_CLIENT_KEY`/`_ALG`/`_KID`; prefer `private_key_jwt` when your server supports it. |
| `PAYER_DAVINCI_PAS_NATIVE` | **No longer a switch.** PAS submit/update always forward to the payer's `/Claim/$submit` along with every other leg; there is no in-process PAS fallback to select. Setting it to any value changes nothing. From shn-gateway v1.0.0, a gateway that sets it, to any value, logs `gateway: WARNING: PAYER_DAVINCI_PAS_NATIVE is set but no longer read` at boot, and one that leaves it unset logs nothing about it; remove it. Releases v0.60.0 through v0.63.x log `gateway: PAYER_DAVINCI_PAS_NATIVE is set but is no longer a switch` when it is set, and releases v0.39.0 through v0.59.x a similar notice whenever it is not `true`, unset included. |
| `PAYER_DAVINCI_CRD_SERVICE_ID` | Optional: names your CDS service for `order-select` and `order-sign` requests. Empty (the default) ⇒ each request goes to the one service your CDS service listing offers for the request's hook (see [CDS service selection](#cds-service-selection)). The named service must be in your listing and answer the request's hook; any other request is refused. |
| `PAYER_DAVINCI_DISPATCH_SERVICE_ID` | Optional: names your CDS service for `order-dispatch` requests, with the same rules. Empty ⇒ the service your listing offers for `order-dispatch`. |
| `PAYER_DAVINCI_CONTRACT_VERSIONS` | Declared Da Vinci contract versions for the partner payer, comma-separated `<contract>@<line>` tokens (e.g. `pa.pas@2.0, pa.crd@2.0`). Two things read this: the connectivity checks verify the partner's published capability against it (`version-drift` on disagreement, FR-G46), and native-forward routing refuses (before forwarding) any leg whose contract shares no line with it (FR-G48); from shn-gateway v0.62.0 only with `REQUIRE_SHARED_LINE=true`, and by default such a leg is forwarded as sent and the carry recorded. Requires `PAYER_DAVINCI_BASE_URL`. Unset ⇒ native-forward legs are unfiltered (today's default) and the checks skip the drift comparison. From shn-gateway v0.59.0 it is also this gateway's own declaration when `SHN_CONTRACT_VERSIONS` is unset (see [Exchange contract lines](#exchange-contract-lines-shn_contract_versions)); a value that names no `pa.crd`, `pa.dtr` or `pa.pas` line, or a `pa.crd`, `pa.dtr` or `pa.pas` line this build cannot exchange, then refuses boot. Editing it then changes the declaration, so the grow-only rule below applies to it too. |
| `PAYER_DAVINCI_BACKEND_HEADERS` | Fixed request headers for a partner system that routes on one (a tenant or plan key its API gateway reads before any payload), as comma-separated `Name: value` pairs, e.g. `X-Route-Key: plan-7`. Sent on every request to the partner's bases — the CDS service listing read, each CRD post, the DTR and PAS operations (`$submit` and `$inquire`) and the connectivity probes — and **never** to `PAYER_DAVINCI_TOKEN_URL`. The message bytes are untouched: this is addressing for your own system, not a change to what the request asserts. Refused at boot: a pair that is not `name: value`, an empty name or value, a name or value that is not a valid HTTP field, a repeated name, and the names this gateway sets itself (`Authorization`, `Content-Type`, `Accept`, `Host`, `Content-Length`, `X-Correlation-Id`) or hop-by-hop names (`Connection`, `Transfer-Encoding`, `Upgrade`, …). A value cannot contain a comma. Requires `PAYER_DAVINCI_BASE_URL`. |
| `PAYER_DAVINCI_BACKEND_TIMEOUT` | How long this gateway waits for your system on each operation it forwards, counted from the leg's arrival at this gateway, as a Go duration from `1s` to `28s` (from v0.59.0). Unset: `25s`, under the requester's 30 s leg budget by enough that a system slower than it is answered as its own timeout, this gateway's framed `504`, while the requester is still waiting, and recorded as your system's `timeout`. Any other value refuses the boot, as does setting it without `PAYER_DAVINCI_BASE_URL`. |
| `PAYER_DAVINCI_BACKEND_CORRELATION` | Whether each operation forwarded to your system carries `X-Correlation-Id` with the leg's id. Unset (or `on`, the default): it does. `off`: no `X-Correlation-Id` is sent, for a system that validates that header its own way; nothing else changes. Any other value refuses the boot, as does `off` without `PAYER_DAVINCI_BASE_URL`. `PAYER_DAVINCI_BACKEND_HEADERS` can never name `X-Correlation-Id`, whatever this is set to. |
| `PAYER_DAVINCI_STRICT_EXTENSIONS` | **No longer read** (from shn-gateway v1.0.0). It reserved a per-peer strict-extensions overlay (FR-G52) that never had a routing effect. A gateway that sets it, to any value, logs `gateway: WARNING: PAYER_DAVINCI_STRICT_EXTENSIONS is set but no longer read` at boot and starts; remove it. A gateway SHN hosts for you no longer renders it. |

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
at every level but `none`; from v0.61.0 it carries `ms`, the time the `$validate` call took; from v1.2.0 its
outcome is `unavailable`, not `invalid`, when the check could not run): it
never changes or refuses your answer. A non-2xx answer is relayed as your system's error,
and a 2xx answer with the status your system gave (from v0.63.0).

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
among the holders it has converged: you run no registrar (your network's discovery names none and `REGISTRAR_URL` is unset), your
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

## Adaptive questionnaire rounds: the re-key and the routes (provider)

From v1.8.0, with `PROVIDER_DAVINCI_INGRESS=1`, your DTR client's adaptive rounds
(`$next-question`) stay on the network, on the questionnaire package's own leg
(`dtr-questionnaire-fetch`), ingress credential and scope. There is no setting for it on your
gateway.

**Upgrade note.** Make the three round routes below reachable at your ingress base before you
upgrade to v1.8.0. If your ingress sits behind your own proxy, WAF or load balancer that admits
only the routes earlier releases served, it must also admit them. From v1.8.0, E-12 rewrites
every tagged adaptive address your DTR client is given to one of these routes, and it has no
opt-out. A proxy that blocks them blocks every round your client sends, where before the
upgrade the client called the payer's own address directly.

**The re-key (registered edit E-12, `dtr-adaptive-address-rekey`).** An adaptive questionnaire
names its rounds' address in its `sdc-questionnaire-questionnaireAdaptive` extension. On a
package or `$next-question` answer your gateway relays to your DTR client, it rewrites each such
address that the payer's gateway tagged (see
[Adaptive questionnaire addresses](#adaptive-questionnaire-addresses-the-payers-tags-and-the-restore-payer))
to an address under your own ingress base:
- a `valueUrl` becomes `<ingress base>/dtr/payer/<payer>/<line>/<tag><remainder>`: only the
  payer's base is replaced, and the remainder after it is kept byte for byte;
- a `valueBoolean` `true` becomes `<ingress base>/dtr/fetch-server/<payer>/<line>/<tag>`.

`<line>` is the DTR line the answer was carried at, and `<tag>` the payer's gateway's tag for the
address. The ingress base is the one your CDex return paths are built under
(`PROVIDER_DAVINCI_INGRESS_BASE_URL`), so the address is under the base your client fetched the
package from, as DTR 2.2.0 requires. The answer carries `X-SHN-Edited:
dtr-adaptive-address-rekey`, its `ETag` is dropped, and the exchange record lists the edit. The
payer's signature over an address it re-keys, where there is one, no longer verifies in your
copy. There is no opt-out: carrying the address would send your client's rounds around the
network. The payer's gateway restores the address before its system sees it (E-13).

E-12 is made only toward a payer whose gateway declares the `v1opsub` request frame. Otherwise,
and wherever the address cannot be re-keyed exactly, it is carried as sent, and the exchange
record's `editsNotMade` says why:

| Reason | When |
|---|---|
| `E-12:payer-gateway-older` | The payer's gateway does not declare `v1opsub`. |
| `E-12:not-the-rounds-address` | On a `$next-question` answer, the address is neither the payer's base nor the address the round was delivered to, so the payer's gateway did not tag it. |
| `E-12:contained-in-package-response` | On a package answer, the address is on a Questionnaire contained in a QuestionnaireResponse, and differs from the tagged top-level entry's. |
| `E-12:request-carried-a-questionnaire` | The package request carried a Questionnaire, so the payer's gateway tagged nothing in its answer. |
| `E-12:not-under-base` | The payer's gateway stated no base, the address is not under it, or it gave the address no tag. A payer's gateway states nothing on a package answer at another DTR line than the one the request claims. |
| `E-12:escaped-address` | The address is written with a JSON escape, so it could not be restored byte for byte. |
| `E-12:base-spelled-differently` | The address is under the stated base but not spelled as it is. |
| `E-12:unreadable-tag` | The payer's gateway's tag is not 22 base64url characters. |
| `E-12:unclean-remainder` | The remainder after the base is not empty or `/`-separated segments of unreserved characters (a trailing slash, `.` or `..` among them). |
| `E-12:unreadable-address` | The extension's value is neither a URL string nor a boolean. |
| `E-12:unreadable-answer` | The answer cannot be read as JSON. |
| `E-12:address-not-writable` | Your gateway cannot write an address of its own: its ingress base is not an absolute http or https URL without user information, query or fragment, or the payer's id or the line is not a usable path segment. |

**The routes.** Your gateway serves the rounds at three routes, beside
`POST /Questionnaire/$questionnaire-package`:

| Route | Carried |
|---|---|
| `POST /dtr/payer/{payer}/{line}/{tag}{remainder}/Questionnaire/$next-question` | To the payer named, at the address E-12 wrote. |
| `POST /dtr/fetch-server/{payer}/{line}/{tag}/Questionnaire/$next-question` | To the payer named, at its DTR base: the address E-12 makes explicit for a `true`. |
| `POST /Questionnaire/$next-question` | Routed by the one adaptive address in this gateway's form that the round's QuestionnaireResponse carries in a contained Questionnaire, for a client that posts its rounds to the base. |

A round is forwarded naming the address it was called at, and the payer's gateway delivers it
there only when its tag verifies for your provider. Each route refuses a call as the package
route refuses it for its credential (`401`) and its key store (`503`), and a request naming a
module builder as the package route refuses it. A literal `.`, `..` or empty path segment is
answered with a `307` to the cleaned path before any check, and the cleaned path faces every
check on its own. The round's own checks follow, in the order the gateway makes them.

On `/dtr/payer/…` and `/dtr/fetch-server/…`:
1. an address naming no payer registered as a payer: `422`
   `the adaptive address names no registered payer`, recorded as `routing`. Whether that payer
   takes the questionnaire fetch is not checked here: the Authorization Framework decides it for
   each round, as for the package request (below);
2. a payer whose gateway does not accept framed DTR operations: `502`
   `payer gateway does not support framed DTR operations (upgrade required)`, recorded as
   `routing`, as on the package route;
3. a payer whose gateway does not declare `v1opsub`: `422`
   `the payer's gateway does not support adaptive addresses yet, so this round cannot be carried on the network`,
   recorded as `routing`;
4. an address not in the form your gateway issues (an encoded slash or traversal, a segment
   outside the unreserved characters, a malformed or undefined line or tag): `422`
   `the adaptive address is not one this gateway issues: …`, recorded as `routing`. A
   fetch-server address takes no segment after its tag: such a path is not one of the three
   routes, and is answered `404`;
5. the body, read now: over the size limit `413` (`routing`), in a content coding the gateway
   does not decode `415` (`integrity`).

On `POST /Questionnaire/$next-question`, the body is read first (`413`, `415` as above), then:
1. no adaptive address in this gateway's form, or one whose prefix is not your ingress base
   exactly: `422`
   `a $next-question round names no payer: it carries no member, order or coverage to route by. Fetch the questionnaire package through this gateway and call the questionnaireAdaptive address it returns`;
   more than one distinct address: `422`, naming that; an address whose remainder is not clean:
   `422`, as on `/dtr/payer/…`; each recorded as `routing`;
2. the checks on the payer and the line the address names, as 1 to 4 above.

Then, on every route, the round's patient, bound as the package route binds its patient:
- an input that is not a QuestionnaireResponse or a Parameters carrying one: `400`
  `parse next-question input failed`, recorded as `authority`;
- no subject: `422`; a subject that is not a Patient reference: `403`; each recorded as
  `authority`;
- your system of record unavailable: its outage answer, as on the package route;
- by default, a member number the round shares with another person it carries, and no Patient
  for the member with a birth date and name: `400 cannot identify the patient: …`, recorded as
  `authority` (with `REQUIRE_KNOWN_MEMBERS=true` the next bullet applies instead);
- a patient your system of record does not hold: carried by default; with
  `REQUIRE_KNOWN_MEMBERS=true`, `403 request patient does not resolve`, recorded as
  `conformance`, the check you opted into;
- a round naming more than one patient is the payload's own consistency: refused `403` at
  `strict`, recorded and carried at `observe`, carried at `none`.

Then the round is carried as the package request is: the line selection (refused for its
lines, as `routing`, only under your own `REQUIRE_SHARED_LINE`, below); a payer whose gateway
stopped accepting framed DTR operations (`502`, as step 2) or declaring `v1opsub` (`422`, as
step 3) since the checks above; then the Authorization Framework's and the Hub's checks.
Last, the payer's gateway: a tag it does not verify is its `422`
`not an address this payer issued, or its base or key changed; fetch the package again`,
recorded as `routing` by the payer's gateway, and relayed to your client.

A round is framed at the line your gateway's line selection picks for that payer, as the
package request is, and is never rebuilt at another. It is refused for its lines, as
`routing`, only under your own `REQUIRE_SHARED_LINE`. The line in its address is the line the
payer's gateway tagged the address at. The payer's gateway delivers the round at that line,
which the tag binds, and records a round framed at another line as a line carry. The payer's answer is carried as sent and checked at your
gateway's conformance level as the package answer is, and E-12 applies to it again.

**Known limits.** A client that posts its rounds to the plain route reaches the payer only when
its QuestionnaireResponse carries an address your gateway re-keyed: one that did not fetch the
package through your gateway, or whose address was carried as sent (any `E-12` reason above),
is refused. Such a client works when it calls the `questionnaireAdaptive` address the package
gives it, as DTR asks. A Questionnaire naming two adaptive addresses cannot be routed by the
plain route, though the client can call either address.

On the hosted gateway service, the edge forwards these three routes for a provider with
`PROVIDER_DAVINCI_INGRESS=1` from shn-gateway v1.8.0. On an older release it answers them `403`,
as it does any route that release does not serve.

## Adaptive questionnaire addresses: the payer's tags and the restore (payer)

An adaptive questionnaire names the address of its `$next-question` rounds in its
`sdc-questionnaire-questionnaireAdaptive` extension: a `valueUrl` under your DTR base, or
`valueBoolean` `true` for the base itself. DTR requires that address to sit under the base the
client fetched the questionnaire from. When a provider's DTR client fetches through its gateway,
that base is the provider's gateway. So from v1.8.0 a provider's gateway re-keys the address to
itself (registered edit E-12), and the client's rounds stay on the network. It does so only
toward a payer's gateway that declares it restores the address: the `v1opsub` request frame in
its registry entry. From v1.8.0 your gateway declares it, with the request frames it serves, in
the capability declaration it makes to the registrar at each start; v1.7.0 does not, so no
provider's gateway re-keys an address toward a v1.7.0 gateway. A gateway rolled back to v1.7.0
gives the declaration back at its first start. From v1.7.0 your gateway does its part, with
nothing to configure:

- **The base and the tags.** On a 2xx answer to a framed `$questionnaire-package` or
  `$next-question` request, your gateway may state two headers inside the sealed answer frame:
  - **`operationBase`**, the base it delivered the operation under. It is stated when an
    adaptive `valueUrl` in the answer is under that base.
  - **`operationAddressTags`**, a tag for each adaptive address it issues:
    - on a package answer, a `valueUrl` on a top-level Questionnaire, and only when the request
      carried no Questionnaire;
    - on a `$next-question` answer, the base itself or the address the round was delivered to;
    - a `true`, wherever it appears.

  A `valueUrl` is tagged only when it is spelled exactly as the base followed by a clean
  remainder and written without JSON escapes. A clean remainder is empty, or `/`-separated
  segments of unreserved characters (letters, digits, `-`, `.`, `_`, `~`), none of them `.` or
  `..`. An address that is not tagged is not re-keyed, and its rounds go to your system
  directly, off the network.

  A tag binds the address to your payer, the provider the answer goes to, the IG line and the
  base. It is keyed from your gateway's own signing identity, so every replica issues and
  verifies the same tags. Nothing is stated when your gateway has no DTR base at the answer's
  line (or only one with a query or fragment), or cannot read the answer. From v1.8.0, nothing
  is stated on a package answer at another DTR line than the one the request claims (a claim
  your gateway cannot answer as claimed, answered at the line its selection gives): a tag binds
  the line it was issued at, and the provider's gateway writes the claimed line into the
  address, so every round would be refused. The address is carried as sent instead, and the
  client calls your system directly, as without the re-key. A request your gateway carries but
  answers at the line it claims (a declared line with no ready lane) is tagged as usual, and a
  round's answer is tagged at its own address's line whatever line the round claims. Your system's answer is relayed exactly as it sent it, and the
  headers ride the frame beside it, never reaching the provider's EHR.
- **Rounds.** A `$next-question` round a provider's gateway sends naming an address
  (`operationSubpath`) is delivered at that address, under your base, only when its tag
  verifies for that provider. Otherwise it is refused
  `422 not an address this payer issued, or its base or key changed; fetch the package again`,
  recorded as `routing`, and nothing reaches your system. A package request naming an address is
  refused `400`, recorded as `integrity`. From v1.8.0, a round framed at another line than its
  address names is delivered at the address's line, which the tag binds, and the difference is
  recorded (the `contract.line-carried` event, and `request` in the exchange record's
  `LineCarried`); it is not refused.
- **The restore (registered edit E-13, `dtr-adaptive-address-restore`).** Before your gateway
  forwards a `dtr-questionnaire-fetch`, `cdex-attachment-submit`, `pas-claim`, `pas-claim-update`
  or `pas-claim-inquire` request to your system, it restores each adaptive address in it that:
  - a provider's gateway re-keyed;
  - your gateway issued to that provider;
  - has a tag that verifies.

  The address goes back to the bytes your system wrote: the `valueUrl`, or `valueBoolean`
  `true`. Nothing else in the request changes. Where your gateway also maps the payer identity
  (above), the two registered edits are made together in one change of the request. The restore
  is made even where a signature covers the address, so that signature no longer verifies in your
  copy. The forward carries `X-SHN-Edited: dtr-adaptive-address-restore`, with
  `; signed="<signatures>"` added when a signature covered the address.

An address in the re-keyed form that names your payer, but that the restore cannot make, is
carried as sent, and the exchange record's `editsNotMade` says why:
- its remainder breaks the clean-remainder rule (`E-13:unclean-remainder`);
- its tag does not verify for the provider that sent it: not one your gateway issued to that
  provider, or issued for a base, line or key that changed since (`E-13:not-verified`);
- it cannot be rewritten (`E-13:unreadable-request`).

A request your gateway cannot read as JSON never reaches the restore: each of these legs reads
the request first and refuses one it cannot read, `400`, before anything is forwarded. So no
E-13 reason is recorded for it. Until a provider's gateway re-keys an address toward yours,
which needs v1.8.0 on both sides, a request carries none to restore, and none of these is
recorded unless a request carries a string in the re-keyed form naming your payer from
elsewhere.

An address in the re-keyed form that names another payer is carried as sent and observed as the
`dtr.adaptive-address-carried` event. The event carries its kind, direction, leg and correlation
only: never the address, its tag or any participant's identifier. Rotating your gateway's
signing key ends every adaptive session in progress. An address tagged under the old key no
longer verifies, so a round to it is refused, and the refusal tells the client to fetch the
package again.

## Ask a provider for documentation: the payer's attachment-request Task (payer, optional)

Set `PAYER_DAVINCI_INGRESS=1` to mount the payer-side Da Vinci ingress. Through it your own system
asks a provider for documentation for a prior authorization by creating a CDex 2.1.0 Task
Attachment Request (`cdex-task-attachment-request`), which the gateway sends through the network
to that provider's gateway. It is in the evolving tier (see `STABILITY.md`).

| Env var | Description |
|---|---|
| `PAYER_DAVINCI_INGRESS` | Set to `1` to mount the ingress on the payer gateway (any non-empty value turns it on, `0` and `false` included). A gateway of any other role refuses to start with it set. |
| `PAYER_DAVINCI_INGRESS_BASE_URL` | The gateway's public base URL: the SMART audience the gateway pins, the token endpoint it advertises, and the base of each provider's base below. **Required** when the ingress is enabled. It must be an absolute `http` or `https` URL with a host, no `?` or `#` and no user information, or the gateway refuses to start. |
| `INGRESS_CLIENTS_FILE` | Your system's registered clients, as for the provider's ingress ([above](#accept-da-vinci-requests-from-a-provider-ehr-provider-optional)). **Required** (≥1 client) when the ingress is enabled. A client that lists no `scopes` is registered for `system/Task.write`; one whose `scopes` leave it out refuses the gateway's start, since every route of this ingress would refuse it. |

**Authenticating.** Your system gets a bearer from the gateway's own token endpoint
(`POST /oauth/token`, SMART Backend Services, `private_key_jwt`), asking for the scope
`system/Task.write`. `/.well-known/smart-configuration` advertises that scope. Each Task route
takes only a bearer granted exactly `system/Task.write`. A bearer granted another scope, or none,
is refused `403` with `WWW-Authenticate: Bearer error="insufficient_scope"`, recorded as
`authority`. A missing or invalid bearer is `401`.

**Where to create the Task.** Each registered provider that takes the request has a base on this
gateway, `{PAYER_DAVINCI_INGRESS_BASE_URL}/cdex/provider/{provider}`. Your system creates the Task
for that provider with `POST {base}/Task`. The base names the provider, so the network routes the
Task by the address alone.

`GET /cdex/providers`, with the same bearer, lists them:
`{"providers":[{"participant":"…","base":"…"}]}`. The list holds every registered provider whose
gateway declares that it takes the request, and nothing else. From shn-gateway v1.7.0, an entry
also carries `identifiers`: the organization NPIs the network attests for that provider, each a
`{"system","value"}` pair, as the public holder feed serves them. The field is omitted for a
provider attested for none.

**At the plain Task endpoint.** From shn-gateway v1.7.0, your system can also create the Task
with `POST {PAYER_DAVINCI_INGRESS_BASE_URL}/Task`, and the network routes it by the organization
its `Task.owner` names. The gateway reads the organization NPIs the owner states, as at a
provider's base (below): on the owner reference itself, on the `organization` of the
PractitionerRole it contains, or on a contained Organization that the owner or that role's
`organization` names. It routes the Task to the one
registered provider the network attests those NPIs for. The practitioner's NPI is never routed
on, and an identifier in another system names no one. An NPI the network attests for no
provider is passed over when another NPI names a provider. Refused `422`, on `routing`, before
the Task crosses the network:
- an owner that cannot be read: an identifier given as a list, a `#id` reference that names no
  contained resource, several, or one that is neither a PractitionerRole nor an Organization, or
  a role whose `organization` is a `#id` reference naming no one contained Organization;
- an owner that names no organization NPI the network attests for a registered provider:
  no owner, a literal reference alone, a practitioner's NPI only, or NPIs no provider is
  attested for;
- an owner whose NPIs the network attests for different providers, whether on two NPIs or on one
  NPI two providers are attested for. The network never picks one.

Each refusal says it is on `routing` and points to the providers' bases, where a Task routes by
its address alone. A Task routed to a provider that does not take the request is refused by the
Hub, `422` (`routing`), as at its base.

**What the network checks.** The network refuses a Task only on its own rules, at every
conformance level, and records each refusal as this gateway's:
- **Reads two ways:** a body that repeats a member name, or names one in another case, is `400`
  (`integrity`).
- **Not a Task Attachment Request:** a body that is not a Task is `403` (`authority`). So is one
  whose `Task.code` is neither
  `http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes#attachment-request-code` (documents by
  code) nor, from v1.7.0,
  `http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes#attachment-request-questionnaire` (a
  questionnaire to complete). The code is read by system and code, so a CDex data request
  (`data-request-code`, `data-request-query`, `data-request-questionnaire`) is refused, as is
  either taken code under another system. A questionnaire request is carried as you sent it, its
  questionnaire canonical included, apart from its `payer-url`, which the provider's gateway re-keys
  to its documentation return path for you (registered edit E-11).
- **No provider at the base:** a base that names no registered provider is `422` (`routing`).
- **No patient to bind:** the patient is bound by its member number (an identifier typed
  `v2-0203#MB`), as the other legs bind theirs. The number is read from the Patient the Task
  contains as `Task.for` (a `#id` reference), or from `Task.for.identifier`. A Task that names no
  member number for `Task.for`, or two that differ, is `422` (`authority`), since the network
  cannot identify the patient. A member your system of record does not hold is carried, unless
  `REQUIRE_KNOWN_MEMBERS=true`, which refuses it `400` (`conformance`). Its network patient
  identifier is derived as on the provider's CDex submission, which names the same number: from
  the member number, with the birth date and family name of a Patient the Task carries only when
  that Patient's `id` is the number or it carries the number under the system `urn:shn:member`.
  A Patient naming the number only under your own identifier system, as the CDex IG's example
  does, is not read for them, so the identifier derives from the number alone. In that same
  reading, a Task carrying no Patient of the member's own but another person (a Coverage's
  subscriber, policyholder or payor) under the member's `id` or `urn:shn:member` number is
  `422` (`authority`): the number is shared, so it identifies no one patient.

Nothing else in the Task decides whether it is carried, and at a provider's base nothing in it
decides where it goes. The network does not judge its `requester`, tracking identifier or
`payer-url`, and reads its `owner` only for the organization NPIs it states.

**The owner at a provider's base.** From shn-gateway v1.7.0, the gateway reads the organization
NPIs `Task.owner` states: an NPI on the owner reference itself, on the `organization` of the
PractitionerRole it contains, or on a contained Organization that the owner or that role's
`organization` names. The practitioner's NPI is never read for this, nor an identifier in another
system. When one is an NPI the network attests for another registered provider, or the owner
cannot be read, the gateway records the observer event `cdex.owner-differs`, with no identifier,
once the Task is to be carried. The Task is carried to the base's provider either way.

**The answer.** The provider's answer is relayed as its side sent it, with its status and media
type. One that repeats a member name is refused `502` (`integrity`), since it can be read two
ways; nothing else in it is read. No `Location` is added: reading a created Task back through the
network is a later leg.

A provider's gateway takes the request only once its participant configures it to (its own
system's Task endpoint, `PROVIDER_CDEX_TASK_URL`). Until a provider's gateway declares that it
takes it, that provider is not listed, and a Task to its base is refused by the Hub, `422`
(`routing`).

## Provider DTR population (`PROVIDER_DTR_*`)

See [INTEGRATION.md](INTEGRATION.md#provider-dtr-population) for the managed-vs-native
tradeoff.

| Env var | Description |
|---|---|
| `PROVIDER_DTR_NATIVE` | `true` to forward DTR population to an SDC `$populate` engine instead of the managed populator. Default `false`. |
| `PROVIDER_DTR_POPULATE_URL` | The SDC `Questionnaire/$populate` endpoint. Required when `PROVIDER_DTR_NATIVE=true`, and on a provider for `ORIGINATION_PROFILE=demo` / `provider-data` (the operated engine those lanes require). A profile on any other role does not require it. |
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
the status or body inside it. From v0.63.0 that includes a success's own status: your
system's `201` or `202` answer reaches the other participant's system as `201` or `202`
(and a `204` as `204`, with no body), where every earlier release wrote `200`. An answer
your gateway built itself is `200`, and so is a success from a peer gateway before v0.63.0,
which framed every success as `200`. A frame stating `204` that carries a body is not read:
the requester's gateway answers `502` and records `integrity`. A **transport fault** on the requester's own side (its
gateway cannot reach the Hub, or its own build or dial fails) is not an application answer
and surfaces as `"hub routing failed"`; the responding gateway's own failures are its
answer (below). From v0.60.0, an Authorization Framework that gives the requester's
gateway no answer (the connection refused, reset or closed before any response, no answer
within the gateway's HTTP client timeout, or a `503` or `504` from a proxy or load
balancer in front of it; the Framework never answers those itself) is answered
`503 {"error":"the authorization service could not be reached, so this leg was not sent to
the payer"}` (an `OperationOutcome` with issue code `transient` on the FHIR operation
routes), and the leg is recorded as this gateway's refusal on rule `authority`, with
`authorizeError` saying why; before, it was `502 {"error":"authorization failed"}`. The leg is not sent to the Hub: a Da Vinci ingress
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
not hold when it requires known members (`400 unknown member`), a request it cannot read, no order to decide on, a federated query or
patient-authored DTR request whose patient cannot be matched to the token's (`403`), a consent it cannot confirm, an ingress validation
failure at enforcement `strict` (`422 ingress validation failed`, issues echoed) — is its
answer about the request, and a frame-capable requester receives it with that status and
body. So is any `4xx` it writes about **its own participant's answer** after that system
answered: an answer that repeats a member name (`403`, at every level), and at enforcement
`strict` a PAS response whose patient linkage is inconsistent or that names another
patient (`403`; from v0.61.0 the parent a dependent's Coverage entry names as a contained
`Patient` in its `subscriber` or `policyHolder` is part of that Coverage, not another
patient, and from v1.4.0 so is the parent carried as a `Patient`
entry of its own that only a Coverage's `subscriber` or `policyHolder` names), a questionnaire package that carries a subject (`403`), or an answer that
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
CRD/DTR/PAS at `2.0.x`, `2.1.x`, and `2.2.x`. That is its **native**
capability. What it **declares** to the network is a separate, operator-chosen subset, and
the declared set is the starting point for routing; qualified native lanes also
support the native-reach and inbound-honor rules below.

From shn-gateway v0.62.0, a peer whose lines share none with yours is carried by default, as if
your two systems were integrated directly: your gateway sends at its own line when nothing
reaches the peer's, answers a request claiming a line it cannot answer at as it answers one
claiming none, and relays an answer stamped with another line than the leg routed. Each is
recorded (the `contract.line-carried` observer event and the exchange record's
`LineCarried`). Set `REQUIRE_SHARED_LINE=true` to have such a pair refused instead.

From shn-gateway v0.63.0, your gateway translates between IG lines only if you turn it on
(`TRANSLATE_IG_LINES=true`). Unset, it translates nothing: a pair that shares no line is
built natively at a line the peer declares where your gateway can build it and has a lane
for it, and otherwise carried as sent and recorded, and a payer's questionnaire package
answer reaches your system exactly as it arrived. With it on, a message your gateway builds
is carried to the peer's line through the registered compatibility steps where a certified
translation exists, and otherwise carried and recorded the same way; a questionnaire package
answer is carried into your gateway's DTR line's names (registered edit E-08).

With it on, a translation is planned only to a line whose validator lane can certify it.
A default lane serves once it has qualified. An explicitly configured lane
(`FHIR_VALIDATE_URL_<line>`, or `FHIR_VALIDATE_URL` at its own line) is used as configured
until it fails at the connection: it
refuses, resets or drops the connection, or cannot be reached. From then on a message is
carried at its own line and recorded, as when no translation exists, or refused as
`routing` under `REQUIRE_SHARED_LINE`. A lane that only fails to answer in time may be
busy, so your gateway asks it for its `GET /metadata` once (at most once every 30 s) on a
connection of its own, and keeps using it if it answers. While a lane cannot be reached,
your gateway asks for its metadata every 30 s for the first hour, then every 5 minutes, and
plans translations to it again at the first answer of any status. The gateway logs
`validator_lane_unreachable` and `validator_lane_reachable` when a lane changes state.

- **The exchange that finds the lane down.** A lane can fail after a translation was
  planned to it: between the walk and the check of what the walk made. That exchange is
  refused `500`, with nothing sent, because your gateway could not certify its own edit.
  The exchanges after it are carried.
- **A lane that is still warming.** A validator can answer its metadata before it has
  warmed up. The first check after it comes back can then run out of time: that exchange
  is refused, and the lane stays in use. The validator image the gateway bundle ships
  (`deploy/validator`) closes every connection until it has warmed up, so it is never
  used early.
- **Governed checks are unchanged.** Every other check calls the lane exactly as before,
  and native reach still needs the lane configured (the one exception, at `none`, is in
  the lane map's egress direction below). Only the planning of a translation reads
  whether the lane can be reached.

| Env var | Description |
|---|---|
| `REQUIRE_SHARED_LINE` | `true` or `false`; unset means `false`. From shn-gateway v0.62.0. Set `true` and your gateway refuses, as `routing`, a pair that shares no declared IG line: a leg to a peer that no shared line, native reach or transform chain reaches (`422 no shared contract line …`); a leg forwarded to your own Da Vinci system (`PAYER_DAVINCI_CONTRACT_VERSIONS`) on the same terms; a request whose frame claims a line your gateway cannot build or validate at (`422 request declares contract version …`); from shn-gateway v0.63.0, whatever lines the pair shares, an amendment or inquiry your gateway builds about a pended prior authorization whose pinned line nothing reaches any more (your system's own is carried exactly as sent) (`422 … no bridge to the pinned line … remains available`; unset, it is built at a line you declare, one the payer also declares if there is one, and carried); a request whose walk to the payer's line would change it, from your own EHR (CDS Hooks or questionnaire-package) or built by your gateway (questionnaire-package, PAS inquiry) (`422 … cannot be carried to the payer's line unchanged`); and a 2xx answer stamped with another line than the leg routed (`502 … response contract version mismatch …`). Unset, each is carried and recorded. It never changes the line a pair that shares one routes at. Before v0.62.0 every one of these was refused. Any other value refuses to boot. |
| `TRANSLATE_IG_LINES` | `true` or `false`; unset means `false`. From shn-gateway v0.63.0. Optional, and off unless you turn it on. Set `true` and your gateway translates between IG lines: a message it builds for a peer that shares no line with it is carried to the peer's line through the registered compatibility steps where a certified translation exists (a chain whose only outcome would be a refusal, such as one that needs PAS's gated 2.0 to 2.1 step, is never planned: the message is carried at its own line and recorded), and a payer's questionnaire package answer from another DTR line is carried into your gateway's line's parameter names (E-08). The boot log states `TRANSLATE_IG_LINES=true` when it is on. Unset, nothing is translated: such a message is built natively at a line the peer declares where it can be, else carried as sent and recorded, and the answer is carried exactly. Before v0.63.0 every gateway translated. Any other value refuses to boot. |
| `SHN_CONTRACT_VERSIONS` | This gateway's own **declared** exchange-contract versions: comma-separated `<contract>@<line>` tokens, e.g. `pa.crd@2.2, pa.dtr@2.2, pa.pas@2.2`. Drives leg selection, the published `CapabilityStatement`s and `.well-known/davinci-configuration`, and the declaration peers route against. Must be a **subset of the native set** (`pa.crd@{2.0,2.1,2.2}`, `pa.dtr@{2.0,2.1,2.2}`, `pa.pas@{2.0,2.1,2.2}`) — a token outside it is a boot error, not a routing outcome. From shn-gateway v0.62.0 no gateway declares `pa.pdex@2.1`: a gateway serves no Patient Access API and builds no PDex resource, and a set still naming it refuses boot with an error saying to remove it (remove it before upgrading; earlier releases boot without it too). Unset ⇒ the build default, the canonical `2.0` line (`pa.crd@2.0`, `pa.dtr@2.0`, `pa.pas@2.0`; before v0.62.0 also `pa.pdex@2.1`). From shn-gateway v0.59.0, a payer that forwards to its own system (`PAYER_DAVINCI_BASE_URL`) and declares that system's versions (`PAYER_DAVINCI_CONTRACT_VERSIONS`) derives this set instead when it is unset: the system's `pa.crd`, `pa.dtr` and `pa.pas` lines (before v0.62.0, plus `pa.pdex@2.1`). The gateway logs `gateway: declaring <set>, derived from PAYER_DAVINCI_CONTRACT_VERSIONS` at boot; the registry entry peers select it against must declare the same set (see [Opting a line in](#opting-a-line-in), step 3). A derived line needs its validator lane exactly as a declared one does, and a system line this build cannot exchange refuses boot, naming it. Earlier releases declare the build default, which advertises lines the forward then refuses. From shn-gateway v0.59.0, any payer that forwards to its own system also refuses to boot when its declaration, set or derived, has no `pa.crd`, `pa.dtr` or `pa.pas` line: no provider could reach it for prior authorization. |
| `FHIR_VALIDATE_URL_2_1` | Optional **2.1** `$validate` address override. Compose default: `http://shn-validator-2-1:8080/fhir`. |
| `FHIR_VALIDATE_URL_2_2` | Optional **2.2** `$validate` address override. Compose default: `http://shn-validator-2-2:8080/fhir`. |
| `FHIR_CERTIFY_URL_2_1` | Optional **2.1** `$validate` address for the certification evidence only. Never a routing lane. |
| `FHIR_CERTIFY_URL_2_2` | Optional **2.2** `$validate` address for the certification evidence only. Never a routing lane. |
| `FHIR_DEFAULT_VALIDATOR_LANES` | Unset, or `none`. `none` states that this gateway's network has no Compose default validator services, so the gateway never probes their names. A CRD, DTR or PAS line other than 2.0 then validates only through its own `FHIR_VALIDATE_URL_<line>` (routing) or `FHIR_CERTIFY_URL_<line>` (evidence), and a declared line without its own `FHIR_VALIDATE_URL_<line>` refuses boot, naming the key (a `FHIR_CERTIFY_URL_<line>` does not count), at every level but `none` (from v0.63.0; [Running at `none` without a validator](#running-at-none-without-a-validator)). A single-line contract would keep riding the canonical validator, as it does with the default lanes; no native contract is single-line from v0.62.0 (`pa.pdex@2.1` was). Any other value is a boot error. |

A `FHIR_VALIDATE_URL_<line>` is a routing lane: setting it makes the line reachable in both
directions (an inbound frame at that line is honoured, origination may target it). A
`FHIR_CERTIFY_URL_<line>` is not: it gives the certification evidence (`certify:` lines) an
address for that line and changes nothing about routing, so a gateway can record 2.2
verdicts while it stays a 2.0 gateway. The evidence uses, in order, `FHIR_CERTIFY_URL_<line>`,
then `FHIR_VALIDATE_URL_<line>`, then the Compose default once it has qualified — by
routing's qualification, which retries for the gateway's life, when it ran the same
qualification (below), or by the evidence's own: the evidence tries the default in a
background loop of its own, started only where certification evidence is collected (never
at `CONFORMANCE_ENFORCEMENT=none`; first attempt 15 s after boot plus a random part of 15 s,
stopping at the first success from either side), so a validator that comes up after boot is
certified against soon after, and routing's lanes never change because of it. While routing is
still retrying the default lane's qualification, the loop posts no corpus of its own; once
routing's attempt passes on the same endpoint, for the same line, with the same qualification
and within the same 120 s, that pass is the evidence's first qualification. Any difference and
the loop runs its own. What that loop sends a validator that is not up: each attempt lasts up to
120 s and, until the validator's `GET /metadata` answers `200`, repeats that request about once
a second (each request waits up to 4 s, then 1 s passes before the next), so an address that
refuses the connection, does not resolve or answers another status gets about one request per
second, and one that accepts the connection but never answers about one every 5 s. After a
failed attempt the loop waits on the gateway's shared retry backoff: 15 s after the first
failure, doubling to 5 min, each wait drawn from the upper half of its step, so the gateways a
deploy restarts together spread apart. An attempt its budget cuts off is discarded, never
resumed. The budget bounds an attempt's cost, not what qualifying means: from v1.0.0 it is 120 s,
routing's budget, where it was 60 s. A metadata `200` ends the probing: when it is a readable R4
CapabilityStatement of at most 4 MiB the attempt then posts the readiness corpus once, and
passes or fails; any other `200` fails the attempt. No exchange waits on that: a line whose default has not
qualified records its verdict as `certification validator unavailable: FHIR_CERTIFY_URL_<line>
and FHIR_VALIDATE_URL_<line> are not configured; default lane qualification pending` (or
`… failed, retrying` after a failed attempt of its own) and dials nothing for that exchange.
While it defers to routing, the state names routing's qualification instead: `default lane
qualification pending (routing qualifying)` until routing's first attempt fails, then
`default lane qualification failed, retrying (routing attempt <n>: <reason>)`, with routing's
attempts so far and its last failure's reason in the words of the list below (from v1.0.0;
before it the evidence ran attempts of its own and named only those); routing's qualification logs `validator_qualification` lines, and the evidence's loop logs a
`certification_lane_qualification` line when an attempt's outcome changes (the first
failure, a change of its reason, then success). Each line carries the lane's line, `base`
address, the `host` it dials, its `state` and `attempts`, the attempts its loop has made; a
`failed` line also carries `next_retry_s`, the wait before the next. A `failed` line also carries a `reason` naming why, in the gateway's
own words: `name does not resolve`, `name lookup failed`, `connection refused`,
`metadata answered HTTP <status>`, `metadata is not an R4 CapabilityStatement`,
`qualification corpus did not pass`, `no answer within the qualification budget`,
`qualification stopped` or `qualification did not pass`. Never a validator's answer.
From shn-gateway v0.57.0 an address set by either key for 2.1 or 2.2 is gated the same way,
on its own qualification by the same corpus: a validator still warming answers errors about
itself (on the 2.2 line, `SLICING_CANNOT_BE_EVALUATED` the first time it meets a PAS answer),
not about the message. Its loop starts at boot where certification evidence is collected
(never at `none`, where the address is never dialed), with no 15 s wait, never defers to
routing, and sends the same traffic on the same schedule afterwards (as described above); until an attempt passes, the line records `certification validator unavailable:
lane not qualified; qualification pending` (or `…; qualification failed, retrying`) and dials
nothing for that exchange, and the loop logs `certification_lane_qualification` lines as
above. The gate re-qualifies after a lane stops answering: once a 2.1 or 2.2 lane (a default or an
address) has qualified, a certification it fails at the connection (the lane refuses, resets or
closes the connection, or cannot be reached) records `… lane not qualified; re-qualifying`, or
for a default lane `…; default lane re-qualifying`. A lane that does not answer within the
certification's 2 s while the collection is still waiting may only be busy: that certification
is recorded `expired` (the certification client's own limits are 2.5 s, so the certification's
2 s always runs out first), and the lane is asked for its `GET /metadata` once, within 4 s, on a
connection of its own that no queued certification holds, at most once every 30 s. An answer
of any status keeps the lane qualified, and each check logs one
`certification_lane_timeout_check` line (`version`, `line`, `host`, `answered`, `at`,
`duration_ms`; a check cut short by shutdown logs nothing); a
lane that does not answer is re-qualified like one that failed at the connection. A
re-qualified lane no longer counts routing's own qualification of a default lane for the
evidence; until the re-qualification passes, the line records the same and dials nothing. A
re-qualification starts at once, or 30 s after the
previous one started if that was more recent. Only its start is limited: while the lane stays
broken, its attempts follow the qualification's own schedule above, and that schedule starts
over with each re-qualification: the re-qualification starts after a random part of 15 s more,
and its attempts of up to 120 s, probing metadata as described above, back off from 15 s to 5
min. A re-qualification is always the evidence's own run, never routing's. An attempt whose
metadata answers `200` with a readable R4 CapabilityStatement runs the readiness corpus instead,
so a lane that answers its metadata but fails the corpus is sent the corpus on that backoff. An answer, whatever it is, does not clear the qualification: a verdict,
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
not `invalid`: the validator reached no verdict on the message. From shn-gateway v0.63.0 so is a
lane that stops without a verdict (`HAPI-0389`: "failed to validate, but no errors", as it can on
a PAS 2.1 or 2.2 request Bundle that declares its profile) or that read no payload (`HAPI-0992`):
`unavailable`, with `error` `certification validator did not judge the payload`. So is a lane
whose errors are a regex check it could not run (`TYPE_SPECIFIC_CHECKS_DT_PRIMITIVE_REGEX_EXCEPTION`,
which a validator reports on a value that matches its type when a regex overruns its time limit; or, from v1.2.0, `REGEX_MATCH_TIMED_OUT`, for a regex a profile sets), alone or with the
unchecked terminology above and the no-match summaries on those entries: `unavailable`, with
`error` `a primitive regex check could not run` (after the terminology reason when there is one). From v1.8.0 so is a lane
whose errors say it does not hold a profile the payload declares (`VALIDATION_VAL_PROFILE_UNKNOWN_NOT_POLICY` on the
payload's own `meta.profile` element, read so on its own, with any `Validation_VAL_Profile_Unknown` for the same profile), alone or with the causes above (a no-match summary or PAS slice-match consequence on an entry follows only from the unchecked terminology or regex check on that entry, never from such a profile, so beside only such a profile it keeps the line `invalid`): `unavailable`, with
`error` `profile unavailable: <profiles>` (at most two named, as a finding names them), or, beside another cause,
`profiles unavailable: <count>` after the terminology reason and before the regex one. A profile another candidate line's lane held was checked there: at the line that lacks it, it keeps the line `invalid`. Any other error
keeps it `invalid`: a structural error, a failed invariant, a code the validator checked and did not
find (`Unknown code`), a required-binding miss on a code system it holds or under a value set
it could expand (a code from a system it does not know, where the value set is drawn from
systems it does), or a fatal issue. A
code system is named as written only when it is an X12 code system canonical
(`https://codesystem.x12.org/<version>/<list>`); any other is named by its size and SHA-256.

**One validator per line — this is not optional.** A FHIR server loads exactly **one**
version of a given IG package, so a single HAPI cannot host CRD 2.0.1 and CRD 2.2.1 at the
same time; a 2.2 payload validated against a 2.0-loaded server is not validated, it is
mis-validated. Each declared non-canonical line therefore needs its own `$validate` lane,
at every level but `none`, which validates nothing (from v0.63.0).
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
a metadata 200 alone is insufficient. From v1.0.0 a failed attempt is retried
inside the startup bound of 600 seconds, on the schedule below; when the bound
runs out first, boot fails with an error naming the line, endpoint and the last
attempt's reason. From v0.63.0, at `none` a declared default lane qualifies in the
background instead, and its failure does not stop the gateway.
The image's `/healthcheck` is an executable observing a qualification marker,
not an HTTP readiness endpoint.
The supplied validator image withholds public metadata and validation until its own
finite worker succeeds, then each gateway performs the corpus above. Its external
`8080/fhir` address stays the same; no extra readiness flag or operator sequence is
required. A terminal image qualification failure remains unavailable until restart
and is reported by the image's `/healthcheck`.

Undeclared defaults qualify in the background without delaying configured `2.0`
startup. Until qualification succeeds they cannot satisfy routing or inbound
frame admission. From v1.0.0 a failed attempt is never terminal: the lane retries
until an attempt passes, 15 s after the first failure, doubling to every 5 minutes,
each wait drawn at random from its upper half so gateways restarted together do not
retry together, and each attempt bounded at 120 s (before v1.0.0 a default lane got
one attempt per gateway lifecycle, and a failed one lasted until restart). The
`validator_qualification` lines carry the lane's state changes only: `started`, the
first `failed` of a run of failures and any change of its `reason`, and `ready`, with
`attempts` and, on `failed`, `next_retry_s`. `/internal/checks` reports each default
lane as `validator-lane-<line>`, failing with `lane-unqualified` until it qualifies,
and the `ValidatorLane` gauge reads 0 for it meanwhile (see "Exchange metrics").
Shutdown cancels and joins workers. Embedders using `Handler` or `HandlerWithClock` must close the returned
`io.Closer` after stopping HTTP service; tests can use `HandlerForTest` or
`HandlerForTestWithClock` and invoke their cleanup before releasing dependencies.
Ordinary validation verdicts do not change synthetic readiness.

A single-line contract would retain a canonical/configured compatibility alias, and
that alias could not satisfy a CRD, DTR or PAS request at its numeric line. No native
contract is single-line from v0.62.0: PDex was, and no gateway declares it now.

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
it — a wider predicate than its own declared set — and, at `none` from v0.63.0, at any
line it declares, with or without a validator for it, so in-flight and stale-routed legs are
answered correctly rather than refused. Configured explicit lanes and successfully
qualified default lanes can cover undeclared native lines, widening this honor window.
An unavailable default never grants admission, and a single-line contract's canonical
compatibility alias (none is native from v0.62.0) cannot satisfy a CRD, DTR or PAS request
at the same numeric line.

**Declared-set changes must grow, never swap or shrink.** Adding a line is safe in either
rollout order. **Removing** one is a breaking operation: a pended prior-authorization pins
its contract line at origination and must resume on that exact line, so dropping a line can
strand pends that can no longer be resumed. From v0.63.0 such a pend's amendment or inquiry is
built at a line you declare (one the payer also declares, else your highest) and carried instead, and the difference recorded (`contract.line-carried`),
unless `REQUIRE_SHARED_LINE=true` refuses it. At `CONFORMANCE_ENFORCEMENT=none` the inquiry,
whose builder is certified at every line, is instead built at its pinned line even with no lane
there. If you must remove a line, drain outstanding
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
   the same peer would only be reachable, if at all, through a transform chain (arm 3, from
   v0.63.0 only with `TRANSLATE_IG_LINES=true`), or carried as sent (refused as `routing`
   under `REQUIRE_SHARED_LINE=true`).
   From v0.63.0, one exception needs no lane. At `CONFORMANCE_ENFORCEMENT=none` your gateway
   checks no payload, so a message it builds itself (an origination, never a request your
   participant sends) is built at a line it has no lane for when that message's builder is
   certified at that line at build time: the whole message, built by the gateway's own
   builder at the line, validated against its profile in the platform's validation gate. Today only the PAS
   inquiry is certified (2.0, 2.1 and 2.2), and an inquiry always follows its pend's pinned
   line, so in practice: at `none`, an inquiry whose pinned line has no lane is built at that
   line rather than carried at one you declare (see "Declared-set changes" below). Every
   other message still needs the lane, and at every other level the lane is required as
   above.
2. **Inbound:** the SAME lane map backs what this gateway **honors** on an inbound request-frame
   `contractVersion` claim (the SDK's `docs/PARTICIPANT_PROTOCOL.md`, §8.6's *native ∩ laned* rule, wider
   than the declared set by design) — so configuring an undeclared lane widens what you'll
   silently accept from a stale-routed peer too, not only what you can build for one. This is
   the bidirectional lane-admission rule, read by both routing directions.

Lanes obey the same grow-only discipline as declared lines, for the identical pend-stranding
reason: a resumed pended exchange needs its pinned line's lane to remain configured for as long
as the pend can still be amended, or, from v0.63.0, it is carried at a line you declare instead (one the payer also declares, else your highest)
(refused under `REQUIRE_SHARED_LINE=true`). Do not remove a `FHIR_VALIDATE_URL_<line>` env var while any
pend may still be pinned to that line, declared or not. A single-line contract would have
nothing to opt into — it has only one native line and always rides the canonical lane; none is
native from v0.62.0 (`pa.pdex` was, and no gateway declares it now).

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
(the platform's per-line use-case conformance mesh) alongside `pa.crd@2.2` and
`pa.pas@2.2`.

A **separate, pre-existing, unrelated** gap surfaced during this verification and is
recorded here rather than fixed (out of scope for that fix — it concerns the Questionnaire a
worked-example payer responder *asks*, not the QuestionnaireResponse it *answers with*): the
worked-example lumbar Questionnaire (content unchanged; the SDK's public `DemoLumbarQuestionnaire`
export was retired on 2026-08-25, and this fixture now lives in the platform's DTR package
and in this module's `fhirseed/`) does not itself conform to DTR 2.2's
`dtr-base-questionnaire` profile (`Questionnaire.subjectType` min=1 unmet, an unmet
`sdc-2` versionAlgorithm constraint, and item-extension slices this build's pinned package
set cannot resolve). This was already true of the existing `testdata/golden/2.2/
questionnaire-package-pa-lumbar-mri.json` golden before that fix and is not newly
introduced by it; the platform's validation gate does not currently catch it (that golden is not among
the gate's pinned profiles, so it validates against base R4 only, never against
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

Verified by `TestManagedPopulatorBuildsAtLine` (`gateway/engine/workstream_pa_populator_test.go`) at the
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
line unreachable there, so a transform chain (arm 3) fires instead when the gateway
translates (`TRANSLATE_IG_LINES=true`, from v0.63.0), or the leg is carried as sent at the
gateway's own line when it does not or no chain resolves — which is the whole point of the
knob: simulating "a build that predates the newer contract lines" without running an older
build.

**Narrowing gates routing, not authoring.** A narrowed gateway still *builds* fully-shaped
content at any line it natively speaks whenever that content rides an unaffected leg — for
example, a DTR questionnaire package authored at 2.2 that then rides an arm-1 PAS submit.
The knob changes what a gateway **routes as**, never what it can **author**.

**Never set this in a participant's deployment config.** It exists for the network's own
exhibits: the **SHN Kit's bridging demo** (see `kit/README.md`), which restarts its own
supervised gateway child with it set (and with `TRANSLATE_IG_LINES=true`) to demonstrate
cross-version bridging against skewed peers, and SHN's own provider test gateways that run
the same exhibits. It is loud wherever it's set — in the boot log:

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
have run; a call on one of module 1's origination routes is one exchange here, so
one result counts the findings of all the legs it sent. At `observe` an exchange captures at most 32 findings on their own, and
its result still counts them all (`"truncated": true`); a check dropped from a
full queue makes the result `"incomplete": true`.

Request handlers enqueue to a bounded queue (4,096 observations, 64 MiB retained,
8 MiB maximum retained body per event). Each publication attempt has a bounded
30-second ownership window so evidence waiting for another source's prerequisite
can arrive without holding a request open. HTTP capture separately limits concurrent
buffering and marks unread, aborted, limited or failed bodies partial. A body the
capture limits left out or cut says so in the observation's `detail` (`body not
kept: capture budget`, `body capture partial: capture budget`; see CHANGELOG.md). Publishing
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
  cut and their handlers get up to 2 s to return, and the gateway closes: its
  in-memory audit outbox drains (up to 10 s) and its conformance checks flush (up to
  10 s), and, with `OTEL_METRICS_EXPORTER=otlp`, the final metrics export runs (up to
  5 s). The publisher then delivers what is queued for up to 10 s and sends a last
  heartbeat (1 s), counting anything left as `stopped`. With collection configured, a
  stop takes up to 43 s in all. Give the container the 45 s stop timeout of
  [DEPLOYMENT.md](DEPLOYMENT.md#stopping-the-gateway), "Stopping the gateway"
  (Docker's default is 10 s).

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
