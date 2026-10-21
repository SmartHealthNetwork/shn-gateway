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
- `limit` is recorded for a coverage eligibility batch of more requests than
  a gateway reads in one batch (from v0.63.0); from v0.58.0 the gateway records an Authorization
  Framework denial as `authority`, whatever its reason; a facility's `consent`
  refusals (a federated query without a consent reference, or one the consent
  service does not permit) are `consent`. A facility's or a PHG's gateway
  refusing a leg is refused by `other`.

## A validator that stops without a verdict is unavailable (v0.63.0)

- **Behavior change in v0.63.0 (conformance):** a FHIR validator that stops without a verdict
  (`HAPI-0389: … failed to validate, but no errors.`) is read as unavailable, as one that
  was given no resource to read (`HAPI-0992`) already is. It is never read as a structural
  defect.
  - **Where it happens:** on PAS 2.1 and 2.2 request Bundles that declare
    `profile-pas-request-bundle`, matching the Claim entry against the two profiles its slice
    allows can end the validation there, with no other entry checked. A PAS request Bundle
    that declares no profile is checked whole, as before.
  - **Each level** now does what it does for a check that cannot run:
    - `none` does not run the check;
    - `observe` and `structural` carry the message and record an `unavailable` finding;
    - `strict` refuses `500` as unavailable.
  - **Before v0.63.0,** such a Bundle read as structural, valid or not:
    - `structural` and `strict` refused it `422`;
    - `observe` recorded it as a structural defect.
  - **Both directions:** a valid Bundle of that shape is no longer refused at `structural`.
    A broken one is no longer refused there either, because the validator did not check it.
- **Behavior change in v0.63.0 (a bridged check):** module 2 decides a translated message's
  check at every level, and refuses a check that cannot run. So a translated PAS 2.1 or 2.2
  request Bundle that declares the profile and stops the validator is now refused `500
  validator unavailable`, where before v0.63.0 it was refused `422 egress validation failed`
  with the validator's issues.
- **Behavior change in v0.63.0 (certification evidence):** the same answer on a certification
  lane, or one that read no payload, is recorded `unavailable` ("certification validator did
  not judge the payload"), not `invalid`.

## A Coverage's subscriber, policyholder or payor is never taken for the member (v0.63.0)

- **Breaking (behaviour), a fix:** a gateway that derives a patient identifier for a member
  its system of record does not hold reads the member's Patient the request carries. A
  Patient that a Coverage in the request names as its `subscriber`, `policyHolder` or `payor`
  (a parent who holds or pays for a dependent's coverage), other than the Coverage's own
  `beneficiary`, is another person and is never read as the member, however the slot names
  it and wherever the request carries it:
  - by `#id`, beside the Coverage among the same resource's contained resources or in the
    Coverage's own (a `contained` that is a single object is read as the one resource a
    lenient reader reads it as);
  - by a Patient reference to its id (an id-less Patient in a Bundle entry has the id its
    RESTful `fullUrl` names) or by a Bundle entry's `fullUrl`;
  - by a logical reference to an identifier it carries (one with no system names the value in
    any system);
  - with the slot holding a Reference or a list of them, and with a slot, a reference or the
    Coverage's `resourceType` spelled in another case, as a reader that ignores case reads
    it.

  A Coverage whose `beneficiary.reference` names the Patient makes no party of it: a
  subscriber covered by the Coverage is its member. The tie holds only where every reader
  reads the request alike: a `beneficiary` that is not one Reference ties nothing, and no
  beneficiary ties a Patient in a request whose FHIR resources spell a name a reference is
  resolved by (`resourceType`, `id`, `fullUrl`, `resource`, `entry`, `parameter`,
  `contained`, `beneficiary`, `reference`) in another case, or nest contained resources.
  Members outside any resource, such as a vendor's own in a CDS Hooks request's `extension`,
  are no FHIR names and are not read. A Patient with a server of its own (an entry's RESTful
  `fullUrl`) is named as a Bundle resolves the reference: an absolute reference names only
  the entry whose `fullUrl` it is, and a relative one resolves against the RESTful base of
  the Coverage's entry; an entry whose `fullUrl` names another id than its Patient's is
  named by none. A Patient under a `urn:uuid` is named by that `fullUrl`, or by its type and
  id only through a relative reference that no RESTful base resolves, and one with no URL of
  its own (carried outside an entry, such as a CRD prefetch Patient, or in an entry without
  a `fullUrl`) is named by its type and id. A reference by type and id names no Patient when
  the request carries different Patients under that id, contained or not, since some readers
  resolve a reference by type and id to a contained resource too. A contained Patient is
  named only by its `#id`, a `#id` names a resource only among those contained with the
  Coverage (its own, or its container's when it is contained itself), and an id two of those
  resources share names neither. Only a Bundle's entries have a `fullUrl` a reference
  resolves by, and a Coverage's references resolve among its own Bundle's entries: an object
  elsewhere holding `fullUrl` and `resource` is no entry, and an entry of another Bundle is
  named only by the RESTful URL it is, while no different Patient is carried under its id,
  though a slot that may name a Patient, or names an identifier it carries, still may be it.
  A Coverage carried in no Bundle names a Patient by its type and id. Where the request
  carries a Coverage, a Patient in an entry whose `fullUrl` (versions, the case of its
  scheme and host, and a default port aside) an entry holding a different resource has, in
  its Bundle or another, is never read as the member. Patients are told apart by the facts a
  patient identifier is derived from (birth date and family name), each as the Patient
  carries it or not: versions of one person's record, or copies of one entry, are one, and
  Patients carrying different facts are told apart, whichever they lack. A reference nested
  within the beneficiary, such as an extension's or an identifier's assigner, ties nothing,
  and neither does a beneficiary spelled in another case. A Patient's id equal to the member
  id is no such tie, since the member may have been read from an identifier, and a contained
  resource's id is local to its container, never a member id. When the involved list looks
  up another patient a request names by a Patient's id, that Patient is the record of the
  very person looked up and is read as theirs.

  Where a Coverage's party slot may name a Patient (a reference, other than the Coverage's
  own beneficiary reference itself, that names no resource the request carries as another
  type, or an identifier in no system or the member system), a Patient bearing the member's
  id or number is read as the member only when that Coverage's `beneficiary` names it: any
  other is one the gateway cannot decide is the member, wherever the request carries it, and
  is never read as the member. Another Coverage naming it as its beneficiary does not decide
  it. A payor naming an Organization does not make it so.

  Before, such a parent carrying the member's family-level member number could be read as
  the member, so the dependent's exchange was authorized, recorded and filed under the
  identifier the parent's own requests derive.
- **New refusal:** a request that carries no Patient for the member with a birth date and
  name, and carries such a party under the member's own member number, is refused at every
  level, by each gateway, before anything is sent:
  `400 cannot identify the patient: the request carries no Patient for its member with a
  birth date and name, and its member number is shared with another person it carries (a
  coverage's subscriber, policyholder or payor)`, recorded as `authority`. The number is shared,
  so the member id alone would identify no one patient.
- A request carrying the member's own Patient is bound from it, on both gateways. A request
  naming no party is bound as before, from the member id alone when it carries no Patient;
  a member the system of record holds is bound through it, as before. The involved list names
  a party under its own identity, from its own record, and leaves out a member it cannot
  identify (`unknown-member`).

## A carried PAS answer is validated at the provider ingress (v0.63.0)

- **Behaviour change:** a payer's answer to a PAS `$submit` (a submission or an amendment) or
  `$inquire` that a provider gateway carries for its own participant's system through its Da
  Vinci ingress is `$validate`d at the gateway's conformance level, against the candidate
  IG lines its answers to the legs it originates are certified against (the routed line
  first, valid on any line is valid). Not at all at `none`; recorded at `observe`; at
  `structural` refused for a structural issue and recorded for a deeper one; refused at
  `strict`, where a check that cannot run is refused `500`. A refused answer is answered
  `502 {"error":"ingress validation failed"}`, as the provider ingress refuses its other
  carried answers, and recorded as a `fhir-ingress` finding from the peer, refused by the
  provider gateway under the `conformance` rule. Before, such an answer was read only by
  the content rules (its IG line's answer graph and its patient linkage), so one that
  conformed to no line was carried at every level with no finding.
- Unlike the answers to legs the gateway originates on the `provider-data` or `demo` lane,
  no reference payer's carried answer is exempt: a participant that sets a level has every
  answer it receives judged.
- It is payload conformance, the participant's own opt-in: a gateway at `none` carries the
  answer exactly as before.
- **Behaviour change (Go API, `scenariodriver`, Evolving):** `InjectShnCorrelation`,
  `BuildAmendedRePOST` and `BuildFederatedAmendedRePOST` set the PAS Claim's one identifier
  to its `urn:shn:correlation` instead of adding it beside the Claim's own identifier, and an
  amendment is the update Claim alone: it declares `profile-claim-update`, its
  `related[0].claim` names the submission by that correlation identifier, with no
  `reference`, and the Bundle carries no prior Claim. PAS 2.0.1 allows a Claim one identifier
  and a request Bundle one Claim, and the goldens these build from are PAS 2.0.1; before, the
  Bundles they built were not conformant at 2.0.1.
- **Behaviour change (Go API, `scenariodriver`, Evolving):** the `Provenance` that
  `BuildAmendedRePOST` and `BuildFederatedAmendedRePOST` add carries `recorded` (1..1 in base
  R4), the Bundle's own `timestamp`. A golden with no `timestamp` is refused. Before, the
  Provenance had no `recorded`, so it was not valid R4.

## A carried Patient's demographics are read by exact member names (v0.63.0)

- **Behaviour change:** a gateway that derives a patient identifier for a member its system of
  record does not hold (from the member id and the birth date and family name of the
  member's Patient the request carries) reads that Patient by its exact member names, as
  every reader of the request does. A Patient naming its `birthDate`, `name` or `family`
  only in another case has no demographics, and neither does a request whose FHIR resources
  repeat a member name, exactly or in another case (a vendor's own members outside any
  resource, such as in a CDS Hooks request's `extension`, are no FHIR names and are not
  read). A Coverage's contained resources named only in another case are no resources, so a
  parent its `subscriber` or `policyHolder` names there is never read as the member; nor is
  a parent in a Coverage that spells, in another case, a name the party rule reads
  (`resourceType`, `contained`, `subscriber`, `policyHolder`, `reference`, `id`,
  `identifier`), which that rule then names no party in though a reader that ignores case
  would: no Patient is read from such a Coverage's contained resources. Before, these names
  were read whatever their case, and such a parent's birth date and name could be read as
  the member's.
- The member a request names is bound as before; only the demographics an identifier is
  derived from change, and only for such requests. A provider gateway opted in to
  enrichment (`ENRICH_NATIVE_REQUESTS`) reads whether a DTR request carries the member's
  Patient the same way, so for such a request it may add the member's Patient from its
  system of record.
- **Behaviour change (Go API):** `engine.PatientDemographics` reads through the gateway's
  strict reader: by exact member names, and no demographics in a Patient that repeats a
  member name or that is past that reader's limits (not well-formed UTF-8 or a lone
  surrogate escape, nesting past 64 levels, more than 1,000,000 tokens). A holder's own
  record read through it (the `fhirsor` connector) is read the same way.

## A participant's 2xx answer reaches the other participant with its status (v0.63.0)

- **Behaviour change:** a payer's system that answers a relayed request with a 2xx other
  than `200` (a `201 Created` FAIS eligibility answer, a `202 Accepted`, a `204 No Content`)
  is answered to the provider's system with that status, on every Da Vinci ingress route
  (CRD, DTR `$questionnaire-package`, PAS `Claim/$submit` and `Claim/$inquire`,
  `CoverageEligibilityRequest/$submit`), at every level. Before v0.63.0 every relayed
  success was written `200`. The bytes are unchanged, as before. The federated query
  (`POST /Task`) follows the same rule; its answer is always built by the facility's
  gateway, so it is `200`.
- The payer gateway frames its system's own status in the answer frame, which has always
  carried a status, and, for a requester that decodes frames, records it on the leg
  (`ExchangeRecord.Status`). An answer the gateway built itself is `200`.
- **Additive in both directions:** a requester before v0.63.0 reads any 2xx frame as a
  success and writes `200`, as it always did; a payer gateway before v0.63.0 frames every
  success `200`, which a v0.63.0 requester writes as `200`. An unframed answer is `200`.
- **New refusal:** a frame stating `204` that carries a body is not read, at every level:
  the requester's gateway answers `502` and records `integrity`.
- A `204` answer has no body: wherever a level judges an answer's content, it judges an
  empty one exactly as it judged an empty `200`.
- **Additive (Go API):** `engine.LegResult.AnswerStatus`, the 2xx status a connector's
  relayed answer was given (`0` is `200`). `scenariodriver.Driver.SubmitPAS` reads the
  outcome of any 2xx answer, not only a `200`.

## Validator lanes are keyed by contract line (v0.63.0)

- **Additive (Go API):**
  - `engine.Config.ValidatorsByContractLine` holds the `$validate` lanes keyed by contract-version token (`"pa.pas@2.0"`). A value may be an `*engine.DiscoveredLane`, which serves once it has qualified.
  - `engine.ContractLineLanes` derives that map from per-line lanes.
  - `engine.CanonicalContractVersion` names the contract version the canonical validator (`FHIR_VALIDATE_URL`) resolves: `pa.pas@2.0`.
- **Deprecated (Go API):** `engine.Config.ValidatorsByLine`, `CanonicalFallbackLines` and `DefaultValidatorsByLine`. When a caller sets them and not `ValidatorsByContractLine`, `New` derives the contract-line lanes they name, with the same resolution as before, except that `pa.pdex`, which no leg uses, resolves no lane. They stay until v1.0.0.
- **The gateway's routing is unchanged:**
  - every contract it declares resolves the same lane at every line as before;
  - a single-line contract still borrows the canonical validator;
  - a multi-line contract still never does.
- `engine.ContractLineLanes` derives lanes for the contracts this gateway builds natively
  (`engine.NativeContractVersions`: CRD, DTR and PAS) and those its `declared` argument
  names, whatever the linked shn-sdk lists. `pa.pdex`, which no gateway declares, has no
  lane, as any contract the gateway does not build; no exchange validates on it.
- The per-line settings (`FHIR_VALIDATE_URL`, `FHIR_VALIDATE_URL_2_1`, `FHIR_VALIDATE_URL_2_2`) are unchanged.

## The gateway's contract sets come from its workstreams (v0.63.0)

- **Additive (Go API):** `engine.LegOperations` returns every compiled-in workstream's leg operations as `engine.LegOperation` rows, as `engine.PALegOperations` returns PA's as `engine.PALegOperation` rows (the same fields). A released gateway compiles in PA and Federated Query Services (below), so it returns the two workstreams' operations together.
- `engine.NativeContractVersions`, `DefaultContractVersions` and `ParseDeclaredContractVersions` read the compiled-in workstreams' contract lines, not the SDK's alone. A released gateway's sets are unchanged: the SDK's native and default sets without `pa.pdex@2.1`, in the SDK's order. The one visible difference is the error for a declared token the gateway cannot build: it lists the gateway's own native set, without `pa.pdex@2.1`, where the SDK's listed it.
- A test build with the `shn_toyworkstream` tag compiles in a second workstream, a test profile, to prove a second workstream is carried. A released gateway is never built with it.

## The federated query is the Federated Query Services workstream's leg (v0.63.0)

- **Narrowed (Go API):** `engine.PALegOperations` no longer returns `federated-query`. The network's leg taxonomy has a second workstream, Federated Query Services (`fqs`), and the federated query is its leg. `engine.FQSLegOperations` returns it, and `engine.LegOperations` still returns every leg, the federated query included. A caller that read the federated query from `PALegOperations` reads it from either of those.
- **Additive (Go API):** `engine.FQSLegOperations` returns the federated query's binding as an `engine.LegOperation` row, as `engine.PALegOperations` returns PA's.
- **The leg is unchanged on the wire:** its transaction type, its operations (`federated-query-submit`, `federated-query-response`), its frames, the roles that hold them, its minimum-necessary scope (`named-docs-only`) and its consent check. A Smart Gateway sends, answers and carries it as before, and the provider ingress's `POST /Task` route is unchanged.
- The discovery descriptor's `operations[]` is unchanged, in content and order.

## A provider's own system sends a coverage eligibility request (v0.63.0)

- **Addition in v0.63.0:** the provider gateway's Da Vinci ingress serves `POST
  /CoverageEligibilityRequest/$submit`, the base FHIR R4 operation (IHE FAIS ITI-124), under
  the same SMART Backend Services auth as the other ingress routes. The body is any of the
  operation's inputs: the `CoverageEligibilityRequest`; a `Bundle` carrying one request with the
  resources it references (IHE FAIS's CoverageEligibilityRequest Bundle); a `Bundle` that is a
  batch of requests, each alone or in such a Bundle; or a `Parameters` whose one `resource`
  parameter holds any of these. A `Bundle` that is neither form is refused `400`, as is one
  whose entries cannot be read one way only (an entry's `resource`, `fullUrl`, `resourceType`
  or `id` written in another case or with another JSON type, or an entry with no typed
  resource).
  - **Carried.** It goes to the payer on the `coverage-eligibility` leg exactly as sent, in the
    form it was sent, and the payer's answer is relayed exactly.
  - **Routing.** Each request is routed by its `insurer` (an identifier, or an Organization it
    carries), or failing that by the first payor of each Coverage it carries and names in
    `insurance`: every one must name a payer, all the same one. References resolve among the
    resources the request carries: a `#id` among the referring resource's contained resources,
    any other reference among the entries of a Bundle carrying the request. A relative
    `Organization/<id>` or `Coverage/<id>` that nothing carried answers is read from the
    participant's system of record, only to route, when that system names the request's patient
    by its member id; a failed read is that system's failure (`502`, `503` unavailable).
    Refused `422`: no payer identifier (`no payer to route the eligibility request
    by: …`, naming the coverage that names none when there is one, or the insurer, coverage or
    payor reference nothing resolved and what resolves it), coverages naming different
    payers (`the eligibility request names more than one payer: …`), a contained id it is
    routed by that more than one contained resource has, or a reference more than one Bundle
    entry answers (`ambiguous reference for routing: …`), and an unregistered payer (`no
    registered payer for identifier …`).
  - **The answer.** Relayed exactly; refused `502` at every level when it repeats a member
    name; refused `502` at `structural` and `strict` (recorded at `observe`) when it is not a
    `CoverageEligibilityResponse` naming its patient, or a `Bundle` of them; and `$validate`d
    at the provider's conformance level, as the answer to an eligibility request the gateway
    builds is (a validator the gateway cannot reach refuses `500` at `strict`).
  - **A batch.** One exchange to one payer: a batch whose requests name more than one payer is
    refused `422 the eligibility requests name more than one payer: …`. The exchange's patient
    is the first request's in document order; every other request's patient is named on the
    exchange's involved list.
  - **Recorded.** The exchange record's route is `eligibility`, a new value in `ExchangeRoutes`.
- **Payer side.** A payer gateway reads every input form as the requests it holds. Answering from
  its records, it answers one request (alone, in a FAIS Bundle, or in a `Parameters`) about that
  request's member, and a batch `501 this payer answers coverage eligibility from its records
  one request at a time, so it does not answer a batch of requests: this is the payer's answer,
  not a network refusal`. Forwarding to its
  own endpoint, it carries any form as received, binds each request's member, and reads a
  `Bundle` answer as its responses (its entries read by their exact member names; an entry
  carrying only its outcome is no response).
  A payer gateway before v0.63.0, or an shn-sdk Responder before shn-sdk v0.63.0, answers it
  `400 parse member failed`. From shn-sdk v0.63.0 a Responder reads every form, and refuses
  it, as a payer gateway answering from its records does.
- **Not advertised yet.** The ingress `CapabilityStatement` does not list the operation until
  the shn-sdk release that follows shn-gateway v0.62.0. The gateway serves it before then.
- **Go API (additive):** `engine.RouteEligibility`.

## An eligibility request is read one way only (v0.63.0)

- **Breaking (behaviour):** a payer gateway reads an inbound `CoverageEligibilityRequest` sent
  alone, or in a `Parameters`, by its exact member names, as it already read the `Bundle`
  forms. Before v0.63.0 it read such a request by a reading that ignores the case of member
  names and takes the last of a repeated one, and answered about the member that reading
  found, where a reader taking names exactly found another patient, or none.
  - A request that repeats a member name, exactly or in another case, anywhere in the body, is
    refused at every level: `400 eligibility request repeats the JSON member "<name>" in
    <where>: it can be read two ways, so it is not read`, recorded as `integrity`.
  - A request that names only in another case a member its patient is read through (its
    `resourceType`, a `Parameters`' `parameter`, the `name` of any of its parameters or the
    `resource` of its `resource` parameter, its `patient`, the patient's `reference`, or, for
    a `#id` patient reference, its `contained`) names no patient: refused at every level, `400
    eligibility request names "<member>" for "<name>": member names are case-sensitive, so the
    request names no patient to bind`, recorded as `authority`. A `Parameters`' other
    parameters are carried as sent, however they spell any member other than `name`.
  - A request's `contained` resources are read for its patient only through a `#id` reference:
    a payer gateway carries a request naming its patient otherwise whatever case its
    `contained` is named in, as before.
  - Before, these were answered about the member the folded reading found, or refused `400
    parse member failed`. A Bundle entry whose own members are named in another case is
    refused as before.
  - The provider gateway's ingress refuses the same requests, before anything is sent, with the
    same texts and rules, where it refused them `400 parse eligibility request failed: …` or
    `400 parse member failed`, or carried a request naming its patient only in another case.
  - None of these texts carries a value from the request. A request that reads one way only is
    read as before.
  - A body the strict reader refuses for its own limits (not well-formed UTF-8 or a lone
    surrogate escape, nesting past 64 levels, more tokens than its bound) is still read when it
    reads one way only, by its exact member names, as a body the strict reader reads: its
    patient reference names its member in every form (an absolute or versioned reference, as
    a relative one), and a `#id` one resolves through the contained resources so named. In
    such a body a repeated member name is refused as message integrity (`… repeats a member
    name: …`); a type, patient, reference or, for a `#id` reference, `contained` named only in
    another case is refused as `authority` (`400 eligibility request names its type or patient
    only in another case: member names are case-sensitive, so the request names no patient to
    bind`); and a `#id` reference no contained Patient answers is refused as below. Other
    members are read as before. The provider gateway's ingress routes only a body the strict
    reader reads, so it refuses such a body before anything is sent: with these texts where its
    patient cannot be read, and otherwise `400 parse eligibility request failed`.
  - In a body the strict reader refuses for not well-formed UTF-8 or a lone surrogate escape,
    the reader that reads it replaces each such byte or escape with U+FFFD, so member values
    that differ on the wire would bind as one. A member value read from such a byte or escape
    (the patient reference itself on the literal path, or the contained Patient's member
    identifier value on the contained path) names no patient one way only: each gateway refuses
    it at every level, before anything is read for the patient, as `authority` (`400 eligibility
    request names its patient by a value carrying a replaced character (not well-formed UTF-8 or
    a lone surrogate escape): readers read it two ways, so the request names no patient to
    bind`). A member value carrying U+FFFD as sent is read as before, wherever else such a body
    is not well-formed.
  - A `#id` patient reference that no contained Patient answers names no patient, including one
    whose `contained` is not a list of resources (an object, a string): each gateway refuses it
    `400 parse member failed` at every level, and never binds the reference's own text.

## A batch of coverage eligibility requests carries at most 17 (v0.63.0)

- **Part of the v0.63.0 addition** (the provider gateway's coverage eligibility ingress, and a
  payer gateway reading the operation's `Bundle` forms, are new in v0.63.0): an input of
  `CoverageEligibilityRequest/$submit` that submits more than 17 requests, in any form, is
  refused at every level, before anything is read from it:
  `413 the batch carries <n> coverage eligibility requests; a gateway reads at most 17 in one batch, so send them in batches of at most 17`, recorded as `limit`. The provider gateway refuses it at
  its ingress before routing it; a payer gateway refuses it on the leg before its system is
  called.
- **Why 17:** the exchange's patient and the 16 other patients the Hub records one leg under (its
  involved list). A larger batch could not be recorded under every patient it names. The gateway
  derives the bound from that size, so the two cannot differ.
- Send a larger set of requests as several batches. A request alone, or a FAIS Bundle carrying
  one, is unaffected.

## The pend ledger has its own package (v0.63.0)

- **Additive (Go API):** package `pa/pend`
  (`github.com/SmartHealthNetwork/shn-gateway/pa/pend`) holds the Prior
  Authorization workstream's payer pend ledger: the `PendLedger` interface a
  store implements, its records, keys, states, refusals and errors, and the
  state machine and matching rules (`PendBegin`, `PendRePend`, `PendDecide`,
  `ResolvePendMatch` and the rest), with `InProgressHeld`, which a store's own
  re-pend uses. It imports no engine package. Every name and every error text
  is unchanged.
- **Deprecated (Go API):** the `engine` names (`engine.PendLedger`,
  `engine.PendKeys` and the rest) stay until v1.0.0. Each type is an alias of
  the `pend` type, so a store whose methods name the engine's types still
  implements `pend.PendLedger`, and each constant, error and function is the
  `pend` one or forwards to it.
- `engine.LedgerOf` and `engine.FallbackDecision` stay in `engine` and are not
  deprecated: they read the engine's `Store`.
- Nothing the gateway relays, refuses or stores changes.

## The in-memory store keeps the pend ledger and continuations as Prior Authorization's own state (v0.63.0)

- **Breaking (Go API):** `*engine.MemStore` no longer has the pend ledger's or
  the continuation store's methods: `RecordPendedKeyed`, `LookupPended`,
  `RecordDecision`, `PendRecordOf`, `BeginClaimUpdateReason`,
  `PutContinuation`, `ReadContinuation` and `Durable`. It keeps the ledger and
  the continuation store as the Prior Authorization workstream's own state, so
  it no longer implements `engine.PendLedger` (`pend.PendLedger`) or
  `engine.ContinuationStore`. It still implements `engine.Store`, every method
  unchanged.
- **Migrating:** reach them through `engine.LedgerOf(store)` and
  `engine.ContinuationsOf(store)`. Both answer for a `*engine.MemStore` and for
  a type that embeds one, as they do for a store that implements the interfaces
  itself, such as the Postgres store. A type assertion of a `*engine.MemStore`
  to `engine.PendLedger` or `engine.ContinuationStore` is now false, and
  `var _ engine.PendLedger = (*engine.MemStore)(nil)` no longer compiles.
- **Breaking (Go API), for a type that embeds `*engine.MemStore` and declares
  some of those methods itself** (to time or fail one of them, say): it no
  longer implements the interface, so its own methods would be skipped for the
  embedded store's. `engine.New` refuses such a store as `Config.Store`,
  returning an error naming the type and the method, so the gateway does not
  start. `engine.LedgerOf` and `engine.ContinuationsOf` answer none for it
  rather than the embedded store's, and never panic. Implement the whole
  interface, forwarding the rest to what `LedgerOf` or `ContinuationsOf`
  answers for the embedded store.
- Nothing the gateway relays, refuses or stores changes. The ledger, its
  rules and retention, and the continuation store are the same code. `Reset`
  clears the auth numbers, the ledger and the continuations as before, each
  under its own lock rather than all under one.

## The PAS answer readers have their own package (v0.63.0)

- **Additive (Go API):** package `pa/pasanswer`
  (`github.com/SmartHealthNetwork/shn-gateway/pa/pasanswer`) holds the Prior
  Authorization workstream's readers of a native PAS answer:
  `NativePASFacts`, `InspectNativePASResponse`, `InspectNativePASResponseAt`,
  the `PASRefusal*` constants, `NativePASRefusalCauses`,
  `NativePASRefusalCause`, `NativePASRefusalCauseAt`,
  `NativePASGraphRefusalCause`, `NativePASUnresolvedReference`,
  `NativePASUnresolvedReferenceAt` and
  `CheckNativePASGraphWithoutClaimResponse`. It imports no engine package.
  Each reads exactly as before, by the one definition the gateway reads an
  answer by; the refusal texts and causes are unchanged.
- **Deprecated (Go API):** the `engine` names of these readers
  (`engine.InspectNativePASResponse` and the rest) forward to `pasanswer`
  and stay until v1.0.0. `engine.NativePASFacts` keeps its own type, field
  for field the same as `pasanswer.NativePASFacts`, and converts to it
  (`pasanswer.NativePASFacts(f)`); each `engine.PASRefusal*` constant is the
  `pasanswer` one, so code naming either compiles and reads the same.
- **Additive (Go API):** `pasanswer.InquiryAnswerReadsAt(body, line) (ok
  bool, detail string)` and `pasanswer.ConsistentInquiryAnswerSubjects(answer)
  bool` are the gateway's readers of a PAS `$inquire` answer: the rule it
  reads the answer by at an IG line, and its subject rule (`patient.answer`).
  The inquiry leg and these readers share one definition, so each reads
  exactly as before.
- **Deprecated (Go API):** `engine.PASInquiryAnswerReadsAt` and
  `engine.ConsistentPASInquiryAnswerSubjects` forward to them and stay until
  v1.0.0.
- Nothing the gateway relays or refuses changes.

## The exchange record vocabulary has its own package (v0.63.0)

- **Additive (Go API):** package `recording`
  (`github.com/SmartHealthNetwork/shn-gateway/recording`) holds the network's
  exchange record vocabulary: `ExchangeOther`, the directions, outcomes,
  refusing parties, refusal rules and backend error classes, and their closed
  lists (`ExchangeDirections`, `ExchangeOutcomes`, `RefusalParties`,
  `RefusalRules`, `BackendErrorClasses`, `AnswerErrorClasses`). It names no
  workstream and imports no engine package, so a reader of recorded exchanges
  can use the gateway's own words without importing the engine. Every value is
  unchanged.
- **Deprecated (Go API):** the `engine` names (`engine.RefusalRules`,
  `engine.ExchangeOther` and the rest) stay until the engine package split,
  before v1.0.0. Each constant is the `recording` one, and each list is the
  same slice.
- `engine.ExchangeKinds`, `engine.ExchangeRoutes` and
  `engine.ExchangeOperations` stay in `engine` and are not deprecated: the
  compiled-in workstreams declare them.
- Nothing the gateway records changes.

## The exchange kinds and routes come from the workstreams (v0.63.0)

- `engine.ExchangeKinds` and `engine.ExchangeRoutes` are derived from the
  workstreams compiled into the gateway: the legs of their catalogs, and their
  ingress routes with the inbound route. The values are unchanged; their order
  is not part of the API, so read them as sets.
- A gateway's ingress diagnostic events are named for its role
  (`provider.ingress.request`, `provider.ingress.response` on a provider, the
  only role with an ingress), as before.

## The relay ownership table is registered by the workstreams (v0.63.0)

- `relay.Check`, `relay.LegOwnership` and `relay.Legs` read the ownership
  table the gateway engine's workstreams register when the engine package
  loads, through the new `relay.RegisterOwnership`. A gateway's table is
  unchanged.
- **Behaviour change for a program that imports `engine/relay` without the
  engine.** No workstream is registered there, so the table is empty:
  `relay.Legs` is nil, `relay.LegOwnership` is empty, and `relay.Check`
  refuses every payload with an `*OwnershipError` that matches both
  `relay.ErrNoOwnershipRegistered` and `relay.ErrOwnershipRefused`. Link the
  engine (`import _ "github.com/SmartHealthNetwork/shn-gateway/engine"`) to
  read the gateway's table.
- `relay.RegisterOwnership` is called once, by the engine. A second call, or
  a call after the table was read, panics.

## A provider's own system sends a consented federated query (v0.63.0)

- **Addition in v0.63.0:** the provider gateway's Da Vinci ingress serves `POST /Task`, the Da
  Vinci CDex task-based data request a data consumer sends a data source
  (<https://hl7.org/fhir/us/davinci-cdex/task-based-approach.html>), under the same SMART Backend
  Services auth as the other ingress routes. The body is a `cdex-task-data-request` Task with one
  `data-query` input.
  - **Carried.** It goes to the facility on the `federated-query` leg exactly as sent, and the
    facility's answer (a facility's gateway answers with the request Task fulfilled with the
    records) is relayed exactly.
  - **Routing.** It is routed by its `owner`, a holder identifier
    (`http://smarthealth.network/ids/holder`), with no default. Refused `422`, saying what is
    missing: an owner naming no holder identifier, a value of another type among them (`no
    facility to route the federated query to: the Task's owner names no holder identifier …`), a
    holder the network does not register, and a holder that is not a facility.
  - **The patient.** `Task.for` is read as the facility reads it, so consent is checked for the
    patient the facility discloses for. Refused `400` (`federated query rejected: …`): a Task that
    is not a data request for one patient's named documents, or that a FHIR reader could read
    another way (a member repeated, or named only in another case, the owner's among them).
  - **Consent.** The Authorization Framework checks consent for that patient with the owner as
    custodian (`403 authorization denied` without it); the facility's gateway then applies its own
    backstop and binds the queried patient to the consented one, and its refusal is relayed
    exactly.
  - **What the network guarantees.** Consent is enforced where the disclosure is made: the
    facility's gateway checks it with its own backstop, binds the queried patient to the
    consented one, and builds the answer it serves from its system of record from that
    patient's records alone, at every level. The network does not guarantee that a facility's
    own system, answering through its gateway, discloses records about one patient only.
  - **The answer.** Relayed exactly; refused `502` at every level when it repeats a member name;
    refused `502` at `strict` (recorded at `observe` and `structural`, relayed at `none`) when it
    reads as about a patient other than the queried one (`RulePatientAnswer`), a best-effort
    check of the answer Task's own patient and of every resource it contains that judges no free
    text or encoded data; refused `502` at `structural`
    and `strict` (recorded at `observe`) when it is not a Task fulfilled with a records Bundle (a
    data-query output naming a Bundle it contains; the answer is not compared with the request); and `$validate`d at the provider's conformance level (a validator the gateway cannot
    reach, or has no lane for, refuses `500` at `strict`).
  - **Recorded.** The exchange record's route is `federated-query`, a new value in
    `ExchangeRoutes`.
- **Not advertised yet.** The ingress `CapabilityStatement` does not list the interaction until a
  later shn-sdk release. The gateway serves it before then.
- **Go API (additive):** `engine.RouteFederatedQuery`; `scenariodriver.Driver.SendFederatedQuery`.

## A released gateway refuses the validator test double (v0.63.0)

- **Breaking (configuration):** a released gateway (the published image, a
  `go install` of `cmd/gateway`, or any untagged `go build`) refuses to boot
  when `SHN_FAKE_VALIDATOR` is set to any value, at every
  `CONFORMANCE_ENFORCEMENT` level and whatever validator is configured beside
  it: `gateway: SHN_FAKE_VALIDATOR is set, but a released gateway has no
  validator test double: refusing to boot rather than appear to validate what
  it never checked (FR-36)`, followed by the supported path. Before v0.63.0,
  `SHN_FAKE_VALIDATOR=1` replaced every validator with a stand-in that checked
  a few PAS and DTR rules and accepted everything else.
- **What to do instead:** unset it, run the IG-loaded `$validate` beside the
  gateway and point `FHIR_VALIDATE_URL` at it. `deploy/bundle/compose.yml`
  ships that pairing, and `deploy/eval` runs a first end-to-end test with a
  real validator. Or, if your gateway is to check no payload, set
  `CONFORMANCE_ENFORCEMENT=none`, which from v0.63.0 needs no validator (the
  next section). Every other level still needs one
  ([CONFIGURATION.md](docs/CONFIGURATION.md#validation-required-to-boot--fr-36-and-conformance-enforcement)).
- The stand-in remains only in SHN's own test builds: a Go test binary, or a
  build tagged `shn_testdouble`, which logs a warning at boot whenever it runs.
- Nothing the gateway relays or refuses changes.

## A gateway at `none` starts without a validator (v0.63.0)

- **Change (configuration):** a gateway at `CONFORMANCE_ENFORCEMENT=none` starts
  with no validator at all: no `FHIR_VALIDATE_URL`, no `FHIR_VALIDATE_URL_<line>`
  for a line it declares, no default validator lane. It logs `gateway: no FHIR
  validator: …` at boot. Before v0.63.0 it refused to boot, as every level still does
  that checks a payload (`observe`, the default, `structural` and `strict`).
- **The boot refusals name it:** with no validator, or a declared line with no
  lane, the refusal at those levels now ends `or set CONFORMANCE_ENFORCEMENT=none to
  check no payload (this gateway then needs no validator)`, and so does the
  `SHN_FAKE_VALIDATOR` refusal (previous section), which still holds at `none`.
- **What it does without one:** it checks nothing, as at `none` with a validator. It
  builds and answers at every line it declares, honors a request that states one, and
  resumes a pended prior authorization at its pinned line. It never translates between
  IG lines: a peer that shares no declared line is carried as sent at its own line, or
  refused as `routing` under `REQUIRE_SHARED_LINE=true`
  ([CONFIGURATION.md](docs/CONFIGURATION.md#running-at-none-without-a-validator)).
- **With a validator, at `none`:** routing is unchanged, except that a declared Compose
  default lane is qualified in the background instead of before traffic, and a line
  the gateway declares is honored and resumed at with or without its lane.
- Nothing changes at any other level.

## A payer's Patient Access API is removed (v0.63.0)

- **Breaking (behavior):** a payer gateway no longer serves the CMS-0057 Patient Access API.
  `GET /metadata`, `GET /ExplanationOfBenefit` and `GET /ExplanationOfBenefit/{id}` answer
  `404` on a payer, as any route the gateway does not serve. Nothing else a payer serves
  changes, and `GET /health` remains every role's liveness.
- **Breaking (configuration):** the gateway no longer reads `AUDIT_URL` or `PHG_URL`, nor
  the `audit` and `phg` endpoints of network discovery; their only use was the Patient
  Access read's audit record and the provider scenario's read of the patient view below.
  `PATIENT_ACCESS_API` (v0.62.0, below) is gone with the routes it switched. A value still
  set for any of the three is read by nothing: the gateway, of any role, boots as before and
  logs one boot warning per setting, `gateway: WARNING: <setting> is set but no longer read:
  shn-gateway v0.63.0 removed …`, naming what the setting served (the Patient Access API,
  its audit record, or the provider gateway's read of the PHG's denial view).
- **Breaking (provider scenario answer):** the UC-08 denial scenario's answer no longer
  carries `patientDenialReason`, `patientDenialReasonDisplay`, `patientDenialReasonSystem`
  or `patientAppeal`, which the provider gateway read from the PHG's denial view. The
  denial itself (`denied`, `rationale`) is answered as before.
- A payer gateway no longer builds or stores a decision ExplanationOfBenefit either (see "A
  payer gateway builds no decision EOB" below).
- The boot refusal for a declared `pa.pdex@2.1` now says a gateway serves no Patient Access
  API and builds no PDex resource, rather than naming the Patient Access API's
  ExplanationOfBenefit.
- **Go API (breaking):** removed `engine.Config.AuditURL`, `engine.Config.PHGURL`,
  `engine.Config.PatientAccessOff`, `engine.ReplayScopePatientAccess`, `scenariodriver.Driver.GetAuthorizations` and
  `scenariodriver.AuthorizationView`. The one-time-use record no longer has a
  `patient-access` scope, so a replay store need not admit it.

## A certification lane that answers slowly keeps its qualification (v0.63.0)

- **Change (behaviour):** a qualified certification lane that does not answer a
  certification within its 2 s, while the collection is still waiting, no longer loses its
  qualification. That certification is still recorded `expired`. The lane is asked for its
  `GET /metadata` once, within 4 s, on a connection of its own, at most once every 30 s, and
  is re-qualified only when that goes unanswered; an answer of any status keeps it
  qualified. Before v0.63.0 every such timeout re-qualified the lane, and the line's
  certifications read `… re-qualifying`, dialing nothing, until a corpus run passed. A lane
  that fails at the connection (refused, reset or closed, or unreachable) re-qualifies as
  before.
- **Addition (log):** `certification_lane_timeout_check`, one line per check, with `version`,
  `line`, `host`, `answered`, `at` and `duration_ms`. A check cut short by shutdown logs
  nothing.

## Translating between IG lines is the participant's opt-in (v0.63.0)

- **Addition (configuration):** `TRANSLATE_IG_LINES` (`true` or `false`; unset means
  `false`, any other value refuses boot), and `engine.Config.TranslateIGLines`. When it is on
  the boot log says `TRANSLATE_IG_LINES=true`.
- **Change (behaviour):** unset, a gateway translates nothing between IG lines. It never
  walks a transform chain (arm 3) and never renames a questionnaire package answer (E-08). A
  pair that shares no line is built natively at a line the peer declares where the gateway
  can (arm 2, unchanged), and otherwise carried as sent and recorded. Before v0.63.0 every
  gateway translated. A deployment that relies on translation sets `TRANSLATE_IG_LINES=true`.
- **Change (behaviour):** with it on, module 2 never plans a chain that takes a gated step
  upward (pa.pas 2.0 to 2.1, whose step refuses every Claim). That pair is carried as sent and
  recorded, or refused as `routing` under `REQUIRE_SHARED_LINE=true`, naming the gated step.
  Before v0.63.0 it was refused `422` at apply time (`semantic-change refusal`). A gated step
  taken downward still plans.
- **Change (behaviour):** with it on, module 2 plans no chain to an explicitly configured lane
  (`FHIR_VALIDATE_URL_<line>`, or `FHIR_VALIDATE_URL` at its own line) that has failed at the
  connection. The leg is carried as sent
  and recorded, or refused as `routing` under `REQUIRE_SHARED_LINE=true`, naming the lane,
  until the lane answers its `GET /metadata` again: every 30 s for an hour, then every 5
  minutes. A lane that only failed to answer in time is asked once, and stays in use if it
  answers. The exchange that finds a lane down after its chain was planned is still refused
  `500`. Before v0.63.0 every such exchange was. The log lines
  `validator_lane_unreachable` and `validator_lane_reachable` are new. Governed checks and
  arm 2 call the lane exactly as before.
- **Change (origination API):** `/scenario/uc03`'s bridge exhibit branch `bridge-refuse` is
  now `line-carry`. Its persona key is `uc03-line-carry` (in `engine.ScenarioRoster` and the
  accounts descriptor); its member and payer are unchanged. Both bridge branches answer with
  `pasRoute`, the PAS leg's route (`engine.RouteInfo`). `bridge-refuse` answers `400`, as
  any unknown branch does.

## A request the gateway carries to the payer's PAS line keeps its builder (v0.63.0)

- **Change (record):** a PAS submission or update the gateway builds and carries to a
  payer's PAS line through the registered transform chain is sealed as the builder's
  message then registered edit E-09 (`registered-transform`). It is recorded as
  `ownership: edited`, with its `builder` and `relayEdits` `["E-09"]`; before v0.63.0
  it was recorded as `authored`, with no edit. The bytes sent are unchanged.
- **Change (behaviour):** before such a request is sealed, the gateway runs the compat
  manifest's own steps again on the request as built. They must make the same bytes
  twice, and only steps the manifest registers may be named. A request that fails
  that check is refused `500`, recorded as a `fidelity` refusal, at every level, and
  nothing is sent. The check at the payer's line still follows, at every level.
- **Go API (additive):**
  - `relay.EditRegisteredTransform` (`E-09`).
  - `relay.Transformed(src, id, out, rederive)` seals a transform's output as `src`,
    an authored payload, then the edit, after running `rederive` twice. Its errors
    are `relay.ErrVerifyMismatch`, `relay.ErrInvalidEdit` and `relay.ErrUnknownEdit`,
    and the scan errors (`relay.ErrInvalidJSON`, a duplicate member name) for
    malformed output.
  - `relay.Payload.Builder` also names the builder of a payload `Transformed` made.
  - `relay.ApplyChanges` refuses E-09 (`relay.ErrInvalidEdit`).
  - The ownership check refuses a transformed payload that names no builder, or one
    that names a builder the transmit does not list, and an edited payload that
    names a builder without a transform.
  - `engine.ExchangeRecord.Builder` and `diagnostics.AccessLine.Builder` name the
    builder of a transformed request too.

## A module ingress client's request is the gateway's own (v0.63.0)

- **Addition (configuration):** an `INGRESS_CLIENTS_FILE` entry may carry
  `"module": "on-behalf"`, registering that client as a module ingress client: a
  module that builds Da Vinci requests on the participant's behalf and sends them
  to the provider ingress. An unknown module, or a `builders` key on any entry,
  refuses to boot. Unmarked clients are the participant's own system, as before.
- **Change (behaviour):** a module client's request names its registered builder in
  the `SHN-Builder` header. It is sealed as the gateway's own message by that
  builder, sent as built, and recorded so. The gateway refuses (`422`, recorded as a
  `fidelity` refusal, at every level, before any leg is sent):
  - a native client that sends `SHN-Builder`;
  - a module client that names no builder, or repeats the header;
  - a builder that is not the module's or does not build the leg's request;
  - a module request the gateway would have to edit.

  A native client's request is carried as before.
- **Addition (record):** `engine.ExchangeRecord.Ownership` (`relayed`, `edited`,
  `authored`; the closed list `engine.ExchangeOwnerships`) and `Builder`: whose
  message the request the gateway sent on the leg was. They are on the access line as
  `ownership` and `builder` (omitted when empty). They are Evolving
  until the network's Audit Plane carries the same content-free record in the
  v1.0.0 signable-content change.
- **Go API (additive):** `engine.BuilderHeader`, `engine.ModuleOnBehalf`,
  `engine.ModuleIDs`, `engine.ModuleBuilders`,
  `engine.IngressClientRegistration.Module`, `engine.ExchangeRecord.Ownership` and
  `Builder`, `engine.ExchangeOwnerships`, `engine.Content.OnBehalf`,
  `diagnostics.AccessLine.Ownership` and `Builder`.

## Each body the gateway transmits is recorded on its own (v0.63.0)

- **Addition (record):** `engine.ExchangeRecord.AnswerOwnership` and `AnswerBuilder`
  (the closed lists `engine.ExchangeOwnerships` and the registered builders): whose
  message the answer the gateway wrote on the call was, as `Ownership` and `Builder`
  say of the request. Every refusal the gateway writes is `authored` by
  `gateway-refusal`, one written before any leg was opened included.
- **Addition (record):** `engine.ExchangeRecord.RequestEdits` and `AnswerEdits`: the
  ids in `Edits`, split by the body each edit was made to. `Edits` is unchanged.
- **Addition (access line):** `answerOwnership`, `answerBuilder`, `requestEdits` and
  `answerEdits` (omitted when empty).
- All of these are Evolving until the network's Audit Plane carries the same
  content-free record, one for each body, in the v1.0.0 signable-content change.
- **Go API (additive):** `engine.ExchangeRecord.AnswerOwnership`, `AnswerBuilder`,
  `RequestEdits` and `AnswerEdits`; `diagnostics.AccessLine.RequestEdits`,
  `AnswerOwnership`, `AnswerBuilder` and `AnswerEdits`.

## A pended request's follow-up is carried when its pinned line has no reach (v0.63.0)

- **Change (behaviour):** an amendment or inquiry the gateway builds itself
  about a pended prior authorization, on its participant's behalf, whose pinned
  line nothing reaches any more (its lane removed,
  the line no longer declared, or, after a restart, a default lane not yet
  qualified again) is built at a line the gateway declares (one the payer also
  declares, else the gateway's highest) and carried as sent to the pend's payer,
  about the same pend, at every conformance level. An amendment's attached
  QuestionnaireResponse is built at the DTR line paired with that line. Before
  v0.63.0 it was refused `422 … no bridge to the pinned line … remains
  available`. An amendment's pend is held in memory and does not outlive a
  restart, so the restart cases reach the inquiry about a continuation.
- **Not affected:** your system's own amendment (`POST /Claim/$submit`) and
  inquiry (`POST /Claim/$inquire`) are carried exactly as sent, as before; they
  carry no pin and are never rebuilt at another line.
- **Recorded:** the observer event `contract.line-carried`, whose detail names
  the pinned line, and the route's `pinned` on `leg.originated`.
- **The opt-in:** `REQUIRE_SHARED_LINE=true` refuses it as before, as `routing`.
  A pend or payer that cannot be resolved is refused as before, at every level.
- **Go API (additive):** `engine.RouteInfo.Pinned`.

## A PAS submission under another patient's correlation id is carried (v0.63.0)

- **Breaking (behavior):** a payer gateway's PAS submit leg no longer refuses a
  submission whose correlation id another patient's authorization already uses. It is
  carried to the payer and its answer relayed, at every conformance level, like any other
  submission. The `409 correlation id already names another patient's authorization` and
  the `502 holder read failed` the leg answered, before forwarding, when its store could
  not say whether the id was taken, are no longer answered.
- **Why:** nothing is shared between two patients' authorizations under one correlation
  id. The ledger keys each authorization by the payer's own binding of the member the
  request names as well as the correlation id, an amendment binds only its own patient's
  pend, and an inquiry matches on the identifiers the payer issued and records nothing for
  another patient (`pend.other-subject`). The refusal named no network primitive: the
  requester's own id, sealed afresh and carrying its own authority, is routed by the Hub
  on purpose.
- **Store outage:** the submit leg reads nothing from the store before forwarding. A store
  that cannot be written when the answer comes back is reported as `pa.local-write-failed`,
  and the payer's answer is still relayed.
- **Go API:** the gateway no longer calls `engine.EOBOwnerLookup`,
  `engine.PendCorrelationLookup` or `engine.DecisionEOBID`, and v0.63.0 removes them with
  the built-in stores' implementations (see "The gateway store's EOB API is removed"
  below).
- The v0.60.0 and v0.52.0 entries below that describe the refusal stay as the history of
  those releases.

## A payer gateway builds no decision EOB (v0.63.0)

- **Breaking (behavior):** a payer gateway no longer builds, validates or stores a decision
  ExplanationOfBenefit when it records the payer's decision, on the PAS submit leg or from an
  inquiry's answer. The decision itself is recorded on the pend ledger as before (decided,
  with the payer's outcome and date), and the payer's answer is relayed exactly as before.
  The store's ExplanationOfBenefit table is no longer written or read. Nothing removes a row
  an earlier release wrote: those rows stay until a later release drops the table on first
  boot (see "The gateway store's EOB API is removed" below).
- **Breaking (observer events):** `pa.decision-eob-not-written`, `pend.inquiry-eob-withheld`
  and `pend.eob-removed` are no longer emitted. `pend.decision-not-recorded` is no longer
  raised with the detail `eob id belongs to another patient`: a decision under a correlation
  id another patient's decision already used is recorded on its own patient's row (see "A PAS
  submission under another patient's correlation id is carried" above). The details of
  `pa.local-write-skipped` and of `pend.decision-not-recorded` for a denial that states
  `preAuthRef` now end `no pend or decision written` and `no decision written`.
- **Breaking (injected responders):** a `LegResponder` no longer returns side-effect FHIR for
  the gateway to validate and fence (`LegResult.SideEffectFHIR` is gone). A responder records
  what it keeps through its `Commit`, as the built-in responder does.
- **Behavior (observe):** at observe, every governed FHIR check now runs off the request path
  (queued, its finding emitted when the validator answers) except a bridged payload's, which
  refuses at every level. The decision EOB check was the one check that stayed on the request
  path at observe, under a 2-second budget; that budget is gone with it.
- **Go API (breaking):** removed `engine.DecisionEOBNotWrittenEvent`,
  `engine.PendInquiryEOBWithheldEvent`, `engine.PendEOBRemovedEvent` and the field
  `engine.LegResult.SideEffectFHIR`. The store's EOB API goes too, in the next section.
- The v0.62.0 and older entries below that describe the decision EOB stay as the history of
  those releases.

## The gateway store's EOB API is removed (v0.63.0)

- **Behavior:** the gateway store no longer reads or writes its ExplanationOfBenefit table.
  The Postgres store still creates the table, empty, on a database that lacks it, and
  nothing removes a row an earlier release wrote: those rows stay until a later release
  drops the table on first boot. Nothing a participant sees changes: with the Patient
  Access API, the correlation-id refusal and the decision EOB gone (above), no route,
  answer or event read the table.
- **Inquiry match:** an inquiry's pend match no longer weighs which authorization the
  inquiry's own lines (their trace numbers and authorization numbers) name. That told only
  the decision EOB what to state; the match and its verdict are unchanged.
- **Go API (breaking)**, as gorelease reports it against v0.62.0:
  - `engine`, removed: `Store.RecordEOB`, `Store.EOBsForPatient`, `Store.EOBByID`,
    `EOBRecord`, `ErrPendEOBInvalid`, `ErrEOBSubjectMismatch`,
    `DecisionEOBID`, `EOBOwnerLookup`, `PendCorrelationLookup`, `PendEOBStale`,
    `PendEOBWritable`, `PendAboutKeys`, `PendMatch.About`, `PendTransition.EOBRemoved`,
    `(*MemStore).RecordEOB`, `(*MemStore).EOBsForPatient`, `(*MemStore).EOBByID`,
    `(*MemStore).EOBOwner` and `(*MemStore).PendedForOtherSubject`.
  - `engine`, changed: `PendLedger.RecordDecision` and `(*MemStore).RecordDecision` lose
    their last parameter, `eob *EOBRecord`: `RecordDecision(subjectPCI, corrID, outcome
    string, decidedAt time.Time, k PendKeys) (PendTransition, error)`.
    `PendLedger.LookupPended` and `(*MemStore).LookupPended` lose `about []PendKeyRef`:
    `LookupPended(requesterHolder string, k PendKeys) (PendMatch, error)`.
    `ResolvePendMatch` loses `about []PendKeyRef` and `aboutHolders []PendCandidate`:
    `ResolvePendMatch(stated PendKeys, candidates []PendCandidate, held []PendKeyRef,
    echoed bool) PendMatch`.
  - `connectors/pgstore`, removed: `(*PgStore).RecordEOB`, `(*PgStore).EOBsForPatient`,
    `(*PgStore).EOBByID`, `(*PgStore).EOBOwner` and `(*PgStore).PendedForOtherSubject`;
    changed: `(*PgStore).RecordDecision` and `(*PgStore).LookupPended`, as on `MemStore`.
  - Renamed: `engine.PendEOBWritable` → `engine.PendDecisionKept`, with the same parameters
    and result. It decides whether the decision the ledger keeps after a write is this
    answer's, so whether its authorization number is indexed (`DecisionPendKeys`).
- **Go API (additive):** `engine.PendDecisionKept`.
- **Migrating:** a third-party ledger (an implementation of `engine.PendLedger`) drops the
  `eob` argument from `RecordDecision` and writes or removes no EOB, drops the `about`
  argument from `LookupPended` and no longer reads which authorizations hold those keys,
  calls `engine.PendDecisionKept` where it called `engine.PendEOBWritable` and stops calling
  `engine.PendEOBStale`; it, and any other caller of `ResolvePendMatch`, calls
  `engine.ResolvePendMatch(stated, candidates, held, echoed)`, the old call without its
  `about` and `aboutHolders` arguments. Nothing replaces them: they only set
  `PendMatch.About`, which only the decision EOB read, and the verdict is reached as before.
  A custom `Store` need no longer implement `RecordEOB`, `EOBsForPatient` or `EOBByID`.
  A ledger left on the old signatures still compiles, because the gateway finds the ledger
  by a runtime type check on the store, but it no longer implements `engine.PendLedger`, and
  the PAS legs then record through the ledger-less fallback without saying so. Add
  `var _ engine.PendLedger = (*YourStore)(nil)` beside the implementation, so the compiler
  refuses the old signatures.
- The "Breaking in v0.60.0" and "Breaking in v0.52.0" entries under "Supported seams"
  below, which tell a custom ledger to call `PendEOBWritable`, `PendEOBStale` and
  `ResolvePendMatch` with `about`, or describe `EOBOwnerLookup`, `PendCorrelationLookup`
  and `ErrEOBSubjectMismatch`, stay as the history of those releases.

## The validator image loads no PDex IG (v0.63.0)

- **Breaking (deployment):** the validator image built from v0.63.0's or a
  later `deploy/validator` no longer loads the Da Vinci PDex 2.1.0 IG, and its
  readiness corpus no longer validates a PDex ExplanationOfBenefit. The corpus
  is 41 rows, three of them profile-resolution rows. No gateway builds a PDex
  resource from v0.63.0, and none declares a PDex contract line from v0.62.0.
- **Upgrade the gateway before the image, or with it.** A gateway qualifies a
  default validator lane by running the same corpus before it routes to it, and
  qualifies a 2.1 or 2.2 lane given by address (`FHIR_VALIDATE_URL_<line>` or
  `FHIR_CERTIFY_URL_<line>`) the same way before it certifies on it. A v0.62.x
  gateway still runs the PDex row, which fails against an image built from
  v0.63.0, so:
  - a default lane it declares fails its boot, and any other default lane never
    becomes ready;
  - a lane given by address still routes, but its certification never
    qualifies, so no certification evidence is collected for that line.

  A v0.63.0 gateway qualifies an earlier image, which still loads the IG,
  because it asks one row fewer.
- `verify.sh` now expects 274, 292 and 1030 indexed StructureDefinitions on the
  2.0, 2.1 and 2.2 lines (37 fewer each).
- Nothing a gateway relays or refuses changes.

## A payer's Patient Access API can be turned off (v0.62.0)

- `PATIENT_ACCESS_API=off` on a payer gateway leaves its Patient Access API unmounted:
  `GET /metadata` and `GET /ExplanationOfBenefit[/{id}]` answer `404`, and nothing else
  changes. Unset or `on`, the default, serves it as before. Any other value, or any value on
  another role, refuses to boot.
- `GET /health` answers on every role either way. It is the gateway's liveness, and it never
  depends on the Patient Access API.
- Temporary: the Patient Access API is being removed in a later release, and this setting goes
  with it.
- **Go API (additive):** `engine.Config.PatientAccessOff`; its zero value serves the routes.

## One patient-match rule on eligibility, federated query and patient-authored DTR (v0.62.0)

- **Behavior change in v0.62.0 (eligibility):** a payer's gateway no longer
  compares the patient an eligibility leg's token names with the member the
  request names. A request whose token names another patient was refused
  `403 token subject does not match request patient` at every level when the
  gateway answered from its system of record. It is now answered about the
  member the request names, at every level, as on the CRD, DTR, PAS and inquiry
  legs and on a payer that declares its own eligibility endpoint. The gateway
  emits `subject.binding-differs`, and, when the Hub reads the involved list,
  names its own binding of the member on the answer (`payer-held`, or
  `payer-derived` for a member a payer that declares its own eligibility
  endpoint does not hold), on both eligibility paths. A member the system of record does not hold is still
  answered `400 unknown member`.
- **Behavior change in v0.62.0 (federated query):** a queried patient the
  facility's gateway does not hold, and one whose identifier is not the
  token's patient's, are refused alike at every level: `403 the queried patient
  cannot be matched to the patient the leg's consent covers` (was `400 unknown
  member` and `403 token subject does not match queried patient`), so the
  refusal does not say whether the facility holds the patient. A member whose
  records the facility's system answers out of shape (two Patients for one
  member, say) gets the same answer (was `502 system of record returned an
  invalid response`), and the exchange record keeps that system's failure; an
  outage still answers `503`. The exchange record names the refusal's rule as
  consent (was authority).
- **Behavior change in v0.62.0 (patient-authored DTR):** the same for the
  patient the PHG's gateway would sign as: `403 the patient cannot be matched
  to the patient the leg's authority names` (was `400 unknown member` and `403
  token subject does not match patient`), recorded as authority, and the same
  for a member whose records are out of shape. A request
  naming no patient is refused `400 patient-dtr request names no patient` (was
  `400 unknown member`).
- No Go API changes.

## A patient reference is read in every form on eligibility and federated query (v0.62.0)

- **Behavior change in v0.62.0:** a coverage-eligibility request's `patient`,
  and a federated query's `Task.for` and its query's `patient` parameter, are
  read in every reference form: relative (`Patient/<id>`), absolute
  (`https://…/Patient/<id>`, with or without `/_history/<version>`) and
  contained (`#<id>`, naming the contained Patient, whose member is the one
  `urn:shn:member` identifier it carries; its id is local to the request and
  is never read as a member). Before, an absolute or
  contained reference was read as a literal member id on eligibility (`400
  unknown member`, or a binding derived from the literal on a payer's own
  endpoint) and refused on federated query (`403 query rejected: cdex:
  Task.for missing/invalid`, or a query patient that did not match Task.for).
  The bytes carried are unchanged.
- Only a well-formed reference is resolved: an optional `http(s)` base, then
  `Patient/<id>` with a FHIR id, then an optional `/_history/<version>`; or
  `#<id>`. A contained reference that names no contained Patient, or more
  than one (an id the request does not carry or carries twice, a contained
  resource that is not a Patient, a contained Patient with no single member
  identifier), names no patient and is refused: `400 parse member failed` on
  eligibility, where a payer forwarding to its own endpoint carried it before;
  `403 query rejected: …` on federated query, before any record is read. On
  eligibility a contained Patient's member identifier is read whatever its
  shape, as an identifier value; the federated query writes only one in a
  FHIR id's shape into its query, and leaves any other as sent, refused as
  before. Any other reference is read as before: on eligibility as the
  member that follows `Patient/` (a bare id, an id outside a FHIR id's shape,
  a trailing path); on federated query, refused as before (a trailing path
  or operation, an escaped character, a percent-encoded reference, another
  resource type).
- A request this gateway cannot read strictly (a repeated member name, one
  that matches a FHIR name only by case folding, or another shape the strict
  reader does not accept) is read exactly as before, so no two readers of it
  see two patients and nothing it answered before is refused. From v0.63.0
  an eligibility request that repeats a member name, or names one only in
  another case, is refused ("An eligibility request is read one way only").
- No Go API changes.

## A pair that shares no IG line is carried by default (v0.62.0)

- **Behavior change in v0.62.0 (requests):** a leg to a peer that shares no
  declared line, and that no native reach or transform chain reaches, is
  carried at this gateway's own highest declared line, exactly as built,
  where it was refused `422 no shared contract line …`. This includes a PAS
  Bundle carried from a provider's own system. A CDS Hooks request from a
  provider's own system whose walk to the payer's line would change it is
  carried as sent, at the line it is in (was `502 the CDS Hooks request
  cannot be carried to the payer's line unchanged`). A payer gateway forwards
  to its own Da Vinci system when that system's declared lines
  (`PAYER_DAVINCI_CONTRACT_VERSIONS`) share none with its own (was `422`).
- **Behavior change in v0.62.0 (claims and stamps):** a request frame
  claiming a line this gateway cannot build or validate at is answered as a
  bare request is (was `422 request declares contract version …`), and a
  claim on a version-neutral leg is ignored (was `422`). A 2xx answer stamped
  with another line than the leg routed is relayed (was `502 … response
  contract version mismatch …`).
- **Behavior change in v0.62.0 (recorded):** each carry raises the observer
  event `contract.line-carried` (`engine.LineCarriedEvent`), whose detail
  names the lines, and the exchange record's `LineCarried` lists where it
  happened (`request`, `answer`); the access line carries it as
  `lineCarried`. A line a peer's frame states is named in them only in a
  token's shape. A walk to the peer's line that is not applied records no
  `leg.transformed` and no transform Provenance.
- **The participant's setting:** `REQUIRE_SHARED_LINE=true`
  (`engine.Config.RequireSharedLine`; `engine.WithRequireSharedLine` on the
  native responder) refuses each of these as before, except a request whose walk
  to the payer's line would change it, which it refuses `422 … cannot be carried to
  the payer's line unchanged` (was `502`). Where the call keeps an
  exchange record (a Da Vinci ingress or an inbound leg), the refusals are
  recorded as `routing`, the answer-stamp refusal included (v0.61.0 recorded
  it as `other`). A claim on a version-neutral leg is ignored either way. It
  never changes the line a pair that shares one routes at.
- Unchanged: a gated transform chain's refusal to make a lossy edit of a
  message this gateway builds (`422`, the fidelity of its own edits), and a
  pend's follow-up, which is never re-selected: it goes at the pend's line,
  or is refused when nothing reaches that line.
- **Go API (additive):** `engine.Config.RequireSharedLine`,
  `engine.WithRequireSharedLine`, `engine.LineCarriedEvent`,
  `engine.LineCarriedRequest`, `engine.LineCarriedAnswer`,
  `engine.LineCarriedValues`, `engine.ExchangeRecord.LineCarried`,
  `diagnostics.AccessLine.LineCarried`.

## A requester the answering gateway does not know yet (v0.62.0)

- **Behavior change in v0.62.0 (inbound):** the answering gateway resolves a
  leg's requester in its registry before acting on the leg, right after the
  leg's authority verifies. On a miss it re-reads the registrar feed once
  (`Config.RegistryRefresh`; one read shared by concurrent legs, bounded to
  2 seconds). A requester still unknown is refused before any handler runs,
  at every conformance level, with `503 {"error":"requester not yet in this
  gateway's registry"}` (uncacheable), `Retry-After: 3` and
  `X-SHN-Not-Acted: requester-unknown`: nothing is adjudicated or sent and no
  decision is recorded; the exchange record keeps the refusal, as `routing`.
  It used to act on the leg and then answer `500 requester not in
  registry`, which the Hub relayed as a recipient `5xx`. Every refusal ahead
  of it (the Hub assertion, the envelope, the authority) is unchanged.
- `X-SHN-Not-Acted` is reserved: the gateway removes it from every other
  answer on its inbound route, so neither a handler nor a connector can state
  on the gateway's behalf that it took no action.
- **Behavior change in v0.62.0 (origination):** a Hub `503` not delivered that
  relays `X-SHN-Not-Acted: requester-unknown` (the recipient's gateway did not
  know this one yet) is answered to the caller as this gateway's own `503`,
  uncacheable, with the relayed `Retry-After` (a whole number of seconds from
  1 to 60, else 3) and `the recipient's gateway has not read this gateway's
  registration yet and took no action on this leg; resend after <n>s` (an
  OperationOutcome with code `transient` on a FHIR operation route). It
  speaks for that leg: on a route that sends several legs, the earlier ones
  may have been answered. The leg is `unreachable`, as every Hub refusal not
  delivered is.
- **Go API (additive):** `engine.Config.RegistryRefresh`, which the
  application layer sets to its registrar feed read when a registrar is
  configured; `engine.NotActedHeader` and `engine.NotActedRequesterUnknown`.

## Refusal texts name the condition, and whether the payer may have acted (v0.62.0)

- **Behavior change in v0.62.0 (refusal texts):** the statuses are unchanged;
  a client that matches these texts exactly should match the new ones.
  - A PAS Bundle with no Coverage entry at all: `400 PAS bundle carries no
    Coverage` (was `PAS bundle missing Coverage.beneficiary`, which a Coverage
    with no beneficiary keeps). The shn-sdk Responder says the same from
    the SDK release after shn-sdk v0.62.0.
  - An answer that repeats a JSON member name, refused at every level, names
    the member and the object that repeats it (at most 64 characters of each
    name; a name that is not a plain identifier is quoted, `["a.b"]`):
    `502 payer CRD response repeats the JSON member "<name>" in <where>: it
    can be read two ways, so it is not carried` (was `payer CRD response is
    not a valid CDS Hooks response: response.json`); the same for `payer PAS
    response` (was `engine: invalid native PAS response Bundle: invalid or
    incomplete PAS response graph`), followed by `; the payer's system
    received and answered this request: check its outcome before resending`
    whichever gateway refuses it; for `payer inquiry answer` (was `engine:
    invalid prior-authorization inquiry answer`); `403 response repeats …` (was
    `response repeats a member name`); and `502 next-question response repeats
    …` (was `next-question response is not a questionnaire-response`).
  - An answer whose frame declares another contract line than the leg was
    routed at: `502 the recipient received this request and answered, but its
    answer could not be accepted (response contract version mismatch: frame
    declares <token>, leg routed <token>); it may have acted on the request:
    check its outcome before resending`, on every provider ingress (was the
    bare `response contract version mismatch: …`), when the participant sets
    `REQUIRE_SHARED_LINE=true`; by default the answer is relayed (see "A pair
    that shares no IG line is carried by default"). The exchange record counts
    the refusal on routing.
  - The payer gateway's own failure (was `500 responder failed`) says whether
    it had forwarded the request to the payer's system: `the payer's gateway
    failed before forwarding this request to the payer's system`, `… after
    forwarding this request to the payer's system, which may have acted on it:
    check its outcome before resending`, or, when it cannot tell (a connector
    that calls its system itself, one wrapping the native responder
    included, or a gateway that keeps no exchange record: no access log and
    no metrics), `the payer's gateway failed, and whether the payer's system
    received this request is not known: check its outcome before resending`.
- No Go API changes.

## A questionnaire package answer crosses a DTR line under its requester's names (v0.62.0)

- **Behavior change in v0.62.0 (relay):** a provider's gateway that declares
  exactly one DTR line renames the line-specific parameter names of a payer's
  `$questionnaire-package` answer to its own line's before the answer reaches
  its participant's system. DTR 2.0.1 and 2.1.0 name the package
  `PackageBundle` and the outcome `operationOutcome` (the output profile's
  name); DTR 2.2.0 names them `packagebundle` and `outcome`. Toward 2.2.0 both
  are renamed; toward 2.0.1 or 2.1.0 only the package is (`outcome` is every
  line's OperationDefinition name). `return` is never renamed. Each package
  QuestionnaireResponse crosses the lines through the registered `pa.dtr`
  compatibility steps along the manifest's chain (its Coverage between
  `qr-context` and `qr-coverage`, its intended-use code system, its origin
  code), each step reported under its own module on a `leg.transformed`
  event with the transform Provenance, and is spliced back into the payer's
  own bytes; every other byte is the payer's. This is a new registered edit,
  E-08 (`dtr-answer-line-names`),
  on the `dtr-questionnaire-fetch` answer a requester's gateway gives its own
  participant. It is made only when every line-specific name in the answer
  belongs to the other line, and the answer is valid at its own line against
  the DTR output Parameters profile. That check runs at every level as the
  edit's own precondition, and records nothing.
- **New refusal, at every level:** the renamed answer must be valid at the
  requester's line (a bridged check, `fhir-bridged`). An answer that is not is
  refused, its text saying the payer answered but its answer cannot be carried
  into the gateway's DTR line: a DTR 2.0.1 package with no QuestionnaireResponse
  (required at 2.2.0), several outcomes toward 2.2.0 (which allows one), or a
  Questionnaire, Library or ValueSet whose content differs by line, which no
  step rewrites. A registered step that refuses the QuestionnaireResponse
  refuses the answer the same way. The gateway never fills. The finding names the edit (`edit`) and carries the
  SHA-256 of the payer's answer (`sourcePayloadSha256`) beside that of the
  renamed answer (`payloadSha256`); the leg is recorded `error`.
- **Behavior change (observe and above, cross-line answers only):** an answer
  that is not valid at its own line is not renamed. It is carried as sent, and the payer-answer check
  reports it by level, as for the gateway's own DTR fetch: nothing at none,
  a finding at observe, decided by its defect at structural, refused at
  strict. Before, such an answer was carried unchecked at every level, as an
  answer in the requester's own line names still is.
- **Unchanged:** a gateway that declares several DTR lines carries every
  answer exactly, since it cannot know which names its participant's system
  reads; so does a gateway whose answer is already in its own line's names.
- **Additive (access line):** `diagnostics.AccessLine.AnswerLine` (JSON
  `answerLine`, omitted when empty) is, on an ingress call, the contract token
  of the IG line the answer the gateway wrote back is written in. From v0.62.0
  the provider's DTR ingress records it: the routed line when it carried the
  payer's answer as sent, its own line when E-08 carried it across. Evidence
  then judges the answer at the line it is written in, not the line its leg was
  sealed at (`engine.ExchangeRecord.AnswerLine` carries it to the line).
- **Go API:** `relay.EditDTRAnswerLineNames` (`E-08`) is new, and the ownership
  table admits it on the `dtr-questionnaire-fetch` requester answer.
  `engine.ConformanceFinding` gains `Edit` and `SourcePayloadSHA256` (both
  omitempty), set only on the check of a payload a registered edit changed.
  `engine.LineFakeValidator` checks the DTR output Parameters profile's package
  and outcome cardinalities, and reads a wrapped `$validate` request as HAPI
  does: it judges the wrapped resource when no profile is passed, and the
  wrapper when one is.
- **Fix (validation):** a check of a `Parameters` against an explicit profile
  now judges the `Parameters` itself. It used to send the wrapped request with
  `?profile=`, which HAPI reads as naming the wrapper, so it judged the
  wrapper. The check now asserts the profile as the wrapped copy's own
  `meta.profile`. Only the request to the gateway's own validator changes. No
  earlier check of a `Parameters` passed an explicit profile.

## A decision EOB states only what the payer's answer and the request supply (v0.62.0)

- **Behavior change in v0.62.0:** the ExplanationOfBenefit a payer gateway records for a
  payer's prior-authorization decision (the Patient Access record, never sent to a peer)
  reads every element from a source, carried as written: `type`, `provider`, `insurance`
  (`focal`, `coverage`), the item's `sequence` and `net` from the request Claim (on an
  inquiry, the inquiry's Claim); `created`, `insurer` and `outcome` from the ClaimResponse
  that carried the decision. Before, it wrote `type` `professional`, the gateway clock as
  `created`, `Organization/payer`, `Practitioner/reviewer-uc08`, `Coverage/<member>` with
  `focal` true, `outcome` `complete`, item `sequence` 1 and a `submitted` `0 USD`
  adjudication.
- **The item's adjudication is the payer's own:** the ClaimResponse's `item.adjudication`
  entries for that item, each with its category, its amount only where the payer stated
  one, and its reasons value for value (the EOB re-marshals them, so `<`, `>` and `&` may
  be escaped; no value changes). The payer's review action (PAS
  `extension-reviewAction`, defined only for ClaimResponse and Claim) is re-expressed value
  for value in `http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/extension-reviewAction`,
  the one defined for the ExplanationOfBenefit adjudication context, through one registered
  table (`extension-reviewActionCode`, `number`, `reasonCode`, `secondSurgicalOpinionFlag`;
  every value copied exactly). A payer sub-extension that table does not map writes no EOB
  and is named in `pa.decision-eob-not-written` by its position only ("decision EOB not
  written: review action sub-extension 2 has no counterpart in the EOB review action"); no
  finding, event or log line echoes a value the payer stated. A review action on the
  ClaimResponse's header `adjudication` or on an `addItem` for the item is carried onto the
  EOB's header `adjudication` and `addItem` the same way. The EOB item is the request item
  that codes the order (on an inquiry, the line whose trace number the payer echoed), its
  `productOrService` carried as that item wrote it; the payer's adjudication paired with it
  is only that item's (on an inquiry, the ClaimResponse item carrying the same trace
  number), never another item's. A request with no item coding the order writes no EOB, and
  so does a decision no payer item answers that request item with: on submit, no
  ClaimResponse item states that item's sequence; on an inquiry, no ClaimResponse item echoes
  the trace number of the line (an answer that echoes none pairs with no line). An EOB that
  carried the request item without the payer's decision on it would read as an approval
  whatever the payer decided, so the decision is recorded without one and
  `pa.decision-eob-not-written` names the position only ("no item of the payer's
  ClaimResponse answers the sequence of the request Claim item that states the decision's
  product coding"; "… carries the item trace number of the inquiry line the decision
  answers"). No
  code system is defaulted, and a `processNote` carries its `number` only when the payer
  stated one. The request item's `net` is added as a
  `submitted` adjudication only when the payer stated no `submitted` entry of its own;
  with neither, the item carries no adjudication. No amount, review action (no `A3`),
  reason code or note is ever added.
- **`preAuthRef` is the payer's own authorization numbers as stated:** its
  `ClaimResponse.preAuthRef`, then each review action `number` on the item's adjudication,
  on the header adjudication and on each carried `addItem`'s adjudication, each once,
  whatever the decision. `outcome` is the ClaimResponse's.
- **Which `addItem`s are carried:** a payer `addItem` with no `itemSequence` (the claim as a
  whole), or one whose `itemSequence` names the request item (by its own sequence, or by
  the sequence of the payer item that answered it). The EOB carries that one item under the
  request item's sequence, so an `addItem` tied to it that also names any other sequence
  writes no EOB, and `pa.decision-eob-not-written` names it by position ("addItem 1 of the
  payer's ClaimResponse names an item sequence the EOB does not carry"); an `addItem` for
  another item is not this decision's. R4 names `ClaimResponse.addItem.subdetailSequence`
  `ExplanationOfBenefit.addItem.subDetailSequence`: the member is carried under the EOB's
  name, its value as written.
- **The EOB claims no profile, and the Patient Access CapabilityStatement no longer
  claims PDex:** it drops `supportedProfile` `pdex-priorauthorization` for
  `ExplanationOfBenefit` and its `implementationGuide` (the PDex 2.1.0 canonical). PDex
  2.1.0's closed adjudication slicing requires an amount, a reason or units on every
  adjudication, so it cannot carry a payer's amountless decision (the reference payer's
  `submitted` entry carrying only its review action) without an amount the payer never
  stated. The EOB is base FHIR R4; it uses the PDex review action extension only because
  that is the one defined for its adjudication context, and carries no `meta.profile`. A
  Patient Access client that selected EOBs by that profile or guide should read them as
  base R4 ExplanationOfBenefit. No gateway declares the `pa.pdex@2.1` contract line
  either (next section).
- **Partner-visible loosening: a payer decision this gateway used to refuse with
  502 is now relayed, at every conformance level.** The decision is never refused over this
  gateway's own record of it:
  - a decision whose detail the EOB cannot carry as written (a `processNote` typed outside
    `NoteType`, an unmapped review action sub-extension) is relayed exactly and recorded
    without an EOB, with `pa.decision-eob-not-written` naming the detail. Before, strict
    answered 502 and lower levels relayed it and wrote nothing.
  - a decision that reads as a denial yet states a top-level `ClaimResponse.preAuthRef`
    (possibly a partial certification) is relayed exactly on `$submit` and on the amended
    re-POST, and on `$inquire` the same; nothing is written from it (no decision, no EOB),
    and the gateway emits `pend.decision-not-recorded` saying so. Before, strict answered
    502. The decision reader still reads the reference payer's own denial shape (`A2` with
    no review action `number`) with a top-level `preAuthRef` as a denial, so that answer is
    relayed and nothing is written (that reading stays open).
  - review reasons in any code system, a denial reason in the payer's own system, an `A6`
    review action, and reason codes on an approval are carried on the EOB as the payer
    stated them (before, the first and last were refused).
  - a decision EOB that fails its own validation no longer refuses the payer's decision at
    strict (before: 422, or 500 when the validator could not answer). The EOB is this
    gateway's side record, neither the participant's payload nor an edit on the wire, which
    are what strict governs. At every level that checks it, and whether the payer's answer
    was relayed or produced by this gateway, the decision is relayed exactly and recorded,
    the EOB is not written, the EOB's validation finding is recorded as relayed, and
    `pa.decision-eob-not-written` names the failure by its first issue's code, message id
    and expression ("decision EOB not written: it fails its own validation (...)"), or says
    its validation could not run, which leaves the EOB unwritten only at `strict`: below
    `strict` that EOB is written, and at every level the check is recorded as an
    `unavailable` finding. On `$inquire` each decision stands on its own: one EOB not written never
    keeps the answer's other decisions or their EOBs. Strict still refuses a participant's
    own non-conformant payload as before, during a validator outage included.
- A provider, insurer or coverage reference is stated by `Reference.identifier` when the
  source reference carries one or the resource it names in the source Bundle does (an NPI
  first, then a member identifier, else its first identifier), with the source's `display`;
  otherwise by the literal reference as written.
- An element the EOB requires that its source does not state (an unreadable request Claim;
  a request Claim without `type`, `provider`, `insurance` or the item's `sequence`; a
  ClaimResponse without `created`, `insurer` or `outcome`) writes no EOB. The decision is
  still relayed exactly and recorded, at every conformance level, and the gateway emits
  `engine.DecisionEOBNotWrittenEvent` (`pa.decision-eob-not-written`), whose detail names the
  element and its source. No Go API is removed; `engine.DecisionEOBNotWrittenEvent` is new.

## No gateway declares `pa.pdex@2.1` (v0.62.0)

- **Breaking (configuration), upgrade step:** remove `pa.pdex@2.1` from
  `SHN_CONTRACT_VERSIONS` before upgrading (on a hosted tenant, before rolling it to
  v0.62.0). A gateway whose declared set still names it refuses to boot: `gateway:
  SHN_CONTRACT_VERSIONS: contract-version token "pa.pdex@2.1" is no longer declared by
  any gateway: the Patient Access API's decision ExplanationOfBenefit is base FHIR R4 and
  does not meet Da Vinci PDex 2.1, so a declaration must not claim it: remove it from the
  declared set (SHN_CONTRACT_VERSIONS)`. The set without it boots on an earlier release
  too, so the step can be taken first. A hosted tenant's saved `SHN_CONTRACT_VERSIONS`
  takes the same step before the tenant is rolled to v0.62.0. SHN's own platform gateways (the
  bridge demo gateways and the provider test gateway) run the image built from the merge, so their declared sets drop the line in
  this same change. The refusal is the declared-set rule a gateway
  already applies (a declared token must be one this build can honour): a published
  declaration must not claim a conformance the gateway's output does not meet.
- The decision EOB is base FHIR R4 and the Patient Access CapabilityStatement claims no
  PDex (previous section), so the line no longer describes anything a gateway does. It
  leaves the native set and the build default: unset, `SHN_CONTRACT_VERSIONS` declares
  `pa.crd@2.0, pa.dtr@2.0, pa.pas@2.0`, and a payer that derives its declaration from
  `PAYER_DAVINCI_CONTRACT_VERSIONS` declares that system's CRD, DTR and PAS lines and
  nothing else (a PDex line the system names is still not a forwarded line). Discovery's
  `bridgedContractVersions` is the SDK's native set, so it names `pa.pdex@2.1` until the
  SDK release after shn-sdk v0.62.0 drops it. Patient Access is not a network leg, so
  no exchange, route or line selection changes.
- The connectivity checks still read a payer system's own PDex publication
  (`/us/davinci-pdex/` canonicals, `davinci_pdex_patient_endpoint`) as `pa.pdex`; with no
  `pa.pdex` line declared, such a publication is not version drift.
- Go API: shn-sdk v0.62.0 still names `pa.pdex@2.1` (`shnsdk.ContractPAPDex21`, in
  `NativeContractVersions` and `SupportedContractVersions`, and accepted by
  `ParseDeclaredContractVersions`); the SDK release after v0.62.0 drops it. The gateway's
  own native set and default omit it, and its declared-set rule refuses it, whatever the
  SDK names. **Go API (additive):** `engine.NativeContractVersions`,
  `engine.DefaultContractVersions` and `engine.ParseDeclaredContractVersions`, the SDK's
  native set, default and parser without `pa.pdex@2.1`, and `engine.ErrPDexUndeclared`, the
  parser's refusal of a set naming it.

## An origination route states the Claim's request facts (v0.62.0)

- **Breaking in v0.62.0:** every prior-authorization origination route (`POST /scenario/*`
  that sends a PAS submission or amendment, including `/scenario/uc06/complete` and
  `/scenario/uc07/complete`) now takes the Claim's request facts from its JSON request
  body, beside the route's existing keys, and carries each exactly as stated:

  ```json
  {"claimFacts": {"claimType": {Coding}, "priority": {Coding}, "serviceCategory": {Coding},
                  "placeOfService": {Coding}, "requestType": {Coding}},
   "certificationType": {Coding}}
  ```

  `claimType` is `Claim.type` (claim-type), `priority` is `Claim.priority`, `serviceCategory`
  is `Claim.item.category` (X12 1365), `placeOfService` is `Claim.item.location[x]` (CMS place
  of service) and `requestType` is the item's `requestType` extension (X12 1525);
  `certificationType` (X12 1322) is the amendment's, on a route that sends one. Before, the
  gateway wrote the same constants on every Claim: `professional`, priority `normal` with no
  system, X12 1365 `1` Medical Care, place of service `11` and request type `IN`, and
  certificationType `I` on every amendment.
- **What a caller must change:** state `claimFacts` on every origination request, and
  `certificationType` on a request that sends an amendment at PAS 2.1 or 2.2. The gateway reads
  these facts from nowhere else — not the order, not an Encounter, not configuration — and
  supplies none of them. A resume request (`/complete`) that states no `claimFacts` amends with
  the facts the pended submission's request stated; its `certificationType` is always its own.
- **Refusal:** a fact the PAS Claim profile snapshot requires at the line and the request does
  not state is refused with 422 before any leg of the request is sent (no coverage check, no
  order-dispatch, no questionnaire fetch, no claim), on the order-dispatch routes
  (`/scenario/homeoxygen`, `/scenario/dispatch`, `/scenario/uc03`) as on the others: `Claim.type`, `Claim.priority` and `Claim.item.category` at
  every line; `Claim.item.location[x]` and `requestType` at 2.1 and 2.2; the amendment's
  `certificationType` at 2.1 and 2.2 on a route that amends in the same request (the UC-04
  in-flow amendment, consented UC-05, the single-call `/scenario/uc06` and `/scenario/uc07`).
  The line is the one the submission's route selection picks, and the check is the SDK
  builder's own rule (`shnsdk.CheckPASClaimFactsAtLine`, `CheckPASUpdateFactsAtLine`). A
  resume request (`/complete`) is checked the same way before its first leg, the UC-07
  patient-DTR exchange included: its member, its order, and the amendment's facts and
  `certificationType` at the pinned line. A fact stated as JSON `null` is absent. At 2.0 a
  stated place of service, request type or amendment `certificationType` is carried (each
  0..1 on `Claim.item` there) and an unstated one is omitted; a submission states no
  certificationType at 2.0 (its `I` Initial is written from 2.1). The body is the SDK builder's text, for example
  `{"error":"shnsdk: conformant submit: Claim.type absent at its source, so the request is not
  built: SHN's build never supplies a value the requester's own system does not state
  (fidelity of SHN's own build) (expected from the requester's ClaimType)"}`. A stated coding
  with no code or system, or from a system the element does not draw from, is refused 422 the
  same way ("not a coding this Claim element can carry"). A body that is not JSON is 400.
- **An amendment's PAS `infoChanged` marker is derived, not stated.** Every amendment the
  gateway sends used to carry `infoChanged` `changed` on every item. It now carries the
  marker only on an item that differs from the recorded submission (`changed`) or that the
  submission had no item for (`added`), read off the bytes the submission sent; an item
  re-sent exactly as submitted carries none (the extension's context is `Claim.item`, so the
  amendment's new documentation is not marked). Every in-flow amendment (UC-04, UC-05) and
  every resume that states the pended submission's facts re-sends its item unchanged, so its
  item is not marked `changed`; it is marked `added` only where the amendment links its
  attached answer to it (next point: the UC-06 and UC-07 resumes at PAS 2.0 and 2.1, when
  the payer's pend names the need), and otherwise carries no `infoChanged`. The reference
  payer re-evaluates only items that carry it, and answers an amendment whose items carry
  none with the decision it holds for the item. The prior-Claim entry restates the
  submission's type and priority as sent, every member of each coding.
- **An amendment links its attached answer to the item the payer asked for it about.** The
  gateway keeps the payer's pended answer with the submission record. When a
  pended-response `Task` names an item's line with a need the amendment's attached answer
  meets (the report's code is the requested attachment code, or the amended
  QuestionnaireResponse answers the requested questionnaire), the amendment's Claim gains a
  `supportingInfo` entry (PASTempCodes `additionalInformation`, referencing the answer), the
  item an `informationSequence` pointing at it, and the item `infoChanged` `added`. Nothing
  else is linked or marked, and nothing is linked on an amendment that reaches the payer at
  PAS 2.2 (bridged or built there), whose `supportingInfo` slice requires an X12 PWK01 report
  type no source states. On the hermetic stack the UC-04 and UC-05 amendments' reports answer
  no need the payer's pend names, so they link nothing; the UC-06 and UC-07 amended
  QuestionnaireResponse answers the questionnaire the pend asks item 1 for, so at PAS 2.0
  and 2.1 item 1 is linked and carries `infoChanged` `added`, and the payer re-evaluates it.
- **The request names its member and order.** Every origination route's body also carries
  `"member"` (the member the route originates for) and, on a route that builds a PAS claim,
  `"order": {"system": ..., "code": ...}` (the product coding of the order the claim facts are
  stated for). The route still resolves its member by scenario, lane and `?personaSet` (see
  `engine.ScenarioRoster()`, which lists every scenario, lane, persona set, member and product);
  the request must name that member. Refusals, before any leg: no `member` → 400; another
  member → 422 `this route originates for <resolved>; the request names <member>`; no `order`
  → 400; an order the member does not hold in the system of record → 422 `the request states
  its claim facts for order <system>|<code> and the member's open order is <system>|<code>: SHN
  does not build a claim carrying facts stated for an order the member does not hold (fidelity
  of SHN's own build)`. `/scenario/uc06/complete` and `/scenario/uc07/complete` name the pend's
  member and order; `GET /scenario/uc07/pending` returns them (`member`, `order`) for a caller
  that holds only the patient. `/scenario/uc01` (eligibility) needs only `member`;
  `/scenario/dispatch` already took `member`.
- An amendment's prior-Claim entry restates the recorded submission's own `type`, `priority`
  and `created` (read off the bytes the submission sent), never the amendment's time. The
  durable continuation records the submitted Claim's `created` (`claim_created`, added to an
  existing pgstore table as `TEXT NOT NULL DEFAULT ''`); an amendment built from a
  continuation recorded before v0.62.0 is refused rather than restating the amendment's time.
- **Additive (`engine`):** `ScenarioPersona` and `ScenarioRoster()` (every scenario, lane,
  persona set, member and product a `/scenario/*` route resolves), the lane names
  `LaneConformance`, `LaneDemo` and `LaneProviderData`, and `Continuation.ClaimCreated` (the
  submitted Claim's `created`, as sent).
- **Additive (`scenariodriver`):** `ScenarioRouteInput`, `ScenarioRouteBody`, `ScenarioKey`,
  `PersonaRouteBody` and `ErrNoScenarioPersona` build an origination route's request body from
  the persona the network's discovery descriptor states (its member, order, claim facts and,
  for an amending route, the amendment's certificationType), refusing a route the descriptor
  states no persona for.
- Release order: shn-sdk first (the builders and `PASClaimFacts`), then this gateway, then the
  Kit.

## Refusal texts carry no internal prefix (v0.62.0)

- **Behavior change in v0.62.0 (refusal texts):** a payer gateway's `502`
  refusals of a payer answer it cannot carry (`invalid native PAS response
  Bundle: …`, `invalid native PAS response decision`, `invalid
  prior-authorization inquiry answer…`) and a provider gateway's `502`
  answers to a failed `$populate` (`$populate upstream failed`, `populated QR
  subject does not match patient`, `no clinical context for member`) no
  longer begin with `engine: `. The status codes and the rest of each text
  are unchanged; a client that matched the prefix should match the text after
  it. No Go API changes.

## An exchange with no capture slot keeps its trace proof (v0.62.0)

- **Behavior change in v0.62.0 (diagnostics):** an observed exchange that
  finds no free capture session (`diagnostics.BodyNotKeptCaptureBudget`)
  still keeps no body, and now keeps one header: the `X-SHN-Test-Trace`
  proof a test endpoint sends, its first value, when it is at most 2 KiB (a
  proof grows with its request URI: one for a plain URI of more than about
  1.3 KiB is past it and not kept, and each `&`, `<` or `>` in the URI
  counts six bytes, so a long query string reaches it sooner). Its other
  headers are dropped as before, and `HeadersComplete` is false unless that
  header, with one value, was its only one. A provider gateway's ingress
  past its capture budget so keeps the proof the evidence needs to link the
  call to the test endpoint's, as it did with a session. The proof takes
  nothing from the capture budget's bytes, so each such event holds at most
  2 KiB of header capture. No Go API changes.

## A PAS answer is read at its IG line (v0.62.0)

- **Behavior change in v0.62.0 (`answer.shape`, opt-in):** a payer's PAS
  `$submit` or amendment answer is read by the graph rule of its IG line, as
  each line's specification states it, instead of by PAS 2.0.1's at every
  line:
  - **2.0.1:** every reference the answer makes resolves to an entry, a
    contained resource of its owner, or a version the Bundle holds, the
    request Claim's included (unchanged);
  - **2.1.0:** an answer need carry only its ClaimResponse and what the
    decision made or changed, so its `ClaimResponse.request` must name the
    request Claim, by a reference to a Claim or by an identifier with a
    value, and no reference need resolve;
  - **2.2.1:** `ClaimResponse.request` must name the request Claim, by a
    reference to a Claim or by an identifier with a value; a reference to
    that Claim need not resolve, and every other reference must.

  At every line the Bundle carries one ClaimResponse, every entry is
  identified by an absolute `fullUrl`, and no reference is empty.
- **The answer is read at its candidate lines,** in the order a payer's
  answer is certified at (FR-G29): the line the leg was routed at, then the
  lines the answer claims, then 2.2, 2.1 and 2.0. It is read only at the
  routed line and at lines this gateway has a validator lane for, the lines
  its certification can find it valid at: a validator that serves several
  lines judges an answer once, at the first of them, so a gateway whose one
  validator serves every line reads an answer at its routed line only. Valid
  on any line is valid. An answer read at a line after the first is recorded at
  that line (`verdict` `valid`, `line`, `declaredLine`, `lines`), at
  `observe`, `structural` and `strict`. Two consequences:
  - a gateway with a 2.1 lane carries any answer that names its request
    Claim, however incomplete for 2.0.1 or 2.2.1; a gateway judging answers
    at 2.0.1 alone still refuses one missing a resource it references;
  - a rule only one line states (PAS 2.2.1 §spec-44, below) refuses only on
    a gateway whose candidate lines all carry it.
- **`$inquire` answers:** an inquiry answer's graph is not read at any line;
  it may name a Claim it does not carry. At 2.2.1 (§spec-44) each
  ClaimResponse it carries names the Claim it answers, by a reference to a
  Claim or by an identifier with a value, or gives a Data Absent Reason on
  its `request`. One that does
  neither is `answer.shape` on a gateway whose candidate lines are 2.2 only.
- **The patient check (`patient.answer`)** reads an answer at the line its
  graph rule held on, or, when it held on none, at the routed line. At 2.0.1
  and 2.2.1 it reads as before. At 2.1.0 a
  subject no entry resolves binds by the patient its `Patient/<id>` names,
  and the Bundle need not carry the ClaimResponse's patient; a Patient it
  carries, and every subject, must still be that patient, one identity (the
  same id at another base is another patient), and a reference that resolves
  to more than one entry is refused at every line.
- `answer.shape` stays as it was otherwise: relayed unread at `none` and
  `observe` (recorded at `observe`, nothing written from the answer),
  refused at `structural` and `strict`.
- **Additive (Go API):** `engine.PASInquiryAnswerReadsAt(body, line) (ok
  bool, detail string)` is the rule above for a `$inquire` answer at IG line
  `2.0`, `2.1` or `2.2`, exactly as the gateway reads one at that line: `ok`
  when the answer meets it, else `detail`, the refusal. It judges at the one
  line it is given; the gateway itself carries an answer that is valid at
  any of its candidate lines. It is exported so a reader of
  retained evidence judges an inquiry answer as the gateway does; it reads
  and decides nothing for the relay. Nothing the gateway relays or refuses
  changes.
- **Additive (Go API):** `engine.NativePASRefusalCause(body) string` and
  `engine.NativePASGraphRefusalCause(body) string` name why
  `InspectNativePASResponse` and `CheckNativePASGraphWithoutClaimResponse`
  refuse a Bundle, as one cause of the closed set
  `engine.NativePASRefusalCauses` (the `PASRefusal*` constants), and `""`
  when they read it. A cause names why, never a value from the Bundle, so a
  reader of retained evidence can say why an answer was not read. They read
  and decide nothing for the relay; the refusal texts are unchanged.
- **Additive (Go API):** `engine.InspectNativePASResponseAt(body, line)`,
  `engine.NativePASRefusalCauseAt(body, line)` and
  `engine.NativePASUnresolvedReferenceAt(body, line)` read a PAS `$submit` or
  amendment answer at IG line `2.0`, `2.1` or `2.2` (any other, or none, as
  2.0.1) exactly as the gateway checks one there: they and the gateway's own
  check share one per-line definition. The line-less readers are these at no
  line. Nothing the gateway relays or refuses changes.
- These readers now live in `pa/pasanswer` (`engine.PASInquiryAnswerReadsAt`
  as `pasanswer.InquiryAnswerReadsAt`); the `engine` names are deprecated
  forwarders that stay until v1.0.0 (see "The PAS answer readers have their
  own package").

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
fails at the connection (it refuses, resets or closes it, or cannot be reached) is unqualified
for certification until it passes again, "re-qualifying", and nothing is dialed meanwhile. A
lane that does not answer within the certification's 2 s while the collection is still waiting
(that certification is recorded expired, since the client's own limits are 2.5 s) is asked for
its metadata once, and re-qualifies only if that goes unanswered (v0.63.0, above). A re-qualification
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

- **`scenarioauth`** (`AudiencePrefix`/`AudienceFor`, `HeaderName`, `PathPrefix`, `TTL`, `MaxIssueSkew`, the `Caller*` labels,
  `Binding`/`BindingHash`, `Sign`, `Transport`, `Client`, `KeyEnv`/`KeyFileEnv`/`GatewayEnv`, `ErrNoGateway`, `ParseKey`, `ParseCallers`,
  `LoadKeyFile`, `KeyFromEnv`, `ClientFromEnv`, `SignerOf`, `StartupLine`, `Caller`, `Verify`): new in v0.63.0.
  It authenticates a caller of a provider gateway's `/scenario` routes. A request carries an
  `X-Holder-Assertion` for the gateway it is sent to (audience `scenario:` and that gateway's holder
  id), signed with the caller's own Ed25519 key under its label, valid for at most one minute. The
  assertion is bound to the request's method, target (path and query) and body. A caller signs
  through the `Transport`, naming the gateway it calls; `ClientFromEnv` reads it from
  `SCENARIO_GATEWAY` and refuses a key without it (`ErrNoGateway`). `Verify` admits only the
  callers, by label and public key, that the gateway passes it, and only for the gateway's own
  holder id, so an assertion for one gateway is refused at every other. A signed request goes out
  on a connection of its own (a copy of the base `*http.Transport` with keep-alives off), so Go's
  transport never resends an assertion the gateway may already have spent; a base of another kind
  is used as it is, and its retries are its own. The wire format, in the
  SDK's `docs/PARTICIPANT_PROTOCOL.md` (§3.4a), is the contract, not this package: the `shn`
  CLI's `scenario-key`, `scenario-sign` and `send-test` implement it too, and what they write
  and sign is tested against this package's `Verify` and key reader. A provider gateway checks
  these assertions when `SCENARIO_CALLERS_FILE` names its callers (`engine.Config.ScenarioCallers`,
  also new in v0.63.0; see `docs/CONFIGURATION.md`), and records each one used in its replay
  store under `engine.ReplayScopeScenarioJTI` (new in v0.63.0), so a replay store that admits
  named scopes must admit that one. **Evolving**: consumers pin exact
  gateway versions.

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
