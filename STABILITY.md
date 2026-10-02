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

## Access lines

**New in v0.58.0.**

- Every gateway writes one `gateway: access: <json>` line for each call it
  answers on a Da Vinci ingress route or for a leg it receives from the
  network, at every conformance level, with nothing to configure. The JSON is
  `diagnostics.AccessLine`, the `ExchangeRecord` below with durations in
  milliseconds (see [docs/CONFIGURATION.md](docs/CONFIGURATION.md), "Access
  lines"). It is an **evolving** surface: fields and closed-set values may be
  added in minor releases, so decode tolerantly.
- With diagnostic collection configured, the same JSON is published as the
  `Detail` of a metadata-only event of kind `diagnostics.KindAccess`
  (`access`), which carries no body and no headers.
- A payer's gateway sends `X-Correlation-Id` with the leg's id on each
  operation it forwards to its payer's own system (not the CDS service
  listing, the connectivity probes or token requests), when the id is one
  token of letters, digits, `.`, `_` or `-`, up to 64 characters. The message
  bytes are unchanged.
- **Breaking (configuration):** `PAYER_DAVINCI_BACKEND_HEADERS` may no longer
  name `X-Correlation-Id`; a gateway configured with it refuses to boot with
  an error naming it, as for the other names the gateway sets itself. In the
  Go API, `engine.WithBackendHeaders` drops the name: the header is only ever
  the leg's id, or absent.
- **Additive setting:** `PAYER_DAVINCI_BACKEND_CORRELATION=off` (Go API:
  `engine.WithoutBackendCorrelation`) stops the header being sent, for a
  payer system that validates `X-Correlation-Id` its own way. Unset or `on`
  sends it; any other value refuses the boot.
- `engine.ReadSystemOfRecord` now returns a wrapper that notes each read on
  the exchange record; a caller that type-asserts its result to the connector
  it was given no longer gets the connector back. `engine.NoSystemOfRecord()`,
  which reads nothing, is returned as it was given.

## One record per exchange

**New in v0.58.0.** Go API only (additive); nothing a participant sends or
receives changes.

- `engine.Config.ExchangeObserved func(engine.ExchangeRecord)` receives one
  record for every call a gateway answers on a Da Vinci ingress route or for
  a leg it receives from the network, after the answer is written. That
  includes every refusal, the earliest authentication refusal among them, and
  a handler that panics. Nil (the default) records nothing.
- A record is metadata only: the leg's correlation id, the caller's own
  `X-Correlation-Id` when it differs, the pa-test door's verified call id, the
  leg, the operation, the contract line, the two holders, the outcome and who
  refused on which network rule, the status, the latency, a count of
  conformance findings, and, on the answering side, the call to the
  participant's own system (its status, latency and error class). It never
  carries a message body or a patient identifier. The only header values it
  carries are the caller's own `X-Correlation-Id`, which the ingress accepts
  only as one bounded token, the call id of a verified `X-SHN-Test-Trace`
  proof, and, from v0.61.0, the Hub's `X-SHN-Delivered` on its error answer
  to a leg (`HubDelivered`, only as `no`, `yes` or `unknown`); a leg id a message chose (a PAS Claim's own `urn:shn:correlation`) is
  recorded as `sha256:<digest>` unless it is one bounded token too.
- It also carries the sha256 of the request and answer envelopes'
  ciphertext, the value the Hub's audit records carry as `payloadBundleHash`,
  so a record joins the Hub's records for the same leg.
- From v0.61.0 it names the registered edits (`Edits`: `E-01` …) the gateway
  applied to the bytes it transmitted on the leg, in transmit order: ids only,
  each once, only for bytes sent (the receiver answered, or the connection
  failed after the request was written). Not a metric dimension.
- Direction, route, exchange, operation, outcome, refusing party, rule and
  backend error class are closed sets (`engine.ExchangeDirections`,
  `ExchangeRoutes`, `ExchangeKinds`, `ExchangeOperations`, `ExchangeOutcomes`,
  `RefusalParties`, `RefusalRules`, `BackendErrorClasses`), each ending in
  `other`: a value the gateway cannot classify is recorded as `other`, never as
  a caller's string.
- **Fixed in v0.59.0:** a payer that requires known members
  (`REQUIRE_KNOWN_MEMBERS=true`) records its `400 unknown member` refusal on
  `conformance`, the rule for a check a participant opted into. v0.58.0
  recorded it on `other`.
- A requester's gateway cannot tell a recipient gateway's refusal inside the
  answer's frame from the recipient's own system answering an error, so its
  record calls either one `upstream-error`; the answering gateway's record
  says which it was. A refusal the recipient's gateway gave at its edge, which
  the Hub reports, is `refused` by `payer-gateway`.
- On the answering side, a refusal of this gateway's own that names no rule,
  given because its participant's system did not answer usably (a service
  listing it could not read, or an error answer a requester without a message
  frame cannot be sent), is recorded as `upstream-error`, with the failed call;
  so is a read or search of its system of record that failed. Every read and
  search of the participant's system of record on a leg it answers is a
  backend call (status 0, with its latency), and so is a read of its CDS
  service listing. `Backend` is the call the answer came from: the operation
  forwarded to the participant's system, or, where none was, the last read; a
  read after the operation (a check of its answer) is counted but does not
  replace it. An answer is `malformed` only when this gateway cannot read it;
  a readable answer a check found fault with keeps no class. At `none`, where
  no conformance check runs, an answer is `malformed` only when the gateway
  itself could not read it: one repeating a member name (the network's own
  rule), the CDS service listing, a system-of-record read the connector
  reports invalid (or fails without saying why), a search page, or a
  facility's records out of shape (another patient's record, no Patient, a
  record without an id or the same record twice). A call cut short because
  the request it served ended has error class `cancelled`, and one that ran
  out of time `timeout`, whatever the participant's connector reported; a
  call or read abandoned while the request was still live (a connector
  cancelling its own read, a client its own attempt) is the system not
  answering. A forwarded operation that runs past the responder's own
  deadline (`engine.WithBackendDeadline`, which the gateway sets from
  `PAYER_DAVINCI_BACKEND_TIMEOUT`) is the participant's system's `timeout`,
  and its exchange that system's upstream error. The exchange of a cut-short call is
  never an upstream error: a refusal it caused, the operation's or a later
  read's, is `other`. A system-of-record read that a
  check at `strict` needed and could not make refuses as that check's
  `conformance` refusal; the read is counted in `BackendCalls`, and, after
  the operation, is not the `Backend`.
- `limit` is reserved; from v0.58.0 the gateway records an Authorization
  Framework denial as `authority`, whatever its reason; a facility's `consent`
  refusals (a federated query without a consent reference, or one the consent
  service does not permit) are `consent`. A facility's or a PHG's gateway
  refusing a leg is refused by `other`.

## Another patient inside a PAS request (v0.61.0)

- **Behavior change in v0.61.0, at `strict` (`patient.mixed`):** a PAS
  `$submit`, amendment or `$inquire` is read for another patient everywhere in
  it (the Bundle's own elements, every entry, every contained resource and
  every element nested in them), not only in its entries' own `patient`,
  `subject` and `beneficiary`:
  - a `Patient` entry is the request's patient by its id or the member
    identifier; a `fullUrl` naming the member does not make it so;
  - a contained `Patient` is the request's patient by the member identifier
    (its id is local);
  - a `Patient`, entry or contained, carrying another member's member
    identifier is another patient, whatever else it carries, unless it is a
    Coverage's party (below), whose own identity is not checked;
  - a literal reference names the patient whose `Patient/<id>` it names,
    compared without any `/_history/<version>`, under any base;
  - a reference whose identifier is in the member system (`urn:shn:member`)
    names that member, in any element, whatever its `type` and beside a
    literal reference or not;
  - any other reference that names a patient by identifier alone, in a
    subject element (`patient`, `subject`, `beneficiary`, `for`,
    `subjectReference`, `patientReference`) or with `type` `Patient`, must
    carry an identifier of the request's patient: the member identifier or
    one its `Patient` entry carries;
  - a Coverage's party (the parent a dependent's Coverage, an entry of the
    Bundle, names as a contained `Patient` through its `subscriber` or
    `policyHolder`; a Coverage carried inside another resource has none) is not
    itself read as the request's patient, but what it references is; the
    slot's own reference, and an identifier beside it that the party
    carries, name the party;
  - the parent carried any other way is another patient: a `subscriber` or
    `policyHolder` naming the parent as a literal `Patient/<parent>`, and a
    `Patient` entry of its own for the parent, even one the member's
    Coverage names. The PAS IG allows the subscriber as a Bundle entry, so
    such a request is refused at `strict` though it is conformant; carry
    the parent contained in the Coverage instead.

  Such a request is refused `403 inconsistent patient in PAS bundle`
  (`403 inconsistent patient in PAS inquiry` for an inquiry) at `strict`, by
  the provider's gateway before it is routed and by the payer's gateway before
  your system sees it; recorded and carried at `observe` and `structural`; not
  checked at `none`. Before, a request whose Coverage contained a resource
  naming another member (a `RelatedPerson`, a `Patient`, or a reference by
  identifier) was carried at every level.
- **Unchanged:** an order, Coverage, QuestionnaireResponse or
  DiagnosticReport entry naming another patient is refused as before, and a
  contained `RelatedPerson` whose `patient` is the request's patient is
  carried.

## A dependent's Coverage passes the patient check (v0.61.0)

- **Behavior change in v0.61.0:** the check that a Coverage is the request's
  patient's accepts a dependent's Coverage that names the parent, its
  subscriber or policyHolder, as a contained `Patient`, whatever identifiers
  that Patient carries (an MRN, or the parent's own member identifier). Such a
  Patient is a party to the coverage, part of the Coverage record itself: it
  is carried byte for byte with the Coverage and never binds or routes
  anything; the Coverage's `beneficiary` still binds it to the request's
  patient. It is accepted only when the slot's own reference (`subscriber`,
  `policyHolder` or both) names it, its `id` is one no other resource in the
  Coverage's `contained` list has, nothing else in the Coverage references it
  (not the beneficiary, the payor, an extension, even one inside the slot, or
  another contained resource), it holds as a contained resource (it contains
  nothing, and its `identifier`, if present, is a list), and every member the
  rule reads is spelled exactly (no `Reference` in the slot, no
  `ResourceType`, `Id`, `Contained` or `Identifier` in the party, no
  `ResourceType`, `Subscriber`, `PolicyHolder` or `Contained` beside the
  Coverage's own). The gateway decides it by shn-sdk's
  `CoverageParty`. Any other contained Patient is checked as the
  request's patient, as before. A gateway
  that derives the identity of a member its system of record does not hold
  never reads it (the member's own carried Patient is read, as before), and
  the patients a request names for the Hub's involved list never include it.
  Both read a contained Patient that is no party (one whose `id` another
  contained resource shares included) as before, so the rule can only take
  patients off the involved list, never add any. Before,
  every contained Patient had to be the request's patient, so such a Coverage
  was refused.
- It applies wherever a Coverage is checked: the coverage read through the
  request's `fhirServer` (routed, and carried as `prefetch.coverage`; before,
  `412 no coverage to route by: fhirServer's answer is not a Coverage
  searchset`), the system of record's Coverage read (routed, and under
  `ENRICH_NATIVE_REQUESTS` carried when that system names the patient by the
  member id; before, `502 system of record returned another patient's
  resource`), a `prefetch.coverage` the EHR sent (before,
  refused `403` at `strict`), and the `$questionnaire-package` coverage, sent or
  obtained.
- **Behavior change in v0.61.0:** the same party passes the check that a PAS
  answer names one patient (rule `patient.answer`), on a `Claim/$submit` answer
  (an amendment's included) and a `Claim/$inquire` answer, at the payer's
  gateway and at the provider's. A Coverage entry of the answer that names the
  parent this way carries it as part of the Coverage: the parent is not read as
  a second patient, and the slot naming it (`subscriber`, `policyHolder`, typed
  `Patient` or not) is not read as a reference to the patient. The party is
  decided by the same rule (`CoverageParty`, as the shn-sdk PAS response check
  decides it), and only a Coverage that is itself an entry of the answer has
  one. The check a
  provider's gateway applies to the PAS request it completes from its system
  of record (`ORIGINATION_PROFILE=provider-data`) reads the party the same
  way. Before, such an answer was refused at `strict` (`403` at
  the payer's gateway, `502` at the provider's, both `PAS response has
  inconsistent patient linkage` on `Claim/$submit` and `PAS inquiry answer has
  inconsistent patient linkage` on `Claim/$inquire`), and below `strict` relayed
  unread, recorded as `patient.answer` at `observe` and `structural`, with no
  pend, decision or ExplanationOfBenefit written from it. Now it is read and
  relayed at every level, and written from as any readable answer is.
- **Unchanged:** a Coverage whose beneficiary names another patient (`502 …
  another patient's coverage` on the `fhirServer` read); a contained
  `RelatedPerson`, bound by its own `patient` reference as before; another
  Patient carried as a standalone entry; and a contained Patient referenced
  from anywhere but those two slots, all refused as before. The check a
  requester applies to a facility's disclosed records accepts no such party. In
  a PAS answer, a contained Patient no slot names, a party also named elsewhere
  (an extension, even one inside the slot, the beneficiary, the payor), a
  contained Patient whose `id` another contained resource shares, one that
  contains anything, whose `identifier` is not a list or that spells a member
  the rule reads in another case, a parent carried as its own entry, and a
  Patient contained in a Coverage that is not itself an entry are another
  patient, as before. On a `Claim/$submit` answer, one whose `id` another
  contained resource shares, or that contains anything, is refused first by
  the answer's graph check (`answer.shape`).

## A versioned Patient reference names the patient it versions (v0.61.0)

- **Behavior change in v0.61.0, at every level:** a versioned reference,
  `Patient/<id>/_history/<version>` (relative, or absolute under any base),
  names the patient `<id>`, as the same reference without the version does.
  Only a trailing `/_history/<version>` is a version: a base whose path
  contains `/_history/` is part of the reference.
  - A PAS `$submit`, amendment or `$inquire` is bound to the member its
    `Claim.patient` names, versioned or not. The order's, Coverage's,
    QuestionnaireResponse's and DiagnosticReport's subjects (and, on an
    inquiry, every `patient`, `subject`, `beneficiary` and `for`) are compared
    with it without the version.
  - The patients a prior-authorization request names for the Hub's involved
    list are read the same way. Another member named by a versioned
    reference is named, and a versioned reference to the request's own
    patient never names that patient a second time.

  Before, the version was read as part of the member id: a versioned
  `Claim.patient` named no member your system of record holds, and a
  versioned reference to the request's own patient was another patient to
  the PAS patient check. Another member named by a versioned reference was
  left off the involved list, or named under an identity derived from the
  versioned id. The `$questionnaire-package` request already read a versioned
  reference this way.
- The member is bound by its id. The version is not compared with the
  `Patient` entry the request carries.
- **Unchanged:** the CRD legs' own patient check (`patient.mixed`) compares an
  order's subject and a Coverage's beneficiary with the hook's `patientId` as
  written, version included. A versioned reference to the hook's own patient
  is another patient there: refused at `strict`, recorded at `observe` and
  `structural`.

## A payor that names no payer, and a urn payor reference (v0.61.0)

- **Behavior change in v0.61.0:** on the CDS Hooks and
  `Questionnaire/$questionnaire-package` ingresses, a Coverage whose payor
  reference resolved (among the request's resources, from the system of
  record, or through `fhirServer`) to a resource that names no payer
  identifier is refused with the reason after `no payer identifier on member
  coverage: `, as a PAS Bundle's is: `the payor Organization carries no
  identifier with both a system and a value, such as a NAIC code or payer id`,
  or `Coverage.payor references a resource that is not an Organization`. In
  v0.60.0 both were the bare `422 no payer identifier on member coverage`.
- A coverage the EHR sent whose payor is a `urn:uuid:` or `urn:oid:` reference
  that nothing resolves is refused `422 no payer identifier on member
  coverage: Coverage.payor is a urn reference no Bundle entry's fullUrl
  matches; send the payor Organization as a Bundle entry whose fullUrl is that
  reference, or a payor identifier`; the payor Organization as an entry, with
  that `fullUrl`, of a Bundle the request carries routes it. In v0.60.0 it
  was the bare refusal.
- The message keeps its prefix, so a matcher on it still matches. A payor of
  another kind that nothing resolves (a `RelatedPerson`, a `Patient`) keeps
  the bare refusal.

## A request fingerprint is whole when its body is not kept (v0.61.0)

- **Behavior change in v0.61.0:** `RequestFingerprint.Complete` means the
  fingerprint's hash saw every byte of the request body. It no longer also
  requires the body to have been kept. Before, an exchange the capture budget
  had no room for (`diagnostics.NewCaptureBudget`'s concurrent exchanges, all
  in use) recorded a whole hash but `Complete: false`, so
  `diagnostics.FingerprintLink` could not match it to the other end of the
  same forward. A fingerprint whose hash missed bytes (the handler stopped
  reading, the read failed, the body was cut off) is still incomplete.
  `Event.BodyComplete` is unchanged: it still says whether the body itself
  was kept.
- It applies wherever a fingerprint is recorded: on an observed request and
  its answer, on a forwarded request (`diagnostics.ObserveTransport`), and in
  `diagnostics.IngressFingerprint`, which the gateway stamps on its
  `leg.sealed` and `leg.failed` records. The gateway observes its own ingress
  with the package's default budget (8 concurrent exchanges a process), so a
  gateway past that many concurrent requests is the one this changes.

## Where a leg's time went, and why its answer was not authorized (v0.61.0)

- **Additive:** the access line of a leg from the network carries `stages`
  (`diagnostics.AccessStages`): the milliseconds spent unwrapping the leg,
  reading the participant's own system, forwarding the operation to it,
  validating synchronously, sealing and authorizing the answer, in the pend
  ledger, and writing the answer. Before, it carried the total and one
  `backend` call, the forward replacing the read before it. The stages do not
  overlap and sum to at most `latencyMs`. No metric dimension changes.
- **Additive:** when the answer to a leg (the participant's system's, or the
  gateway's own refusal) cannot be authorized, the access line's
  `answerError` names why
  (`cancelled`, `timeout`, `auth`, `unreachable`, `other`;
  `engine.AnswerErrorClasses`), and the gateway logs one line naming the
  class and the leg's id. Before, nothing said why. The answer to the Hub is
  unchanged: `502 authorization failed`.
- **Additive:** a conformance finding from a check that called the validator
  carries `validatorMs`, the time its `$validate` calls took, every line tried
  together, and each entry of its `lines` carries `ms`, that line's own call's
  time. Each is rounded down to whole milliseconds on its own and left out
  when that is 0, so the lines' `ms` need not add up to `validatorMs`. At
  `observe` they are the only record of a deferred check's time.
  Likewise a `crd.embedded.validated` observation carries `ms`, its
  `$validate` call's time.
- **Go API (additive):** `engine.ExchangeRecord.Stages` (`engine.LegStages`)
  and `engine.ExchangeRecord.AnswerError`, `engine.AnswerErrorClasses`;
  `engine.ConformanceFinding.ValidatorMs`, `engine.LineVerdictSummary.Ms` (a
  `LineVerdictSummary` literal without field names no longer compiles: name
  its fields);
  `diagnostics.AccessLine.Stages` (`diagnostics.AccessStages`) and
  `diagnostics.AccessLine.AnswerError`.

## The coverage read through the request's fhirServer is carried (v0.61.0)

- **Behavior change in v0.61.0 (on by default; not an enrichment opt-in):** when
  a provider's gateway routes a CDS Hooks request by the coverage it read
  through the request's own `fhirServer` (the request carries no
  `prefetch.coverage` key, the system of record names no patient for the
  member, and `CDS_FHIR_SERVER_READ` is not `off`), it carries that coverage as
  `prefetch.coverage`, creating `prefetch` when the request has none. This is a
  new registered edit, E-07 (`cds-callback-coverage-carry`), on the
  `crd-order-dispatch` and `crd-order-select` requests a provider's gateway
  carries. It compensates for E-01: with `fhirServer` and `fhirAuthorization`
  removed, the payer cannot read that coverage itself, and a payer that needs
  it (the Da Vinci reference payer answers `400` without one) can now decide.
  In v0.60.0 nothing either read returned was carried.
- The value is a `searchset` the gateway writes (as for the system-of-record
  fill, E-02): a `match` entry for each Coverage routing chose (the active ones,
  else all of them), in the server's order, and an `include` entry for the
  payor Organization routing resolved for each of them (by routing's own rule:
  the first payor, read only when it carries no identifier of its own), whether
  the server's search returned it (resolved by `fullUrl` or
  `Organization/<id>`) or the gateway read it
  (`GET {fhirServer}/Organization/<id>`); a second payor, or an Organization
  routing did not resolve, is never included. Each resource is the server's
  bytes exactly; each `fullUrl` is a `urn:uuid:` the gateway assigns, except an
  included Organization that a carried Coverage names by an absolute reference
  on the request's `fhirServer` base (the base compared with the scheme and
  host lowercased, the default port dropped and a trailing slash trimmed):
  its `fullUrl` is that reference, exactly as the Coverage writes it and
  already in the Coverage's own bytes, so the reference resolves in the
  `searchset`. Each included record has at most one such reference: a search
  entry answers an absolute reference only when its `fullUrl` is that
  reference, and the Organization read only the reference written on the
  `fhirServer` base itself, so two chosen Coverages writing an Organization's
  address differently each resolve their own record, and both records are
  carried, each with the reference that resolved it. A relative reference
  resolves by type and id either way. `total` is
  the number of Coverages. The records are byte for byte, so any reference a
  record holds (an absolute one on the server included) is carried as written.
  Nothing else of the server's answer is carried: no link, no entry address of
  the server's, no Bundle `id` or `meta`, no
  `OperationOutcome`, no Coverage routing did not choose and no Organization
  only such a Coverage names. What is carried is checked against the request's
  patient again (another patient's record refuses the request `502` at every
  level, as the read already does).
- The request is built once it is routed: a request refused while routing (an
  ambiguous coverage, no payer identifier, an unregistered payer, a refused
  Organization read) carries nothing and is not sent, as before. The rest of the
  request is the EHR's bytes, with `fhirServer` and `fhirAuthorization` removed.
- Unchanged: a `coverage` prefetch key the EHR sends, even `null`, is never
  changed or replaced; with `CDS_FHIR_SERVER_READ=off`, or no `fhirServer`,
  nothing is read or carried and the request is refused `412` as in v0.60.0; a
  coverage read from the system of record is carried only under
  `ENRICH_NATIVE_REQUESTS=true` (E-02); the payor Organization read for a
  coverage the EHR sent is never carried.
- **Payers:** a CDS Hooks request may now arrive with a gateway-written coverage
  `searchset`, recognisable by its `urn:uuid:` entries, where v0.60.0 sent none.
  A payer gateway that maps its payer identity maps it like any other coverage.
  An EHR server whose entry addresses use another base than the `fhirServer`
  URL it hands out may see a payer answer `400` (or `422` from a payer gateway
  that maps its identity) for a Coverage whose absolute payor reference is on
  that other base, where v0.60.0 carried no coverage: that Organization is
  carried under a `urn:uuid:`, which the reference does not resolve.
- **Go API:** `relay.EditCDSCoverageCarry` (`E-07`) is new, and the ownership
  table admits it on the two CRD request transmits a provider's gateway carries.
  `engine.WithFHIRServerTrustForTest` is test support (it panics outside a test
  binary); it is not a stable API.
- **Additive (records and diagnostics):** the registered edits a gateway applies
  to the bytes it transmits on a leg are now named, E-07 among them. The access
  line gains `relayEdits` (`diagnostics.AccessLine.RelayEdits`) and the exchange
  record `engine.ExchangeRecord.Edits`: the edit ids (`E-01` …), in transmit
  order, each once, only ids the registry holds, and only for bytes sent (a
  provider's request to the network, a payer's forward to its own system):
  the receiver answered, with anything (a refusal or an unreadable answer
  included), or the connection failed after the request was written. A request
  never sent names none: refused before it was sent (by routing or by the
  Authorization Framework), a connection that could not be made, or a bearer
  token that could not be obtained. Ids only, never a value an edit removed
  or wrote. A line whose leg carried its message exactly has no `relayEdits` key,
  so it is the v0.60.0 line byte for byte. With diagnostic collection configured,
  the `leg.sealed` event of a provider's request and the `native.request` event
  of a payer's forward carry the ids the request was built with as their `Detail`
  (`diagnostics.RelayEditsDetail`, `{"relayEdits":[…]}`), captured before the
  send: a `leg.sealed` once the request is sealed, before the Authorization
  Framework is asked, and a `native.request` just before the forward. So the
  event can name edits the record does not, for a request then never sent; a
  transmit with no edit has no `Detail`, as before. `engine.RelayEditName` and
  `engine.RelayEditReceivedBy` read the registry: an edit's registered name, and
  the party whose received bytes it changes (a requester's request edit reaches
  the recipient; the payer identity mapping, E-03, reaches no other party). An
  edit made inside a request a gateway builds itself (the supplemental report's
  subject, E-06) is not named: that request is the gateway's own, not a relayed
  one.

## A patient the system of record names by another id, under the opt-in (v0.61.0)

- **Behavior change in v0.61.0 (`ENRICH_NATIVE_REQUESTS=true` only):** when
  the provider's system of record names the patient by an id other than the
  request's member id (`context.patientId` on CDS Hooks, the patient the
  coverage and orders name on `$questionnaire-package`), a CDS Hooks request
  that carries its `patient` but no `prefetch.coverage`, and a
  `$questionnaire-package` request that carries no `coverage` parameter, are
  routed as without the opt-in: by the coverage read from the system of record
  under its own Patient id only to choose the payer (every Coverage, routed on
  the active ones, else the others), fenced to that id alone, so a Coverage it
  returns naming `Patient/<member id>` (another patient there) is refused `502`
  at every level. Nothing is added: no `prefetch.coverage` (E-02), no
  `coverage` parameter (E-04) and, as before, no `referenced` Patient (E-05);
  a value from that system would name the patient by an id the request does
  not use. In v0.60.0 both requests were refused `422` (`system of record names
  the patient differently from context.patientId; supply patient and coverage
  prefetch in the request` on CDS Hooks, `system of record names the patient
  differently from the request; supply the coverage parameter in the request`
  on `$questionnaire-package`), where the same request without the opt-in was
  routed. The opt-in never leaves a member with less to route by than the
  default; a prefetch value it cannot fill is still decided by `strict`, below.
- A CDS Hooks request under the opt-in that leaves out `patient` is still
  refused `422` at `strict` before anything is read (the patient cannot be
  supplied under the request's id) and, below `strict`, sent without it, as in
  v0.60.0; a coverage it also leaves out is then read to route by, as above.
  The refusal's text is now `system of record names the patient differently
  from context.patientId; supply the patient prefetch in the request`; a client
  matching the text up to the semicolon keeps matching. History keys are left
  out with the `not-run` reason `patient named differently in the system of
  record`, unchanged.
- The `$questionnaire-package` refusal `system of record names the patient
  differently from the request; supply the coverage parameter in the request`
  is gone: no request is refused with it.

## A body the capture budget left out says so (v0.61.0)

- **Additive (diagnostics):** an observed exchange's event (`diagnostics.ObserveHTTP`,
  `diagnostics.ObserveTransport`) now says when the capture budget, not the
  body cap, kept its body from being captured whole. Its `Detail` is
  `diagnostics.BodyNotKeptCaptureBudget` (`body not kept: capture budget`)
  when the exchange found no free capture session, so neither its headers nor
  its body were kept, and `diagnostics.BodyPartialCaptureBudget`
  (`body capture partial: capture budget`) when the budget's bytes ran out
  part way, so a prefix was kept. `BodyComplete` is false in both cases, as
  before. A body cut by the body cap, or captured whole, carries no such
  detail. Only a body's loss is named: headers cut short by the budget show
  only as `HeadersComplete` false. An event whose body the queue's retained-body
  limit later cuts keeps the budget's detail rather than `body capture
  partial: retention cap reached`.
- **Go API (additive):** `diagnostics.BodyNotKeptCaptureBudget` and
  `diagnostics.BodyPartialCaptureBudget`.

## The Hub's word on whether a leg was delivered (v0.61.0)

- **Additive:** the access line of a call from a participant's own system
  whose leg the Hub answered with an error carries `hubDelivered`: the value
  of the Hub's `X-SHN-Delivered` header on that answer
  (`engine.HubDeliveredHeader`), which says whether the recipient's gateway
  received the leg:
  - `no`: the Hub refused before forwarding, or the recipient's gateway was
    not reached or refused the leg at its edge;
  - `yes`: the recipient's gateway answered, and its answer was lost on the
    way back (the Hub could not read, verify, decode or audit it);
  - `unknown`: the recipient's gateway may have received it (it answered
    5xx, the connection failed after the request was sent, or it did not
    answer in time).
- The key is absent when the leg drew no error answer from the Hub's route
  handler: for example, the leg was answered, it never reached the Hub, no
  answer came back from the Hub (a timeout, or a connection that failed after
  the request was sent), or the error came from in front of the Hub (no
  `X-SHN-Delivered`, or a value other than these three). The gateway never infers it. When a call makes more than one leg,
  it is the last leg's. A leg from the network never carries it.
- Unchanged: what the caller is answered, the line's `outcome` and
  `refusal`, and the metrics (it is not a metric dimension).
- **Go API (additive):** `engine.ExchangeRecord.HubDelivered` and
  `diagnostics.AccessLine.HubDelivered` (JSON `hubDelivered`, omitted when
  empty).

## Observe checks do not hold the message (v0.60.0)

- **Behavior change in v0.60.0:** at `observe` (the default level), a payload
  check that can only record (`$validate` of a request or answer, and the CRD
  answer's embedded resources) is queued and does not hold the message; its
  finding is written when the validator answers. The answer's bytes and status
  are unchanged. The queue is bounded: a check that finds it full is dropped and
  logged (`gateway: observe check dropped`). `structural` and `strict` still wait
  for every check, because they refuse on it, and a payload this gateway
  translated between IG lines is checked before the answer at every level.
- A payer gateway's checks of the decision ExplanationOfBenefits it builds still
  run before the answer, because whether each decision is written depends on
  them. At `observe` all of one exchange's decision checks share one fixed
  2-second budget; a check the budget does not reach is recorded as unavailable
  and its decision written. At `structural` and `strict` they are not bounded.
- `Gateway.Close` flushes the queue, waiting at most 10 seconds before it cancels
  the validator calls still running (each is recorded as unavailable).
  `WaitObserverCompletion` waits for the checks an operation queued.
- **Access lines:** at `observe`, `findings` carries `"deferred": true` with no
  count (`count` 0, `kinds` left out); `refused` is still carried.
  `AccessFindings.Deferred` and `FindingSummary.Deferred` are new fields; nothing
  is removed. Read a call's findings from its `conformance:` lines.
- **Diagnostic collection:** each finding is also captured as a
  `diagnostics.KindConformanceFinding` event (metadata only, bound to its call
  like the gateway's other events), and at `observe` each exchange is closed by
  one `diagnostics.KindConformanceResult` event with its finding count
  (`truncated` past `diagnostics.FindingEventsPerLeg`, 32, findings captured on
  their own; `incomplete` when a check's finding was not recorded, because it was
  dropped or failed while running).
- `Gateway.WaitObserveChecksForTest` is test support for harnesses that read
  findings; it is not a stable API.

## A coverage read through the request's fhirServer (v0.60.0)

- **Behavior change in v0.60.0 (on by default):** a CDS Hooks request that
  carries no `prefetch.coverage`, for a member the provider's system of record
  does not hold, and that names `fhirServer`, has its coverage read through that
  server, only to choose the payer, instead of being refused. One
  `GET {fhirServer}/Coverage?patient={context.patientId}` (no status filter, no
  `_include`; one page), with the request's `fhirAuthorization` token as a
  Bearer credential when it carries one. It routes on the active Coverages when
  any is active, otherwise on the others when they name one payer (two payers
  among the chosen Coverages are `422 ambiguous coverage for routing`). A
  Coverage whose `status` cannot be read (not a string) counts as not active,
  as on the system-of-record path. When a
  chosen Coverage names its payor only as
  `Organization/<id>` on that server and the answer does not resolve it, one
  more read, `GET {fhirServer}/Organization/<id>`, with the same token and
  checks: at most two reads a request. Before, the gateway never called
  `fhirServer`.
- `CDS_FHIR_SERVER_READ` takes `private` (the default; unset is the same),
  `public` or `off`. `private` reads an `https` server in the participant's own
  network or on the internet: any port at a private address, port 443 at a
  public one, and never an address no mode reads (loopback, link-local with the
  cloud metadata and credential endpoints, `fd00:ec2::254`, `fd00:ec2::23`, `fd20:ce::254`, `100.100.100.200`, unspecified,
  multicast, broadcast, `0.0.0.0/8`, `240.0.0.0/4`, `::/96`, a zoned address,
  or a NAT64 or 6to4 address embedding one). `public` reads only an `https`
  server on port 443 at a public address. `off` is the opt-out: the gateway
  never calls `fhirServer`, and a request with no coverage to route by is
  refused `412` (v0.59.0 answered `422`). Any other value refuses
  to boot, and so does a value other than `off` set without
  `PROVIDER_DAVINCI_INGRESS`; unset needs no ingress. A gateway SHN hosts runs
  `public` (or `off`, if its tenant sets that).
- Nothing either read returns is carried: the request is sent as the EHR sent
  it, with `fhirServer` and `fhirAuthorization` still removed (E-01, unchanged).
  From v0.61.0 the coverage read is carried (E-07; see that section).
- The read is fenced in every mode: an `https` base URL with no query, fragment
  or credentials; the mode's address rules on an address literal, on every
  resolved address and again on the address connected to; no redirects, no
  proxy, verified TLS (1.2 or later); 2 s to connect, 2 s for TLS, one 4 s
  budget for both reads, at most 512 KiB an answer; a Coverage `searchset`
  about the request's patient, and the Organization asked for. Each refusal
  begins `no coverage to route by: ` and names its reason. See
  CONFIGURATION.md, "Reading the coverage through `fhirServer`".
- **Behavior change in v0.60.0: a CRD request the provider gateway cannot
  obtain a coverage to route by for is answered `412`** (CDS Hooks: the service
  could not obtain the data it needs), on the CDS Hooks ingress only. That is
  `no coverage in request or system of record` (a `null` coverage prefetch, or
  none the system of record holds; `422` before), the request for a member the
  system of record does not hold (`422 patient not found in system of record`
  before; now `412 no coverage to route by: send prefetch.coverage or
  fhirServer (this gateway's system of record names no patient for this
  member)` when it names no `fhirServer`, and `412 no coverage to route by: send
  prefetch.coverage (this gateway's system of record names no patient for this
  member, and it does not read fhirServer)` under `off`), and every refusal of
  the read, including no Coverage for the patient and no Organization for the payor
  (`404` or `410`). A Coverage naming another patient and an answer that is
  not the payor Organization are `502` at every enforcement level;
  routing ambiguity stays `422` (two payor Organizations by reference alone,
  `ambiguous coverage for routing`, a payer id no route registers, `no payer
  identifier on member coverage`, which a payor reference to another server, a
  versioned one, or an Organization with no payer identifier also gets); a
  malformed request keeps its `400` or `422`; DTR and PAS never answer a `412`
  of their own. The exchange record records each `412` as the provider
  gateway's refusal, rule `routing`.
- **Changed text in v0.60.0:** a DTR `$questionnaire-package` request for a
  member the system of record does not hold, with no `coverage` parameter, is
  still refused `422`, now with `no coverage to route by: send the coverage
  parameter (this gateway's system of record names no patient for this
  member)`, where it was `patient not found in system of record`. A client
  matching the text exactly should match the new text; `patient not found in
  system of record` is still answered under `ENRICH_NATIVE_REQUESTS=true` at
  `strict` when the system of record names the member's Patient but returns no
  such Patient.
- Each read is recorded as its own `prefetch.obtained` event with key
  `coverage` and `source` `fhirServer` (its path and query, never the host);
  every other `prefetch.obtained` event keeps `source` `system-of-record`.

## A CRD request with no Coverage at a payer that maps its identity (v0.60.0)

- **Behavior change in v0.60.0:** at a payer gateway running payer backend
  identity mapping (`PAYER_DAVINCI_PAYOR_OWN` with `PAYER_DAVINCI_PAYOR_BACKEND`),
  a CDS Hooks request (`order-select`, `order-sign`, `order-dispatch`) that
  carries no Coverage at all has no payor to map: it is forwarded to the
  payer's own system exactly as it arrived, and that system answers it. Such a
  request has a `prefetch`, or `prefetch.coverage`, that is absent or `null`,
  or a `prefetch.coverage` Bundle whose `entry` is absent, `null`, or holds
  only entries whose `resource` is an `OperationOutcome`; and no
  `"resourceType": "Coverage"` object anywhere else in the request (another
  prefetch member, `context`, a nested Bundle, a contained resource), the
  `resourceType` member matched without regard to case in its name and its
  value, and a `prefetch`, `coverage` or `entry` member named in another case
  counting as present. v0.59.0
  and earlier refused it `400 payer backend identity mapping: inbound Coverage
  carries no resolvable payor identifier (…)`. A provider gateway sends such a
  request when it routes by a Coverage it read only to choose the payer
  (above), and carries nothing it read (from v0.61.0, a coverage read through
  `fhirServer` is carried, E-07).
- **Unchanged:** a Coverage whose payor cannot be read, a `prefetch` or
  `prefetch.coverage` that is present but not a JSON object, a coverage Bundle
  with any other entry, and a request whose only Coverage is outside
  `prefetch.coverage` are refused with that `400`; a Coverage naming another
  payer with `400 … does not match …`; Coverages naming two payers and an
  unresolved payor reference with `422`; a Coverage naming the payer is
  re-stamped as before. The payer's own level still decides: an `order-select`
  or `order-sign` request whose coverage beneficiary cannot be read is the
  request's own shape, refused at `structural` and `strict` and carried below.
  A DTR request with no coverage parameter was already forwarded as it arrived;
  a PAS Bundle with no Coverage is still refused.

## An Authorization Framework that does not answer (v0.60.0)

- **Behavior change in v0.60.0:** when the Authorization Framework gives a
  request leg no answer (the connection to `POST {authz}/authorize` is refused,
  reset or closed, or fails some other way before any response; no answer
  within the client timeout; or a `503` or `504`, which the Framework never
  answers itself, from a proxy or load balancer in front of it), the
  originating gateway answers its own `503`, uncacheable:
  `{"error":"the authorization service could not be reached, so this leg was not sent to the payer"}`
  (an `OperationOutcome` with issue code `transient` on the FHIR operation
  routes). Before, it was `502 {"error":"authorization failed"}`. The leg is
  not sent to the Hub. The exchange record's outcome is `unreachable`, with no
  refusing party, and `LegMetric` counts the leg `unreachable`, not `failed`.
- The gateway makes the call once more, with a fresh holder assertion (the
  Framework accepts each assertion once), only when the first attempt failed
  with a refused, reset or closed connection before its request was written.
  After the write the Framework may already have decided and recorded its
  decision, so that call is not repeated.
- **Unchanged:** a `403` from the Framework is `403 authorization denied`
  (`LegMetric` `denied`); any other status from it, or an answer the gateway
  cannot read, is `502 authorization failed` (`failed`). Neither is retried.
  A payer gateway signing its answer's leg gets the same single retry; a
  failure there is still its `502 authorization failed` to the Hub.

## The PAS payor reference (v0.60.0)

- **Behavior change in v0.60.0:** the Da Vinci ingress (`Claim/$submit`,
  `Claim/$inquire`) resolves the first Coverage's `payor` reference among the
  Bundle's entries. An absolute reference (a URL, or a `urn:uuid`) names the
  entry whose `fullUrl` equals it, as FHIR resolves it in a Bundle, and a
  relative `Organization/<id>` names the entry with that type and id,
  whatever its `fullUrl`. Before, only the relative form resolved, so a Bundle
  referencing its payor Organization by `fullUrl` was refused
  `422 no payer identifier on member coverage`; it now routes. A reference
  several entries answer routes when every one is an Organization naming the
  same payer identifier (a Bundle that repeats its payor, as before); when
  they name different payers, or one names none, it is refused, where before
  the first one was taken when it named a payer. A reference no entry
  answers is still refused:
  the gateway never reads the payor from outside the Bundle. A payer
  gateway's identity mapping matches payor and insurer references the same
  way, and still refuses a reference several entries answer, as before.
- The `no payer identifier on member coverage` refusal on these routes now
  adds, after `: `, which part failed, when the gateway can tell (an
  unreadable body or Coverage, or a payor Organization that carries an
  identifier with both a system and a value but cannot otherwise be read, such
  as one with a non-string `id`, keeps the bare text). It echoes nothing the
  request carried.
  A client matching the text exactly should match its prefix. The reasons (the
  first is seen on `$inquire`: a `$submit` Bundle with no Coverage is refused
  `400 PAS bundle missing Coverage.beneficiary` before routing):
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

## The CRD and DTR payor reference (v0.60.0)

- **Behavior change in v0.60.0:** the CDS Hooks and
  `Questionnaire/$questionnaire-package` ingresses resolve every Coverage
  `payor` reference among the resources the request carries (every prefetch
  value, or every resource parameter, and the entries of each that is a
  Bundle) by the PAS rule: an absolute reference (a URL, or a `urn:uuid`)
  names the Bundle entry whose `fullUrl` equals it, and a relative
  `Organization/<id>` names the resource with that type and id. Before, an
  absolute reference resolved only to an entry of the coverage value itself
  (a Bundle of Coverages carrying its payor), so one naming another prefetch
  value's or parameter's entry was refused `422 no payer identifier on member
  coverage` (or, for a coverage read from the system of record or through
  `fhirServer`, looked up there); it now routes on the request's own
  Organization.
- A reference several of those resources answer routes when every one is an
  Organization naming the same payer identifier (the same Organization under
  two prefetch keys, as before). When they name different payers, or one
  names none, the request is refused at every level with `422 no payer
  identifier on member coverage: Coverage.payor matches more than one
  resource of the request, and they do not name one payer`, and the payor is
  never read from the system of record or `fhirServer` instead. Before, the
  first one was taken: the coverage value's own entry, then the first in key
  (CRD) or parameter (DTR) order.
- A CDS Hooks coverage value is read by its exact member names: a value whose
  `resourceType`, `entry` or an entry's `resource` (or that resource's
  `resourceType`) is written in another case holds no Coverage to route by,
  and neither does a value that is neither a Coverage nor a Bundle. Such a
  request is refused `422 no payer identifier on member coverage` (at
  `strict`, a `resourceType` or an entry's `resource` written in another case
  is refused first, `403`). Before, these
  members were read in any case, and a value with a `payor` but no
  `resourceType` was routed.
- For a coverage the system of record supplied, the records its Coverage
  search returned are resolved among together with the request's resources,
  and a disagreement among them is refused with `… Coverage.payor matches more
  than one resource of the request and the system of record's Coverage search,
  and they do not name one payer`. Several Coverages a DTR search returns are
  checked for one payer the same way, refused as before with `422 ambiguous
  coverage for routing: …` when they do not name one.
- A DTR request with no coverage, routed by the system of record's Coverage
  without `ENRICH_NATIVE_REQUESTS`, resolves that Coverage's payor reference
  among the request's resources and the records the search included, then in
  the system of record, as it does under the opt-in. Before, it was looked up
  only among the request's own resources and refused `422 no payer identifier
  on member coverage`. A reference nothing answers is resolved as before.
- A client matching these refusals' text exactly should match its prefix.

## CDS Hooks prefetch templates (v0.60.0)

- **Behavior change in v0.60.0:** the CRD discovery's prefetch templates
  (`GET /cds-services`, every service) are the Da Vinci reference payer's
  order-sign templates: `coverage` is
  `Coverage?patient={{context.patientId}}&status=active`, `serviceHistory`
  `ServiceRequest?patient={{context.patientId}}&status=active,completed`,
  `deviceHistory`
  `DeviceRequest?patient={{context.patientId}}&status=active,on-hold,completed`,
  `medicationHistory`
  `MedicationRequest?patient={{context.patientId}}&status=active,completed`
  and `questionnaireResponses`
  `QuestionnaireResponse?patient={{context.patientId}}&status=completed`;
  `patient` is unchanged (`Patient/{{context.patientId}}`). Before, each named
  the patient `Patient/{{context.patientId}}`, had no status filter, and the
  coverage and device templates asked for `_include=Coverage:payor` and
  `_include=DeviceRequest:performer`. CDS Hooks 2.0 does not list `_include`
  among the query features a client supports, so an EHR that left out a key it
  could not fulfil can now send it. What the payer receives from an EHR that
  fulfils the templates is only its active coverage and its active or completed
  orders (active, on hold or completed device orders; completed
  questionnaire responses), without the payor Organization or the device
  performer unless the EHR adds them.
- The gateway's own search of its participant's system of record for the same
  values (the fill under `ENRICH_NATIVE_REQUESTS=true`, including the
  `$questionnaire-package` Coverage it appends, and the history values of a CRD
  request it originates) uses the same status filter, read from the same
  table, and keeps its includes: a value it reads is carried to the payer,
  which cannot fetch the payor Organization or a device order's performer
  itself once `fhirServer` is removed. The query is
  `<type>?patient=Patient/<id>&status=<codes>` (with
  `&_include=Coverage:payor` or `&_include=DeviceRequest:performer`). A filled
  value is what the system of record answers that search, so a server that
  applies the filter no longer returns a record the template excludes (a
  cancelled coverage, a draft or cancelled order, an in-progress questionnaire
  response); the gateway does not filter the answer again by status. A member
  with no active Coverage has `null` filled coverage (and no `coverage`
  parameter is appended to a `$questionnaire-package` request); the request
  is still routed, by the routing read below, as without the opt-in. So is a
  request whose system of record cannot answer the filtered search: the
  filled coverage is left out and the routing read chooses the payer.
- The coverage the gateway chooses a payer by is the one exception on the
  filter, the same everywhere: a CDS Hooks or `$questionnaire-package` request
  that carries none (read only to route by, not added; under the opt-in, read
  when the filled search finds no Coverage or cannot be answered, and
  otherwise the filled search routed by the same rule), a CRD request the
  gateway originates (its own request, nothing filled into a partner's
  message) and the inquiry about a pended prior authorization it submitted. It reads every Coverage
  (`Coverage?patient=Patient/<id>&_include=Coverage:payor`) and routes on the
  active ones when any is active, so a stale cancelled coverage naming another
  payer no longer makes routing ambiguous (before, every Coverage counted, and
  two payers was a `422`); when none is active, on the others, provided they
  name one payer (`422 ambiguous coverage for routing` otherwise), so the
  payer of a coverage no longer in force is still the one that answers. An
  originated request carries the Coverages it is routed by, with their payors
  (a payor only a Coverage not chosen names is left out), and its
  `$questionnaire-package` and PAS legs, and an inquiry about a pended PAS
  submission, name the same coverage (before, every Coverage counted for both,
  so a stale one at another payer refused them `422 ambiguous coverage for
  routing`, and a cancelled one listed first was the one they named).
- A coverage the EHR itself sent that names its payor Organization by
  reference alone, which the request does not resolve (what an EHR fulfilling
  the coverage template exactly sends), has that Organization read only to
  choose the payer; nothing is added to or changed in what is carried.
  `Organization/<id>` is an id on the EHR's server, so a CDS Hooks request
  reads it where that server is: when the request names no `fhirServer`, or
  one at the system of record's own FHIR base (compared with the scheme and
  host lowercased, the default port dropped and a trailing slash trimmed),
  from the system of record when that system names the patient by
  `context.patientId`, otherwise (or when that system holds no such
  Organization) once through that `fhirServer`; when the request's
  `fhirServer` is at another base, only once through it, never from the
  system of record. The read through `fhirServer` uses its
  `fhirAuthorization` token, within the fhirServer read's checks and budget,
  and not with `CDS_FHIR_SERVER_READ=off`. It is recorded as a
  `prefetch.obtained` event with key `coverage`, `source` `fhirServer` and
  `reason` `payor Organization of the coverage the EHR sent` (followed by the
  read's reason when refused), and logged as that read. Its refusals keep the
  fhirServer read's statuses (`412` for none there or a refused token, `502`
  for an answer that is not the Organization), with the read's reason after
  `no payer identifier on member coverage: ` instead of `no coverage to route
  by: `, since the request carried a coverage (for example `412 no payer
  identifier on member coverage: fhirServer holds no Organization for the
  coverage's payor`). A failed read of the Organization in the system of
  record is the system-of-record failure (`503 system of record
  unavailable`, or `502` for an answer it cannot read), and `fhirServer` is
  then not read. A `$questionnaire-package`
  request, which names no `fhirServer`, reads it only from the system of
  record, when that system names the patient by the request's member id; a
  system that cannot name the patient counts as one that does not hold it,
  so that request is refused `422` with the reason and remedy below, as a CDS Hooks
  request naming no `fhirServer` is. A reference written on another server,
  versioned, with a fragment, a leading slash or a dot segment is never read.
  Before, such a request was
  refused `422 no payer identifier on member coverage`, except a
  `$questionnaire-package` request the gateway edited (under
  `ENRICH_NATIVE_REQUESTS`, its Patient appended), whose payor reference,
  written any way, was read from the system of record: a reference written
  any way but `Organization/<id>` (versioned, with a fragment or a query,
  for example) is now refused there as everywhere (the default refused it
  already). It still is refused when
  nothing can read the payor, now in the form of the PAS refusals above:
  the prefix, `: ` and the reason, and only for a payor nothing could
  resolve (not the request, the system of record or the `fhirServer` read)
  the remedy after the reason. The message keeps its prefix, so a matcher on
  it still matches. The remedy is added only for a reference to an
  Organization, and is the one that resolves that reference; an unresolved
  payor of another kind (a RelatedPerson, a Patient) keeps the bare refusal,
  and so, in v0.60.0, does a resolved payor Organization that carries no
  payer identifier (v0.61.0 gives its reason, above). Every other refusal of
  the payor read is one of these texts:
  - `no payer identifier on member coverage: Coverage.payor is a reference to an Organization the gateway could not read; send the payor Organization with the coverage, or a payor identifier`
    (`422`; CDS Hooks and `$questionnaire-package`; a reference written
    `Organization/<id>`, which the Organization sent with the coverage
    resolves)
  - `no payer identifier on member coverage: Coverage.payor is a reference to an Organization the gateway could not resolve; send the payor Organization as a Bundle entry whose fullUrl is that reference, or a payor identifier`
    (`422`; CDS Hooks and `$questionnaire-package`; an absolute reference
    that a Bundle entry's `fullUrl` can be, with no `/_history/`, query,
    fragment or dot segment, which an entry with that `fullUrl` of a Bundle
    the request carries resolves)
  - `no payer identifier on member coverage: Coverage.payor is a reference to an Organization the gateway does not read; send a payor identifier with the coverage`
    (`422`; CDS Hooks and `$questionnaire-package`; any other reference to an
    Organization: versioned, with a fragment, a leading slash or a dot
    segment)
  - `no payer identifier on member coverage: the request's coverages name more than one payor Organization by reference alone`
    (`422`; CDS Hooks, two different Organizations by reference alone left
    for the `fhirServer` read)
  - on CDS Hooks, a refused `fhirServer` read of the payor Organization, `412`
    except the last, which is `502`:
    - `no payer identifier on member coverage: fhirServer is not an absolute base URL (no query or fragment)`
    - `no payer identifier on member coverage: fhirServer must be https`
    - `no payer identifier on member coverage: fhirServer must not carry credentials`
    - `no payer identifier on member coverage: fhirAuthorization is not a bearer token`
    - `no payer identifier on member coverage: fhirServer's address is one a gateway never reads (loopback, link-local, metadata or reserved)`
    - `no payer identifier on member coverage: fhirServer at a public address must use port 443`
    - `no payer identifier on member coverage: fhirServer must use port 443`
    - `no payer identifier on member coverage: fhirServer's address is not public`
    - `no payer identifier on member coverage: fhirServer's host did not resolve`
    - `no payer identifier on member coverage: fhirServer redirected; redirects are not followed`
    - `no payer identifier on member coverage: fhirServer's TLS could not be verified`
    - `no payer identifier on member coverage: fhirServer could not be reached`
    - `no payer identifier on member coverage: fhirServer did not answer in time`
    - `no payer identifier on member coverage: fhirServer's answer exceeds 512 KiB`
    - `no payer identifier on member coverage: fhirServer refused the fhirAuthorization token`
    - `no payer identifier on member coverage: fhirServer answered with an error`
    - `no payer identifier on member coverage: fhirServer holds no Organization for the coverage's payor`
    - `no payer identifier on member coverage: fhirServer's answer is not the payor Organization`
  In v0.60.0 a resolved payor Organization that carries no payer identifier
  keeps the bare `422 no payer identifier on member coverage`; v0.61.0 gives
  its reason (above).
- Under the earlier templates `_include=Coverage:payor` brought the
  Organization with the coverage, so such a request was routed with no read;
  an EHR that fulfils the templates exactly is now refused when neither the
  system of record nor the `fhirServer` read resolves the payor. A payor
  identifier avoids that, and so does the payor Organization sent with the
  coverage where its reference resolves: beside it for `Organization/<id>`,
  or as an entry of a Bundle the request carries whose `fullUrl` is the
  absolute reference.
- **Unchanged:** the gateway's search for a gateway-originated order (a draft
  order), the Coverage read for eligibility (UC-01) and on the payer side, and
  a facility's clinical data search are not narrowed by status.
- **Breaking (Go API):** `engine.SearchDateRange.AnyOf` is a slice, so
  `SearchDateRange` is no longer comparable: code that compares two values
  with `==` or keys a map by one does not build. Compare the fields instead,
  `AnyOf` with `slices.Equal`. A narrowing with
  `AnyOf` set is a token filter (`Param=a,b`, any of the codes) that
  `engine.SoRSearchQuery` writes after the patient and any date range and
  before the include; a code holding a character a query or token list gives a
  meaning to is refused (`SearchMalformed`). A connector that sends
  `engine.SoRSearchQuery`'s query (the built-in FHIR connector does) needs no
  change; one that builds its own query must apply it.
- **Go API (additive):** `engine.SystemOfRecordFHIRBase`, an optional
  connector interface: `FHIRBase()` names the FHIR base the connector reads,
  compared with a CDS Hooks request's `fhirServer` as above (`fhirsor.SoR`
  implements it with its configured base). A connector that does not implement it is never at the
  same base: a sent payor is then read in its system of record only for a
  request naming no `fhirServer`.

## The payer system's deadline (v0.59.0)

- **Behavior change in v0.59.0:** a payer gateway waits for its participant's
  system at most `PAYER_DAVINCI_BACKEND_TIMEOUT` (25 s by default), counted
  from the leg's arrival at the gateway, on each operation it forwards: under
  the requester's 30 s leg budget. A system slower than that is answered to
  the requester as the gateway's own framed `504` (the system's timeout; the
  message says whether the request was sent and may have been acted on),
  where before the requester's own gateway's hub-leg timeout ran out first.
  The access line records the call as `timeout` and the exchange as
  `upstream-error`, so it counts in `BackendError`. The gateway logs one line
  for it:

  `gateway: upstream payer <leg label> call timed out after <deadline>, this gateway's deadline for its system: answered 504 (host <host>, leg <leg>, correlation <id>, request written: yes|no)`

  If the gateway's own work before the call used the whole deadline, it does
  not send the operation to its system. The access line records the exchange
  as `other` and no call of the forwarded operation; if a read ran before it,
  such as the member's lookup or the CDS service listing, the last one is
  still its `backend`. The gateway answers its own `504` saying the payer's
  system did not receive the request.

- **Upgrade effect:** a payer system that answers between the deadline (25 s)
  and the requester's leg budget (30 s) could get its answer through before;
  from v0.59.0 it gets the gateway's `504`. For a system that needs longer,
  raise `PAYER_DAVINCI_BACKEND_TIMEOUT`, up to `28s`.
- **Additive setting:** `PAYER_DAVINCI_BACKEND_TIMEOUT`, a duration from `1s`
  to `28s`; any other value refuses the boot. Go API:
  `engine.WithBackendDeadline(d)`; without it the responder sets no deadline
  of its own, as before.

## A payer's declaration follows its own system (v0.59.0)

- **Behavior change in v0.59.0:** a payer gateway that forwards to its own
  system (`PAYER_DAVINCI_BASE_URL`), with that system's versions declared
  (`PAYER_DAVINCI_CONTRACT_VERSIONS`) and `SHN_CONTRACT_VERSIONS` unset, no
  longer declares the build default. It declares its system's `pa.crd`,
  `pa.dtr` and `pa.pas` lines, plus `pa.pdex@2.1`, which it answers itself,
  and logs:

  `gateway: declaring <set>, derived from PAYER_DAVINCI_CONTRACT_VERSIONS (SHN_CONTRACT_VERSIONS is unset); the registry entry peers select this gateway against must declare the same set`

  Before, it declared the build default and refused (FR-G48) every leg whose
  contract shares no line with its system, so a system on another line was
  unreachable. An explicit `SHN_CONTRACT_VERSIONS` still wins.
- **Upgrade effect:** such a payer's declared set can change. Its registry
  entry must declare the new set (CONFIGURATION.md, "Opting a line in",
  step 3), and a derived `2.1` or `2.2` line needs its validator lane
  (`FHIR_VALIDATE_URL_<line>`), exactly as a declared one does.
- **Breaking (configuration):** these now refuse to boot, naming what to set:
  - a payer forwarding to its own system whose declaration, set or derived,
    has no `pa.crd`, `pa.dtr` or `pa.pas` line (for example
    `SHN_CONTRACT_VERSIONS=pa.pdex@2.1`);
  - with `SHN_CONTRACT_VERSIONS` unset, a `PAYER_DAVINCI_CONTRACT_VERSIONS`
    that names no `pa.crd`, `pa.dtr` or `pa.pas` line, or one this build
    cannot exchange;
  - with `SHN_CONTRACT_VERSIONS` unset, a derived line with no validator lane.
- **Go API (additive):** `engine.DeriveDeclaredContractVersions(system)` (the
  set such a payer declares) and `engine.RequireForwardedContractLine(declared)`
  (the refusal above).

## Native PAS response contract

PAS operation responses must be complete Bundles containing exactly one
ClaimResponse and a closed set of referenced resources. **Every decision a payer
gives is relayed as the payer gave it**: the gateway does not poll its own payer
for a later decision, does not replace a payer's answer with one it assembled,
and stamps no contract line of its own on a message it did not produce. A payer
that pends answers `pended`, and the requester obtains the determination by
asking for it (`Claim/$inquire`, leg type `pas-claim-inquire`).

**Breaking in v0.46.0** (the assembly path is gone):

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

## A payer that keeps no system of record

**Additive in v0.58.0.**

- A payer whose own system answers every exchange (`ROLE=payer` with
  `PAYER_DAVINCI_BASE_URL`) may leave `FHIR_DATA_URL` unset. Earlier releases refused
  to boot without it on every role; every other gateway still does. A deployment that
  boots on v0.57.0 boots unchanged.
- Such a gateway binds each member from the request that names it, exactly as it binds
  a member a system of record does not hold, and files its records under that binding.
  It answers coverage eligibility only through `PAYER_ELIGIBILITY_URL`; without it an
  eligibility request is answered `501` "coverage eligibility is not offered by this
  payer", in the `{"error": …}` shape of every refusal the payer's gateway makes itself.
  `REQUIRE_KNOWN_MEMBERS=true` without a system of record refuses to boot. Patient Access
  returns no explanations of benefit on such a gateway: they are filed under its binding
  of each member, never under the identifier a patient-access token names, as for any
  member a system of record does not hold.
- **Go API (additive):** `engine.NoSystemOfRecord()` (a `SystemOfRecord` that holds no
  one; pass it as `Config.SoR`) and `engine.ErrNoSystemOfRecord` (the failure of a read
  it cannot answer).

## Origination profiles

**Behavior change in v0.57.0.**

- `ORIGINATION_PROFILE` takes `demo` or `provider-data`; unset means `demo` for
  `ROLE=provider`. Any other value now refuses to boot, on every role, with
  `gateway: invalid ORIGINATION_PROFILE "<value>" (must be demo|provider-data, or unset)`.
  The value is matched exactly, so `Demo` or ` provider-data` refuses too.
- Earlier releases accepted an unknown value, and a provider's gateway then built
  the requests it originates on no known lane: its PAS request carried a payer
  Organization that no system of record supplied. A deployment that boots on
  v0.56.0 with an accepted value boots unchanged.
- **Go API:** `engine.New` returns an error for a non-empty
  `Config.OriginationProfile` that is not one of `engine.OriginationProfiles()`, on
  every role, matched exactly:
  `gateway: Config.OriginationProfile "<value>" is not an origination profile (must be demo|provider-data)`.
- **Go API:** a provider built with an empty `Config.OriginationProfile` still
  serves its Da Vinci ingress and relays, but refuses to originate. Every route that
  originates (`POST /scenario/uc01` through `uc08`, `uc02-payerb`,
  `uc02-unknownpayer`, `uc07hcpcs`, `homeoxygen`, `dispatch`, `uc06/start`,
  `uc06/complete`, `uc07/start`, `uc07/complete` and `pa/inquire`) answers `503`,
  `Cache-Control: no-store`, with
  `{"error":"origination profile not set: set ORIGINATION_PROFILE (demo|provider-data)"}`,
  before it reads, builds or sends anything. The routes that only touch the
  gateway's own pended state (`uc06/cancel`, `uc07/cancel`, `uc07/pending`,
  `reset`) serve as before. Earlier releases originated on no known lane instead. The
  published binary never builds such a provider: it sets `demo` for an unset
  `ORIGINATION_PROFILE`. Payer, facility and PHG gateways originate nothing and may
  leave it empty.
- A prior authorization a provider pended while it had a lane cannot be followed up
  after it is rebuilt with an empty profile, even when its `Store` keeps the
  continuation: `pa/inquire`, `uc06/complete` and `uc07/complete` refuse as above.
  A parked UC-06 or UC-07 can still be cancelled, and the participant's own system
  can still inquire through `POST /Claim/$inquire` on the ingress.
- **Go API (additive):** `engine.OriginationProfiles` (the accepted values; it
  returns a copy).

## Conformance enforcement levels

`CONFORMANCE_ENFORCEMENT` takes `none`, `observe` (the default when unset),
`structural` or `strict` (see [docs/CONFIGURATION.md](docs/CONFIGURATION.md)).

**Behavior change in v0.55.0: an unknown code system is recorded at `structural`.**

- A coding whose code system the validator does not know
  (`Terminology_TX_System_Unknown`) is now recorded at `structural`, like any
  other code system the validator cannot check, when nothing else in the message
  refuses at that level. Earlier releases refused it at `structural`.
- The validator reports this issue as an error only for a system in the HL7 FHIR
  namespace. That includes a misspelled HL7 system URL, which the validator
  cannot tell apart from one it has not loaded, so it too is recorded.
- `strict` still refuses it, and `observe` still records it.
- The validator image built from v0.55.0's or a later `deploy/validator`
  resolves the `version-algorithm` code system on the 2.0 and 2.1 lines, which
  earlier images reported as an unknown code system. An image built before
  v0.55.0 still reports it, and `strict` refuses it; rebuild the image with the
  gateway.
- In v0.55.0, apart from this and the provider's check of a payer's answers
  (below), the four levels behave as in v0.54.0. v0.56.0 adds the check of a
  CDS Hooks answer on the legs a provider originates (below).

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
  unchanged in v0.53.0.
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

## The supplemental report on an amendment the gateway builds

**Behavior change in v0.57.0.**

- The `fhirsor` connector's `SupplementalReportContext` returns the report
  exactly as the FHIR server holds it, its subject included. Earlier releases
  replaced the report's `subject` with `{"reference":"Patient/<member id>"}`
  in the connector, whatever patient it named.
- When the gateway attaches that report to a PAS amendment it builds (UC-04),
  the only change it makes to the report's content is registered edit E-06
  (`evidence-subject-rekey`): `subject.reference` is re-pointed from the
  Patient your system holds the member under to `Patient/<member id>`, and
  only when the report names that Patient.
- The amendment is a bundle the gateway builds with the SDK's PAS update
  builder, which, as before, gives the report its bundle-local id, drops its
  `meta.profile` and re-encodes it (member order and whitespace) as it places
  it in the bundle.
- The amendment is refused, and no amendment is sent, when the report has no
  `subject.reference` (`422 supplemental report names no subject.reference`);
  names any other subject (`422 supplemental report's subject is not the
  member's patient in the system of record`); carries a signature that
  covers the report (a `Signature` in the report itself, outside any resource
  it contains, or a signed `Provenance` it contains that targets it: `422
  signed content cannot be edited (E-06, <carrier>)`); or cannot be read as a
  resource (`502 supplemental report is not a resource`). Earlier releases
  rewrote the subject whatever the report named.
- **Breaking for a custom connector** that copied the earlier `fhirsor`
  behavior, rewriting the subject to `Patient/<member id>` while your system
  holds the patient under another id: that report no longer names the
  Patient your system holds, so the amendment is refused. Return the report
  as your system holds it. A connector whose system holds the patient under
  the member id, and returns the report naming `Patient/<member id>`, is
  unaffected.
- No configuration changes. Go API: `relay.EditEvidenceSubjectRekey` (`E-06`)
  is added; nothing is removed.

## Refusals of a request frame are framed

**Behavior change in v0.57.0.**

- A refusal the gateway writes about a request frame after the leg is
  authenticated and decrypted, before any leg handler runs, is framed as its
  answer with `200` to the Hub, like a leg handler's refusal. The refusals are:
  a contract line it cannot build (`422`); one it has no validator lane for
  (`422`); a claim on a version-neutral leg (`422`); a frame that does not
  decode (`400 request frame decode failed`); and an operation header on a leg
  that defines none (`400 operation header is not defined for this transaction
  type`).
- A frame-capable requester's gateway now relays the status and reason to its
  participant. Earlier releases wrote these bare, and the Hub reported its
  failed forward. A requester that negotiated no frame is unaffected.
- The no-validator-lane reason no longer ends in the requirement identifiers
  `(FR-36/FR-G29)`; it now reaches your participant, so it names only the
  line and the refusal.
- No configuration or Go API changes.

## The payer's media type on a relayed answer

**Behavior change in v0.57.0.**

- A payer's gateway frames a success answer it relays from its payer's system
  with the media type that system stated, instead of always
  `application/fhir+json`, on every leg: a CDS Hooks answer, a questionnaire
  package, a PAS answer to a claim, an amendment or an inquiry, and the
  eligibility answer of a payer that declares its own endpoint. An answer the
  gateway built, or one that states no type, is still framed
  `application/fhir+json`.
- The provider's CRD ingress writes the payer's stated type to the EHR (the
  reference payer answers `text/json;charset=UTF-8`) instead of always
  `application/json`. A CDS Hooks answer is never a FHIR resource, so one
  framed `application/fhir+json` (as every payer gateway before v0.57.0 frames
  it), stating no type, or stating one that does not parse as a media type is
  still written as `application/json`.
- The provider's DTR and PAS ingress still write `application/fhir+json` to
  the EHR, the type of the FHIR resources they answer with, whatever type the
  frame carries.
- No configuration or Go API changes.

## An empty application error body

**Behavior change in v0.57.0.**

- When a participant's system answers a leg non-2xx with an empty body, the
  payer's gateway relays it as it came: the requester's gateway receives the
  status, an empty body, and the media type the system stated (or none), and
  writes the same to its participant. Earlier releases wrote
  `{"error":"<leg>: recipient answered <status> with no error detail"}` in its
  place, as `application/json`.
- A non-empty answer is unchanged: its bytes and media type are relayed, and
  one that states no media type is still read as `application/fhir+json`.
- The gateway's own refusals (no answer from its system to relay) keep their
  `{"error": …}` body, which names the leg and status when there is no reason
  to give.
- A requester that negotiated no frame still sees the Hub's failed forward. The
  bare answer the Hub discards is the gateway's own refusal,
  `{"error":"<leg>: recipient answered <status>; its answer is carried only in
  a message frame"}`, never the participant's bytes: a bare answer reaches the
  Hub unsealed. Earlier releases wrote a gateway `{"error": …}` body there
  too, with other wording.
- No configuration changes. The interim relay builder
  `defect-empty-error-substitution` is retired, so no frame may carry the
  gateway's own body in place of a participant's application error. Its
  exported constant `relay.BuilderInterimEmptyErrorSubstitution` remains,
  deprecated, so code naming it still compiles; `relay.Authored` refuses it.

## Amendments a provider's gateway builds, after a payer's 409

**Behavior change in v0.56.0.**

- When a provider's gateway builds a PAS amendment (`pas-claim-update`) for its
  participant (the scenario flows, and resuming a pended request with a
  clinician's or the patient's answers) and the payer answers `409` (a version
  conflict: its store refused the amendment while it resolved the same claim,
  and kept nothing), the gateway builds and sends the amendment once more under
  a new correlation id, and relays the payer's answer to that one. A second
  `409` is relayed as it came.
- Each attempt is its own leg, with its own `leg.originated`/`leg.response`
  pair and its own Hub records. The new `leg.resent` observer event
  (`engine.LegResentEvent`) links them: its correlation id is the re-send's, and
  its `Detail` names the refused attempt's (`refusedCorrelationId`).
- An amendment the participant sends through the Da Vinci ingress is relayed as
  before: its `409` reaches the participant, who decides whether to resend. A
  payer's gateway relays its payer's `409` and never resends (v0.54.0, below).
- No configuration changes. The Go API adds `engine.LegResentEvent`, the new
  event's kind. An observer consumer that switches on `Kind` sees one new kind.

## CDS Hooks answers on the legs a provider originates

**Behavior change in v0.56.0.**

- A provider's gateway checks the payer's CDS Hooks answer on a CRD leg it
  originates itself (`order-sign`, `order-select` and `order-dispatch`) against
  the CDS Hooks response rules, at its own conformance level, before it reads
  the payer's coverage information from it. It already did so for an answer it
  relays to its participant's own system through the Da Vinci ingress, and a
  payer's gateway does so for its own system's answer.
  - `none`: no check (a repeated member name, which is message integrity, is
    still refused at every level).
  - `observe`: each broken rule is recorded as a finding, and the exchange
    continues.
  - `structural`: a broken structure is refused (`502 payer CRD response is not
    a valid CDS Hooks response: <rule>`); a card's summary length, CRD topic
    and selection behavior, and an action's resource, are recorded.
  - `strict`: any broken required rule is refused.
- There is no exception for the network's reference payers: their gateways
  already pass their own answers at `strict`. (The reference-payer exception
  below applies only to their DTR and PAS answers.)
- **Compatibility.** A provider gateway at `structural` or `strict` paired with
  a payer whose CDS Hooks answers break a required rule, and whose own gateway
  does not refuse them (it runs `none` or `observe`), now refuses those answers
  on the legs it originates, where earlier releases read and acted on them.
  Some payers' CRD answers today omit a member CDS Hooks requires (a system
  action's `description`, or `cards` itself), so a provider at `structural` or
  `strict` paired with such a payer sees `502` on these legs. This includes a
  SHN Kit provider set to `structural` or `strict`, once the Kit pins v0.56.0 or
  later; the Kit's default is the gateway's own, `observe`. At `observe`, the
  hosted and Kit default, only new findings appear.
- No configuration or Go API changes. An answer relayed through the Da Vinci
  ingress is checked once, as before.

## A payer's DTR and PAS answers on the legs a provider originates

**Behavior change in v0.55.0.**

- A provider's gateway builds and sends some legs itself, on its
  `ORIGINATION_PROFILE` lane (`demo`, which is what unset means for
  `ROLE=provider`, or `provider-data`). It now checks the payer's answer on
  these legs at its own conformance level: the DTR `$questionnaire-package`
  answer, the DTR `$next-question` answer, and the PAS ClaimResponse answer to a
  submit or update, including on a resumed exchange. Earlier releases checked
  no payer's answers on these lanes. From v0.56.0 the payer's CDS Hooks answer
  on the CRD legs it originates is checked too (above).
- **Not checked:** `$inquire` answers, and the answers a gateway relays to its
  participant's own system through the Da Vinci ingress (the native PAS and
  questionnaire relays), as before.
- **Reference payers.** An answer is not checked only when both of these hold:
  the gateway is on the `demo` or `provider-data` lane, and the payer identity
  the member's Coverage names, which the leg was routed by, is one of SHN's
  reference payers: `urn:oid:2.16.840.1.113883.6.300` `00001`, `00300` or
  `00301`, or one of SHN's two bridging-demo payers under `urn:shn:demo-payer`
  (`SHN-BRIDGE-DEMO`, `SHN-BRIDGE-REFUSE`), which front `00001`. Their packages
  do not yet conform: the 2.0 reference payer's DTR package fails DTR 2.0.1. The
  set changes only in a gateway release, never through configuration.
- **Lines tried, in order.** Each answer is checked against the IG lines the
  gateway can validate:
  1. the line the leg was sent at: the payer's declared line, or the gateway's
     own line when the payer declared none;
  2. the lines the answer claims, first through a versioned `meta.profile`, then
     through a line-specific structural marker;
  3. the rest of 2.2, 2.1 and 2.0.

  Each line is tried at most once, and only where the gateway has a validator
  for it. A validator that serves several lines is called only once, so a
  gateway with one validator for every line judges the answer once, at the line
  the leg was sent at. The check stops at the first line where the answer is
  valid, and a further line is tried only when the line before it was not
  valid. The line the leg was sent at uses the validator client's own timeout;
  every other line gets at most 2 seconds, and one that does not answer in time
  counts as unavailable for that line. So when the gateway has no validator for
  the line the leg was sent at, the first line it actually checks gets the
  2-second bound.
- **The verdict.** An answer that is valid on any line is valid. If no line is
  valid, the check is unavailable when the line the leg was sent at, or a line
  the answer claims, could not be checked (its validator did not answer, or the
  gateway has none for it), and also when no line's validator answered.
  Otherwise the verdict is the best one any line gave; a deeper-rule defect
  beats a structural one.
- **By level:**
  - `none`: no validator call and no finding.
  - `observe`: every outcome that is not valid is recorded, and the answer is
    relayed.
  - `structural`: a structural or unclassified defect is refused; a deeper-rule
    defect and an unavailable check are recorded; the answer is otherwise
    relayed.
  - `strict`: every defect is refused, and an unavailable check is refused with
    `500`.
- **A refusal.** The gateway's own client receives `422 {"error":"ingress
  validation failed: <issues>"}`, with the issues from the line that decided. An
  unavailable check at `strict` answers `500` (`validator unavailable`, or a
  message naming the line that has no validator lane). The payer has already
  received the request and answered it.
- **Findings.** An answer valid at the line the leg was sent at records nothing.
  Any other checked answer records one conformance finding, except an
  unavailable check at `strict`, which is refused without one. The finding
  carries `declaredLine` (the line the leg was sent at), `lines` (each line
  considered, in order, with its verdict: `valid`, `structural`, `deeper` or
  `unavailable`; the line the leg was sent at, or a line the answer claims, that
  has no validator is listed as `unavailable`, and no validator diagnostic
  appears here) and `line` (the line the decision came from). An answer valid
  only on a line after the one the leg was sent at gets one finding with
  `verdict` `valid` and `decision` `record`; this is not a defect. When the
  gateway has no validator for any of the lines, the answer is judged once, as
  before v0.55.0, and its finding carries no `declaredLine` or `lines`.
- **A known limit.** An answer valid on any supported line is relayed at every
  level. For example, an answer sent at 2.2 that is structurally invalid at 2.2
  but valid at 2.0 is relayed at `strict`. A payer's declared line may be the
  gateway's default rather than the payer's own claim, and the network cannot
  yet tell the two apart.
- **Compatibility.** At the default level (`observe`) the check records findings
  and refuses nothing; the validator calls add time. At `structural` a
  structurally invalid answer is refused (`422`), and at `strict` any defect is
  refused (`422`), as is an answer the validator cannot check (`500`). The check
  is the provider's gateway's own and does not depend on the payer's release. A
  provider's gateway before v0.55.0 on the `demo` or `provider-data` lane checks
  no payer's answers.
- No configuration changes. See also
  [docs/INTEGRATION.md](docs/INTEGRATION.md#conformance-enforcement).
- **Go API (additive):** `engine.ReferencePayerIdentities` (the identities above;
  it returns a copy), `engine.ConformanceFinding.DeclaredLine`,
  `engine.ConformanceFinding.Lines` and `engine.LineVerdictSummary`.
  `engine.ConformanceFinding.Verdict` can now be `valid`, on a payer-answer
  finding that is not a defect; the signatures are unchanged.

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

## Networks without default validator lanes

**New in v0.55.0.**

- `FHIR_DEFAULT_VALIDATOR_LANES` accepts unset or `none`; any other value
  refuses to start (see [docs/CONFIGURATION.md](docs/CONFIGURATION.md)).
  - Unset keeps the earlier behavior: the gateway probes the Compose default
    validator names (`shn-validator-2-1`, `shn-validator-2-2`) for a line with no
    address of its own.
  - `none` states that this gateway's network has no such services. The gateway
    creates no default lane and never probes their names.
- With `none`, a CRD, DTR or PAS line other than 2.0 that `SHN_CONTRACT_VERSIONS`
  declares needs its own `FHIR_VALIDATE_URL_<line>`; otherwise the gateway
  refuses to start, naming the key. `FHIR_CERTIFY_URL_<line>` does not satisfy
  that requirement. A line with neither key dials nothing for certification
  evidence (Observational source certification, below). A single-line
  contract's line (`pa.pdex@2.1`) still rides the canonical validator, as it
  does with default lanes.
- SHN Cloud sets `FHIR_DEFAULT_VALIDATOR_LANES=none` for hosted gateways on
  v0.55.0 or later. It is not a tenant setting. A self-hosted gateway that
  leaves it unset keeps the default lanes.
- **Failed probes name their cause.** The `validator_qualification` and
  `certification_lane_qualification` log lines now carry the `host` they dial,
  and a `failed` line also carries a `reason`: `name does not resolve`,
  `name lookup failed`, `connection refused`, `metadata answered HTTP <status>`,
  `metadata is not an R4 CapabilityStatement`, `qualification corpus did not
  pass`, `no answer within the qualification budget`, `qualification stopped`
  or `qualification did not pass`. The line never includes error text or a
  validator's answer. A lane that never answers keeps the cause of its last
  failed attempt, so it still says why.
- **Rolling back.** A gateway before v0.55.0 ignores
  `FHIR_DEFAULT_VALIDATOR_LANES`: it probes the Compose default names again, and
  its failed probes carry no `host` or `reason`.

## Bridging refusals answer `422`

**Behavior change in v0.55.0.**

- When a request cannot be carried to the recipient's IG line without changing
  what it asserts, the gateway refuses it and sends nothing. That refusal now
  answers `422`, where it answered `502`. The body is unchanged:
  `{"error":"shn: semantic-change refusal: …"}`.
- On the wire, a leg is bridged only when the recipient's line is not one the
  gateway carries natively. Every published line is native, so today this is
  reachable only on a PAS submit or update the gateway builds itself while its
  egress is narrowed with the demo-only `SHN_DEMO_EGRESS_NATIVE_LINES` (see
  [docs/CONFIGURATION.md](docs/CONFIGURATION.md)). A DTR `$next-question` or
  questionnaire-package fetch is carried to the payer's line unchanged, so it
  never takes this refusal.
- Any other failure while bridging is the gateway's own fault and stays `502`: a
  step that cannot parse the payload, a missing chain, or a Provenance that does
  not round-trip.
- The bridging demo lane keeps its structured `200` refusal.

## A Coverage named twice in a DTR QuestionnaireResponse

**Behavior change in v0.55.0.**

A DTR QuestionnaireResponse can name its Coverage both in `qr-coverage` and in a
Coverage `qr-context` entry. When the gateway's transform chain bridges one
between 2.1 and 2.2, in either direction:

- **Fold.** A single Coverage `qr-context` entry is removed when it names exactly
  the same Coverage reference as the `qr-coverage` entry and equals it apart from
  its `url`. The `qr-coverage` entry keeps its position: going up it stays in
  place, and going down it becomes the `qr-context` entry in place. Earlier
  releases left two entries naming the same Coverage.
- **Refused**, as a semantic-change refusal:
  - two different Coverages, in either extension;
  - references that are not exactly equal, such as an absolute and a relative
    reference, or an identifier-only reference;
  - going up, a `qr-context` entry that names a Coverage by anything but a
    relative `Coverage/<id>` reference; going down, one that does so and does not
    exactly repeat the `qr-coverage` reference;
  - more than one Coverage `qr-context` entry;
  - a repeat that differs from the `qr-coverage` entry in anything but its `url`;
  - a result that would name one Coverage twice.
- The 2.2 to 2.1 direction could not refuse before. It now refuses these shapes.
- With no repeat present, the single Coverage entry is relocated in place, as
  before.
- **Where it runs.** No transmitted leg bridges a DTR QuestionnaireResponse
  today: a DTR questionnaire fetch is carried to the payer's line unchanged, and
  a PAS bundle's embedded QuestionnaireResponse is left as sent. The fold and its
  refusals apply to the transform chain itself (`engine.RunTransformChain`, and
  the observer listener's `POST /demo/transform`, which answers a refusal with
  `422` and its own body).

## When the requester stops waiting

**New in v0.55.0.**

- A payer's gateway still waiting on its payer's system when the request it
  serves ends now logs its own line. The request ends when the requester stopped
  waiting or when the payer's gateway is shutting down:

  `gateway: upstream payer <leg label> call abandoned after <s>s: the request it serves ended (<cause>) (host <host>, leg <leg>, correlation <id>, request written: yes|no)`

- It covers the CRD, DTR, PAS submit, update and inquiry calls, and the coverage
  eligibility forward. It names the upstream host only, never the path, query,
  headers or body. Earlier releases wrote no payer-side line for it.
- It is written alongside the existing `upstream payer … unreachable` or
  `read failed` error, when the request had already ended; those errors are
  unchanged (see [docs/CONFIGURATION.md](docs/CONFIGURATION.md)).

## Enrichment of native requests

**Behavior change in v0.54.0: a Da Vinci-native request is carried as sent unless
the participant opts in to enrichment.**

- `ENRICH_NATIVE_REQUESTS` takes `true` or `false`; unset means `false`, and any
  other value refuses to boot. It governs the provider ingress's three enrichments,
  each read from the participant's own system of record: the CDS Hooks prefetch fill,
  the `$questionnaire-package` Coverage append and the `$questionnaire-package`
  Patient append (registered edits E-02, E-04 and E-05).
- By default none of them runs. A CDS Hooks request is sent with only `fhirServer`
  and `fhirAuthorization` removed (from v0.61.0, and with the coverage read
  through that `fhirServer` to route by carried, E-07); a
  `$questionnaire-package` request is sent byte for byte. Earlier releases added the advertised prefetch values and the
  Coverage a request left out without being asked; an EHR that relied on that now
  sends them itself, or its gateway sets `ENRICH_NATIVE_REQUESTS=true`.
- A coverage a request leaves out is still read from the system of record, to
  choose the payer; it is not added to the request. That read is made under the
  system's own Patient id, so a system that names the patient by another id no longer
  refuses such a request by default, on the CDS Hooks and questionnaire-package
  ingress alike (nor, from v0.61.0, under the opt-in: see "A patient the system of
  record names by another id, under the opt-in"). A request whose coverage cannot be found is refused as before.
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
is not configured and that the network has no default validator lanes. From v0.57.0 a 2.1 or
2.2 address is gated too, on its own qualification by the same qualifier; a gated client
starts nothing when constructed, and its qualification starts with the certification worker
(`engine.GatedCertificationValidator.Start`), so at `CONFORMANCE_ENFORCEMENT=none` no lane is
dialed:
until it passes, the line's verdict is unavailable, "lane not qualified", and nothing is
dialed for the exchange, so a validator still warming never answers a verdict. The gate
re-qualifies after a lane stops answering: a qualified 2.1 or 2.2 lane (a default one too) that
fails at the connection (it refuses, resets or closes it, cannot be reached, or does not answer
within the certification's 2 s while the collection is still waiting; that certification is
recorded expired, since the client's own limits are 2.5 s) is unqualified for certification
until it passes again, "re-qualifying", and nothing is dialed meanwhile. A re-qualification
starts at most once every 30 s; its attempts then follow the qualification's own schedule,
which starts over with each re-qualification: each attempt lasts up to 60 s and repeats
`GET /metadata` until it answers `200` (each request waits up to 4 s, then 1 s passes before
the next; a `200` that is a readable R4 CapabilityStatement of at most 4 MiB leads to one run
of the readiness corpus, any other `200` fails the attempt), and attempts are 15 s apart for
the first hour after the loop starts, then 5 min apart. A validator that refuses the
connection, does not resolve or answers another status therefore gets about one metadata
request per second, and one that accepts the connection but never answers about one every
5 s, for most of that first hour, and a 60 s burst of the same about every 6 min after it. The
same schedule applies before a lane's first qualification. A 5xx or malformed answer marks only that
certification unavailable, and the collection's own deadline or shutdown only that
certification expired; either leaves the lane qualified. A lane restarted between certifications, with no
failure in between, is not detected. The 2.0 line is
not gated: it certifies on the gateway's own validator without this qualification, so a freshly
started 2.0 validator can still answer a certification before it has warmed. From v0.56.0,
where certification evidence is collected, `Run` also sends each certification address it was
given one request at boot, off the request path and recording no evidence, so that a new
process's first request to a validator is not a real certification's (docs/CONFIGURATION.md);
from v0.57.0 it sends only the 2.0 validator it certifies against, since a 2.1 or 2.2 address's
qualification already sends it the readiness corpus. The handler constructors do not. Embedders may
supply independent clients through `Config.CertificationValidatorsByLine`; they must
not share routing validators or qualification wrappers. `engine.NewQualifiedCertificationValidator`
gates such a client on a qualifier; `engine.QualifyValidatorLane` is the one the app uses.
From v0.57.0 neither `engine.NewGatedCertificationValidator`
nor `engine.NewQualifiedCertificationValidator` starts its qualification loop.
`engine.New` calls `Start` on each when it starts its certification worker. An embedder that
uses such a client without `engine.New` must call `Start` itself, or the lane never qualifies.
From v0.57.0 such a client, never started and not closed, records every certification as
unavailable, "certification validator unavailable: certification validator not started (call
Start)", dials nothing, and logs one `gateway: certification_lane_not_started` line (with its
`version`, `line`, `base`, `host` and `at`) at its first certification;
`engine.CertificationNotStartedReason` is that reason. `engine.New` starts every client before
its worker certifies anything, so a gateway built with it never records this.
Missing clients are recorded as unavailable. HTTP server execution failures are unavailable
rather than conclusive invalidity. From v0.57.0 an answer whose every error is terminology
the validator could not check (a code system it does not hold, the required-binding miss on
that element when the validator also says it could not expand the bound value set for want of
each system the miss names, a Bundle entry's no-match summaries on any line when every other
error on that entry is one of these, and on the 2.1 and 2.2 lines a PAS Claim entry's match of
neither Claim profile, read as the structural level reads it: the no-match summaries naming
both Claim profiles and the other profile's own cardinality minimums and maximums, attributed
to it alone; the entry must itself carry such terminology) is unavailable, naming the
code system, rather than invalid; any other error keeps it invalid. Only a valid verdict
certifies. Logs beginning `certify: ` and `leg.certified` observer details contain
JSON metadata, hashes and verdicts, without payload snapshots. External validator
issues and errors are represented only by bounded counts, byte lengths and
SHA-256 digests; diagnostic text is never retained or broadcast in evidence, except that an
unavailable verdict names a code system the validator could not check when it is an X12 code
system canonical (any other by its size and SHA-256).

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

**Breaking in v0.39.0** (payer wiring):

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

**Breaking in v0.39.0** (wire behaviour — new refusal class):

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

**Breaking in v0.52.0** (wire behaviour — a timed-out Hub leg answers `504`):

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

**Breaking in v0.60.0** (`Store` connectors: an inquiry answer decides only the claim it names):

- `engine.PendLedger.LookupPended` is `LookupPended(requesterHolder string, k engine.PendKeys,
  about []engine.PendKeyRef) (engine.PendMatch, error)` instead of returning
  `(subjectPCI, corrID string, found, ambiguous bool, err error)`. `PendMatch` carries the
  authorization when its `Verdict` is `engine.PendMatchFound`, and otherwise why none was
  named: `PendMatchNoStrongKey`, `PendMatchNone`, `PendMatchAmbiguous`,
  `PendMatchDisagrees` (with the disagreeing key `Kind`) or `PendMatchRequesterKeyOnly`.
- A lookup now **searches by the strong keys only**, the payer's own ClaimResponse
  identifier and its preAuthRef (`engine.StrongPendProbe`). A request identifier or an item
  trace number no longer finds an authorization on its own: reused example claims share
  them, and matching on one recorded one claim's decision on another. The one authorization
  a strong key finds must agree with every key the answer states, of each kind it holds
  (`engine.PendKeysAgree`), except that a match by the payer's preAuthRef does not require the
  ClaimResponse identifier to agree: a payer may issue a new ClaimResponse for its decision.
  Two authorizations holding one preAuthRef are ambiguous. A ClaimResponse identifier whose
  value is the value of one of an authorization's own request identifiers, whatever system
  it is stated under, is the requester's claim identifier echoed by the payer
  (`engine.EchoedClaimIdentifier`): it never finds that authorization, and when it is all
  that would have, the lookup answers `PendMatchRequesterKeyOnly` and records nothing.
- `about` are the follow-up's own keys (`engine.PendAboutKeys`: its lines' trace numbers and
  authorization numbers). They never search. `PendMatch.About` is true only when the match
  holds every one of them and no other authorization in the requester's namespace holds
  any; only then is a decision's EOB built from the follow-up's own lines. A follow-up with a
  line that states neither has no `about` keys.
- A third-party `PendLedger` backend must, inside one transaction or lock: probe its index
  with `engine.StrongPendProbe(k)` (at most two distinct authorizations), leaving out an
  authorization a ClaimResponse identifier finds only as an echo of its own request
  identifier (and noting that it did); when there is exactly one, read the keys it holds in
  the requester's namespace and the distinct authorizations holding any `about` key (at
  most two); and return `engine.ResolvePendMatch(k, about, candidates, held, aboutHolders,
  echoed)`. Reuse
  `ResolvePendMatch` rather than reimplementing the verdict, so a custom backend answers as
  `MemStore` and `PgStore` do.
- `RecordDecision` is `RecordDecision(subjectPCI, corrID, outcome string, decidedAt time.Time,
  k engine.PendKeys, eob *engine.EOBRecord)`: `k` carries the requester and the decision's
  keys, `k.PreAuthRef` its authorization number. It writes the decision's EOB only when
  `engine.PendEOBWritable` says the decision the ledger keeps after the write is this one: the
  same outcome, dated no earlier. A losing or older decision no longer replaces the kept
  decision's EOB; a later restatement of the kept decision may supply an EOB an earlier answer
  could not, and advances the date the decision is kept by. When the write changes the kept
  outcome and brings no EOB of its own, the decision EOB (`engine.DecisionEOBID`) is removed in
  the same write (`engine.PendEOBStale`) and `PendTransition.EOBRemoved` says so; a
  `RecordPendedKeyed` re-pend that supersedes a decision removes it the same way. The keys
  `engine.DecisionPendKeys` names are indexed under the authorization's requester in the
  same write: the kept decision's authorization number for an authorization already in the
  ledger, and every key of `k` for one decided with no row before it (a payer that decided at
  submit), which then belongs to `k.RequesterHolder`. A decision that names a requester for
  an authorization filed under another requester is refused with
  `engine.ErrPendRequesterMismatch`, as a re-pend is. A custom backend must apply the same
  rules.
- A payer gateway's inquiry emits one `pend.inquiry-unmatched` observer event counting, by
  verdict, every ClaimResponse in the answer it could not record (before, those were skipped
  silently), and one `pend.inquiry-eob-withheld` event counting the decisions it recorded
  without an EOB because the inquiry's own lines do not name the authorization alone and it
  has none yet. A ledger write that removes a stale decision EOB emits `pend.eob-removed`. The
  payer's answer is relayed unchanged either way.
- `engine.PendCorrelationLookup.PendedForOtherSubject` reports another patient's authorization
  under the correlation id in any state, decided included. A payer gateway therefore refuses
  (`409`) a submission for one patient under a correlation id another patient's decided
  authorization holds even when that decision has no EOB (one with no product coding, or one
  removed when its outcome changed); before, only an EOB filed for that patient or an
  undecided authorization refused it. A custom store implementing the capability must count
  decided rows too.
- **Upgrading.** A ledger is not rewritten on upgrade. Decisions and EOBs it stored before
  v0.60.0 keep their state, including a decision an earlier answer filed on the wrong claim, and
  a decision recorded at submit before v0.60.0 holds no keys for a later inquiry to find it by.
  Inquiries from v0.60.0 on are judged under these rules.

**Breaking in v0.54.0** (a payer gateway's pend ledger records and never gates):

- A payer gateway no longer refuses a PAS amendment itself. The `409`s for no pend
  recorded, an authorization already decided, and another amendment in progress are gone.
  Every amendment reaches the payer's system and its answer is relayed. The ledger
  records that answer; an amendment that did not bind a local pend is noted
  `pend.amendment-unbound:<reason>`.
- A record that cannot be written after the payer answered no longer withholds the
  answer (it was `502 holder write failed`). The gateway emits `pa.local-write-failed`.
- The payer's own `409` version conflict is relayed; the payer's gateway no longer re-sends
  the amendment once. The `retry:version-conflict` observer note is gone. (From v0.56.0 the
  provider's gateway, as the requester, re-sends an amendment it built once; see above.)
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

**Breaking in v0.52.0** (wire behaviour — a correlation id belongs to one patient):

- A payer gateway refuses a prior-authorization submission with `409` (`correlation id
  already names another patient's authorization`) before its payer's system is asked, in
  two cases: a decision EOB for another patient is filed under that correlation id, or
  another patient's authorization is still awaiting its decision under it. Such a
  submission previously reached the payer. If the gateway cannot read its store to
  decide, it answers `502 {"error":"holder read failed"}` without asking the payer (framed
  as its answer since v0.54.0; before that the requester saw `502 hub routing failed`).

**Breaking in v0.52.0** (`Store` connectors):

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
  `observer.Hub`): new in v0.19.0 and **evolving** — field additions and event-kind
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
  (`engine.Config.DemoEdgeCapture`) is set, which also requires
  `OBSERVER_ADDR` to be set — otherwise the flag is gated off at config load rather than
  capturing into a store nothing could ever read. `POST /demo/transform`'s existing 200 and
  422 response bodies also gain an additive `chain` field in v0.35.0 — the
  compatibility-chain hops the run walked (or attempted), in the same shape already published
  on observer events; every existing field on both responses is unchanged. New exported engine
  surface backing these v0.35.0 additions, each its own release-notes bullet:

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
  the UC-01…08 scenario-driving package. New in v0.19.0 and **evolving** — signatures and
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
  `engine.LegOutcomeRouted/Answered/Denied/Unreachable/Failed`): new in v0.29.0 and
  **evolving** — a nil-safe hook that receives one outcome string per origination-leg event at
  the roundTrip choke point. Nil (the published-binary default) means no emission; the hook
  carries no payloads and is conformance-neutral (`TestLegMetric_ConformanceNeutral` — responses
  are byte-identical hook-on vs hook-off). `gateway/app` wires it to CloudWatch EMF behind the
  `METRICS_SERVICE` opt-in (see `docs/CONFIGURATION.md`). `Unreachable` means the Hub leg did not
  complete; it also covers a Hub refusal after an unverifiable recipient response envelope and
  does not prove the responder itself was unreachable. Requires `shn-sdk` ≥ v0.31.0.

- **`ExchangeObserved`** (`engine.Config.ExchangeObserved func(engine.ExchangeRecord)`):
  new in v0.58.0 and **evolving** — fields and closed-set values may be added in minor
  releases; decode tolerantly. See "One record per exchange" above. Nil means no record; a
  panic in it is recovered and never reaches the answer (`TestExchangeRecord_Panics`).

- **`GET`/`POST /internal/checks` results** (evolving surface, since v0.32.0; structured
  `failure` since v0.33.0). Each result is `{id, target, ok, detail, checkedAt,
  latencyMs}` plus, on failing results only, `failure {code, hint}` with `code` drawn
  from a closed set (`unreachable`, `http-status`, `invalid-capability-statement`,
  `credential-rejected`, `not-checked`, `internal`). From v0.59.0, a `$metadata` check of
  a base configured with a credential reads authenticated and adds an informational
  `anonymous {ok, detail, failure}` object, the same read without the credential, which
  never decides `ok`. Additive-only intent: existing keys
  and `detail` strings are stable fallbacks; new keys may appear in 0.x minors — decode
  tolerantly, never with unknown-field rejection. See `docs/CONFIGURATION.md`
  ("Operational checks") for semantics and redaction guarantees.

**Additive setting, v0.49.0: `PAYER_DAVINCI_BACKEND_HEADERS`** — fixed request headers
for a partner system that routes on one, sent on every request to the partner's bases and never
to its token endpoint (`docs/CONFIGURATION.md`, native-forward payer mode). Unset ⇒ every request
is byte-identical to an earlier release's; the option adds headers only, never changes a body, and a
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

`engine.Config.Diagnostic` (an `engine.DiagnosticSink`, which is
`func(diagnostics.Event) bool`) is a separate opt-in sink
for raw HTTP and participant-stage observations. `DiagnosticTraceKey` verifies
optional private ingress call attribution. `engine.WithNativeDiagnostic` adds the
same sink to native forwarding. Nil disables each hook. Sinks run synchronously,
may be called concurrently, must return promptly without blocking, and must
reserve bounded memory before copying read-only event bytes. Sink panics are
contained. The app supplies the bounded `diagnostics.Queue` publisher and owns its
shutdown in both `Run` and the closable `Handler` constructors.

A `diagnostics.Queue` counts each sequenced event once as it leaves, and its
`Health` carries the counts (additive, omitted when zero): `acknowledged`,
`discarded` (declined as out of scope, or withheld by kind) and `droppedBy`, the drops by reason
(`queueFull`, `expired`, `oversized`, `unencodable`, `test`, `stopped`, and the
new `invalid`). `Health.CountsAgree` counts `invalid` among the drops.
`Queue.Drop` counts an expiry. The publisher drops an event as `invalid`, without
retrying it, when the ingest answers `400 invalid_event` or `409 conflict`; an
older publisher retried it until its ownership window ran out. An accounts
service older than these counts refuses a heartbeat that carries them.

`diagnostics.PublisherConfig.BatchURL` (additive; empty keeps one event per
request) names an ingest's batch endpoint. With it set, the first heartbeat is
sent at once, and once the ingest answers a heartbeat with the header
`X-SHN-Evidence-Batch` (`diagnostics.HeaderEvidenceBatch`) the publisher posts
up to 256 due events, bounded by 1 MiB of retained cost, in one request signed over its
whole body as `{"events":[…]}`, and settles each by the ingest's per-event
result (`accepted`, `binding_pending`, `scope_ignored`, `kind_not_admitted`,
`invalid`, `conflict`, `unavailable`). An event the ingest could not take now,
or a batch it refused whole, is due again after a backoff until each event's
own ownership window runs out; a test event the ingest did not take is not
retried. A `413` halves later batches, which grow back as batches settle in
full; a `413` for one event alone drops it as `oversized`. A sink that never sends that header (a
participant's own sink, or an ingest older than batches) gets one event per
request, as before. A batch answered 404 or 405, or with a success without one
result for each event in order, falls back to one event per request until a
heartbeat's answer carries the header again. The app sets `BatchURL` to its
sink URL's path plus `/batch` and `Drain` to 10 s, and its queue holds 4,096
events.

`diagnostics.PublisherConfig.Drain` (additive; zero keeps stopping at once)
bounds a final delivery after the publisher's context ends: it keeps sending
what is queued, an event or batch in flight at the stop included, until the
queue is empty or `Drain` has passed, then counts what is left as dropped
(`stopped`) and sends a last heartbeat with the counts. An event emitted after
the drain has ended is neither delivered nor counted; the app stops the
publisher last, after its listener has shut down and the gateway has closed. A
request still running when the listener's 5 s shutdown grace ends is cut, and
its handler has up to 2 s more to return, and emit, before the publisher stops.

A batch result of `kind_not_admitted` (the ingest never admits that kind from
this publisher) withholds the kind: its later events are counted as discarded,
and as `suppressed` in `Health` (additive, part of `discarded`), without being
sent, until ten minutes later, when its events go to the ingest again and its
answer decides afresh. No other result withholds a kind, and a sink that never
answers `kind_not_admitted` (one that does not take batches) gets every kind.
Events of the kind already queued when it is withheld are still sent.
`Health.CountsAgree` refuses `suppressed` above `discarded`, or without the
other counts. `Queue.TryEmit` returns `true` for an event of a withheld kind: it is
counted, not queued. `RunPublisher` uses `PublisherConfig.Clock` as the queue's
clock, read from emitting goroutines with the queue locked, so a `Clock` must be
safe for concurrent use and must not call into the queue. A collector that decodes
heartbeats strictly must know `invalid` and `suppressed` before a publisher sends
them.

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
