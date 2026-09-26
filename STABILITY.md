# Stability and versioning

## Versioning policy

`shn-gateway` follows semantic versioning. The module is currently **pre-1.0
(0.x)**:

- **MINOR** versions (0.x.0 → 0.(x+1).0) may carry breaking changes. Each
  breaking change is called out in the release changelog.
- **PATCH** versions (0.x.y → 0.x.(y+1)) contain backwards-compatible fixes
  only.

A published version tag is **never re-tagged** with different content. The Go
module proxy caches a tag's tree permanently; always bump to a new version
rather than moving an existing tag.

This gateway requires `shn-sdk` — see `go.mod` for the pinned version.

## Native PAS response contract

PAS operation responses must be complete Bundles containing exactly one
ClaimResponse and a closed set of referenced resources. **Every decision a payer
gives is relayed as the payer gave it**: the gateway does not poll its own payer
for a later decision, does not replace a payer's answer with one it assembled,
and stamps no contract line of its own on a message it did not produce. A payer
that pends answers `pended`, and the requester obtains the determination by
asking for it (`Claim/$inquire`, leg type `pas-claim-inquire`).

**Breaking in this release** (the assembly path is gone):

- `engine.WithPendReQuery` is REMOVED. It configured the pend re-query the poll
  performed; there is no poll.
- `engine.LegResult.ResponseAssembled` is REMOVED. A partner `LegResponder` that
  set it no longer compiles. It marked an answer this gateway had assembled, and
  a relayed answer is never one — delete the assignment; nothing replaces it.
- The `leg.assembled` observer event is REMOVED. Nothing assembles a terminal PAS
  response, so nothing emits it. Consumers should drop the case; no event takes
  its place (`leg.response` already records the answer that was relayed).
- `engine.PASWaitDefault` is now `0` (was 30s). An originator route waits only when
  its caller asks it to. A caller that relied on the default to turn a pend into a
  determination now receives the pend and its continuation, and asks.
- `engine.PASWaitMax` is now 30s (was 120s). These still compile, so the change is
  silent at build time: a caller asking for more than 30s is capped rather than
  refused. Nothing is lost by it — the inquiry schedule always finished near 26s, so
  every wait above that was already a no-op that reported itself as a longer one.
  The bound and the schedule are now derived from each other and held together by a
  test, so they cannot drift apart again.

## Conformance enforcement levels

`CONFORMANCE_ENFORCEMENT` takes `none`, `observe` (the default when unset),
`structural` or `strict` (see [docs/CONFIGURATION.md](docs/CONFIGURATION.md)).

**New in v0.54.0** (v0.53.x and earlier refuse to boot on `structural`):

- The new `structural` level runs every check, refuses a message whose structure is
  broken with the status and body `strict` gives it, and records every other
  defect as `observe` does. A FHIR validator issue is classified by the
  validator's message id: only invariants and the validator's recognized issues
  for a code outside its code list (licensed code systems included) are
  recorded, and every other FHIR profile issue, any other terminology issue,
  every fatal issue and an issue it cannot classify refuses. A validator that cannot run is recorded, not
  refused.
- A `Parameters` resource is now sent to the validator inside the operation's
  `resource` parameter, so the validator reads it. A validator that answers that
  it was given no resource (`HAPI-0992`) is treated as unavailable at every
  level: `strict` refuses it with `500` (it was a `422` naming the validator's
  text before), `observe` and `structural` record it as unavailable and relay.
- Unset, `none`, `observe` and `strict` are otherwise unchanged.

**Behavior change in v0.53.0** (v0.52.0 refuses to boot on `observe`):

- An unset `CONFORMANCE_ENFORCEMENT` now means `observe`, not `none`. Like
  v0.52.0's `none`, `observe` runs every check and records each defect, but
  refuses nothing a check finds: the content defects earlier releases' `none`
  refused at every level (below) are now relayed with a finding. A gateway still
  needs its validator to boot, as before. `engine.Config.ConformanceEnforcement`'s
  zero value is still `strict`: only the published binary's environment loader
  maps an unset value to `observe`.
- `none` now runs no payload conformance check. It no longer validates, applies
  the CDS Hooks response rules or the content checks, gathers certification
  evidence or validates the resources a CDS Hooks answer embeds, and it records
  no finding. A gateway that sets `none` explicitly and relied on it for findings
  sees none; remove the setting, or set `observe`, to keep them.
- The new `observe` level runs every check, records each defect as a finding (the
  `conformance:` log line and the `conformance.observed` event) and carries the
  message as sent, apart from the gateway's registered edits (the callback
  removed, prefetch and coverage obtained where the participant has opted in to
  them, and payer identity mapping; see the participant protocol §7a.4).
  Operators who want findings without refusals set `observe`.
- A content defect (a request's or answer's own shape or internal consistency, a
  prefetch value the system of record cannot supply, an answer this gateway
  cannot read) refuses only at `strict`, with the status and body it has always
  had. Below `strict` the message is carried or relayed as sent, apart from the
  gateway's registered edits, and a PAS or inquiry answer relayed unread writes
  nothing to the local record.
- Network rules refuse at every level: authentication, authority (including a
  token presented with a request other than the one it was issued for), consent,
  the patient binding (as changed below), routing and addressing, replay, a
  repeated member name in any body, the contract line stamped on an answer's
  frame, and the check of a payload this gateway translated between IG lines.
- `strict`'s conformance refusals keep their statuses and bodies. `strict` now
  also records a finding of kind `content` when it refuses a message for a
  content defect, and a finding carries an optional `verdict` field
  (`unavailable` when a check could not finish; absent for an invalid result).

**Behavior change in v0.53.0: members the system of record does not hold are
carried by default.**

- On the CRD, DTR and PAS legs such a subject is now carried, identified by the
  member id and the Patient the request carries for it, on the provider ingress
  and the payer inbound alike. Send the same Patient, unchanged, on every leg of
  one exchange: for a member the payer does not hold, an amendment carrying a
  different Patient, or none, finds no pended authorization and is refused
  `409`, and an inquiry's decision is not recorded (observer event
  `pend.other-subject`). Earlier releases refused it with `unknown member`.
  `REQUIRE_KNOWN_MEMBERS=true` opts in to that refusal; any value other than
  `true` or `false` refuses to boot.
- **Breaking (Go API):** `engine.Config.AcceptUnknownMembers` is removed; use
  `engine.Config.RequireKnownMembers`. Its zero value carries unknown members.
- **Deprecated:** `SHN_ACCEPT_UNKNOWN_MEMBERS` only logs a warning and will be
  removed in a later release. Set to anything but `0` or `false` together with
  `REQUIRE_KNOWN_MEMBERS=true`, the gateway refuses to boot.
- A CRD request for a member the system of record does not hold must carry its
  own `coverage` prefetch (otherwise `422`, at every level); history prefetch the
  gateway cannot read for that member is left out, with the reason recorded. With
  `REQUIRE_KNOWN_MEMBERS=true` the member is refused instead.
- The provider's own Patient append on `$questionnaire-package`, which earlier
  releases applied under `SHN_ACCEPT_UNKNOWN_MEMBERS`, is no longer applied: a
  request carrying no Patient is carried as sent. It returns as a participant
  opt-in (`ENRICH_NATIVE_REQUESTS`, below). The other registered edits (callback
  removed, prefetch obtained, coverage obtained, payer identity mapping) are
  unchanged in this release.
- The receiving gateway no longer compares the patient the leg's token names with
  the patient the request names on the CRD, DTR, PAS and inquiry legs: it handles
  the member the request names as it would directly. A request whose member the
  two sides identify differently now reaches the receiver's own system instead of
  being refused `403 token subject does not match request patient`. Everything
  the payer gateway records about an exchange (the pend ledger, a decision
  ExplanationOfBenefit, the correlation it claims, an inquiry's decision) is filed
  under its own binding of the member the request names, never under the patient
  the token names. Eligibility, federated query and patient-authored DTR keep
  their own check of the token's patient. A token presented with a
  request other than the one it is bound to is refused, as before. When the
  payer's binding is not the token's patient, the payer answers as usual and
  raises the observer event `subject.binding-differs`; the network's audit
  records the exchange under the patient the token names.
- **Mixed releases:** a payer gateway before v0.53.0 still compares the token's
  patient with its own binding, and refuses a member its system of record does not
  hold (`400 unknown member`) unless it sets `SHN_ACCEPT_UNKNOWN_MEMBERS`. A
  request that a v0.53.0 payer would accept can therefore still be refused by an
  older payer: `400 unknown member`, or `403 token subject does not match request
  patient` when the two sides identify the member differently. A provider gateway
  from v0.53.0 also no longer appends its own Patient to a
  `$questionnaire-package` request, so for a member the provider holds and an
  older payer running with `SHN_ACCEPT_UNKNOWN_MEMBERS` does not, that request is
  refused `403 token subject does not match request patient`. Upgrade payer
  gateways before the provider gateways that send to them.
- **Breaking (Go API):** a `LegResponder` on the payer's CRD, DTR, PAS and inquiry
  legs is handed the payer's own binding of the member the request names as its
  subject, not the leg token's subject.

## Identifiers for members the system of record does not hold

**Behavior change in v0.55.0.**

- A member your system of record does not hold is carried, as before. The
  network identifier the gateway gives such a member is now in its own
  namespace, so it can never equal the identifier of a member your system of
  record holds. Earlier releases could give such a member the identifier of a
  member your system of record holds, and so file the exchange, and what the
  payer recorded about it, under that member.
- The bytes a provider and a payer exchange are unchanged. Only the network's
  own identifiers differ: when the provider holds a member and the payer does
  not, the two gateways now identify that person differently. Each gateway
  files its own records under its own identifier; the network's audit records
  the exchange under the identifier the request's authorization names.
- A payer gateway raises `subject.binding-differs` on every exchange about a
  member only one side's system of record holds, since the two identifiers now
  always differ; earlier releases raised it there less often. Expect more of
  these events after the upgrade. An exchange about a member neither side
  holds, with the same Patient on both sides, raises none.
- **Mixed releases:** a payer gateway before v0.53.0 still compares the token's
  patient with its own binding. For a member the provider's system of record
  does not hold, a v0.55.0 provider's identifier no longer equals the one such
  a payer holds for that member, or derives for it under
  `SHN_ACCEPT_UNKNOWN_MEMBERS`, so the request is refused `403 token subject
  does not match request patient`.
  Upgrade payer gateways before the provider gateways that send to them, as for
  v0.53.0.
- **Upgrade note (payer gateways).** A pended authorization recorded before the
  upgrade for a member your system of record does not hold was filed under the
  earlier identifier, so after the upgrade it is not found under the member's
  new one. An amendment for it still reaches your system and your answer is
  relayed, but it binds no pend and the gateway records nothing for it (it
  notes `pend.amendment-unbound`); an inquiry's decision for it is relayed but
  not recorded (it notes `pend.other-subject`). Before upgrading, let open
  pended authorizations for members your system of record does not hold reach
  a decision. Members it holds, and every exchange with no open pend, are
  unaffected.
- No configuration changes. `REQUIRE_KNOWN_MEMBERS=true` still refuses such a
  member instead.
- **Go API (additive):** `pgstore.OpenPends` and `pgstore.OpenPend`, which
  list a payer's undecided pended authorizations for an operator. It only
  reads, and never creates or alters the store's schema.

## Patients an exchange involves

**New in v0.55.0 (additive).**

- When the network's discovery descriptor lists `involved` in `hubAccepts`,
  each prior-authorization leg names, in its envelope's `involved` list, the
  other patients it involves, each with its own token, so the network records
  the exchange under each of them as well as the leg's own patient:
  - on a request, every other member the request carries, identified through
    your system of record as the leg's subject is (`request-named`);
  - on a payer's answer, the payer's own binding of the member, when it differs
    from the patient the leg's token names (`payer-held` or `payer-derived`).
- It runs at every conformance level, `none` included: it is the network's
  audit record, not a check of your payload. It never refuses a request, never
  records a conformance finding, and never changes the bytes exchanged. On the
  requester side it reads your system of record once for each distinct member
  a request carries, up to 32.
- The whole pass for one leg runs within 2 seconds (token requests four at a
  time), so it cannot hold a leg past that.
- A patient it cannot name is left out and the leg is sent as usual: past 16
  patients, a system-of-record read that fails, a member not held under
  `REQUIRE_KNOWN_MEMBERS=true`, a token that is refused, cannot be obtained,
  or does not carry the involvement asked for, or one not named before the
  2 seconds run out. Each raises the observer event
  `involved.omitted` (Detail: the reason) and, with `METRICS_SERVICE` set,
  counts in the EMF metric `InvolvedOmitted{reason}`.
- The descriptor is read once, at start, and the boot line says which way it
  went. A gateway started before the network lists `involved` sends nothing
  until it restarts.
- No configuration changes. Without `involved` in `hubAccepts` a leg carries
  no list, reads nothing more, and requests no further token.
- **Go API (additive):** `engine.Config.HubAcceptsInvolved`,
  `engine.Config.InvolvedBudget` (zero selects 2 seconds) and
  `engine.Config.InvolvedMetric`; `engine.InvolvedOmittedEvent`.

## Enrichment of native requests

**Behavior change in v0.54.0: a Da Vinci-native request is carried as sent unless
the participant opts in to enrichment.**

- `ENRICH_NATIVE_REQUESTS` takes `true` or `false`; unset means `false`, and any
  other value refuses to boot. It governs the provider ingress's three enrichments,
  each read from the participant's own system of record: the CDS Hooks prefetch fill,
  the `$questionnaire-package` Coverage append and the `$questionnaire-package`
  Patient append (registered edits E-02, E-04 and E-05).
- By default none of them runs. A CDS Hooks request is sent with only `fhirServer`
  and `fhirAuthorization` removed; a `$questionnaire-package` request is sent
  byte for byte. Earlier releases added the advertised prefetch values and the
  Coverage a request left out without being asked; an EHR that relied on that now
  sends them itself, or its gateway sets `ENRICH_NATIVE_REQUESTS=true`.
- A coverage a request leaves out is still read from the system of record, to
  choose the payer; it is not added to the request. That read is made under the
  system's own Patient id, so a system that names the patient by another id no longer
  refuses such a request by default, on the CDS Hooks and questionnaire-package
  ingress alike (it still does under the opt-in). A request whose coverage cannot be found is refused as before.
- With `ENRICH_NATIVE_REQUESTS=true` the three edits behave as in earlier releases,
  and the Patient append no longer depends on the unknown-member setting.
- Requests the gateway builds itself (`ORIGINATION_PROFILE`) are not affected.
- **Go API (additive):** `engine.Config.EnrichNativeRequests`; its zero value adds
  nothing.

## Payer eligibility endpoint

Added in v0.54.0.
- `PAYER_ELIGIBILITY_URL` declares a payer's own coverage-eligibility endpoint. A
  coverage-eligibility request is then carried to it exactly and the payer's answer relayed
  (an error answer as the payer's error). No answer is built from the payer's records in its
  place, whatever the payer's system answers or fails to answer.
- Unset, the default, is unchanged: the gateway answers eligibility from the payer's records.
- On the forwarded path the member is bound by the payer's own records and a token naming
  another patient is not refused, as on the prior-authorization legs. An answer about another
  patient, or naming its patient otherwise than by reference, is relayed below `strict` and
  refused at `strict` (`RulePatientAnswer`); one that cannot be read as a
  `CoverageEligibilityResponse`, or names no patient at all, is refused at `structural` and `strict` (`RuleAnswerShape`); one naming
  the patient by the payer's own Patient id for that member is the same patient. A failed read
  of the payer's records for that comparison refuses only at `strict`.
- The request, and the payer's answer, are `$validate`d at the conformance level (no call at
  `none`); the relayed PAS and questionnaire answers are not. A request the level refuses is
  refused before the payer's system is called.
- **Go API (additive):** `engine.WithEligibilityURL`, a `NativeOption`.

## Observational source certification

PAS ingress and native POST forwarding collect source-profile evidence after dispatch.
Each supported payload is checked independently at all three PAS/DTR lines; the
certified set and nearest source line are observations only. They do not change
routing, authority, payload bytes, acceptance, response stamps or lane readiness.
The native record describes the final POST attempt, before any polling or terminal
assembly. The provider ingress record describes the bytes dispatched and relayed.
At `CONFORMANCE_ENFORCEMENT=none` no evidence is collected and no worker starts.

The app creates separate clients at each configured validator endpoint and never
invents one: per line it uses `FHIR_CERTIFY_URL_<line>` (an address for the evidence
alone, never a routing lane), then the routing lane `FHIR_VALIDATE_URL_<line>`, then the
Compose default only once it has qualified — by routing at boot or by the evidence's own
background attempts afterwards, which never change routing's lanes; until then the line's
verdict states that no lane is configured and the qualification's state, verbatim, and no
exchange waits on or dials for a qualification. With `FHIR_DEFAULT_VALIDATOR_LANES=none`
there is no Compose default: nothing is probed, and a line with neither key states that it
is not configured and that the network has no default validator lanes. Embedders may
supply independent clients through `Config.CertificationValidatorsByLine`; they must
not share routing validators or qualification wrappers. Missing clients are recorded
as unavailable. HTTP server execution failures are unavailable rather than conclusive
invalidity. Logs beginning `certify: ` and `leg.certified` observer details contain
JSON metadata, hashes and verdicts, without payload snapshots. External validator
issues and errors are represented only by bounded counts, byte lengths and
SHA-256 digests; diagnostic text is never retained or broadcast in evidence.

Each gateway owns one worker, a 32-payload queue and a 256-record evidence ring.
Candidates have a two-second limit, collection has a six-second limit, and queued
work expires after thirty seconds. Overflow records retain metadata only; bounded
observer notifications report drops explicitly. Callbacks must return promptly and
support concurrent invocation. Completion is stored before callback delivery.

Call `Gateway.Close()` after stopping service and before releasing dependencies.
It cancels observation HTTP work, closes idle connections and joins the worker.
It waits for cooperative callbacks; it cannot forcibly cancel a blocked callback.
The app runner and managed app handlers own this cleanup. Tests may use
`FlushCertificationForTest` as a context-bounded completion barrier, and
`DisableCertificationForTest` for evidence-off comparison runs.

## Observer source completion

The existing opt-in, loopback-only `OBSERVER_ADDR` listener also serves
`POST /barrier`. Success is HTTP 200 with `Content-Type: application/json`,
`Cache-Control: no-store`, and
`{"protocol":1,"incarnation":"<opaque>","events":N}`. The server waits for at
most five seconds, bounded further by request cancellation. A failed or canceled
wait returns a diagnostic non-2xx response without a successful event count;
deadline failures return 504 and other completion failures return 503. Wrong
methods return 405.

`GET /health` remains immediate and adds `protocol:1` and the same nonempty
`incarnation` to its existing `events` count. It never waits or submits validation.
`GET /events` includes `X-SHN-Observer-Incarnation` before sending frames; SSE
IDs and data bytes retain their representation. Each Hub construction gets a fresh
random identity, so counts from different source instances are not interchangeable.

`Gateway.WaitObserverCompletion(ctx)` snapshots HTTP operations already entered
through `Gateway.Handler()`, awaits their return (including deferred evidence
enqueue), then snapshots accepted certification work and awaits its observer
callbacks. Operations admitted after the first snapshot do not hold that operation
cutoff; certification accepted before the second snapshot is included. Direct
`OriginateLeg` calls outside Handler and requests not yet entered are outside this
protocol. Simultaneous external traffic is not a causal attribution guarantee.
Completion proves diagnostic delivery through the source callback, not receipt by
an SSE client; clients must separately catch up to `events` in the same incarnation.

This accounting is diagnostic only: clinical responses, asynchronous certification,
routing, authority, payload bytes and readiness remain unchanged. The ordinary
app health wrapper bypasses operation tracking. `FlushCertificationForTest` retains
its accepted-queue-only semantics. `observer.Hub.HandlerWithBarrier(wait)` enables
the capability with a context-cooperative completion waiter; nil and the existing
`Handler()` retain count-only health and do not expose `/barrier`. Both forms serve
the SSE incarnation header. No observer listener is created by default.

## Supported seams

Partners may depend on the following packages across minor versions (breaking
changes will be noted in the changelog):

| Package | Description |
|---|---|
| `engine` | Leg-processing core — the `Config`, `Engine`, and `Handler` types; `SystemOfRecord` and `Store` connector interfaces (`shnsdk.Adjudicator` is the SDK-level decision interface — see below) |
| `app` | Config-only gateway runner — `app.Run` and `app.Handler`/`app.HandlerWithClock` for embedding |
| `connectors/fhirsor` | FHIR-backed `SystemOfRecord` connector |
| `connectors/pgstore` | Postgres-backed `Store` connector |
| `connectors/scaffold` | Runnable `SystemOfRecord` skeleton for custom / legacy backends |
| `connectors/smartauth` | SMART Backend Services HTTP client for FHIR SoR authentication |

`engine.Config.Adjudicator` (`shnsdk.Adjudicator`) is **declared for source
compatibility but no longer consumed by the engine** since the v0.39.0 payer
retirement (its breaking entry below); setting it alone does nothing. The
supported custom-adjudication paths are (1) the **standalone `shnsdk.Responder`**,
which drives the same `shnsdk.Adjudicator` interface outside the gateway (see the
SDK's PREVIEW guide §3c; `crd-order-dispatch` is not currently served), and
(2) **native-forward** to your own Da Vinci endpoint (`PAYER_DAVINCI_*`).
In-gateway injection — a `LegResponder` on `Config.Responder` — remains an
internal 0.x seam: do not depend on it (see below).

**Additive native population diagnostics (FR-G23).**
`engine.NewNativePopulator(client, url)` retains its exact two-argument function
type and behavior. `engine.NewNativePopulatorWithFailureObserver(client, url,
observer)` adds an optional immutable callback; the old constructor delegates with
nil. `engine.PopulateFailure` has only `Stage`, `Reason`, and `Status` with JSON
names `stage`, `reason`, and `status`. The app adds `version: 1` when formatting
its fixed-field output record (see the README troubleshooting table).

Callbacks run synchronously, once per upstream failure, and may run concurrently
for separate calls. They must return promptly and be safe for concurrent use.
There is no mutable setter, background delivery, payload retention, or callback
error return. A nil callback disables diagnostic delivery and formatting. Subject
and canonical refusals keep their distinct behavior and produce no upstream record.

`connectors/smartauth.IsTokenAcquisitionError(err)` recognizes only failures
marked at the bearer transport's token acquisition boundary, through wrapped
errors. Existing error text and the immediate unwrap target are preserved;
resource-request errors with similar text remain unmarked. This does not change
token cache, refresh, credentials, timeout, or retry behavior.

`connectors/smartauth.WithTokenAcquisitionObservation(ctx)` returns a derived
context and a `*TokenAcquisitionObservation` with a read-only `Failed()` method.
Use one fresh handle per `http.Client.Do`; its zero value is unmarked and it must
not be copied after use. The handle records only an actual token acquisition
failure returned by the bearer transport, never an acquisition in progress or a
timeout alone. It retains no error or payload and can be read concurrently.

Native population creates fresh evidence for every request, even with a nil
callback, and shadows any inherited caller observation. The derived context
preserves values, cancellation and deadlines. This request-local evidence keeps
the acquisition boundary available when an outer client timeout replaces the
returned error chain; resource timeouts remain transport failures. No client-wide
or global failure state is introduced.

**Breaking in this release** (payer wiring):

- `engine.New` returns `(*Gateway, error)`. It errors — rather than starting — for the two
  conditions a deployment can hit with otherwise-valid config: a `role=payer` gateway with
  no content occupant, and an unusable ingress client registration.
- A `role=payer` gateway REQUIRES `engine.Config.Responder` (from the published binary:
  `PAYER_DAVINCI_BASE_URL`). The engine no longer synthesizes an in-process payer from
  `Config.Adjudicator`; a payer with no occupant fails closed at boot rather than answering
  Da Vinci legs out of the gateway itself.
- Every role REQUIRES a `SystemOfRecord` (from the published binary: `FHIR_DATA_URL`). The
  in-process persona stub (`engine.StubHolderData`) is gone. Its Store half survives as
  `engine.NewMemStore` — the in-memory `Store` default, carrying no persona content.

**Breaking in this release** (wire behaviour — new refusal class):

- **The gateway now enforces the FR-16 / FR-27 attestation requirements at the inbound
  gate, before dispatch, and answers `403`.** This runs on all three PAS entrances — the
  payer inbound `pas-claim` and `pas-claim-update` legs, and the provider-facing Da Vinci
  ingress. No earlier gateway inspected attestations on the wire, so **every refusal in this
  class is new**: traffic a v0.38.x gateway forwarded to the occupant can now be stopped at
  the door, and the occupant never sees it.

  A `QuestionnaireResponse` item that declares itself manually entered (the DTR
  information-origin extension with `source="manual"`) and names a `Practitioner` author
  must carry a complete clinician attestation: `npi`, `text`, and `date`, each present and
  non-empty. One naming a `Patient` author must carry a complete
  `questionnaireresponse-signature`: a signature `type` code, `when`, a `who` carrying a
  non-empty `reference` OR an `identifier` with a non-empty `value` (`system` optional —
  both of FHIR R4's legal `Signature.who` forms are accepted), and `data`. Whitespace-only
  counts as empty. A system-sourced item — one with no manual-source
  marker at all — is untouched and requires no attestation. The refusal names the failing
  requirement, the item's `linkId`, and the specific field that is absent or empty.

  `Config.Adjudicator` is unaffected in shape; what changes is that a nonconformant item is
  refused before any adjudication runs, rather than being handed to it.

**Breaking in this release** (wire behaviour — a timed-out Hub leg answers `504`):

- An originating gateway whose Hub leg produces no answer within its HTTP client's
  timeout (`engine.Config.Client.Timeout`; 30 seconds in the published binary) no longer
  reports it as `502 {"error":"hub routing failed"}`. The caller receives `504` with
  `error` set to `no answer on the hub leg within 30s (hub leg timeout)` — the number
  is the client's own timeout, applied by the gateway as its own deadline on the leg;
  `hub leg timed out` with no number when the caller's request deadline ended the wait
  first — and the FHIR operation routes
  carry it as an `OperationOutcome` with issue code `timeout`. A caller that matched the
  generic `502` for a timed-out leg now sees the `504`. `LegMetric` outcomes are unchanged:
  a timed-out leg is still `unreachable`. Every other Hub-leg transport fault keeps the
  generic `502`.

**Breaking in v0.54.0** (wire behaviour — the outcome behind the Hub is reported, not `502 hub routing failed`):

- An originating gateway no longer reports every failure behind the Hub as
  `502 {"error":"hub routing failed"}`, which now means only that the Hub could not be
  reached. Instead:
  - the Hub's own refusal reads `hub refused the exchange: <reason>`: `409` for a replayed
    envelope, `502` for every other Hub refusal;
  - an Authorization Framework denial is `403 authorization denied`;
  - a recipient gateway's edge refusal is `502 the recipient's gateway refused the exchange
    (<status>)`;
  - a leg the recipient may have received or answered is a `502` that says so and asks the
    caller to check the outcome before resending.
- A timed-out Hub leg whose request had already been sent adds `; the recipient may have
  received this request: check its outcome before resending` to the `504` text.
- A payer gateway's own failure after the leg is authenticated (its system not reached,
  or reached with no usable answer; a system-of-record read that failed; a validator it
  cannot reach) is framed as its answer, so the requester reads that status and message.
  On a PAS submit or update, every refusal it makes after its payer's system answered
  adds `the payer's system received and answered this request: check its outcome before
  resending`.
- `LegMetric`: a framed counterpart failure is `answered`, not `failed`. The counterpart
  reports it, and it no longer counts toward the requester's leg errors.
- The Hub (`POST /route`) marks every error `X-SHN-Delivered: no | yes | unknown`.

**Breaking in v0.54.0** (a payer gateway's pend ledger records and never gates):

- A payer gateway no longer refuses a PAS amendment itself. The `409`s for no pend
  recorded, an authorization already decided, and another amendment in progress are gone.
  Every amendment reaches the payer's system and its answer is relayed. The ledger
  records that answer; an amendment that did not bind a local pend is noted
  `pend.amendment-unbound:<reason>`.
- A record that cannot be written after the payer answered no longer withholds the
  answer (it was `502 holder write failed`). The gateway emits `pa.local-write-failed`.
- The payer's own `409` version conflict is relayed; the gateway no longer re-sends the
  amendment once. The `retry:version-conflict` observer note is gone.
- `engine.PendBegin` and `engine.PendRePend` take the current `PendRecord` and the store's
  clock (`PendBegin(cur PendRecord, found bool, now time.Time)`,
  `PendRePend(cur PendRecord, found bool, created, now time.Time)`), so a backend applies
  `engine.PendInProgressStale`. An amendment's hold lapses after it, and a re-pend leaves a
  live hold, its time included, alone. A third-party `PendLedger` backend must pass both
  and must not refresh `LastTransition` itself after `PendRePend`.

**Breaking in v0.54.0** (a caller's `X-Correlation-Id` is a trace value, never spent):

- At the Da Vinci ingress, the caller's `X-Correlation-Id` is a trace value. Each call's
  leg is sent under a freshly minted id, or under the PAS Claim's `urn:shn:correlation`
  (the payer's key for the authorization). A value equal to one of the Claim's own
  identifiers is also kept as the leg id. Every answer still echoes the caller's value as
  `X-Correlation-Id` and adds the leg's id as `X-SHN-Leg-Id`. The gateway logs the mapping.
- Reusing an `X-Correlation-Id`, or retrying, is never refused. The Hub's replay guard
  keys the envelope (its correlation id and ciphertext hash), so only a byte-identical
  envelope is refused `409`, and every call is sealed afresh.

**Breaking in this release** (wire behaviour — a correlation id belongs to one patient):

- A payer gateway refuses a prior-authorization submission with `409` (`correlation id
  already names another patient's authorization`) before its payer's system is asked, in
  two cases: a decision EOB for another patient is filed under that correlation id, or
  another patient's authorization is still awaiting its decision under it. Such a
  submission previously reached the payer. If the gateway cannot read its store to
  decide, it answers `502 {"error":"holder read failed"}` without asking the payer (framed
  as its answer since v0.54.0; before that the requester saw `502 hub routing failed`).

**Breaking in this release** (`Store` connectors):

- `RecordEOB` and `RecordDecision` on `engine.MemStore` and `pgstore.PgStore` return
  `engine.ErrEOBSubjectMismatch` instead of replacing an EOB filed for another patient.
  A decision caught this way is not recorded. The gateway emits a
  `pend.decision-not-recorded` observer event and relays the payer's answer as sent.
- Two optional `Store` capabilities are added, and both built-in stores implement them.
  A custom `Store` gets each half of the pre-forward correlation check above by
  implementing its capability:
  - `engine.EOBOwnerLookup` covers an EOB already filed under the id.
  - `engine.PendCorrelationLookup` covers an authorization still awaiting its decision.

  Without either capability, the custom store's submissions reach the payer as before.
- `(*engine.Gateway).CertificationClientForTest`, a test helper outside the supported
  surface, is removed.

## Evolving surfaces

These surfaces are new and intentionally **not yet pinned to a stability tier**
(neither "supported" nor "internal-only" in the senses above) — they are
expected to change shape as their consumer matures:

- **Observer stream** (`OBSERVER_ADDR`, `engine.Config.Observer`, `ObserverEvent` JSON,
  `observer.Hub`): new in this release and **evolving** — field additions and event-kind
  additions may happen in minor releases. The SHN Kit's `shnkitd` daemon (`kit/relay`) is now
  this stream's first real consumer: a local desktop inspection tool that SSE-subscribes to a
  provider-role gateway child's `/events` and re-emits frames onto its own run-timeline bus,
  stamped with the active run's identity. That consumer stays payer-role-aware (a payer-role
  gateway's stream is validation-only — `kit/relay`'s package doc), and pins an exact gateway
  version like any other consumer. The surface stays **evolving**, not yet a pinned stability
  tier — it will graduate once the Kit's inspector stabilizes.

  **v0.26.0** adds the `sor.read` event kind (the gateway's `SystemOfRecord` reads, one event
  per call) — an event-kind addition, covered by the evolving-contract clause above.

  **v0.34.0** adds `POST /demo/transform` on the same observer listener (`engine.RunTransformChain`
  exported for it) — a loopback-only JSON shim over the real compat-chain machinery, not itself
  part of the SSE stream (a run through it never appears on `/events`). `shnkitd`'s
  `POST /api/bridging/exhibit` is now this endpoint's first real consumer, proxying it
  over embedded reference content so the Kit's engine exhibit provably runs "the same modules
  your live legs route through" — same binary, same manifest. Same evolving posture as the rest
  of this surface: consumers pin exact gateway versions.

  **v0.35.0** adds `GET /demo/capture/{correlationId}` on the same observer listener: a
  loopback-only read-back of this gateway's own bounded, in-memory record of one transformed
  egress leg's pre-seal before/after payload pair — never a wire exchange, never audited, and
  never checked by any conformance surface (see `docs/CONFIGURATION.md`, "Demo-only pre-seal
  edge capture"). It is populated only when the new env `SHN_DEMO_EDGE_CAPTURE`
  (`engine.Config.DemoEdgeCapture`) is set, which as of this release also requires
  `OBSERVER_ADDR` to be set — otherwise the flag is gated off at config load rather than
  capturing into a store nothing could ever read. `POST /demo/transform`'s existing 200 and
  422 response bodies also gain an additive `chain` field on this same release — the
  compatibility-chain hops the run walked (or attempted), in the same shape already published
  on observer events; every existing field on both responses is unchanged. New exported engine
  surface backing this release, each its own release-notes bullet:

  - `engine.ChainSteps(contract, from, to string) []ChainStep` — a read-only accessor reporting
    the compatibility chain `RunTransformChain` would walk, without running any step function.
  - `engine.EdgeCapture` — the pre-seal before/after payload-pair type the capture store holds
    (`CorrelationID`, `LegType`, `Contract`, `From`, `To`, `Chain`, `LossReports`, `Before`,
    `After`, `CapturedAt`).
  - `(*Gateway) EdgeCaptureFor(id string) (EdgeCapture, bool)` — the production read seam the
    capture-fetch endpoint reads through.
  - `(*Gateway) RecordEdgeCaptureForTest(e EdgeCapture)` — a test seam over the same store for
    cross-package tests that need to seed a known capture entry without driving a full leg
    through the engine.
  - `Config.DemoEdgeCapture bool` — the config field `SHN_DEMO_EDGE_CAPTURE` parses into.

  Same evolving posture as the rest of this surface: consumers pin exact gateway versions.

- **`scenariodriver`** (`Config`, `Driver`, transport methods, builders, `Cards`/`ParseCards`):
  the UC-01…08 scenario-driving package. New in this release and **evolving** — signatures and
  return shapes may change in minor releases as the SHN Kit's daemon and the live conformance
  gate exercise it further. Consumers pin exact gateway versions.

- **`GET /health`** (served by the `app` runner in front of the engine handler): the shared
  SHN health payload — `service` (the gateway's holder id), optional `version`
  (from `SHN_VERSION`), `uptimeSeconds`, a worst-check-wins `status`, and a `checks`
  array (`registrar-poller` when a registrar feed is configured; `store` when the
  durable Postgres store is configured). The payload shape is the published
  `shn-sdk/health` contract (v0.29.0) and is non-sensitive by construction —
  statuses, timestamps, counts, and coarse error classes only. **Evolving**: check
  names and the set of registered checks may change in minor releases; the JSON
  field shape follows the `shn-sdk/health` package's compatibility.

- **`fhirseed`** (`Client` and its methods, `CRPrepopLibraries`, `DemoProviderPersonasBundle`,
  `DemoLumbarLibrary`, `PutGlobalArtifact`, `ProviderDataSeedBundle`, `ConformantSeedBundle`):
  the partner/Kit FHIR seed loader, baked persona fixture, and the two downloadable seed-bundle
  getters (embedded baked artifacts). **Evolving** — the seed sequence, fixture contents, and
  bundle bytes may change in minor releases as the Kit stabilizes its seeding needs. Consumers pin
  exact gateway versions.

  **BREAKING in v0.39.0** (evolving tier — announced, not guarded): `SandboxProviderPersonasBundle`
  is renamed `DemoProviderPersonasBundle` and `SandboxLumbarLibrary` is renamed
  `DemoLumbarLibrary`. The bytes each returns are unchanged; only the names are: the old prefix
  named the retired preview-era demo world and describes nothing in the platform today. A
  consumer on the old names updates the two call sites and re-pins.

- **`LegMetric`** (`engine.Config.LegMetric func(outcome string)`, consts
  `engine.LegOutcomeRouted/Answered/Denied/Unreachable/Failed`): new in this release and
  **evolving** — a nil-safe hook that receives one outcome string per origination-leg event at
  the roundTrip choke point. Nil (the published-binary default) means no emission; the hook
  carries no payloads and is conformance-neutral (`TestLegMetric_ConformanceNeutral` — responses
  are byte-identical hook-on vs hook-off). `gateway/app` wires it to CloudWatch EMF behind the
  `METRICS_SERVICE` opt-in (see `docs/CONFIGURATION.md`). `Unreachable` means the Hub leg did not
  complete; it also covers a Hub refusal after an unverifiable recipient response envelope and
  does not prove the responder itself was unreachable. Requires `shn-sdk` ≥ v0.31.0.

- **`GET`/`POST /internal/checks` results** (evolving surface, since v0.32.0; structured
  `failure` since v0.33.0). Each result is `{id, target, ok, detail, checkedAt,
  latencyMs}` plus, on failing results only, `failure {code, hint}` with `code` drawn
  from a closed set (`unreachable`, `http-status`, `invalid-capability-statement`,
  `credential-rejected`, `not-checked`, `internal`). Additive-only intent: existing keys
  and `detail` strings are stable fallbacks; new keys may appear in 0.x minors — decode
  tolerantly, never with unknown-field rejection. See `docs/CONFIGURATION.md`
  ("Operational checks") for semantics and redaction guarantees.

**Additive setting, next release: `PAYER_DAVINCI_BACKEND_HEADERS`** — fixed request headers
for a partner system that routes on one, sent on every request to the partner's bases and never
to its token endpoint (`docs/CONFIGURATION.md`, native-forward payer mode). Unset ⇒ every request
is byte-identical to this release's; the option adds headers only, never changes a body, and a
deployment that does not set it is unaffected.

## Internal seams (not for partner use)

Everything under `engine.*` beyond the supported seams listed above — including
`engine.LegResponder`, `engine.NewNativeResponder`, `engine.Populator`, and their
helper functions and types — is gateway-internal and **unstable**: it may change
in any 0.x minor version without notice, and none of it is a published `shn-sdk`
contract. Partners should not import or depend on these directly.

To customize partner behavior, use the stable public paths instead: implement
`shnsdk.Adjudicator` and run it behind the standalone `shnsdk.Responder` (or
native-forward to your own Da Vinci endpoint) to control payer decisions, and build against
the published `shnsdk` types for wire data — never the internal `engine.*`
equivalents. Internal seams are promoted to `shnsdk` once their shape has proven
stable; until then, treat them as an implementation detail that may disappear or
change shape without notice.

## Unsupported internals

`internal/` packages and `cmd/` binaries are implementation details and may
change without notice between any versions. Do not import `internal/`
directly.

## Cross-version conformance contract

The **`shn-sdk` wire vectors** and the **SHN Participant Protocol
specification** (published with `shn-sdk`) are the conformance contract across
gateway versions. A gateway that passes the wire-vector suite is conformant
with the SHN exchange protocol regardless of the gateway version it runs.

## Optional diagnostic events (evolving)

`engine.Config.Diagnostic func(diagnostics.Event) bool` is a separate opt-in sink
for raw HTTP and participant-stage observations. `DiagnosticTraceKey` verifies
optional private ingress call attribution. `engine.WithNativeDiagnostic` adds the
same sink to native forwarding. Nil disables each hook. Sinks run synchronously,
may be called concurrently, must return promptly without blocking, and must
reserve bounded memory before copying read-only event bytes. Sink panics are
contained. The app supplies the bounded `diagnostics.Queue` publisher and owns its
shutdown in both `Run` and the closable `Handler` constructors.

`diagnostics.IngressFingerprint(ctx)` snapshots the handler-consumed ingress hash;
it never reads the body. `diagnostics.IngressBody(ctx)` exposes a synchronous read-only view of the bounded
body for existing observer adapters. `diagnostics.RequestIdentity(ctx, event)` projects the
verified envelope identity carried by `WithRequestIdentity`. A partial fingerprint
cannot establish a byte link. HTTP transport events inherit that identity through
token acquisition and native forwarding. No SDK metadata or wire field changes.

`connectors/smartauth.Config.Transport` optionally selects the authorized
operation transport beneath bearer injection; nil retains `http.DefaultTransport`.
`Config.HTTPClient` continues to select only the token-acquisition client. Keys,
token caching, caller-request cloning and failure behavior are unchanged.

The existing observer event names and completion barrier remain available.
Ingress observation now tees reads performed by the original handler, preserving
authentication ordering and read failures. The legacy ingress event fires when the original
handler finishes reading, before response commitment; unread rejected bodies are
reported on return as incomplete. Ingress events add `payloadIncomplete: true`
when request or response bytes were unread, truncated, failed, or unavailable due
to capture limits. An absent payload with this flag does not assert an empty body.
Complete events omit the flag and retain their existing JSON shape. Durable HTTP
events finalize on return. The existing observer
callback's panic behavior is retained, independently of the isolated diagnostic
sink. The SSE payload representation is unchanged; durable diagnostics carry raw
bytes directly. This source addition is not a published release or a version pin.


Relayed non-2xx responses preserve the participant's declared `Content-Type` with
its exact body bytes. The existing `application/fhir+json` fallback applies only
when that media type is absent; locally authored and empty-body refusal rules are
unchanged. This fixes a prior hardcoded media type on nonempty foreign errors.
