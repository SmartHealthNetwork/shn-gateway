# Changelog

What each `shn-gateway` release changed, section by section, newest release
first. Each release's notes are also published with it on GitHub. What the
stability promise covers is in [`STABILITY.md`](STABILITY.md).

## A token request follows no redirect (v1.9.0)

- **Changed (every role, SMART Backend Services to your own system):** the gateway's token
  request to your authorization server (`FHIR_TOKEN_URL`, `PAYER_DAVINCI_TOKEN_URL`,
  `PROVIDER_DTR_POPULATE_TOKEN_URL`, `PROVIDER_CDEX_TOKEN_URL`) follows no redirect, whatever
  the redirect's origin or code, at every conformance level: its body is your client's
  credential (the client secret, or the signed assertion), and OAuth defines no redirect of
  it. A `3xx` answer fails the token request as a token endpoint that does not answer fails
  it: no token could be obtained, so the call that needed it is not sent and fails as it
  does when your authorization server cannot be reached. That holds for every `3xx`, one with
  no `Location` or with one that cannot be read included. A redirect from `https` to `http` is
  refused and recorded as any request's is (`https-to-http-refused`); any other failure names
  the answer's status and the origin its `Location` named, if any, never its path or query.
- **Added (exchange record):** the access line's `backend.redirects` names a token request's
  refused redirect `token-redirect-refused`.

## A request goes to the payer the network registers for any identifier of any payor (v1.9.0)

- **Changed (requester):** a provider's gateway routes a PAS, CRD, DTR or coverage eligibility
  request, and an exchange it originates from its own records, by the identifiers the payer
  directory registers (shn-sdk v1.10.0's `ChooseRegisteredPayer`), read from every payor of the
  Coverage, never by position. An Organization that lists an NPI or an EIN before its payer
  identifier, or a registered payor after an unregistered one, now goes to that payer. Earlier
  releases read only the first identifier of `payor[0]` and refused such a request `422 no
  registered payer for identifier <the NPI>`. A request whose payors name their payer first
  behaves as before, except as below.
- **Changed (requester):** these are refused `422` (routing), before any leg, at every
  conformance level:
  - no identifier is registered: `no registered payer for identifier <identifiers>: name the
    payer by an identifier the network registers for that payer, on the Organization that
    Coverage.payor references (see the Participant Wire Protocol §1a, Participant directory)`,
    or, on the eligibility insurer path, naming the insurer Organization the request names;
  - the identifiers name two different registered payers, including an Organization that
    earlier releases routed by its first identifier: as ambiguous;
  - a payor after `payor[0]` with no inline identifier that names no payer the gateway can read
    (its reference resolves to nothing, to an Organization with no identifier, or to another
    kind of resource): `no payer identifier on member coverage: payor[<i>] "<reference>" cannot
    be read as a payer Organization with an identifier, so the coverage's payer is unknown`
    (on a coverage eligibility request, `no payer to route the eligibility request by: …,
    payor[<i>] "<reference>" cannot be read as …`).
  `payor[0]`'s own refusals are unchanged, and it is refused even when a later payor names a
  payer. A `Patient` or `RelatedPerson` payor (self-pay) names no payer and is passed over; its
  identifiers never appear in a refusal.
- **Changed (requester):** a payor reference that several entries of the request answer, every
  one an Organization, names every identifier they name, and the directory decides as it does
  across payors: identifiers no participant registers are passed over, and two registered payers
  are ambiguous. Several entries that are not all Organizations are refused `422 no payer
  identifier on member coverage: Coverage.payor matches more than one resource of the request,
  and not every one is an Organization` (`… more than one entry of the Bundle …` for a PAS
  Bundle). Earlier releases routed several entries only when they named the same payer
  identifier.
- **Changed (requester):** a payor that carries an identifier of its own already names its payer:
  its reference counts when what the request carries resolves it, and is never read from the
  system of record or the EHR's `fhirServer` to route. A failed read of another payor's
  Organization leaves that payor unknown; when no payer is then found, a system-of-record read's
  failure is that system's (`502`, or `503` when it is unavailable), never a routing refusal, and
  a `fhirServer` read's is that read's refusal.
- **Changed (requester):** through the request's `fhirServer`, the gateway reads each payor
  Organization routing needs (one with no identifier of its own that nothing the request or the
  search answers), up to five for one request, and, on a coverage it read through `fhirServer`,
  carries each in `prefetch.coverage` (nothing is carried for a coverage the EHR sent). Earlier
  releases read one and refused a second (`… name more than one payor Organization by reference
  alone`); more than five are now refused `422 no coverage to route by: fhirServer's coverages
  name more payor Organizations by reference alone than this gateway reads for one request (5)`
  (for a coverage the EHR sent, `no payer identifier on member coverage: the request's coverages
  name more payor Organizations by reference alone than this gateway reads for one request (5)`).
- **Changed (requester):** a request the gateway builds from the participant's own records names
  the payer it routes to: a PAS Claim's insurer, and an inquiry's, is the Organization of the
  payor routing chose, never a payor named only because it is first; a questionnaire request
  carries the Organization of every payor of its Coverages as the system supplied it, and a CDS
  Hooks request's carried coverage search result includes the Organization of every payor
  routing resolved.
- **Changed (payer):** with the payer identity mapping on, a payer's gateway maps the
  identifier of every payor that names one of its own (its inline identifier or, failing that,
  the identifier the Organization it references lists, wherever it sits; edit E-03), and leaves
  every other identifier as sent. Earlier releases mapped only `payor[0]`, by its inline
  identifier, else its Organization's first identifier, and refused a Coverage whose first
  identifier was not one of the payer's own. A Coverage none of whose payors names one of the
  payer's own is still refused, judged by `payor[0]`, as before.
- **Changed (payer):** a payer's gateway answering a coverage eligibility request from its own
  records reads a member's several Coverages as naming it when each carries one of its own
  identities (those it publishes on the network feed and, with the mapping on, its configured
  and backend identities) on some payor, in any position; otherwise, as with no identity known,
  when the payer directory resolves their identifiers to one holder, or, when it resolves none of
  them, when they name the same identifiers in any order. Earlier releases compared the Coverages'
  first identifiers, so Coverages agreeing only on a first NPI and naming different identifiers
  after it were answered; they are now ambiguous (`422 ambiguous coverage for routing: the
  member's Coverage records do not name one payer`), unless each carries one of the payer's own
  identities, or the payer directory resolves them to one holder.

## The gateway builds on shn-sdk v1.10.0 (v1.9.0)

- **Changed:** the gateway pins shn-sdk v1.10.0. Payer routing uses its new
  `CoveragePayerCandidates` and `ChooseRegisteredPayer` (see "A request goes to the payer the
  network registers for any identifier of any payor"). Every SDK function the gateway called
  before answers as before for what it reads, and it reads none of the decision detail v1.10.0
  adds (`OtherReasons`, an `A6` approval's review action).

## A redirect from your own system is never followed to http, and never carries your token or configured headers elsewhere (v1.9.0)

- **Changed (every role, every call to your own system):** when your system of record,
  your Da Vinci endpoint in native-forward mode, your CDS service listing, your Task
  endpoint, your `$populate` engine, or a connectivity check (`/internal/checks`) is
  answered with a redirect, the gateway follows it as a direct client would, with two
  rules, whether or not the call carries a token. (A token request to your authorization
  server follows no redirect at all: see "A token request follows no redirect".) A redirect
  to another origin (another scheme, host or port) is followed
  without `Authorization` and without any header you configure in
  `PAYER_DAVINCI_BACKEND_HEADERS`, whatever its name or value, and neither is sent after
  the first hop to another origin, for the rest of the redirect chain, even when it
  leads back to your system: another origin chose that hop's path, and with a `307` or
  `308` its method and body. Your system answers such a request as it chooses (a `401`,
  say), and that is the answer, with no fresh token fetched. A chain of redirects within
  your system's own origin keeps your token and those headers on every hop. Every other
  header of the request is carried as a direct client would carry it. A `401` from
  another origin is its answer, with no fresh token fetched. **A redirect from
  `https` to `http` is never followed, whatever its origin, at every conformance level**:
  a `307` or `308` would send the request's body in cleartext (for a token request, your
  client secret or signed assertion). A read your system redirects that way fails as a
  read your system cannot answer does, and a token request as one your authorization
  server does not answer. A forwarded
  operation is refused `502` on message integrity, attested delivered: the requester's caller
  is told your system received the request, since it answered, and to check its outcome
  before resending. A search of your system of record still follows no redirect at all.
- **Added (exchange record):** the access line's `backend.redirects` names each redirect
  your system or your authorization server's token endpoint answered a call with, in
  order, by its class and never its URL: `same-origin`, `cross-origin`,
  `https-to-http-refused` for the downgrade that was not followed, or
  `token-redirect-refused` for a token request's redirect, which is never followed. A forward, or a read of
  your CDS service listing, it ends records your system's `3xx` status with
  `backend.errorClass` `http-3xx`; a read of your system of record it ends has
  `backend.errorClass` `http-3xx`. A `$populate`
  request it ends writes `gateway: populate_failure` with stage `transport`, reason
  `https_to_http_refused` and the redirect's status.

## A 401 from your own system gets one fresh token, and the request is sent once more (v1.9.0)

- **Changed (every role, SMART Backend Services to your own system):** when your system
  of record, your Da Vinci endpoint in native-forward mode (its CDS service listing
  included), your Task endpoint (`PROVIDER_CDEX_TASK_URL`), or your `$populate` engine
  answers `401` to the token the gateway cached, the gateway drops that token, fetches a
  new one from your authorization server and sends the same request once more, byte for
  byte the same apart from `Authorization` (its correlation id included), as a direct
  SMART client does. **Your own system may now see a request twice when it answers `401`
  to an expired token, as with a direct client.** That includes a PAS submission: a `401`
  is your system refusing the credential before acting on the request. Only once: a `401`
  to the fresh token too is your system's answer, passed back as it is. A `403`, or any
  other status, is never resent, and a request whose body cannot be read again is not
  resent (the next request goes with a fresh token). If the fresh token cannot be
  obtained, your system's `401` is the answer, as before. Requests refused together with
  the same token share one fresh token. A system that answers `401` for a reason other
  than the token (a key it checks in front of the token, a scope it never grants) now
  costs a token request and a second send for each request. Before, the gateway kept
  sending the cached token until it aged out, so every request failed until then.
- **Added (exchange record):** the access line's `backend.resends` names each call sent a
  second time: `reason` `credential-refreshed-after-401`, and `challenge`, what the `401`'s
  `WWW-Authenticate` said, recorded as observed and never used to decide (`invalid_token`,
  `other` or `none`). `backend.calls` counts each resend. A resend to your `$populate`
  engine, or by the connectivity checks (`/internal/checks`), is made the same way but is on
  no access line.

## With the payer identity mapping on, a PAS inquiry with no Coverage is carried as sent (v1.9.0)

- **Changed (payer, payer backend identity mapping):** a PAS `Claim/$inquire` that reaches
  your gateway carrying no `Coverage` anywhere is forwarded to your own system as it arrived,
  apart from the registered address restore (E-13), at every conformance level, as it already
  is without the mapping. Before, with
  `PAYER_DAVINCI_PAYOR_OWN` and `PAYER_DAVINCI_PAYOR_BACKEND` set, it was refused `400 payer
  backend identity mapping: inbound Coverage carries no resolvable payor identifier`. Such
  an inquiry names no payor for the mapping to re-stamp, and the network has already routed
  it to you; whether a Coverage is missing is your own system's to answer. An inquiry that
  holds a Coverage only where the mapping does not read one is still refused with that 400:
  the mapping reads only the Bundle's own entries spelled exactly `"resourceType": "Coverage"`,
  so an inquiry whose only Coverage is, for example, contained, nested in another resource, or
  an entry that spells `resourceType` or `Coverage` in another case is refused. A submission or
  amendment with no Coverage is refused as before.
- **Added (exchange record):** `editsNotMade` gains `E-03:no-coverage`, recorded when the
  mapping is on and a PAS inquiry, a CDS Hooks request or a DTR `$questionnaire-package`
  request your gateway forwards to your own system carries no Coverage anywhere, so the
  mapping made no edit (for a `$questionnaire-package` request, at `none` and `observe`: at
  `structural` and `strict` such a request is refused first). A CDS Hooks or DTR request with no Coverage was already forwarded as
  it arrived; its record now says so.
- **Unchanged (provider):** a provider's gateway routes an inquiry by its own Coverage, so one
  with no Coverage is still refused before any leg, at every level, as routing (`422 no payer
  identifier on member coverage`).

## The provider's `$populate` call is sent as a FHIR operation call (v1.9.0)

- **Changed (provider, `PROVIDER_DTR_POPULATE_URL`):** the SDC `Questionnaire/$populate` request the gateway sends
  to your own populate engine (`PROVIDER_DTR_POPULATE_URL`) carries `Content-Type:
  application/fhir+json`, as a direct FHIR client sends it; it was `application/json`. A provider
  system that parsed only `application/json` now receives `application/fhir+json`, which a FHIR
  server must accept. `Accept` lists `application/fhir+json` first and still lists
  `application/json` second, so an engine that answers as `application/json` is read as before.
  The request's body is unchanged.

## A request leg no authority could be obtained for tells its caller why (v1.9.0)

- **Changed (requester):** when the Authorization Framework gives no authority for a request
  leg, the leg is still not sent and its record is unchanged, but the caller's answer names the
  Framework's status and the failure's class in place of the opaque `502 authorization failed`.
  It never carries the Framework's own `error` text, nor a header of its answer apart from a
  `429`'s `Retry-After` (below). The answers, as
  `{"error": …}` or an `OperationOutcome` on the FHIR operation routes:
  - a `502` with `"rule":"audit"`: `502` `the authorization service could not record its
    decision, so this leg was not sent`;
  - any other `4xx` but `403` and `429`: `502` `the authorization service refused this gateway's request
    (answered N), so this leg was not sent`;
  - any other status: `502` `the authorization service failed (answered N), so this leg was
    not sent`;
  - a `2xx` with no token the leg can use, or one that does not verify for it: `502` `the
    authorization service's answer carried no token this leg can use, so this leg was not sent`;
  - an answer whose body could not be read: `502` `the authorization service's answer could not
    be read, so this leg was not sent`;
  - any other failure: `502` `the authorization service failed, so this leg was not sent`.
- **Changed (requester):** a Framework `429` is the gateway's own `503` `the authorization
  service is limiting this gateway, so this leg was not sent`, not retried, carrying the
  Framework's `Retry-After` only when it sent exactly one value that is a delay in whole seconds
  or an HTTP date in its preferred form (IMF-fixdate), and none otherwise. It is recorded, and counted in `LegOutcome`, as the
  Framework unavailable (`unreachable`), as a Framework that cannot be reached is: the access
  line's `authorizeError`, and `answerError` for a 429 on an answer's authorize call. Before, a
  Framework `429` was `502 authorization failed`, with `authorizeError` `other`, and counted
  `failed` in `LegOutcome`.
- **Unchanged:** `403` is `403 authorization denied`; no answer, a `503` or a `504` is the
  `503` that the authorization service could not be reached. A responder's own failure to
  authorize its answer before its system has answered is still a plain `502` to the Hub, which
  relays only its status.

## The CDex data request is checked at egress (v1.9.0)

- **Changed (module 1, UC-05):** the CDex data request Task the gateway builds and sends a
  facility is now checked at egress, at your conformance level, against the profile it declares
  in `meta.profile` (`cdex-task-data-request`), by the same validator that checks the facility's
  answer. A defect is recorded at `observe` and the request sent; it is refused `422 egress
  validation failed: …` before the request is sent at `strict`, and at `structural` for a
  structural issue (a deeper one, an invariant or a code outside its code list, is recorded and
  the request sent). A
  validator that cannot be reached refuses `500 validator unavailable` at `strict` and is recorded
  at `observe` and `structural`. Nothing is checked at `none`. A validator that does not hold the
  profile reads it as a structural issue: with your own validator at `structural` or `strict`,
  load `hl7.fhir.us.davinci-cdex` 2.1.0, or every data request is refused (the supplied validator
  image loads it). The PAS submission was already sent when the data request is refused: the
  payer's pend stands, and the answer says which legs were carried.
- **Removed (payer's gateway):** the egress check of a `$questionnaire-package`, PAS submission,
  update or inquiry answer the gateway built itself. A payer's gateway answers those legs only with
  its participant's own answer, relayed as sent, so the check could never run.

## A PAS request the gateway builds carries the report, its Provenance and the prior Claim as written (v1.8.0)

- **Changed (requester, amendments the gateway builds):** the provider's gateway builds its PAS
  requests with the SDK's Carried builders (shn-sdk v1.9.0). The update builder carries the
  report it attaches (UC-04: your system's supplemental report; UC-05: a facility's report and
  the facility's Provenance) as it reaches the build, apart from the registered edits made to it
  before (E-06, E-14): under its own id, with its `meta` (`meta.profile` included), member order
  and whitespace as written, at an entry whose `fullUrl` is built from that id, so the references
  in it resolve in the bundle under FHIR R4's Bundle rules. The facility's Provenance keeps its
  `target` as the facility wrote it. Releases before v1.8.0 gave these records bundle-local ids
  (the Provenance's `target` rewritten to match), dropped their `meta.profile` and re-encoded them.
  The submitted Claim an amendment carries as its prior Claim is carried byte for byte, where
  earlier releases compacted it.
- **Changed (requester, every PAS request the gateway builds):** the orders, Coverage, the
  persons the member's Coverage names and the payer's Organization keep the `meta.profile` your
  system wrote, where earlier releases dropped it.
- **Changed (requester, wherever your gateway runs `$validate` on a PAS request it builds):** the
  profiles your records declare, which the request now keeps, are in the request your gateway's
  own `$validate` checks, so each record is checked against them. A record that does not conform
  to a profile it declares is a defect, and a profile your gateway's validator does not hold is a
  check that could not run, each decided as any such finding is: for a request sent at the line
  it was built at, by the conformance level the participant set for its own gateway (nothing is
  checked at `none`, and no default refuses a payload); for a request module 2 translates
  between IG lines, a translation a participant opts into (`TRANSLATE_IG_LINES`), by module 2's
  check of a translated payload, which refuses anything not valid at every level. Earlier
  releases dropped these profiles from the request before the check; UC-04's report was, as
  before, also checked on its own against the profiles it declares.
- **Removed (requester):** the provider's gateway no longer writes a PAS request's references in
  absolute form, which releases before v1.8.0 did on the origination profiles that relay the
  reference payer's bytes, unregistered and named nowhere. A request's references resolve under
  FHIR R4's Bundle rules.
- **Changed (requester):** the records the provider's gateway adds to a request it built (the
  clinical records a provider-data request references, the records the order's requester leads
  to and a dispatched supplier, each where the request does not already hold it, as the Claim's
  provider or the payer's Organization, say) are spliced into the request's bytes as read from
  your system. Earlier releases decoded and re-encoded the whole request to add them. A record that reaches the splice and that
  it cannot carry as read (one that is not exactly one JSON object, one that is not UTF-8 or holds
  a lone surrogate escape, or one naming a member twice in an object, whatever its case under
  Unicode case folding; an order's requester or a dispatched supplier with no `resourceType` or
  `id` spelled exactly, whose entry is never addressed by an id the record does not state) is
  refused `422`, the fidelity of the gateway's own edit (`a record read from the system of record
  cannot be carried as read`, and why), and no PAS request is sent.
  Most such records are answered before the splice, as in earlier releases, by the read that
  takes them from your system or the edit made to them first, with the status that answers them.
- **The facility's Provenance target (requester, UC-05):** carried exactly as the facility wrote
  it, whatever it names; the provider's gateway never rewrites or judges it. The report attached
  is one with an `id` spelled exactly (a report with none is never the one attached), and where
  several of the facility's Provenances name it, the one whose `target` also names it in the
  amendment (the relative form) is taken. The report rides
  under its own id at the amendment's base, so a `target` the facility wrote as its own
  `fullUrl` (its server's, or the federated query's `urn:shn:fedquery:<n>`) names no entry of the
  amendment, and the payer's `pas.update-provenance` check reads it so. Releases before
  v1.8.0 rewrote the `target` to the report's bundle-local id, an unregistered change, whatever
  it named.
- **UC-04's Provenance (requester):** the Provenance the provider's gateway builds for your
  supplemental report has an id of its own for each amendment (`prov-<the amendment's
  correlation>`, or `prov-` and a digest of it when that is not a FHIR id), so two amendments
  attributing one report never share one. Earlier releases
  gave it the bundle-local id every amendment shared.
- **Newly refused (requester, every PAS submission and amendment the gateway builds):** two edits
  of the gateway's own build that cannot be made, checked once right after the build, each refused
  `422`, the fidelity of the gateway's own build, with nothing sent:
  - A record the build addresses by its own `resourceType` and `id` (the Coverage, the payer's
    Organization, the Claim's provider) that states either only in another spelling (`"ID"`, say),
    or any record whose `resourceType` is spelled so. Such a record has no
    FHIR id, and the build would address it (its entry's `fullUrl`, and the references the Claim
    and the Coverage make to it) by an id the participant never gave: `a record read from the
    system of record cannot be carried as read: it states its resourceType or id only in another
    spelling`.
  - A record the build gives an id of its own (the order, the `QuestionnaireResponse`, and the
    Provenance of an amendment with no report) that spells its id another way, alone or beside its
    own `id`. The build's id would sit beside that spelling, and a body naming its id twice under
    case folding is one the network's message integrity does not seal, since it can be read two
    ways: `a record read from the system of record cannot be carried as read: the build's own id
    for it cannot be set beside the id it spells another way`. Once the build carries these records
    as written, a lone member spelled so is carried, as the rest of the record is. A record the build
    carries under its own id (the Provenance attributing a report, say) that names its id in two
    spellings is the participant's own ambiguity, never refused as the build's re-id.

  This is a deliberate change: earlier releases, depending on the request, built and sent such a
  request or answered it as the gateway's own `500`. A report, or the Provenance attributing it,
  spelled so is refused by the build itself (below); a person the Coverage names spelled so is,
  as before, refused `422` as a person the request cannot carry.
- **Newly refused (requester, UC-04 and UC-05):** an amendment whose report or Provenance the
  build cannot carry as written: one with no id (the gateway never mints one), one that is not
  exactly one JSON object (a member named twice or spelled in another case among them), one that
  is not UTF-8 or holds a lone surrogate escape, one past the build's own limits (16 MiB, nesting
  64 deep, 1,000,000 JSON tokens; the sealed PAS update is capped at 8 MiB in any case), or a
  bundle in which two entries of one type would share an id (FHIR bdl-7). Each is refused with
  `422`, the fidelity of the gateway's own build, and no amendment is sent. Earlier releases gave
  such records ids of their own. A UC-04 report with no id is refused before the build, `500
  supplemental report missing id`, as before.
- **Changed (payer):** the payer's gateway reads an amendment's Provenance `target` (the
  `pas.update-provenance` content check) by the SDK `Responder`'s rule: exactly `<type>/<id>` or
  exactly the supplemental entry's own `fullUrl`, either one version specific only at the
  resource's own `meta.versionId`. Newly matched: a version-specific target at the resource's own
  version. Newly not matched: a reference under another base that ends in the resource's type and
  id, which under FHIR R4 names another server's resource. The check is a content rule, as
  before: not run at `none`, recorded at `observe` and `structural`, refused at `strict`.
- **Not carried as written yet (requester):** the order (`ServiceRequest` or `DeviceRequest`)
  takes a bundle-local id and is re-encoded; the Coverage, the persons it names and the payer's
  Organization keep their `meta.profile` but are re-encoded, the Coverage's payor and beneficiary
  pointed at the request's entries; on an amendment with no report (UC-06, UC-07), the amended
  `QuestionnaireResponse` and its Provenance take bundle-local ids, the Provenance's `target`
  pointed at the `QuestionnaireResponse`; and the Claim's requesting provider (a
  `PractitionerRole` or `Organization`, which may be the order's requester or the dispatched
  supplier) is re-encoded as the build places it.
- **Compatibility:** a payer whose own system cannot resolve relative references sees its own
  system's refusal, carried as sent, for a request a provider's gateway from v1.8.0 builds. A
  facility's Provenance whose `target` is version specific is now carried so; a payer's gateway
  before v1.8.0 reads no version-specific target, so at `strict` it refuses that amendment
  (`pas.update-provenance`), where earlier requesters rewrote the target.

## A questionnaire response the gateway's own engine filled has its declared profiles asked for (v1.8.0)

- **Changed (module 1, the DTR questionnaire response checks):** a response the gateway's own
  engine filled (its managed `$populate` fill or its automatic fill) declares the gateway's own
  profiles, the line's DTR profile the gateway adds included. Each is now asked for at the
  response's DTR check, as the line's DTR profile already was: a validator lane that does not hold
  one refuses the response as a structural issue where the level refuses one (`structural`,
  `strict`), and records it at `observe`. As in releases before v1.8.0, it is read as a structural
  defect, never as a check that could not run. A profile your own `PROVIDER_DTR_POPULATE_URL`
  engine declared on a response it returned, and that the lane does not hold, is from v1.8.0 a
  check that could not run, as any profile a payload declares is: an `unavailable` finding at
  `observe` and `structural`, the gateway's own `500` at `strict`, where releases before v1.8.0
  refused it `422` at `structural` and `strict`.
- **The DTR profile the gateway adds** to a response it completes is the one its DTR check then
  asks for, at every line, so at every level that runs the check the response cannot reach a payer,
  alone or in a PAS Bundle, without its profile having been checked at its own line.

## The gateway builds on shn-sdk v1.9.0 (v1.8.0)

- **Changed:** the gateway pins shn-sdk v1.9.0.

## A profile a payload declares that the validator does not hold is a check that could not run (v1.8.0)

- **Changed (every `$validate` check the conformance level governs, ingress and egress, and
  certification):** a resource may declare in `meta.profile` a profile that no package your
  validator lane loads defines (a facility's own report profile, say). The validator then says it
  has not checked the resource against that profile. From v1.8.0 that is read as a check that
  could not run, with the verdict `unavailable`, never a pass and never a defect of the payload,
  and decided as every check that could not run is: nothing runs at `none`; at `observe` and
  `structural` an `unavailable` finding is recorded and the message carried; at `strict` the
  gateway refuses with its own `500`, saying `profile could not be checked: <profiles>`, named as
  the finding names them. Releases before v1.8.0 read it as a structural defect: recorded at
  `observe`, refused with `422` at `structural` and `strict`.
- **Only the payload's own declaration:** the validator's not-checked issue
  (`VALIDATION_VAL_PROFILE_UNKNOWN_NOT_POLICY`), read on its own, must name the payload's own
  `meta.profile` element, holding exactly the profile the issue names, and a
  `Validation_VAL_Profile_Unknown` quoting that profile beside it is read with it; for a `Parameters` body
  (a questionnaire package answer), which the gateway sends its validator wrapped in another
  `Parameters`, the element as the validator names it within that wrap. A profile the gateway
  asks the validator for explicitly stays an error, decided as a structural defect. Beside a
  structural error the payload is refused for that error wherever the level refuses one, and a
  Bundle entry's no-match summary does not follow from such a profile: it reads as it would
  without the profile beside it, and in certification, beside only such a profile, it keeps
  the line `invalid`.
- **Only a profile no line held:** where the gateway reads a payload at several lines (a payer's
  answer at its candidate lines, a certification's lines, and module 2's search for a
  questionnaire package answer's own line), a profile one line's validator lacks and another's
  holds (another IG line's own profile, say) was checked there: the line that lacks it reads as
  before, a structural defect, and the search for an answer's line moves on to the line that
  holds it. Only a profile no line the gateway consulted holds is a check that could not run; a
  line that did not answer or stopped without a verdict held nothing, and is an outage, as
  always.
- **Where else the verdict shows:** the observer's `validate.result` detail says `validator
  unavailable`, and the `crd.embedded.validated` outcome is `unavailable`, for an embedded resource
  declaring such a profile (the observer reports each line's validator alone: where another line
  held the profile, it says `validator unavailable` for the line that lacks it while the check's
  finding is structural); module 2's search for a questionnaire package answer's own line
  records `E-08:check-could-not-run` only when no line it searched held the profile.
- **A translated payload:** module 2's check of a payload it translated between IG lines, refused
  at every level, refuses one declaring such a profile with the gateway's own `500` saying so,
  where releases before v1.8.0 refused it `422` as a structural defect.
- **Finding:** a finding gains `unresolvedProfiles`, naming the profiles the payload declares
  that the validator does not hold, on an `unavailable` finding they made unavailable and on an
  invalid one beside the defect that decided it. Each profile is named by its size and SHA-256,
  never as written, like every other payload-quoted string a finding keeps: the digest is the SHA-256 of the profile URL's bytes as
  declared, so you match it by hashing your own profiles' URLs. At most eight are named, sorted,
  and the rest counted (`and N more`).
- **Certification:** a `certify:` line whose lane does not hold a profile the payload declares,
  and found nothing else, is `unavailable` with `error` `profile unavailable: <profiles>` (two
  named at most); beside unchecked terminology or a regex check that could not run, the profiles
  are counted (`profiles unavailable: <n>`).
- **Go API:** `validatorissue.ProfileUnknownNotPolicy` and `validatorissue.ProfileUnknown` name the
  two issues; `engine.ConformanceFinding` gains `UnresolvedProfiles`;
  `engine.UnresolvedDeclaredProfilesForTest` is test-only introspection, like
  `engine.ClassifyFHIRForTest`.

## An adaptive questionnaire's rounds stay on the network (E-12) (v1.8.0)

- **Added (registered edit E-12, `dtr-adaptive-address-rekey`):** on a `$questionnaire-package`
  or `$next-question` answer a provider's gateway relays to its own DTR client, it re-keys each
  adaptive address the payer's gateway tagged to its own ingress:
  - a `valueUrl` to `<ingress base>/dtr/payer/<payer>/<line>/<tag><remainder>`, the remainder
    after the payer's base kept byte for byte;
  - a `valueBoolean` `true` to `<ingress base>/dtr/fetch-server/<payer>/<line>/<tag>`.

  The answer names the edit in `X-SHN-Edited` and loses its `ETag`. The re-key is made even where
  a signature covers the address, so the payer's signature over a re-keyed answer no longer
  verifies in the provider's copy, as the provider's over a restored request no longer verifies
  in the payer's (E-13). It is made only toward a payer whose gateway declares the `v1opsub`
  request frame.
- **Added (`editsNotMade`):** for an adaptive address not re-keyed, carried as sent:
  `E-12:payer-gateway-older`, `E-12:not-the-rounds-address`, `E-12:contained-in-package-response`,
  `E-12:request-carried-a-questionnaire`, `E-12:not-under-base`, `E-12:escaped-address`,
  `E-12:base-spelled-differently`, `E-12:unreadable-tag`, `E-12:unclean-remainder`,
  `E-12:unreadable-address`, `E-12:unreadable-answer` and `E-12:address-not-writable`.
- **Added (provider ingress, `dtr-questionnaire-fetch`):** three routes for the rounds, on the
  package's own leg, credential and scope: `POST /dtr/payer/{payer}/{rest...}` (the address E-12
  writes), `POST /dtr/fetch-server/{payer}/{line}/{tag}/Questionnaire/$next-question` (a `true`
  made explicit) and `POST /Questionnaire/$next-question` (routed by the one adaptive address in
  this gateway's form that the round's contained Questionnaire carries). A round is forwarded
  naming its address (`operationSubpath`), and the payer's gateway delivers it there only when
  its tag verifies. A call the package route refuses for its credential or key store is refused
  the same way. The rounds' own checks refuse as `routing`, apart from a content coding
  (`integrity`), an input or patient that cannot be bound (`authority`), a patient the system of
  record does not hold under `REQUIRE_KNOWN_MEMBERS` (`conformance`) and another patient at
  `strict`. CONFIGURATION lists them in order, per route.
- **Added (provider ingress):** the CapabilityStatement at `GET /metadata` lists SDC's
  `$next-question` beside `$questionnaire-package`. A client still calls each round at the
  `questionnaireAdaptive` address the package names, as DTR 2.2.0 has it do.
- **Added (payer):** a payer's gateway declares the request frames it serves, `v1opsub` among
  them, in the capability declaration it makes to the registrar at each start, so providers'
  gateways re-key addresses toward it and send it rounds. A provider's gateway declares no
  request frames of its own. A gateway rolled back to an earlier release gives the declaration
  back at its first start.
- **Added (payer):** a round framed at another line than its address names is delivered at the
  address's line and recorded as a line carry (`contract.line-carried`, `LineCarried` `request`).
- **Changed (payer):** a payer's gateway states no address tags on a `$questionnaire-package`
  answer at another DTR line than the one the request claims. The tag would bind its own line
  while the provider's gateway wrote the claimed line into the address, so every round would be
  refused. The address is carried as sent instead (`E-12:not-under-base` at the provider's
  gateway), and the client calls the payer directly. A request carried but answered at the line
  it claims is tagged as usual.
- **Upgrade note (provider):** make the three round routes reachable at the ingress base before
  upgrading. A provider whose own proxy, WAF or load balancer admits only the routes earlier
  releases served must add them. E-12 rewrites every tagged adaptive address to one of them and
  has no opt-out, so a proxy that blocks them blocks every round.
- **Changed (monitoring):** the HTTP server metric names the three new routes, so its series
  bound rises by three method-route pairs.
- **Changed (docs):** the provider's attachment-request Task section no longer says an adaptive
  questionnaire's rounds go outside the network.

## An amendment carries a facility's report with only its subject.reference re-pointed (E-14) (v1.8.0)

- **Changed (requester, UC-05):** when the gateway attaches a facility's report, read on the
  federated-query leg, to a prior-authorization amendment it builds, the only change it makes
  to the report is the new registered edit E-14 (`facility-report-subject-rekey`): the report's
  `subject.reference` is replaced with the Claim's patient reference. The `display`,
  `identifier` and `type` of its `subject`, and every other byte of the report, are the
  facility's as they reach the build. Releases before v1.8.0 replaced the whole `subject` with
  `{"reference":<the Claim's patient>}`, dropping its other members, and named no edit. The
  SDK's PAS update builder carries the report and the facility's Provenance as written (see "A
  PAS request the gateway builds carries the report, its Provenance and the prior Claim as
  written").
- **Precondition, unchanged:** the edit is made only once the report, and the Provenance that
  attributes it, are shown to be about the Claim's patient. A report about anyone else is
  refused with `422`, as before, with no edit made.
- **Carried as sent:** a report whose `subject.reference` is exactly the Claim's patient reference,
  and one whose `subject` names the member by identifier alone. Releases before v1.8.0 turned
  an identifier-only subject into a reference; v1.8.0 carries it as the facility wrote it.
- **Newly refused:** a report a signature covers (in the report, or in the facility's signed
  `Provenance` that attributes it), when the edit would change it, with
  `422 signed content cannot be edited (E-14, <carrier>)`, and no amendment is sent. Releases
  before v1.8.0 replaced the subject of a signed report. A signed report already naming the
  Claim's patient needs no edit and is carried as the facility wrote it.
- **Access line:** an amendment that carries a report E-14 changed names `E-14` in
  `relayEdits` and `requestEdits`, before any `E-06`. It stays `ownership: authored` with its
  `builder`.
- **Go API:** `relay.EditFacilityReportSubjectRekey` (`E-14`) is registered on
  `pas-claim-update`, on `DiagnosticReport.subject.reference`, and is an input edit
  (`relay.InputEditIDs` is `E-06`, `E-14`).

## A payer's gateway tags its adaptive questionnaire addresses and restores them (E-13) (v1.7.0)

- **Added (`dtr-questionnaire-fetch`):** on a 2xx answer to a framed `$questionnaire-package` or
  `$next-question` request, a payer's gateway states, inside the sealed answer frame, the base it
  delivered the operation under (`operationBase`), when an adaptive `valueUrl` in the answer is
  under it. It also states a tag for each adaptive `$next-question` address it issues
  (`operationAddressTags`), on the rules CONFIGURATION states: a `true`, and a `valueUrl` spelled
  exactly as that base followed by a clean remainder and written without JSON escapes. The answer is relayed exactly as the payer's system sent it. A provider's
  gateway reads the headers only from the release that turns on its re-key of the address
  (registered edit E-12). A header that cannot be carried is named in the exchange record's
  `answerHeadersNotCarried`.
- **Added:** a `$next-question` round that names its address (`operationSubpath`) is delivered at
  that address only when its tag verifies for the provider that sent it. Otherwise it is refused
  `422 not an address this payer issued, or its base or key changed; fetch the package again`,
  recorded as `routing`. A package request naming an address is refused `400`.
- **Added (registered edit E-13, `dtr-adaptive-address-restore`):** on `dtr-questionnaire-fetch`,
  `cdex-attachment-submit`, `pas-claim`, `pas-claim-update` and `pas-claim-inquire`, a payer's
  gateway restores an adaptive address a provider's gateway re-keyed, whose tag it issued to that
  provider and verifies, to the bytes the payer's system wrote. It does so before forwarding the
  request to that system.
  - The restore is made even where a signature covers the address, so that signature no longer
    verifies in the payer's copy.
  - The forward names the edit in `X-SHN-Edited`.
  - A provider's gateway does not re-key the address in v1.7.0. From the release that makes
    E-12, it re-keys an address only toward a payer's gateway that declares the `v1opsub`
    request frame, which v1.7.0 does not. So no request carries an address to restore yet.
- **Added (`editsNotMade`):** for a re-keyed address naming this payer that the restore cannot
  make, carried as sent: `E-13:unclean-remainder`, `E-13:not-verified` and
  `E-13:unreadable-request` (an address that cannot be rewritten). A request the payer's gateway
  cannot read as JSON is refused `400` by the leg's own read before the restore, so it records no
  E-13 reason.
- **Added (observer, full):** `dtr.adaptive-address-carried`, for an address in the re-keyed form
  that names another payer, carried as sent. It carries the event's kind, direction, leg and
  correlation only.
- **Changed (STABILITY):** a third recorded exception covers E-12 and E-13, at the adaptive address
  only, on the legs named above. The only edits the gateway may make to signed content are now
  E-10, E-11, E-12 and E-13.

## Registered edit ids for an adaptive questionnaire's address (v1.7.0)

- **Go API (additive):** `relay.EditDTRAdaptiveAddressRekey` (`E-12`,
  `dtr-adaptive-address-rekey`) and `relay.EditDTRAdaptiveAddressRestore` (`E-13`,
  `dtr-adaptive-address-restore`) are registered in the relay's closed edit set. The gateway does
  not make E-12 in v1.7.0; it makes E-13 only on an address a provider's gateway re-keyed
  with E-12.

## The gateway builds on shn-sdk v1.8.0 (v1.7.0)

- **Changed:** the gateway pins shn-sdk v1.8.0.

## Pre-Hub refusals that recorded no rule now name it (v1.7.0)

- **Access line and exchange record:** three refusals a gateway makes before the request
  reaches the Hub, which recorded `refusal.rule` `other`, now name their rule.
  - **An origination body over the network's limit:** `413` on the module origination routes
    (`/scenario/…`) that read their body against the limit, now `routing` (the rule of every
    size refusal) with `refusal.reason`.
  - **A payer without framed DTR operations at the DTR package route:** `502`, now `routing`
    with its reason, as module 1's origination records it.
  - **A member your system of record does not hold, under `REQUIRE_KNOWN_MEMBERS=true`:** on
    the CRD, DTR package request, PAS submission and inquiry, coverage eligibility and federated
    query routes, now `conformance`, as the CDex routes already recorded it: a check the
    participant opted into, so it carries no reason. Without the setting, such a member is
    bound from the request and carried, as before.
- **Unchanged:** every answer's status and bytes.
- **Documentation:** `REQUIRE_KNOWN_MEMBERS` is documented for every route that binds a
  member, not only the CRD, DTR and PAS legs.

## A refusal before the Hub records its reason (v1.7.0)

- **Access line and exchange record:** a refusal the gateway makes of its own participant's
  call before the request it refuses reaches the Hub now carries `refusal.reason` on its
  access line, and `RefusalReason` on its `engine.ExchangeRecord` (on a module's origination
  route, an earlier leg of the same call may have been sent). This covers refusals for authentication or
  scope at an ingress, message integrity, authority read from the body, routing with no
  recipient, and size.
  - The reason is the gateway's own fixed refusal sentence: the answer the caller is given,
    or the fixed sentence in it when the answer adds a detail (an identifier, a member name,
    a count, the contract lines). It is never a value from the request, a credential or patient data; a value the
    gateway has not registered is written as `other`.
  - The field is core; its sentences may be reworded or added to in a minor release, so
    decide by `refusal.rule`.
  - Such refusals reach no Hub record, so the gateway's own line and record keep them.
- **Unchanged:** every answer's status and bytes. Refusals of a leg received from the network,
  refusals by the Hub, a peer or the Authorization Framework, and a refusal of an answer that
  came back from the network carry no reason.

## A payer's attachment-request Task at the plain Task endpoint, routed by its owner (v1.7.0)

- **New (payer, `PAYER_DAVINCI_INGRESS`):** your system can create the Task with
  `POST {PAYER_DAVINCI_INGRESS_BASE_URL}/Task`. The gateway routes it by the organization its
  `Task.owner` names: the NPIs the owner states on the owner reference, on the `organization` of
  the PractitionerRole it contains, or on a contained Organization that the owner or that role's
  `organization` names. It goes to the one
  registered provider the network attests them for. The practitioner's NPI is never routed on,
  an identifier in another system names no one, and an NPI attested for no provider is passed
  over when another names a provider.
- **Refused `422`, on `routing`, before the Task crosses the network:**
  - an owner that cannot be read;
  - an owner that names no organization NPI the network attests for a provider;
  - an owner whose NPIs name different providers. The network never picks one.

  Each refusal points to the providers' bases, which work as before. A Task routed to a provider
  that does not take the request is refused by the Hub, `422` on `routing`, as at its base.
- **Records:** exchange records name the route `cdex-request` (`RouteCDexRequest`, new in the
  engine). The observer tag is `cdex-request-ingress`, as at a provider's base.
- **Hosted payers:** the tenant's edge forwards `POST /Task` once the tenant runs shn-gateway v1.7.0 or later.
- **STABILITY.md:** `POST /Task` is now listed once per role, `POST /Task (provider)` for the
  federated query and `POST /Task (payer)` for this route, each classed by its own workstream.

## A payer's attachment-request Task at a provider's base: the provider's identifiers and the owner (v1.7.0)

- **Listing (payer, `PAYER_DAVINCI_INGRESS`):** each entry of `GET /cdex/providers` now also
  carries `identifiers`: the organization NPIs the network attests for that provider, each a
  `{"system","value"}` pair, as the public holder feed serves them. It is omitted for a provider
  attested for none.
- **The owner (payer):** the gateway now reads the organization NPIs `Task.owner` states, on the
  owner reference, the `organization` of the PractitionerRole it contains, or a contained
  Organization that the owner or that role's `organization` names. The practitioner's NPI is
  never read for this. When one is an NPI the network attests for another registered provider,
  or the owner cannot be read, the gateway records the new observer event `cdex.owner-differs`,
  with no identifier. The Task is carried to the base's provider as sent, as before.
- **Reading the Task (`pa/cdextask`):** `Owner.ContainedOrganization` is the Organization a
  `Task.owner` reference, or its contained PractitionerRole's `organization`, names when the Task
  contains it, with its identifiers. Before, an owner naming one read as an unreadable
  PractitionerRole, and a role's contained organization was not read.

## A payer's attachment-request Task may ask for a questionnaire (v1.7.0)

- **Added (`cdex-attachment-request`, evolving):** the leg now also takes a CDex 2.1.0 Task
  Attachment Request coded
  `http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes#attachment-request-questionnaire`,
  a request for a questionnaire to be completed, besides one coded `attachment-request-code`.
  The payer's gateway and the provider's gateway take it alike. It is carried exactly as the
  payer's system sent it, apart from its `payer-url`, which the provider's gateway re-keys to its
  documentation return path (registered edit E-11, `cdex-task-return-rekey`), as for a request by
  code. That includes the questionnaire canonical in its `questionnaire-context` input and any
  `signature-flag` input. The canonical is the payer's identifier for the questionnaire, so it is
  never rewritten. Before, such a Task was refused `403` (`authority`) at both gateways. Your
  system fetches the questionnaire through its gateway's `$questionnaire-package`, on the
  network. An adaptive questionnaire's `$next-question` rounds go to the address its
  `questionnaireAdaptive` extension names, the payer's own system, outside the network.
- **Unchanged:** a Task with any other code is refused `403` with
  `the Task's code is not one this leg carries`, recorded as `authority`, at every conformance
  level. That includes the CDex data requests (`data-request-code`, `data-request-query`,
  `data-request-questionnaire`), either taken code under another system, and a Task with no code.
  The refusal's text now names both codes the leg takes.

## A provider-data request carries its evidence for a member your system keys by its own record id (v1.7.0)

- **Fix (requester, `ORIGINATION_PROFILE=provider-data`):** when your system of record holds the
  member's `Patient` under its own record id rather than the member id, the gateway names the
  patient by the member id in the prior-authorization request it builds. Registered edit E-06
  (`evidence-subject-rekey`) now also covers the clinical records it reads from your system to
  complete that request. For each `Condition`, `Observation`, `ClinicalImpression`, `Goal` or
  `DiagnosticReport` the request references, a record whose own `subject.reference` is exactly
  that record id is re-pointed to `Patient/<member id>`. Nothing else in the record changes, and a
  resource contained in it is not edited. Before, every such request was refused with
  `502 PAS evidence linkage failed`, after the CRD and questionnaire legs had gone out. That
  applied on each submission and update, at every conformance level.
- **Refused:** a record naming anything else, including another patient and a versioned or
  absolute reference, is refused with `502 PAS evidence linkage failed`.
- **Newly refused:** a record naming `Patient/<member id>` while your system holds the member
  under another id. In your system's own numbering it is another record. It was carried before;
  it is now refused with `502 PAS evidence linkage failed`. One a signature covers is refused
  with `422 signed content cannot be edited (E-06, <carrier>)`, as the supplemental report is.
- **Access line:** a request the gateway built that carries a record E-06 re-pointed (these
  records, or the supplemental report an update carries) now names `E-06` in `relayEdits` and
  the exchange record's `requestEdits`. It stays `ownership: authored` with its `builder`, since
  the edit is made to the record before the build. Through the registered transform it names
  `["E-06","E-09"]`. Earlier releases re-pointed the supplemental report's subject but did not
  name the edit there; the provider-data evidence is re-pointed only from v1.7.0, as above.
  A request whose records your system already names by the member id names nothing.
- **Unchanged:** a system that holds the member's `Patient` under the member id, a request that
  references no clinical record, how the supplemental report is re-pointed and refused, the
  `demo` profile, and your Da Vinci ingress and relays.
- **Go API:** `relay.EditEvidenceSubjectRekey` (`E-06`) is registered on `pas-claim` as well as
  `pas-claim-update`, on the five records' `subject.reference`. `relay.InputEdited`,
  `relay.InputEditIDs` and `Payload.InputEdits` are new: they note on the gateway's own message
  the registered edits made to a record its build carries (`E-06` alone). `relay.Rule` gains
  `InputEdits`, the input edits a transmit admits.

## A prior-authorization inquiry your gateway builds carries a dependent's parent (v1.6.0)

- **Behavior change (requester, `ORIGINATION_PROFILE`):** the inquiry the gateway builds to
  continue a pended authorization (`POST /scenario/pa/inquire`, and a route's own `?wait`)
  carries a dependent's parent as the submission does: a person the member's Coverage names as
  its `subscriber` or `policyHolder` by a reference to a `Patient` record of its own. The gateway
  reads that person's record from your system of record by the submission's rule, from the
  Coverage it reads again for every inquiry, and the inquiry carries it as a `Patient` entry of
  its own, your record's own bytes, the Coverage's party by shn-sdk's `CoverageEntryParties`
  (shn-sdk v1.6.0's `PASInquiryRecords.CoverageParties`). Before, the inquiry carried the
  Coverage alone, with a `subscriber` naming no entry of it, which a payer's gateway at `strict`
  refused as `patient.mixed`.
- **Refusals, on `POST /scenario/pa/inquire`, before the inquiry is sent:**
  - a person the Coverage names whose record your system does not hold, or whose record cannot
    ride the inquiry (shn-sdk's `ErrPASCoveragePartyNotCarried`, in its words): `422`, recorded
    as `fidelity` at stage `build`;
  - your system failing to answer the read, or answering with another record than the person's:
    `503` or `502`, recorded as a backend failure.

  On a route's own wait, either keeps the payer's pend and its continuation, as any inquiry that
  cannot be made does: the inquiry is not sent, and the observer event `pa.inquiry-failed` names
  why.
- **Unchanged:** a member who is their own subscriber, or a Coverage naming its parent as a
  contained `Patient`, is inquired about as before.
- **Changed:** the gateway pins shn-sdk v1.6.0.

## The integration guide covers the CDex documentation legs (v1.6.0)

- **Documentation (`docs/INTEGRATION.md`, "Documentation for a prior authorization (CDex)"):**
  the guide now says how a provider sends documentation to a payer (`$submit-attachment`, from
  v1.1.0) and how a payer asks a provider for it (the CDex Task Attachment Request, from v1.5.0):
  what each side sets, where a payer's system creates the Task, and what reaches a provider's
  system, linking each to its section of `docs/CONFIGURATION.md`. The "Native Da Vinci ingress"
  section's note that a signature inside a message travels untouched now names the re-keyed
  documentation return address as the exception. Nothing a gateway does changes.

## The stability promise and configuration guide name the release each statement applies to (v1.6.0)

- **Documentation (`STABILITY.md`, `docs/CONFIGURATION.md`):** statements that named their
  release relative to the reader, or named none, now name the release they apply to. Nothing a
  gateway does changes.

## A request your gateway builds names the member in a self-subscriber's Coverage slots (v1.6.0)

- **Fix (requester, `ORIGINATION_PROFILE`):** when your system of record names the member's
  `Patient` by its own record id rather than the member id, the requests the gateway builds name
  the patient by the member id. That re-key now also covers a Coverage's `subscriber` and
  `policyHolder` that name the member's own record, as well as its `beneficiary`. Before, such a
  Coverage named the subscriber by your system's record id, a record the request did not carry:
  - on the CRD request's prefetch `coverage`;
  - on the questionnaire-package request's `coverage`;
  - on the prior-authorization inquiry the gateway builds to continue a pend
    (`POST /scenario/pa/inquire`, and a route's own `?wait`). With shn-sdk v1.6.0's inquiry
    builder, such an inquiry would otherwise be refused as naming a person whose record was not
    supplied.
- **Unchanged:** the PAS submission and amendment, which already named the member in each slot;
  a slot naming another person, such as a dependent's parent; and a system that names the
  member's `Patient` by the member id.

## Another patient a request names is recorded under an identity it binds to, or left out and counted (v1.6.0)

- **Changed:** when the network reads the involved list, a patient a request names that your
  system of record does not hold as a member is never recorded under an identifier derived from
  its id alone: it is left out of the involved list (counted as `involvedOmitted`, reason
  `unknown-member`). Before, it was recorded under such an identifier, which names no one the
  network knows: a patient named by your system's record id, or by another server's, was
  recorded under an identifier derived from that id. On coverage eligibility, a patient a
  request carries as a Patient under that id or member identifier with a birth date and family
  name of its own is still recorded under the identifier derived from them; on every other leg
  it is left out and counted (below). A patient your system holds as a member is recorded under
  its held identity, as before.
- **Changed:** on PAS submit and update, the leg's own patient is decided by the request's
  structure alone, from the operative (first) Claim, as the leg's own binding reads it: the name
  its `patient` reference reads as, and, when that reference resolves to exactly one Patient
  entry (by the entry's `fullUrl`, or a `Patient/<id>` reference to the entry's id), that
  entry's `fullUrl`, id and member identifiers. None of these is recorded, or counted as left
  out, as another patient, unless another Patient the request carries goes by it too. Every other patient a PAS request names is recorded
  only under the identity your system of record holds for it, and is otherwise left out and
  counted: on PAS a Patient carried with demographics but not held is no longer recorded under
  an identifier derived from them.
- **Changed:** on CRD, DTR and the PAS inquiry, another patient a request names is recorded only
  under the identity your system of record holds for it (as on PAS submit and update, above), and
  only when nothing the request carries ties that name to the leg's own patient: every other
  patient is left out and counted (`unknown-member`), a Patient the request carries with a birth
  date and family name of its own but not held included, so none is recorded under an identifier
  derived from the request. A name is tied when some Patient the request carries, however it carries it
  (contained, in a Bundle entry with or without an id of its own, or as a Coverage's party), goes
  by it and by one of the leg's own patient's names, or by a name tied in turn: by its id, a
  `fullUrl` naming its id, or an identifier's value in any system. The leg's own patient's names
  are what the leg's binding reads it by: on CRD `context.patientId` (a `Patient/` prefix taken
  off), whether or not another resource references it, and never recorded as another patient.
  A name the request gives a patient that cannot be identified, or whose read fails, may be the
  leg's own patient read another way, so names tied to it are left out too (for a failed read,
  `system-of-record-unavailable`), and so are names tied to a member past the 32 members one
  request's pass reads, which is never read (`overflow`). Each distinct Patient the request carries
  under the leg's own patient's names beyond the first is counted too, never folded silently into
  the leg's own patient. When the involved pass runs out of time before it has followed every
  tie, no other patient is recorded for the request: each is counted (`budget`).
- **Known omissions, accepted:** on those legs, another patient your system of record does not
  hold, and a held one tied to the leg's own patient's names (its own member number on the leg's
  own Patient, a second Patient under its name that agrees with the first, or a Coverage's party
  that goes by the same name), are counted as left out. A single Patient the request carries under
  the leg's own patient's name is the leg's own patient, as the leg's own binding reads it.
- **Changed:** a Coverage's `subscriber` or `policyHolder` names no involved patient on any leg,
  even when it names the person by a bare reference the request does not carry (a dependent's
  parent, as a request your gateway builds sends it on CRD and DTR). A contained party, and on PAS
  a party carried as an entry of its own, were already no member.
- **Known omissions, accepted:** a recorded patient is never another person, so these are left
  out and counted rather than risk a wrong name:
  - on PAS, another patient carried with demographics that your system of record does not hold;
  - on PAS, a name the leg's own patient goes by that another Patient the request carries goes
    by too, `Claim.patient`'s own included;
  - on PAS, the leg's own patient's entry names other than `Claim.patient`'s own, when
    `Claim.patient` resolves to no entry or to several (for example, when it names the entry by
    its member identifier rather than its id or `fullUrl`);
  - on coverage eligibility, the leg's own patient named in a later request of a batch without
    the Patient that request's binding reads.
- The subject binding of every leg is unchanged: a member your system does not hold is bound as
  before, from the member id and the Patient the request carries, unless `REQUIRE_KNOWN_MEMBERS`
  is set.

## The configuration docs say which gateways read REQUIRE_KNOWN_MEMBERS (v1.6.0)

- **Documentation (`docs/CONFIGURATION.md`, "Members your system of record does not hold"):**
  a provider's and a payer's gateway read `REQUIRE_KNOWN_MEMBERS` wherever they bind a patient a
  request names, the federated query at a provider's ingress and the CDex attachment request
  included. A facility's gateway answers only the federated query, and a PHG's only the
  patient's questionnaire package; on that leg each refuses a patient its system of record does
  not hold (`403`), at every level, whatever the setting. Nothing a gateway does changes.

## A provider's gateway carries the payer's attachment request to its own system (v1.5.0)

- **Added (evolving):** from v1.5.0, `PROVIDER_CDEX_TASK_URL` names your system's own Task
  endpoint. With it set, a provider's gateway declares `cdex-attachment-request`, serves the
  route the Hub delivers it on, and carries the payer's Task to that endpoint. It requires the
  forward's SMART Backend Services credentials, `PROVIDER_CDEX_TOKEN_URL`,
  `PROVIDER_CDEX_CLIENT_ID` and `PROVIDER_CDEX_SCOPE` (no default) with either
  `PROVIDER_CDEX_CLIENT_KEY` and `PROVIDER_CDEX_CLIENT_ALG` (`ES384`, `RS384` or, on this
  setting only, `RS256`; `PROVIDER_CDEX_CLIENT_KID` optional, no `kid` sent without it) or
  `PROVIDER_CDEX_CLIENT_SECRET`, and `PROVIDER_DAVINCI_INGRESS` with its base URL. A gateway
  with any of them missing, or of another role with any of them set, does not start.
- **Added:** the Task reaches your system as the payer sent it but for its `payer-url`, which is
  re-keyed to your gateway's documentation return path for the payer that sent it (registered
  edit E-11, disclosed by `X-SHN-Edited: cdex-task-return-rekey`), and with
  `X-SHN-Requester: <payer participant id>; identifiers="<system>|<value> …"` naming that payer
  and the identities the network attests for it. A Task whose `requester` identifier is not one
  of them, or cannot be read, is carried and noted as the observer event
  `cdex.requester-differs`. Your system's answer, a `201` or an error, reaches the payer as your
  system wrote it.
- **Added:** before the Task is sent, your gateway refuses, at every conformance level, a body
  that reads two ways (`400`, integrity), one that is not a Task or whose `Task.code` is not
  `PASTempCodes#attachment-request-code` (`403`, authority), and a Task naming no one member
  number (`422`, authority); your gateway binds the member number against your system of record,
  as on every delivered leg. A Task whose `payer-url` cannot be re-keyed is carried as sent and
  the exchange record says why (see the `payer-url` re-key entry); only a re-key with no return
  path to write, for a payer whose participant id cannot form one, or one that fails its own
  verification, is refused `422` on the fidelity of SHN's own edit, and the Task is not sent.
- **Added:** the forward, its token request included, is bounded at 25 seconds, as a payer's
  gateway bounds its calls to its own system by default, so a provider's system that does not
  answer is answered to the payer inside the network's leg budget: `504 the provider's system …
  in time`. A token endpoint that does not answer in 10 seconds, the token client's own timeout,
  reads as `502 the provider's system could not be reached`.
- **Added:** the endpoint kinds `cdex-task` and `cdex-task-token` for the forward and its token
  endpoint, checked at boot like every outbound setting.
- **Note:** a hosted provider tenant takes it the same way on SHN Cloud, authenticating with
  `PROVIDER_CDEX_CLIENT_SECRET` or with a key SHN Cloud mints for it (`PROVIDER_CDEX_CLIENT_ALG`);
  the client assertion carries no `x5c`. A provider the network's provisioning registers itself
  cannot take the leg yet.

## A payer's attachment-request Task's payer-url is re-keyed to the network return path, even where signed (v1.5.0)

- **Before you upgrade:** this is a second edit the gateway makes to signed content, after the
  PAS pend's re-key, recorded as its own exception in `STABILITY.md`. It applies only to the
  payer's attachment-request leg (`cdex-attachment-request`), which is new in v1.5.0.
- **Added:** as a provider's gateway carries a payer's attachment-request Task to its own
  system, it replaces the `valueUrl` of the Task's one `payer-url` input with its own
  documentation return path for the payer that sent the Task (registered edit E-11,
  `cdex-task-return-rekey`), so documentation the provider sends there reaches that payer
  through the network. The value is rebuilt from that payer's network identity, never read
  from the Task. It is made by default, even where a signature covers the `payer-url`; that
  signature then no longer verifies in the provider's copy, and the forward names the edit, and
  any signature it broke, in `X-SHN-Edited`
  (`cdex-task-return-rekey; signed="<signatures>"`).
- **Added (record):** a Task the gateway cannot re-key is carried as the payer sent it, and the
  exchange record's `EditsNotMade` says why:
  - `E-11:no-structural-payer-url`: its one `payer-url` is not a non-empty `valueUrl`.
  - `E-11:unreadable-payer-url`: it has several `payer-url` inputs, or an input that cannot be
    read, so which address to re-key is unknown and none is.
  - `E-11:payer-does-not-take`: the payer is not known to take the documentation submission.

  A Task whose return path cannot be written (a payer whose participant id cannot form one), or
  whose re-key fails its own verification, is refused `422` as a fidelity refusal of the
  gateway's own edit, and is not sent.
- **Unchanged:** every other edit of signed content is still refused `422`, and E-10 and E-11
  are made only at a `payer-url`.

## A payer's own system creates the attachment-request Task at a provider's base on its gateway (v1.5.0)

- **Added (evolving, payer):**
  - **The ingress.** `PAYER_DAVINCI_INGRESS=1` mounts the payer-side Da Vinci ingress for the
    `cdex-attachment-request` leg. It needs `PAYER_DAVINCI_INGRESS_BASE_URL` and
    `INGRESS_CLIENTS_FILE`, by the provider ingress's all-or-nothing rule, and a gateway of any
    other role refuses to start with it set.
  - **Where your system posts.** Your system creates a CDex 2.1.0 Task Attachment Request at a
    provider's base on the gateway, `POST {PAYER_DAVINCI_INGRESS_BASE_URL}/cdex/provider/{provider}/Task`,
    and the gateway sends it through the network to that provider. `GET /cdex/providers` lists the
    bases of the registered providers whose gateways declare the leg, which a provider's gateway
    does only once it is configured to take the Task. There is no plain `POST /Task`:
    a Task names its provider only by an identifier the network does not hold for registered
    providers.
  - **Authentication.** It uses SMART Backend Services from the gateway's own `POST /oauth/token`.
    The Task routes take only a bearer granted exactly `system/Task.write`, which
    `/.well-known/smart-configuration` advertises on a payer's gateway. A client registered with no
    scopes is registered for it; one whose scopes leave it out refuses the gateway's start.
  - **Refusals**, made by the network at every conformance level and recorded as refused by
    `payer-gateway`:
    - a bearer without the scope: `403 insufficient_scope` (`authority`);
    - a body that reads two ways: `400` (`integrity`);
    - not a Task, or a `Task.code` other than `PASTempCodes#attachment-request-code`: `403`
      (`authority`);
    - a base naming no registered provider: `422` (`routing`);
    - a Task that names no one member number for `Task.for` (an identifier typed
      `v2-0203#MB`, on the contained Patient or on `Task.for.identifier`): `422`
      (`authority`). The patient is bound by that number. For a member your system of record
      does not hold, the identifier derives as on the provider's CDex submission: from the
      number alone, unless a carried Patient has the number as its `id` or under
      `urn:shn:member`. In that reading, a number shared with another person the Task carries,
      with no Patient of the member's own, is also `422` (`authority`);
    - a provider's answer that repeats a member name: `502` (`integrity`). It can be read two
      ways; an answer is otherwise relayed as the provider's side sent it.
  - **No `Location`** is added to the answer.
- **Changed (record):** a payer's gateway records a refusal on its own ingress as refused by
  `payer-gateway`. Until this ingress, only a provider's gateway had one.
- **Additive:**
  - the exchange route `cdex-request-base` and the observer tag `cdex-request-ingress`.
- **Additive (Go API):**
  - package `pa/cdexroute`: `cdexroute.ProviderBase` and `cdexroute.ProviderPrefix`;
  - `engine.TaskWriteScope`.

## A leg goes only to a gateway of the role that answers it (v1.5.0)

- **Added:** a gateway that receives a leg whose row names another role for its recipient (a
  misrouted or injected envelope) refuses it `422` before reading its token or calling its
  system (`this gateway cannot take <leg>: it is a <role> gateway, and <leg> is answered by
  <roles>`), recorded as `routing` and as its own refusal, at every conformance level. Before, a
  payer's gateway that received a provider's leg answered it with that leg's handler.
- **Added:** the network's Hub refuses a leg to a recipient registered with another role than
  the leg's row names for its recipient, before reaching it. Your caller gets that `422` with the
  Hub's reason as it stands (`the recipient cannot take <leg>: it is registered as <role>, and
  <leg> is answered by <roles>`), as for a leg the recipient does not take, recorded as refused by
  the Hub on `routing`.
- **Changed (record):** on the requester's gateway, a refusal by the recipient's gateway that the
  Hub relays is recorded as refused by the gateway of the role the leg's row names for its
  recipient: `provider-gateway` for the payer's attachment request, `payer-gateway` for every
  leg a payer answers, as before. A facility's or PHG's gateway has no refusing party of its own yet,
  so a refusal by one now reads `other` where it read `payer-gateway`.
- **Changed (record):** an answer over the size this gateway reads is recorded as this gateway's
  own refusal, whichever role sent the leg. It read `provider-gateway` on every gateway.
- **Changed:** when the Authorization Framework does not answer, the `503` reads `the
  authorization service could not be reached, so this leg was not sent to the recipient`, where it
  named the payer: a payer's gateway sends a leg to a provider too.
- **Changed:** a provider's gateway serves the route the Hub delivers legs on, and needs the
  Hub's transport key, only once it takes a leg the network delivers to it: from v1.5.0, the
  payer's attachment request, with `PROVIDER_CDEX_TASK_URL` set. Without it, a provider's gateway
  serves neither, as before. Which role serves the route is now decided by the legs it declares.

## The payer's CDex attachment request is a leg of the network (v1.5.0)

- **Added (evolving):** a new leg, `cdex-attachment-request` (staged
  `pa.cdex-attachment-request`): a payer's CDex 2.1.0 Task Attachment Request to a provider, the
  one leg a payer sends. Its request frame is `payer-payment` and its response frame
  `provider-disclosure`, with operations `cdex-attachment-request` and
  `cdex-attachment-request-response` and scope `attachment-request-only`. It is marked as coming
  in v1.5.0, so the network routes it only to a provider whose registry entry lists it.
- **Added:** a provider's gateway carries the Task only to a Task endpoint its own system has
  (`PROVIDER_CDEX_TASK_URL`). A provider's gateway without one does not declare the leg or
  serve the route the Hub delivers it on, so the Hub refuses a Task to it `422` (`the recipient
  does not take …`), recorded as refused by the Hub on `routing`, at every conformance level.
  Every other leg each role declares is unchanged.
- **Changed (record):** a provider's gateway records a refusal it makes of a leg the network
  delivered to it as its own, refused by `provider-gateway`, where it recorded `other`. Until
  this leg, no leg was delivered to a provider's gateway.
- **Changed:** the gateway pins shn-sdk v1.5.0.

## A questionnaire fill that fails the gateway's own check is refused, and every answer after a leg says which legs were carried (v1.5.0)

- **Before you upgrade (requester, `ORIGINATION_PROFILE`):** on the `/scenario/*` routes, a filled
  QuestionnaireResponse that fails the gateway's own check of its fill is now `422` (was `502`),
  recorded as `fidelity` at stage `verify` (was `other`), and ends with the sentence below. It
  comes after the coverage check and questionnaire legs, which were sent and stand, and the PAS
  claim is not sent:
  - `populated QR subject does not match patient`: a fill about another patient than the
    request's, or naming no patient, whether your operated `$populate` engine returned it or the
    gateway's own check finds it;
  - `populated QR questionnaire does not match canonical`: a fill for another questionnaire than
    the one fetched, or naming none.

  A fill that names the request's patient and the fetched questionnaire, with or without a
  version, is carried as before.
- **Changed (requester):** every other answer your gateway writes itself after a leg of the
  request was carried now ends with the sentence saying which legs it sent, for example `The
  crd-order-select and dtr-questionnaire-fetch legs this request sent were carried and stand.`,
  said once. Its status and what it is recorded as are unchanged. Among them:
  - `fetched questionnaire package has no Questionnaire`, `fetched questionnaire url parse
    failed` and `fetched questionnaire does not match advertised canonical`;
  - `expected pended response`, `parse pended response failed`, `claim response parse failed`,
    `expected PA-required card` and `expected denial but got approval`;
  - a payer answer your conformance level refuses, and your own built messages failing their
    validation after a leg;
  - `holder write failed (auth number)`;
  - on `/scenario/uc06/complete` and `/scenario/uc07/complete`, the same answers after the legs
    that call sent.

  An answer written before any leg is unchanged, and an answer relayed from a leg or the network
  is carried as it came. See CONFIGURATION, "Where a request your gateway builds is refused".

## A request your gateway builds carries a dependent's parent (v1.4.0)

- **Behavior change (requester, `ORIGINATION_PROFILE`):** a member whose Coverage names another
  person as its `subscriber` or `policyHolder`, by a reference to a `Patient` record of its own
  (a dependent's parent, the PAS IG's subscriber), now gets a prior-authorization request. Before,
  the request was not built: `500 build bundle failed`. On the `/scenario/*` routes, the gateway
  reads that person's record from your system of record at the PAS claim's build. Every PAS
  submission and amendment it builds under that Coverage carries the record as a `Patient` entry
  of its own. The Coverage's slot names that entry, the Coverage's party by shn-sdk's
  `CoverageEntryParties` (shn-sdk v1.4.0's `CoverageParties`). The request the gateway completes
  from your system of record (`ORIGINATION_PROFILE=provider-data`) keeps that entry. Any other
  second `Patient` entry is still refused there.
- **Refusals, at the PAS claim's build:**
  - a person the Coverage names whose record your system does not hold, or whose record cannot
    be carried as it is (it contains other resources, makes a reference, or is another version
    than the slot names): `422`, recorded as `fidelity` at stage `build`;
  - your system failing to answer the read, or answering with another record than the person's:
    `503` or `502`, recorded as a backend failure.

  Each comes after the legs the request sent before the claim (the coverage check, and the
  questionnaire when the payer asked for one), which stand. The answer names them.
- **Unchanged:** a member who is their own subscriber, or a Coverage naming its parent as a
  contained `Patient`, is built as before. A prior-authorization inquiry the gateway builds for a
  dependent does not yet carry the parent.

## A gateway reads a CDex Task Attachment Request by structure (v1.4.0)

- **Additive (Go API):** package `pa/cdextask`
  (`github.com/SmartHealthNetwork/shn-gateway/pa/cdextask`), which reads a CDex Task Attachment
  Request by structure. No route of v1.4.0 uses it.

## On a provider's /scenario/uc05, a facility record about another patient is refused at strict only, unless it is the claim update's evidence (v1.4.0)

- **Before you upgrade:** this changes what a provider gateway that declares
  `ORIGINATION_PROFILE` (`demo` or `provider-data`) answers on `POST /scenario/uc05` when the
  facility's federated answer carries a record about a patient other than the member.
  - **A record that is not the evidence:** at `none`, `observe` and `structural`, it is now
    carried, and the flow goes on to the claim update. Examples are a `DocumentReference`
    leg's record, or a record beside the report on the `DiagnosticReport` leg. The record is
    `patient.answer` (recorded at `observe` and `structural`; nothing is recorded at `none`).
    Before, it was refused `502 federated response refused: …` at every level. At `strict`
    it is still refused with that `502`, now recorded as `conformance`.
  - **The report the claim update carries as evidence** (and the `Provenance` that
    attributes it): if it is about another patient, it is now refused
    `422 build update bundle failed: … (fidelity of SHN's own build)` at every level. This
    is recorded as `fidelity` at the build stage, and no claim update is sent. Before, it was
    `502 federated response refused: …`. A caller that reads the `502` for this case reads
    the `422` instead.
  - **Both refusals** now say which legs the request had already sent; those legs were
    carried and stand.
- **Why the two differ:** the claim update is the provider's own message, and the gateway
  builds the report into it with the report's `subject` re-pointed at the Claim's patient,
  as it already did. Built from another patient's report, it would state what the facility
  never said, so that is a refusal of the gateway's own build, at every level. Any other
  record on either leg is used for nothing; whether it is about the member is the content
  check `patient.answer`. A participant's own federated query, through the facility's Da
  Vinci answer, already applies that check the same way.
- **Also refused 422 at every level:** on the report's leg, an answer whose entries the gateway
  cannot index to resolve the evidence's references (a repeated `fullUrl`, an entry whose
  `resource` is `null`). On the `DocumentReference` leg, the same answer is `patient.answer`,
  as above. Before, both were `502 federated response refused: …` at every level.
- **Unchanged:** a provider that does not declare `ORIGINATION_PROFILE` serves no
  `/scenario` route. A facility answer that cannot be read, or holds no report with its
  `Provenance`, is `502 federated response parse failed: …` as before.

## A system-of-record search your server says it cannot run is told apart from an error (v1.4.0)

- **Before you upgrade (provider):** the built-in FHIR connector no longer reads every error
  status from your FHIR server's search as "search not supported". It now reads the status and
  the answer's `OperationOutcome` by structure, never its text. Only `501`, or a `4xx` other
  than `401`, `403` and `429` whose `OperationOutcome` has at least one `error` or `fatal`
  issue, every one with the code `not-supported`, means your server cannot run the search. Any
  other error answer (`400`, `404` or `422` without that `OperationOutcome`) is a backend
  failure. These change, at every
  conformance level:
  - **A CDS Hooks or `$questionnaire-package` request that carries no coverage,** when the
    coverage search gets such an error answer: `502 the system of record answered the coverage
    search with an error` (was `422 coverage not in request, and the system of record cannot
    search for it`). It is never recorded as a refusal.
  - **A federated query's records,** when a search gets such an error answer: `502` (was the
    connector's `FacilityRecordsContext` records, one per type).
  - **An originated `order-select` request's draft order,** when its search gets such an
    error answer: `502` (was `422 the system of record cannot search for the member's draft
    order`).
  - **Recorded outcome:** such a search is now recorded as outcome `failed` (was
    `unsupported`) in the `prefetch.obtained` event and the holder log line. A monitor that
    matches `unsupported` sees the new value.

  A FHIR server that answers a search it does not support with a bare `400` or `404` now gets
  `502`s where it got `422`s. To keep the `422`, answer `501`, or include an
  `OperationOutcome` issue with code `not-supported`.
- **Unchanged:**
  - A prefetch value whose search gets an error answer is left out, as before.
  - The coverage read a request your gateway originates first makes to route by, behind the
    built-in FHIR connector, still answers `502` for a definite not-supported answer, as for an
    answer past the gateway's bounds.
- **New (connector authors):** `engine.SearchFailed` (`failed`) is the outcome of a search
  your system answered with an error that does not say it cannot run it. Return
  `engine.SearchUnsupported` only for a definite answer: the gateway reads it as your system
  being unable to supply the value. `SearchFailed` is recorded as a backend failure of class
  `http-4xx`.

## A PAS or DTR endpoint a payer publishes is the base its operations go under (v1.4.0)

- **Before you upgrade:** if your system publishes a full operation URL in its
  `.well-known/davinci-configuration` (for example `…/Claim/$submit` as
  `davinci_pas_submission_endpoint`) instead of the base HRex 1.2.0 defines,
  publish the base. The gateway adds the operation's path to the published
  value, so a full operation URL becomes
  `…/Claim/$submit/Claim/$submit`. A system that publishes a base, or publishes
  no `.well-known/davinci-configuration`, needs nothing.
- **Change (behaviour):** a payer gateway that follows the PAS or DTR endpoint
  the payer publishes in its `.well-known/davinci-configuration` invokes each
  operation under it, as HRex 1.2.0 defines the value:
  - `davinci_pas_submission_endpoint#<line>` is the base for `/Claim/$submit`
    (submit and update) and `/Claim/$inquire`;
  - `davinci_dtr_qpackage_endpoint#<line>` is the base for
    `/Questionnaire/$questionnaire-package`, and the gateway sends
    `/Questionnaire/$next-question` under the same base.

  Earlier releases posted every one of those operations to the published value
  itself, as if it were the operation's own URL. A payer publishing a base, as
  the IG defines, therefore received each PAS request Bundle at its FHIR base,
  and an inquiry went wherever a submission did.
- The operation's path is added where the published value's path ends,
  before any query or fragment: the slashes that end the published path are
  dropped first, so none is doubled, and the rest of the URL the gateway
  builds is the published value as written, sent as any URL the gateway
  calls is. Nothing is read from how the published path ends: a value that
  already names an operation is a base like any other.
- The CDex `$submit-attachment` forward already read
  `davinci_cdex_attachsubmit_endpoint` as a base; it now shares the same rule.
- Unchanged: a published value is followed only when it is same-origin with
  the base its contract uses. With none, the PAS and DTR forwards go under the
  configured base as before, and so does the CDex forward, except that a query
  or fragment on `PAYER_DAVINCI_BASE_URL` now stays after
  `/$submit-attachment` instead of coming before it.

## The validator and evaluation images run on the Temurin 21.0.13+9 distroless Java base (v1.4.0)

- **Changed:** the validator image (`deploy/validator`) and the evaluation HAPI image
  (`deploy/eval/hapi`) run on the Temurin 21.0.13+9 `gcr.io/distroless/java21-debian13:nonroot`
  base, pinned by digest, in place of Temurin 21.0.12.1+1. What the validator checks does not
  change.
- **Changed:** the new base's Java trust store no longer carries the Comodo "AAA Certificate
  Services" root. After the build removes the eMudhra emSign roots, as before, both images trust
  145 roots, where they trusted 146.
- **Changed:** `deploy/validator/verify.sh` checks the built image's environment against the
  runtime base it runs on, the image's `hapi-runtime` stage. That base must set the same variables
  as the validation engine image its WAR comes from, to the same values, apart from the JRE's
  version, which may differ only within the same Java feature release. The engine image still
  fixes the image's command, user and working directory.
- **Unchanged:** a validator or evaluation image built from a release before v1.4.0, or a
  validator tag published before it, keeps its own base: rebuild it from v1.4.0, or move to a
  validator tag published from it. The install bundle builds its validator from source, so
  rebuilding the bundle at v1.4.0 moves it to the new base.

## A request your gateway builds is refused at the leg that consumes what is missing (v1.4.0)

- **Before you upgrade (requester, `ORIGINATION_PROFILE`):** the `/scenario/*` routes your
  gateway answers now refuse at the first leg that cannot be built, never before an earlier leg
  that can still succeed. Some refusals that came before any leg now come after the coverage
  check (CRD) and the questionnaire (DTR) legs, which were sent and stand. Some change status. A
  caller that reads "nothing was sent" from these refusals must read their last sentence instead.
  These now come after legs were sent:
  - **At the PAS claim, after CRD and DTR,** `422`, recorded as `fidelity` at stage `build`:
    - the member the request names, and the order it states its claim facts for. A request
      naming no member or no order was `400`; it is now `422`;
    - a claim fact the routed line requires;
    - a payer Organization record your system does not hold;
    - the records the order's requester leads to;
    - the requesting provider your records cannot supply. This was `502`; it is now `422`;
    - UC-03's oxygen attestation clinician;
    - a dispatched order's supplier your system does not hold. This was `502`; it is now `422`;
    - an order with no product coding a claim's item can name. This was `502` before any leg;
      it is now `422` at the claim, and UC-02's coverage check now carries such an order.
  - **At the amendment, after the submission the payer pended:** an amendment's
    `certificationType` and `reportType`, UC-06's attesting clinician and UC-06's answer. The
    pend stays at the payer, as it would for any amendment not sent.
  - **At `/scenario/uc06/complete` and `/scenario/uc07/complete`,** the member, order, facts and
    route the amendment is built from are checked at the amendment. A request naming no member
    or no order was `400`; it is now `422`. An order of record with no product coding was `502`;
    it is now `422`. On `/scenario/uc07/complete` this comes after the patient questionnaire
    exchange, which needs none of it.
  - **At UC-07's patient questionnaire exchange:** UC-07's answer.
  - **A record your system fails to answer for** now fails where the record is first needed,
    not before any leg. It is answered `503` or `502`, a backend failure, never a refusal. The
    payer Organization record fails at the questionnaire request when the coverage check asks
    for a questionnaire, and otherwise at the PAS claim. The requester's records, the requesting
    provider and a dispatched order's supplier fail at the PAS claim.
  - **A payer that declares no framed DTR operations** is refused at the questionnaire leg,
    after CRD: `422` (was `502`), recorded as `routing`.
  - **No patient health gateway registered** for UC-07's patient exchange: `422` (was `500`),
    recorded as `routing`, before that exchange (after the submission, on the single call).
- **Changed (requester), before any leg:**
  - A `wait` that is not a whole number of seconds is refused `400` before any leg, on every
    route that follows a decision; it was refused after CRD and DTR.
  - A member your system does not hold: `400 unknown member`, now recorded as `routing`.
  - No open order or draft order, an open order about another product than the route
    originates for, a dispatched order with no id or performer, or a record the gateway cannot
    name the member in: `422` (was `502`), recorded as `fidelity` at stage `build`.
  - A record your system returns about another patient: `422` (was `502`), recorded as
    `fidelity` at stage `verify`.
  - A Coverage search your system fails to answer usably: before any leg, still `503` when it
    does not answer and `502` otherwise, now recorded as a backend failure (was `other`), or as
    `integrity` for an answer that repeats a member name, and ending with the sentence below.
    Behind the built-in FHIR connector this is any failure of that search, including an answer
    past the gateway's bounds and an error status, one saying your system does not support the
    search included. Behind a connector whose coverage read is not that search, the coverage
    the coverage check carries fails the same way when its search does, and is `502` (was
    `422`) for an answer the gateway cannot read or an error answer that does not say the
    search cannot run; a connector that cannot search, a definite not-supported answer, or an
    answer past the gateway's bounds stays `422`, recorded as `fidelity` at stage `build`.

  A read your system fails to answer stays `503` or `502`, before any leg or after some, and is
  now recorded as a backend failure wherever it comes (it was recorded as `other`, or, after a
  leg, as a refusal): the patient, the open order, a payer the coverage names by reference, the
  clinical context the questionnaire is filled from, an order's supporting records, the order's
  requester, the supplemental report UC-04's amendment attaches, the evidence a PAS request
  links, the patient reference a pended claim is recorded with, and, on `/scenario/pa/inquire`,
  the order, patient, coverage, provider and payer records an inquiry is built from. The
  evidence, the pended claim's patient reference and the inquiry's reads were `502` whatever
  the failure, and now answer `503` when your system does not answer. Your operated `$populate` engine failing to answer is recorded the
  same way. These failures after a leg now end with the sentence below too: a fill or an adaptive
  questionnaire round that fails, a decision that cannot be followed, a coverage card that
  cannot be read, an order with no id, evidence that cannot be linked, and a questionnaire
  request the gateway cannot carry unchanged. An answer relayed from a leg or the network is
  carried as it came. The sentence is said once.
- **Changed (requester), the refusals above:** each refusal and backend failure listed above ends
  with a sentence saying which legs it carried: `The crd-order-select and dtr-questionnaire-fetch
  legs this request sent were carried and stand.`, or `No leg of this request was sent.` A
  request that can still succeed without the refused leg is carried, whatever that leg would
  have needed:
  - a payer's not-covered answer;
  - the no-PA coverage check;
  - UC-01's eligibility check, which no longer checks the member the request names;
  - UC-05 when the disclosure is denied.

  See CONFIGURATION, "Where a request your gateway builds is refused".

## The involved list reads a request as the leg's deciding readers do (v1.4.0)

The involved list, the other patients a prior-authorization request carries and the Hub records the exchange under, now reads the request with the same strict decoder as the leg's subject binding and routing. A request that repeats a member name, exactly or in another case, names no other patient, just as an unreadable request names none.

Nothing changes on the wire, and no exchange's involved list changes. Every request this gateway sends on such a leg is already read strictly first, and one that repeats a member name is refused before it is sent. The involved list no longer depends on that.

## A Coverage's parent carried as a Patient entry is no longer read as another patient (v1.4.0)

- **Fix:** a coverage search the gateway runs on your system of record may return
  `Patient` entries as included records. The gateway now drops every one except the Patient
  a Coverage's `beneficiary` names, instead of reading it as another patient's record.
  - The usual one is the parent a dependent's Coverage names as its `subscriber` or
    `policyHolder`. R4 also lets a Coverage's `payor` name a Patient.
  - Earlier gateways refused such a search (`502`, at every conformance level) on these reads:
    - the routing read;
    - the coverage `ENRICH_NATIVE_REQUESTS` adds;
    - the coverage of a CRD request the gateway originates.
  - A dropped Patient is not sent to the payer.
  - The Patient a `beneficiary` names is kept and carried as before, and must be the patient.
    Each Coverage is still checked against the patient by its `beneficiary`.
  - The routing read of a `$questionnaire-package` request already checked each Coverage on
    its own and never carried an included record, so it does not change.
- **Fix:** the check that a prefetch or `$questionnaire-package` value is about one patient
  (`patient.mixed`) now accepts a Coverage's party carried as a `Patient` entry of its own,
  as it already accepted one contained in the Coverage. That check is not run at `none`,
  records at `observe` and `structural`, and refuses at `strict`.
  - The entry must be one shn-sdk's `CoverageEntryParties` names for its Bundle, the same
    rule the PAS checks use. Among its conditions: nothing else in the Bundle refers to the
    entry, and the Coverage's beneficiary is another `Patient` entry of the Bundle.
  - Any other `Patient` entry is still checked against the patient.
- **Fix:** the coverage read through a CDS Hooks request's `fhirServer` no longer refuses an
  answer because it holds a `Patient` entry (`412 … is not a Coverage searchset`, at every
  level). It treats one the way the gateway's own searches do.
  - A `Patient` entry a Coverage's `beneficiary` names is checked with the Coverages and must
    be the patient (`502` when it is another).
  - Any other `Patient` entry, such as the parent the EHR's server returns as an entry of its
    own, is dropped before the check.
  - What the read carries as `prefetch.coverage` does not change: the chosen Coverages and their
    payor Organizations, never a `Patient`.
- **Unchanged:** the federated-query checks read only a party contained in the Coverage.

## A dependent's parent carried as a Patient entry of its own is the Coverage's party, on the request and the answer (v1.4.0)

- **Behavior change:** a Coverage's party may be a `Patient` entry of its own that only a
  Coverage's `subscriber` or `policyHolder` names, as well as a Patient contained in the
  Coverage. The entry is named by its exact `fullUrl` or by a relative reference that resolves
  to it against the Coverage entry's base (never by id alone), and nothing else in the Bundle
  refers to it. shn-sdk's `CoverageEntryParties` decides it (shn-sdk v1.3.0); the gateway keeps
  no copy of the rule. The rule does not check that the Coverage's `beneficiary` is the member:
  the checks below do, and refuse another member as before. The PAS IG carries its subscriber
  profile this way.
  - The request's check for another patient (rule `patient.mixed`), on `Claim/$submit`, an
    amendment and `Claim/$inquire` (the inquiry's member check included), reads the parent's
    entry as the Coverage's party, as it reads a parent contained in the Coverage: its own
    identity is not read as another patient, the slot's reference and an identifier beside it
    that the parent carries name the parent, and what the parent's entry references is read.
  - The answer's check (rule `patient.answer`), on a `Claim/$submit` answer and a
    `Claim/$inquire` answer, at the payer's gateway and the provider's, reads it the same way:
    the parent's entry is no second patient, and the slot naming it, typed `Patient` or not, is
    no reference to the patient.
  - The patients a request involves, which the Hub records the exchange under, do not include
    the parent, and do include a patient the parent's entry names (one it links to, say),
    as for a parent contained in the Coverage. A refused token for that patient leaves them
    out with the refusal counted, and the leg is sent as usual.

  Before, such a request was refused at `strict` (`403 inconsistent patient in PAS bundle`,
  `403 inconsistent patient in PAS inquiry`), and such an answer refused at `strict` (`PAS
  response has inconsistent patient linkage`, `PAS inquiry answer has inconsistent patient
  linkage`), each recorded and carried at `observe` and `structural`. Now both are carried and
  read at every level, and the answer written from as any readable answer is.
- **Unchanged:** a `Patient` entry no Coverage names, one a relative reference names that does
  not resolve to its `fullUrl`, one anything else in the Bundle refers to (the Claim, an
  extension, a beneficiary), and another patient's Coverage entry beside that patient are
  refused at `strict` as another patient, and recorded and carried at `observe` and
  `structural`; nothing is checked at `none`. A `$submit` answer whose slot names, by a
  relative reference, an entry the Bundle does not hold is refused by the answer's own graph
  check (`answer.shape`) as before. The request a provider's gateway builds from its system of
  record (`ORIGINATION_PROFILE`) carries the parent as an entry of its own from v1.4.0
  (see "A request your gateway builds carries a dependent's parent"). The
  checks of CDS Hooks prefetch and `$questionnaire-package` values read a Coverage's party
  carried as a `Patient` entry by the same rule; federated query values read only a contained
  party.

## A payer's 2xx answer reaches the provider's system with its ETag, Last-Modified and Retry-After (v1.3.0)

- **Before you upgrade:** nothing to configure. The three headers reach a participant's system
  only when both the payer's gateway and the provider's gateway run v1.3.0 or later; until both
  do, answers arrive without them, as before. Once they do, your system receives a payer's
  `ETag`, `Last-Modified` and `Retry-After` on a 2xx answer as it would on a direct call, so a
  system that acts on them (a cache keyed on `ETag`, a client that honours `Retry-After`) starts
  doing so.

- **Added:** the gateway frames these three from its system's 2xx answer, and the
  requesting gateway writes them to its own participant's system as sent, on every Da
  Vinci ingress route, so they read as on a direct call. A value is never judged as a
  validator or a date. A non-2xx answer and an answer the gateway built state none.
  Locations are not carried: each is an address into the payer's system.
- **Added (record, access line):** `engine.ExchangeRecord.AnswerHeadersNotCarried` and
  the access line's `answerHeadersNotCarried` name a header that was not carried, and
  why, never its value: `value-not-carriable` (not an HTTP field value, or not UTF-8),
  `over-frame-limit`, `over-answer-limit` (with it, the answer
  would leave less than 64 KiB under the network's limit for its authorization, so it is
  sealed without its headers before that is minted) or `decoded-from-gzip`. The answer goes through without it: a header never
  costs an answer.
- **Changed:** an answer carrying a strong `ETag` is no longer gzip-coded for a caller
  that accepts gzip, since coding would make the validator untrue. A weak `ETag` is coded.
  A strong `ETag` on an answer the transport decoded from gzip is left out and recorded.
- **Changed (edits):** registered edits E-08 and E-10 drop the payer's `ETag` from the
  answer they change, under their own disclosure. `relay.ValidateEdits` refuses an edit
  of an answer whose row does not drop it (`relay.Edit.DropsAnswerHeaders`).
- **Fixed:** the limit on an answer also covers its frame header: an answer whose own
  `Content-Type` is over the frame's 64 KiB header limit is refused `502 response body too
  large`, delivered, recorded as `routing`. Before, the requesting gateway failed to read
  the frame.

## A questionnaire-package request your gateway builds carries the payer Organization your Coverage names (v1.3.0)

- **Changed (requester):** when your gateway builds the `$questionnaire-package`
  request itself, and a Coverage it carries names its payer (its first `payor`
  reference, the one routing reads) by a reference to a record outside itself, the
  request now also carries that payer Organization as a `referenced` parameter, each
  record your system supplied once, exactly as your system of record holds it.
  - It is the record your Coverage search included for a relative reference.
  - Otherwise, it is the record your system supplied for the same reference when the
    gateway read the payer of the Coverage it routes by.
  - A payer that resolves the Coverage's payor from the request itself can now do so.
    Before, the request carried the Coverage alone, and such a payer could refuse it.
  - A Coverage that contains its payer Organization, or names its payer by identifier
    alone, adds nothing.
  - So does a reference that no record your system supplied answers, or that several
    different Organizations answer. Leaving it out refuses nothing.
  - Once the request is built, such a reference is recorded at every conformance level
    as a `dtr.referenced` event: outcome `left-out`, reason `none-answers` or
    `several-answer`, and the reference's resource type only. Nothing is minted.

## A Coverage naming its payer by identifier alone is refused at the PAS build, not as an internal error (v1.3.0)

- **Changed (requester):** when your gateway builds a prior-authorization request
  from a Coverage that names its payer by identifier alone, with no reference to any
  Organization, it now answers `422 the member's Coverage names its
  payer by identifier alone, so your system of record supplies no payer Organization
  for a prior-authorization request to name`, recorded as `fidelity` at stage `build`.
  Before, it answered `500 build bundle failed`, recorded as the gateway's own failure.
  No `pas-claim` leg is sent, at every conformance level. A coverage check that ends
  the exchange is carried as before, so the refusal comes only when a
  prior-authorization request would be next, after any coverage and questionnaire legs.

## A route that amends its own claim refuses, before the claim, a reference the amendment certainly cannot carry (v1.3.0)

- **Changed (module 1, the UC-04 demo lane and the single-call UC-06 and UC-07):** from PAS 2.1 an amendment carries the submitted claim whole, so a reference your request states inside a claim fact must resolve in the amendment's bundle. On these routes, which amend in the same request, the gateway now checks just before the `pas-claim` leg, with shn-sdk v1.3.0's `CheckPASSubmittedFactsCarriedAtLine`, and refuses with no claim sent a reference to a resource type the amendment's bundle has no entry of, an absolute one under another base, and one that is not a type and an id: `422`, recorded as `fidelity` at stage `build`, the fidelity of the gateway's own build. Before, the claim went out first, the payer held a pend the amendment could not answer, and the request failed after it.
  - On these routes, a request stating such a reference whose payer answer did not pend answered `502 expected pended response`. It now answers `422`, recorded as `fidelity` at stage `build`, before that leg, as does any other failure the request would have met past that point.
  - A reference to a type the amendment's bundle does hold, or a local one, is still decided at the amendment's build, after the claim, as before.
  - A request that can end without a claim is carried as before: a CRD not-covered answer, UC-05's consented branch, which can end at `consentDenied`, and a two-phase `/start`, which ends at the pend. `/complete` amends a pend an earlier request made and is unchanged.
- **Changed:** the gateway pins shn-sdk v1.3.0.

## A patient a Coverage's party names is recorded as involved (v1.3.0)

- **Fix:** when the network lists `involved` in `hubAccepts`, a prior-authorization
  request's `involved` list now names a patient that only a Coverage's party names.
  The party is a `Patient` contained in the Coverage that its `subscriber` or
  `policyHolder` names, such as a dependent's parent. A patient the party links to, or
  names in any other element, is now listed.
  - The parent itself is still no member of the request, and the slot's reference to
    it still names no one.
  - Gateways v0.61.0 to v1.2.0 left such a patient out of the list, so the network had
    no record of the exchange under them and made no authorization decision about
    their involvement. Upgrading records them.
  - This holds wherever the request is carried, at `none`, `observe` and
    `structural`. At `strict` the PAS legs refuse a request that names another patient
    under `patient.mixed` before it is sent, as before.
  - As for any involved patient, a refused token leaves the patient out, with the
    refusal counted (`involvedOmitted`), and the leg is sent as usual. The list never
    refuses a request.

## Requests the gateway originates: what an active order means at CRD 2.0, and what your conformance check covers (v1.3.0)

- **Documentation:** "Requests the gateway originates" in `docs/CONFIGURATION.md` now
  states what each CRD line's order profile says about an active order, and what your
  gateway's conformance check of an order it originates covers.
  - CRD 2.0.1's `profile-devicerequest` and `profile-servicerequest` fix `status` to
    `draft`. The `order-sign` and `order-dispatch` requests carry your active order as
    your system holds it, so at CRD 2.0 the order does not conform to those profiles.
    CRD 2.1.0 and 2.2.1 fix no status.
  - The network carries the order as sent and never changes its status.
  - At `observe`, `structural` and `strict`, your gateway checks the order it
    originates for `order-sign` and `order-select` against base FHIR R4 and the
    profiles the order declares in its own `meta.profile`, not against the routed
    line's CRD order profile. An order that declares US Core, or no profile, draws no
    finding for its status at `observe`. The `order-dispatch` order is not checked
    before the `order-dispatch` request is sent.

  No behaviour changes.

## The validator check pulls its curl image by digest (v1.3.0)

`deploy/validator/verify.sh` now names the curl image it probes each validator lane with by digest, `curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69`, so every run uses the same image, whatever a registry's tag serves that day. The validator images, and what the check proves, do not change.

## The 2.2 validator loads Clinical Practice Guidelines, so a DTR package's Library profiles are checked (v1.3.0)

- **Fixed (validator image, 2.2 line):** the image loads `hl7.fhir.uv.cpg` 1.0.0, the version `fhir.cqf.common` 4.0.1 depends on. A DTR package that carries the published FHIRHelpers or FHIR-ModelInfo Library declares CPG profiles. Without the package the validator answered an error for each, "has not been checked because it could not be found", having checked nothing, and your gateway recorded it at `observe` and refused it at `structural` and `strict`, unless you tolerate that payer. It now checks those profiles.
  - A resource that uses nothing from CPG reads as before.
  - The 2.0 and 2.1 lines do not change.
  - The 2.2 lane indexes 1,170 StructureDefinitions at start, up from 1,030, and `verify.sh` expects that number.

## An amendment links its attached answer at PAS 2.2 on the report type the request states (v1.2.0)

- **Additive (provider, amending routes):** a request may state `reportType`, one X12 755
  Coding: the X12 PWK01 report type of the answer the amendment attaches, the requester's
  own statement. At PAS 2.2 the amendment's `supportingInfo` entry then carries it as
  `extension-documentInformation`'s `reportTypeCode`, and the item the payer's pend ties the
  answer to gets its `informationSequence` and an `infoChanged` marker: `added`, or `changed`
  where the amendment changes that item otherwise. It is never read from the pend and never
  defaulted. With no statement, nothing links at PAS 2.2, as before.
- **Changed (provider, amendments, with `shn-sdk` v1.2.0):** a payer's pended
  `CommunicationRequest` now ties an attached answer to the item line it names, as a pended
  `Task` does: by a LOINC payload code the answer's `code` carries, or by the questionnaire
  its `extension-requestedQuestionnaire` names. So an amendment below PAS 2.2 answering such
  a pend now links with no statement, and that item carries the same `infoChanged` marker. A
  payer may then re-evaluate the item instead of answering with the decision it holds.
- **Changed:** a statement whose `system` is not X12 755, or that has no `code`, is refused
  `422` before any leg, at every level, as the amendment's facts are. The code itself is not
  checked: the list is licensed.
- **Changed (Go API):** `scenariodriver.ScenarioRouteInput` sends the `reportType` an amending
  persona states, and returns an error for a caller's `extra` naming `reportType`.
- **Additive (record, access line):** `engine.EditNotMadeReportTypeNotCarried`, in
  `engine.EditNotMadeValues`. A stated report type the amendment does not carry goes on
  without it and `editsNotMade` names `amendment:report-type-not-carried`: its line has no
  place for one, its build is bridged to another line, or the pend ties the answer to no
  item. A bridged amendment links nothing at PAS 2.2, as before.

## A check the validator could not run is reported as that, everywhere it is read (v1.2.0)

- **Fixed:** the validator's `REGEX_MATCH_TIMED_OUT`, for a regex a profile sets,
  is read as a check that could not run, by its message id, as
  `TYPE_SPECIFIC_CHECKS_DT_PRIMITIVE_REGEX_EXCEPTION` is. Before, it read as a
  structural defect.
- **Changed (provider, `TRANSLATE_IG_LINES`):** a check that could not run at a
  questionnaire package answer's own line, or a lane for that line still
  qualifying, no longer reads as "not valid at that line". The answer is carried
  as sent and the record names `E-08:check-could-not-run` (`editsNotMade` on the
  access line), at every level, even if another line of its name family
  validates: it is never renamed on a guess. Before, it could be renamed on one
  attempt and carried unrenamed on the next, with nothing recorded. A line the
  gateway has no lane for is unchanged: it is passed over. At `none`, the same
  record is made while the lane for your own line is qualifying: the renamed
  answer could not be checked, so it is carried as sent.
  - At `structural` and `strict` the gateway's own check of the answer runs
    first and can refuse it, as before, and then `editsNotMade` stays empty.
    When the check could not run at the routed line, or at every line it judges
    the answer at, that is `500` at `strict`, and at `structural` the answer is
    carried and recorded. Otherwise, when no line is valid, a defect that level
    refuses is `502`.
- **Fixed:** the `crd.embedded.validated` observation's outcome and the
  `validate.result` diagnostic event report a check that could not run as
  `unavailable` and `validator unavailable`, not `invalid`. The event has no line
  to read a PAS request Bundle's follow-on errors at, so on that Bundle it can
  still say `invalid` where the finding says `unavailable`.
- **Fixed:** `fhirseed.PutGlobalArtifact` says the check could not run, not that
  the artifact failed validation. It still does not write it.
- **Additive (Go API):** the `validatorissue` package (`CouldNotRun`,
  `OnlyCouldNotRun`, `RegexException`, `RegexTimedOut`), and
  `engine.EditNotMadeDTRCheckCouldNotRun` in `engine.EditNotMadeValues`.

## The gateway image can build from a local module directory (v1.2.0)

- **Additive:** `Dockerfile` reads Go modules from an optional `gomodules` build
  context (`--build-context gomodules=<dir>`, or compose `additional_contexts`),
  a directory laid out as a module proxy, when the build argument
  `GO_MODULES_PROXY` points at it (`file:///gomodules,off`). `go.sum` checks what
  it reads, as it checks a download, and a module the directory does not hold
  fails the build. A build that passes neither reads Go's default proxy as before.

## The image's runtime base is pinned by digest in the Dockerfile (v1.2.0)

`Dockerfile` and `Dockerfile.evalseed` now name each base image by digest, so a build from source uses the base a release used, whatever a registry's tag serves that day. The image's contents do not change: the published image was already built on this digest. Each release's notes name the runtime base digest.

## The order-dispatch refusal names the device history's statuses (v1.2.0)

- **Changed (requester):** the `422 build order-dispatch request failed` refusal
  for an order the device history does not hold now names the statuses that
  history selects: `… does not hold the dispatched order DeviceRequest/<id> at a
  status the device history selects (active, on-hold, completed)`. The history is
  searched at the published Da Vinci prefetch template's statuses, so "does not
  hold" alone read as if the order did not exist. The words are the same for an
  absent order and for one at another status. With the FHIR connector the open
  order is read at `active`, so an order at another status is answered
  `502 no open order for member in SoR` first: it reaches this refusal from a
  connector of your own, or if its status changes between the two reads. The
  status, the rule and the stage are unchanged: `fidelity` at stage `build`.

## The validator's primitive regex checks hold their verdict under load (v1.2.0)

- **Fixed (validator image):** `deploy/validator/backport` replaces the
  validator's regex guard with `org.hl7.fhir.core` 7.0.3's, verbatim, in every
  image and lane built from it. The guard evaluates on the calling thread, where
  it ran each regex in a new thread against a wall clock. It answers most of
  the regexes the FHIR specification puts on primitives with hand matchers,
  which use no regex engine and read no clock, and it reads the clock for any
  other regex only once per 1,024 characters that regex reads. So a CPU-starved
  validator gives a valid primitive value under about 1,000 characters the
  verdict it gives with CPU to spare, and a very long valid `code` or `oid`
  value, which overflowed the regex engine, validates clean.
  - The limit stays 500 ms, and a runaway regex, which reads far more characters
    than its value holds, still stops at it.
  - Short of CPU, a longer value can still reach the limit where a regex stays
    in the engine, and a regex that reads a value more than once can reach it on
    a shorter one. A `decimal`, `dateTime` or `instant` that does reads as a
    check that could not run, as before, and so does a regex a profile sets. A
    regex in a FHIRPath constraint reports no such id, and reads as that
    constraint failing.
  - `/app/backport-provenance.json` is now format `validator-backport-v2`, and
    records the utilities JAR and the regex classes beside the explicit-profile
    class. The images also carry the label
    `org.shn.validator.backport.regex-classes`.

## golang.org/x/net v0.62.0 (v1.2.0)

- **Changed:** `golang.org/x/net` moves to v0.62.0, the fix for GO-2026-6860 and
  GO-2026-6866 in its `html` package. This gateway does not import that package,
  so neither reached it, but a binary scan reads the module's version. Its
  requirements move `golang.org/x/crypto` to v0.58.0, `x/sync` to v0.24.0,
  `x/sys` to v0.49.0 and `x/text` to v0.43.0.

## A responder verifies its answer token, and attests its system answered when it cannot send the answer (v1.1.0)

- **Changed (responder):** the gateway verifies the token `POST /authorize`
  mints for each answer before sealing it in, as it does for a request leg:
  signed by the Authorization Framework's key and bound to exactly that answer
  (its frame, operation, correlation, this gateway as holder, the leg's
  patient and the answer's payload hash). A token that does not verify is a
  refusal on per-leg authority, a network rule, at every conformance level.
- **Changed (responder):** once the participant's system has answered (it may
  have acted), an answer the gateway cannot authorize is never sent, and the
  gateway says its system answered. It answers the Hub's forward with a raw
  `502 {"error": <reason>, "rule": <rule>}` marked `X-SHN-Delivered: yes`:
  - rule `authority`, refused by the gateway: the token does not verify
    (`answer token does not verify`), the Framework answered with no usable
    token (`answer authorization carries no token`), or the Framework could not
    be reached, timed out or failed (`answer authorization unavailable`; the
    record's answer error class says which, so it is not read as a denial);
  - rule `authority`, refused by the Authorization Framework: it denied the
    answer (`answer not authorized`);
  - rule `audit`, refused by the Authorization Framework: it could not record
    its decision (`answer authorization not recorded`).

  The Hub relays the rule to the requester's gateway, marked
  `X-SHN-Delivered: yes`: `502 forward to recipient failed: the recipient's
  gateway refused its system's answer`. The requester's gateway records the
  payer gateway's refusal on that rule, delivered yes, and answers its caller
  `502 the recipient received this request and answered, but its gateway
  refused to send the answer; it may have acted on the request: check its
  outcome before resending`. The token outcome says what happened (`failed`,
  `denied` or `unavailable`).
  - Before, an answer token that did not verify was sent, and the Hub refused
    it (`response leg authorization failed`, delivered yes); the requester
    recorded the Hub's refusal on `authority`. An answer the Framework
    answered with no token, or could not authorize, was answered with the
    payer gateway's plain `502 authorization failed`, which the Hub reported
    as delivery unknown and the requester recorded as `upstream-error`.
  - Only the system answering the forwarded operation counts, whatever its
    status. The gateway's own refusal, made before its system was asked to act
    (a read of its CDS service listing is not asking), an answer built from
    its records and a forward that got no answer still answer the plain
    `502 authorization failed`, which leaves delivery unknown, when they
    cannot be authorized.
  - A requester that left while the answer was being authorized is no refusal
    of the answer: the plain `502`, recorded `other`, on no rule. A refusal the
    gateway had already made keeps its rule, and a failure of its system stays
    an upstream error.
  - An answer the Framework authorized is sent as before.

## A regex check the validator could not run is not a payload verdict (v1.1.0)

- **Fixed:** an error the validator reports because it could not run a
  primitive's regex check (`TYPE_SPECIFIC_CHECKS_DT_PRIMITIVE_REGEX_EXCEPTION`)
  is read as a check that could not run, by its message id. The validator
  reports it on values that match: when it is short of CPU it gives up on a
  primitive's type regex after a fixed wall-clock limit, so the same message
  could be refused on one attempt and pass on the next; and a very long valid
  `code` or `oid` value can overflow its regex engine. A malformed `code`, `oid`
  or base64 value is refused by the validator's own checks before its regex
  runs, and stays refused.
  - Alone, or beside deeper rules' errors only, the check is `unavailable`:
    refused with `500` at `strict`, recorded as an `unavailable` finding and
    relayed at `observe` and `structural`, not run at `none`. Before, it read
    as a structural defect: refused `422` at `structural` and `strict`, and
    recorded and carried at both in an answer from a payer it tolerates.
  - Beside a structural error the message is invalid for that error, at every
    level that refuses one, as before.
  - A certification lane whose only errors are such checks (with any unchecked
    terminology and the no-match summaries on those entries) records
    `unavailable` with `error` `a primitive regex check could not run` (after
    the terminology reason when terminology was unchecked too), never `invalid`
    and never certified.
  - So `structural` relays a message whose primitive check could not run,
    recorded `unavailable`, as it relays any check that could not run: under
    CPU starvation, and for a very long valid `code` or `oid` value, which it
    refused before. A gateway at `strict` refuses either with `500`, including
    an answer from a payer it tolerates, which it carried before: a check that
    could not run is never tolerated.

## The IG-loaded images can build from a local package directory (v1.1.0)

- **Additive:** `deploy/validator/Dockerfile` and `deploy/eval/hapi/Dockerfile`
  read each IG package from an optional `igpackages` build context
  (`--build-context igpackages=<dir>`, or compose `additional_contexts`), a
  directory of `<name>-<version>.tgz` archives, before downloading it from the
  package registry. With the build argument `IG_PACKAGES_OFFLINE=1` the build
  refuses the registry, so a package the directory does not hold fails it.
  Every package, from either source, is checked against the sha256 the
  Dockerfile pins, as before. A build that passes neither downloads and checks
  every package exactly as before. `deploy/validator/verify.sh` passes
  `IG_PACKAGES_DIR`, when set, as that context.

## A requester records the Hub's large envelope refusal as routing (v1.1.0)

- **Changed (requester):** the Hub's `503 large envelope budget exhausted`, with
  `Retry-After`, is recorded as the Hub's `routing` refusal: transient, the
  network cannot carry this body right now. The caller's answer is that `503`
  with the Hub's `Retry-After`, a whole number of seconds from 1 to 60, or 5
  when the Hub's cannot be read.

## Module 1's coverage and unstated-claim-fact refusals record their rule (v1.1.0)

- **Changed (monitoring):** three refusals on module 1's origination routes
  now record their rule instead of `other`. Their answers are unchanged.
  - `422 no coverage on file for member` (the participant's own system holds
    no Coverage for the member, so no payer can be addressed) is `routing`, as
    a coverage naming no payer the network registers already was.
  - `422 ambiguous coverage for routing: the member's Coverage records do not
    name one payer` (Coverages naming different payers, or one whose payer
    cannot be read) is `routing` on the module 1 routes it ends (those that
    originate a submission, and `/scenario/pa/inquire`): no one recipient can
    be addressed. A waiting call's later inquiry that meets it keeps the pend,
    as before, and records no refusal.
  - A prior-authorization request that does not state a claim fact the PAS
    line requires, or states one its element cannot carry (`422` naming the
    element), is `fidelity` at stage `build`, as claim facts stated for an
    order the member does not hold already were.
- **Changed (monitoring):** a PAS build that fails for any other reason is
  still the gateway's own `500`, now recorded `other` in every case: an
  internal error, never a refusal. Before, when a leg of the same call had
  already been answered, it was recorded as this gateway's refusal on rule
  `other`.

## A Hub transport key that is not an Ed25519 public key refuses to start (v1.1.0)

- **Changed (Go API):** `engine.New` for a role that mounts the Hub-facing
  inbound (payer, facility, PHG) refuses to start when `HubTransportPub` is
  not an Ed25519 public key (32 bytes), where it refused only an empty one. A
  key of another length is a configuration error said at boot. Before, a
  request carrying a Hub assertion made the verifier panic, and the request
  was dropped. A gateway that fetches the key from the network's discovery
  is unaffected: that fetch already checks its length.

## Module 1's build refusals record what failed (v1.1.0)

- **Changed (monitoring):** the `422 build ... request failed` refusals on
  module 1's origination routes (the order-select, order-sign and
  order-dispatch requests, and the questionnaire-package request) now record
  what failed, instead of `other`. Their answers are unchanged.
  - The participant's own records cannot be built from faithfully: an order
    record the gateway cannot read, or a device history that does not hold
    the order being dispatched. That is `fidelity` at stage `build`, as an
    ordering clinician named nowhere already was.
  - The gateway's own assembly failing (reading the Coverage searchset it
    assembled from the participant's records, or sealing the
    questionnaire-package request) is `fidelity` at stage `verify`.
  - Each of these also reports a `leg.failed` observer event naming the leg,
    as an ordering clinician named nowhere already did.
  - A refusal by the shn-sdk request builder itself is still recorded on rule
    `other`: the SDK does not yet say whether the inputs or the call were at
    fault.

## An involved patient's token is verified, and a patient left out is on the record (v1.1.0)

- **Changed (requester and responder):** this gateway now verifies each involved
  patient's token before putting it in the leg's involved list, with shn-sdk's
  `VerifyInvolved`: signature, expiry, leg, operation, frame, holder, patient and
  payload hash. One that does not verify leaves its patient out, counted as
  `authorization-failed`, as a Framework failure does, and **the leg is sent,
  with no record of the exchange under that patient.** A token without the
  involvement asked for stays `involvement-not-carried`. Before, the
  gateway listed the token. On a request the Hub then refused the whole leg, and
  on an answer it dropped the entry. A token for another patient passed the Hub,
  which binds no subject, and recorded the exchange under that patient; it no
  longer reaches the list.
- **Additive:** a patient left out is now on the exchange record for every omit
  reason, on any gateway, with or without metrics: the record's
  `InvolvedOmitted`, and the access line's `involvedOmitted`, count them by
  reason, and are absent when no one was. Before, only the observer event and
  the metric said so.

## An amendment carries the submitted Claim as sent, and one that cannot is refused (v1.1.0)

- **Changed (requester):** from PAS 2.1, the amendment this gateway builds on
  module 1's routes carries the submitted Claim as sent as its prior-Claim entry,
  with its items, from shn-sdk v1.1.0. Before, that entry restated the Claim: its
  own id, status, use and references, and no items. A PAS 2.0 amendment still
  carries no prior-Claim entry.
- **Changed (requester):** an amendment whose recorded submission cannot be
  carried as the requester submitted it is refused `422`, in shn-sdk's own words,
  as `fidelity` at stage `build`, at every conformance level. From PAS 2.1 that is
  a claim fact stating a reference the amendment cannot carry as sent. No
  amendment is sent, and this gateway never rebuilds the Claim. Before, it answered
  `500 build update bundle failed`, recorded `other`. The submission itself was
  already sent, so the payer's pended authorization stands.

## A request leg with no authority is recorded as refused on authority (v1.1.0)

- **Changed (requester):** when no authority could be obtained for a leg this
  gateway was sending, the exchange record, the access line and the refusal
  metrics now say so: refused by this gateway on rule `authority`. That covers
  an authorize answer with no usable token, a token that does not verify for
  the leg, and the Authorization Framework failing or unreachable. A denial,
  and the Framework's audit refusal, are recorded as before. A request that
  ended first is no refusal. The answers are unchanged: `503` when the
  Framework could not be reached, else `502 authorization failed`, and the leg
  is not sent. A leg's `LegOutcome`, and so `LegError`, is unchanged: it is the
  signal an outage of the Framework alarms on.
- **Additive:** the record's `AuthorizeError`, and the access line's
  `authorizeError`, say why, with the classes `answerError` uses.

## A gateway bounds the large bodies it holds at once (v1.1.0)

- **Added (requester and recipient):** a gateway holds a bounded number of
  bytes of documentation attachments and large envelopes at once. A submission
  that does not fit is refused at the provider's gateway `503`
  `attachment budget exhausted` with `Retry-After`, recorded as its `routing`
  refusal, at every level, before any of it is read. One it holds must arrive
  within 30 seconds, or it is refused `400`, so a sender that stalls cannot keep
  the budget. An envelope over 8 MiB that
  does not fit is refused at the recipient's gateway `503` with `Retry-After`
  and `X-SHN-Not-Acted: at-capacity`, recorded as its `routing` refusal, before
  its system is called. An envelope at or under 8 MiB is never counted.
- **Added (requester):** the caller of a leg the recipient's gateway refused
  that way gets `503` with the wait: the Hub relays the attestation, and nothing
  was done, so the leg can be sent again.
- **Added (configuration):** `ATTACHMENT_INFLIGHT_BYTES` sets the budget. Unset,
  it is a tenth of the task's memory, read from the container's cgroup or, on
  Amazon ECS, from the task metadata endpoint, or 64 MiB when neither states it,
  and never below the attachment ceiling. The boot log states the value and
  where it came from.

## A documentation attachment has its own size bounds, and a payer may declare the largest file it accepts (v1.1.0)

- **Changed (requester and recipient):** a CDex documentation attachment
  submission (`cdex-attachment-submit`) is no longer held to the 8 MiB the
  network carries on every other leg (just under 6 MiB of body). Its body may
  be up to the network's attachment ceiling, 16 MiB, and its sealed envelope
  up to 22 MiB, the SDK's per-leg caps (`MaxAttachmentBytes`,
  `MaxRequestBytesFor`). The provider's gateway reads a submission up to its
  ceiling and refuses one over it `413` `request body too large` (`routing`)
  before reading anything in it; the payer's gateway refuses one over its own
  ceiling the same way before its system is called. An envelope over its
  leg's limit is refused `413` naming that limit. A gateway reads an envelope
  up to the largest limit of any leg, then holds it to its own leg's. A
  gzip-coded submission or envelope has the same bounds as an uncoded one.
- **Added (configuration):** `ATTACHMENT_CEILING_BYTES` sets a lower ceiling
  than the network's 16 MiB; a higher one refuses to boot.
  `PAYER_ATTACHMENT_MAX_BYTES` is a payer's optional declaration of the
  largest single inline attachment (file) its system accepts, in bytes after
  base64 decoding, the value its capability declaration states as
  `attachmentMaxBytes`; a provider
  gateway refuses to boot with it.
- **Added (requester):** a provider's gateway reads the payer's declared
  `attachmentMaxBytes` from the registry feed and refuses a submission
  carrying an inline file over the smaller of it and its own ceiling `413`
  `attachment too large: an inline attachment is N bytes, over the M the
  payer's system accepts` (`routing`), before anything is sent, at every
  level. Only inline files are measured, each on its own and from its base64
  length, never decoded and never from `Attachment.size`: a file by `url`,
  content with no inline file and `data` that is not well-formed base64 are
  carried.
  A payer that declares no limit has only the ceiling applied.

## An answer over the network's limit is refused, never relayed cut short (v1.1.0)

- **Fixed (responder):** an answer from the payer's system over what the
  network carries (the sealed answer is bounded by 8 MiB, so just under 6 MiB
  of answer fits) is refused for size and never relayed cut short. The
  payer's gateway answers the Hub `502 {"error":"response body too large",
  "rule":"routing"}` with `X-SHN-Delivered: yes`, its statement that its
  system answered and which refusal it made, and records its own `routing`
  refusal. That holds for a `2xx` answer and for an
  application error alike.
  - Before, a `2xx` answer was read up to 8 MiB and sealed as read, so an
    answer over it was relayed cut short (a DTR questionnaire package
    included), and one between about 6 and 8 MiB sealed over the Hub's limit
    and failed there. A non-`2xx` answer over 6 MiB was a `502` saying the
    payer's system gave no answer the gateway could carry.
- **Changed (Hub):** the Hub relays that statement as `502 forward to
  recipient failed: the recipient's gateway refused its system's answer`,
  delivered `yes`, with the statement's `rule` when it is one of the closed
  refusal rules (never the recipient's own reason), and refuses a `2xx`
  answer over the limit it reads itself as `502 response body too large`, delivered
  `yes`. It honours `X-SHN-Delivered: yes` from a recipient's gateway only on
  a `502`, which a gateway sets only when it refuses to send its system's
  answer.
- **Fixed (requester):** the requester's gateway answers its caller `502
  {"error":"response body too large"}` with `X-SHN-Delivered: yes`, and records
  a `routing` refusal by the payer's gateway, the Hub or itself, whichever
  refused the answer, with the Hub's delivery `yes` when the Hub refused it
  (none when this gateway refused the Hub's answer). A payer gateway's
  refusal on another rule is answered `502` saying the recipient answered and
  its gateway refused to send the answer, delivered `yes`, and recorded on
  the rule the Hub relayed (`other` when it relayed none). A missing
  `X-SHN-Delivered` on a gateway's own answer means delivery is not stated.

## Request bodies may be gzip-coded, and answers are gzip-coded on request (v1.1.0)

- **Added:** the provider's ingress (its PA routes, coverage eligibility, the
  CDex `$submit-attachment` routes at the plain endpoint and at each payer's
  return path, and `GET /cdex/payers`), the token endpoint, the route the
  network delivers a leg to, and the operator's `/scenario` routes read a
  request body sent with `Content-Encoding: gzip` decoded. What the
  gateway reads, carries, verifies, hashes and records is the decoded body,
  so a gzip-coded exchange is the exchange it would be uncoded; a `/scenario`
  caller's assertion is verified over the decoded body. The network's limit
  applies to the decoded body: one that decodes past it is refused `413
  request body too large`, as an uncoded one is, and is never inflated past
  the limit. The coded body is read no further than the most any gzip of a
  body at the limit takes: a coded body that decodes to little or nothing
  for longer is refused `413` the same way.
- **Changed (network rule, every level):** a request body in any other
  content coding, or in a stack of codings (`gzip, gzip` included), is refused
  `415 unsupported content coding` with `Accept-Encoding: gzip`, the coding
  the gateway decodes, and recorded as this gateway's refusal on message
  integrity. The Hub refuses one the same way, and a requester's gateway
  records the Hub's `415` as the Hub's refusal on message integrity. The body
  is not read to guess what it is. The coding is refused where the body is
  read, so a caller is refused first on any authentication that does not
  need the body: the ingress's bearer, the network's delivery assertion, the
  Hub's holder assertion, a `/scenario` caller with no assertion. Where the
  caller's credential is verified over the body (the token endpoint's form, a
  stated `/scenario` assertion), the coding is refused first. A FHIR operation
  route answers it as an `OperationOutcome`, as it answers each of its
  refusals; the token endpoint answers it as an OAuth error, `invalid_request`.
  - Before, the coding was not looked at: a body labelled with a coding but
    sent uncoded was read as sent, and a gzip-coded body was refused as
    whatever its coded bytes failed to parse as (`400`). A body that is not a
    whole gzip stream is still `400 read body failed`.
- **Added (answers):** on the provider's ingress, the CDex routes and
  `GET /cdex/payers` included, and the route the network delivers a leg to,
  an answer of at least 1 KiB is gzip-coded when the
  caller sends `Accept-Encoding: gzip`, and every answer states `Vary:
  Accept-Encoding`. An answer with no body, an event stream, or one already
  coded is sent as written. The token endpoint's answer, which carries a
  bearer, and the operator's `/scenario` answers, which carry a scenario's
  resume token, are never compressed and have no `Vary` header: a compressed
  answer's length can reveal a secret repeated across answers. The tokens an
  envelope carries are each bound to one leg and refused if replayed, so
  envelope answers are compressed.

## A gateway declares what it takes, and a leg its recipient does not take is a 422 (v1.1.0)

- **Added (declaration):** at each start a gateway declares to the registrar the
  legs it compiles for its role (`PUT /register/{id}/capabilities`). A gateway that
  answers none declares an empty list. It sends the declaration again, backing off
  to every 10 minutes, until the registrar has it. A `409` is logged once and not
  resent: the registrar holds a declaration issued no earlier, or the holder's list
  was set when the network was founded and no declaration applies to it. A `404` or
  `405`, as a registrar older than the declaration answers, is logged once after
  four tries and then tried hourly in silence, so a registrar updated later has the
  declaration with no restart. A failure is logged and never stops the gateway.
  `GET /health` gains the check `capability-declaration`, with a registrar, and with
  status always `ok`: `lastSuccess` when it was stored, `lastError` when it was not,
  and neither before the first answer.
- **Added (registry):** the registry a gateway converges from the feed carries each
  holder's `legs` and `attachmentMaxBytes`.
- **Changed (caller):** the network now refuses, before forwarding, a leg its
  recipient does not declare it takes: the Hub answers `422` with
  `X-SHN-Delivered: no`. A provider's gateway answers its caller `422` with the Hub's
  reason as it stands, where such an answer would be `502 hub refused the exchange: …`.
- **Changed (exchange record):** that refusal is the Hub's, on `routing`; before,
  such a reason read `other`. Both changes read the status and the header, never the
  reason.

## A pended PAS answer's payer-url is re-keyed to the network (v1.1.0)

- **Changed (provider, default):** a provider's gateway relaying a payer's
  answer to a PAS submit, update or inquiry to its own system re-keys the
  `payer-url` of each Task in it (registered edit `E-10`,
  `pas-pend-return-rekey`) to its documentation return path for the payer the
  exchange was routed to, `cdexroute.ReturnBase(PROVIDER_DAVINCI_INGRESS_BASE_URL,
  <payer participant id>)`, so documentation the provider sends there reaches
  that payer through the network. Only a `payer-url` input written as a
  `valueUrl` is re-keyed; nothing else in the answer changes, and the payer's
  answer crosses the network as its system sent it. The re-key is made only for
  a payer whose registry entry lists `cdex-attachment-submit`: the Hub refuses that
  leg to a payer that does not take it, so a payer on an earlier release keeps its
  own address, carried as sent.
  - Before, the answer was relayed exactly, and the `payer-url` named the
    payer's own address, off the network.
  - A gateway with no return path to write refuses such an answer `502` as a
    fidelity refusal of its own edit, stage `build`. A released gateway does
    not reach it: see the boot change below.
- **Changed (boot; can stop an upgraded gateway from starting):** with
  `PROVIDER_DAVINCI_INGRESS` on, the gateway refuses to start unless
  `PROVIDER_DAVINCI_INGRESS_BASE_URL` is an absolute `http` or `https` URL
  with a host, no `?` or `#` and no user information, so every return path can
  be built under it. These shapes now refuse to boot, where before they started:
  - a base with no host (`http://`, `https://`);
  - a base with a query, an empty one included (`https://gw.example/fhir?tenant=1`,
    `https://gw.example/fhir?`);
  - a base with a fragment, an empty one included (`https://gw.example/fhir#x`,
    `https://gw.example/fhir#`);
  - a base with user information (`https://user:pass@gw.example`), which every
    re-keyed `payer-url` would carry.

  A missing base and one that is not `http` or `https` refused to boot before
  and still do. A gateway with the provider ingress off is unaffected: it
  starts with any of these values set, as before. `engine.New` applies the same
  rule to `Config.IngressBaseURL` when the ingress is on, and returns an error
  for those shapes, where before it checked only that the base was set.
- **Changed (signed content):** the re-key is made even where an in-payload
  signature covers the `payer-url`; that signature then no longer verifies in
  the provider's copy. It is the only edit the gateway makes to signed content:
  every other edit is still refused `422 signed content cannot be edited`.
  This is a recorded exception to [`STABILITY.md`](STABILITY.md)'s terms for
  the full PAS pass-through, scoped to this edit (STABILITY.md, "One recorded
  exception, v1.1.0").
- **Additive (wire):** the answer the gateway writes to its own system names
  each disclosed edit in `X-SHN-Edited`, one value per edit: the edit's name,
  then `; signed="<signatures>"` when a signature covered what it changed,
  each signature named once by its carrier
  (`pas-pend-return-rekey; signed="Bundle.signature"`).
- **Additive (record, access line):** `engine.ExchangeRecord.EditsNotMade`,
  and the access line's `editsNotMade` (`diagnostics.AccessLine.EditsNotMade`), name a default
  edit the gateway could not make, and why (`engine.EditNotMadeValues`):
  `E-10:no-structural-payer-url` for a `payer-url` that is not a string
  `valueUrl`, carried as sent, `E-10:unreadable-answer` for an answer that
  cannot be read as JSON, and `E-10:payer-does-not-take` for a payer whose list
  lacks the leg, that declares no list, or that the gateway cannot read.
  `ExchangeRecord.AnswerEdits` names `E-10` when the
  re-key was made.
- **Additive (Go API):** `relay.EditPASPendReturnRekey` (`E-10`) and
  `relay.Payload.SignedEdited`; `engine.EditedHeader`,
  `engine.ExchangeRecord.EditsNotMade`, `engine.EditNotMadeNoStructuralPayerURL`,
  `engine.EditNotMadeUnreadableAnswer`, `engine.EditNotMadePayerDoesNotTake`
  and `engine.EditNotMadeValues`. The
  ownership rules of the `pas-claim`, `pas-claim-update` and
  `pas-claim-inquire` answers to the requester's system permit `edited` with
  `E-10`.

## A provider's system sends CDex documentation to a payer through the network (v1.1.0)

- **Added (requester, evolving):** the CDex 2.1.0 `$submit-attachment`, a new
  exchange leg, `cdex-attachment-submit` (staged `pa.cdex-attachment-submit`).
  A provider gateway with its Da Vinci ingress takes it at
  `POST /$submit-attachment`, routed by the submission's `PayerId` to the
  registered payer it names, and at each registered payer's return path,
  `POST /cdex/payer/{payer}` and `POST /cdex/payer/{payer}/$submit-attachment`,
  routed to that payer. `GET /cdex/payers` lists the payers that take the leg with
  their return paths. The submission and the payer's answer are carried as sent.
  Beside the size bounds, the content coding rule and the Hub's `422` for a payer
  that does not take the leg, the gateway refuses it with no registered payer to route to (422, `routing`: at the
  plain endpoint, no `PayerId`, two that differ, or one naming no registered
  payer; at a return path, a path naming no registered payer), when it names
  no one patient to bind (400, `authority`: a body that is not a
  `Parameters`, no readable `MemberId`, or two that differ), when it
  repeats a member name (400, `integrity`) or above the attachment ceiling (413),
  and a payer answer that repeats a member name is refused 502 (`integrity`).
  A `PayerId` that names another payer than the return path's, or one naming
  no registered payer, is carried and noted (`cdex.payer-id-differs`). The leg
  rides no declared contract line.
- **Added (recipient, evolving):** a payer gateway carries the submission to
  the payer's own `$submit-attachment`, under the CDex base the payer
  publishes in its `.well-known/davinci-configuration`
  (`davinci_cdex_attachsubmit_endpoint`), when same-origin with
  `PAYER_DAVINCI_BASE_URL`, else under `PAYER_DAVINCI_BASE_URL`. It binds the
  member from `MemberId` as on the other legs, and relays the payer's answer
  exactly, its media type included.
- **Additive:** a provider gateway's `.well-known/davinci-configuration` names
  its ingress base as `davinci_cdex_attachsubmit_endpoint#2.1`; the exchange
  kind `cdex-attachment-submit`, the exchange routes `cdex-submit` and
  `cdex-return-submit` and the observer tag `cdex-ingress` are added. The
  exchange metrics' series bound rises with the new kind (`MONITORING.md`).
- **Additive (Go API):** package `pa/cdexroute`
  (`github.com/SmartHealthNetwork/shn-gateway/pa/cdexroute`) builds a return
  path's address (`cdexroute.ReturnBase(ingressBase, payer)`, at
  `cdexroute.Prefix` plus the payer's path-escaped participant id), from the
  ingress base and the payer's participant id alone; and
  `scenariodriver.Driver.SubmitAttachment` (Evolving) posts a submission
  through the ingress.

## A request body over the network's limit is refused 413 (v1.1.0)

- **Fixed:** a request body over the network's limit (8 MiB, as the
  participant protocol states) is refused `413` with the published reason,
  `request body too large`, and recorded as a `routing` refusal: no route on
  the network carries it. That covers every provider ingress route (CRD, DTR,
  PAS, PAS inquire, eligibility, the federated query), the module 1 route
  `POST /scenario/pa/inquire`, the route the network delivers a leg to on the
  recipient's gateway, and the Hub's `/route`. A FHIR
  operation route answers it as an `OperationOutcome` with the reason as its
  diagnostics, as it answers each of its refusals; CDS Hooks, the delivery
  route and the Hub answer `{"error":"request body too large"}`.
  - Before, the body was cut at the limit and then refused as whatever the
    cut-off bytes failed to parse as: `400 parse … failed` at the ingress,
    `400 decode envelope failed` at the Hub, and `400 decode envelope failed`
    recorded as `integrity` at the delivery route.
- **Fixed (requester):** a request whose sealed envelope would be over the
  limit (the ciphertext is base64 in the envelope, so a body somewhat under 8
  MiB can seal over it) is refused `413 request body too large: sealed for the
  network it is over the 8 MiB an envelope may carry` before anything is
  sent, and before a token is minted unless only its token and involved list
  take its envelope over the limit, recorded as this gateway's `routing`
  refusal.
  The Hub's own `413` reaches the caller as a `413` and is recorded as the
  Hub's `routing` refusal.
  - Before, such a request was sent and cut at the Hub, and the caller got
    `502 hub refused the exchange: decode envelope failed`.

## The FHIR connector refuses an answer that repeats a member name (v1.1.0)

- **Changed (network rule, every level):** the bundled FHIR system-of-record
  connector read the participant's own FHIR server leniently: an answer that
  repeats a member name, exactly or under case folding, was read one way (the
  last of the repeated members) wherever no stricter reader looked at it
  afterwards, and acted on. FHIR JSON allows no such answer (property names
  SHALL be unique). Every read and search the connector makes now refuses it as
  the system's invalid response, wherever in the answer the repeat is, and the
  gateway records the refusal on message integrity, the call `malformed`, on a
  leg it answers. What each answer becomes; a repeat inside a resource that a
  stricter reader already refused keeps its refusal, except where noted:
  - **Federated query (a facility's gateway):** the member's Patient search
    repeating a member at the Bundle level was answered as if clean; it is now
    the patient-match `403`. The member's own Patient read repeating a member
    was refused `502 system of record returned another patient's resource`
    (the patient fence's strict reading); it is now `502 system of record
    returned an invalid response`. Records read from a server that cannot
    search, repeating a member at the Bundle level, were read one way; they are
    now refused `502`.
  - **Legs a payer's gateway answers (CRD order-select and order-dispatch, PAS
    claim and update, inquiry, the DTR questionnaire fetch):** the member's
    Patient search repeating a member at the Bundle level bound one reading
    and the leg was answered; it is now `502 system of record returned an
    invalid response`.
  - **Coverage eligibility (a payer's gateway):** the member's Patient, the
    Coverage and the insurer Organization repeating a member were read one
    way; each is now `502 system of record returned an invalid response`.
  - **Patient-authored DTR:** the member's Patient search repeating a member at
    the Bundle level was read one way; it is now the patient-match `403`.
  - **Requests a provider's participant sends (CRD, DTR and PAS ingress and
    origination):** the member's Patient, the payor Organization a request is
    routed by, the order, its supplier, the supplemental report, and the
    clinical facts a DTR fill or home-oxygen evidence reads were read one way
    where they repeated a member, and a filled value could come from either
    reading; each is now `502 system of record returned an invalid response`
    to the participant's own caller (a prefetch fill below `strict` carries the
    request unfilled, as for any read that fails).
  An answer with no repeated member is read exactly as before, including one
  the gateway's strict reader cannot finish (a string that is not well-formed
  UTF-8, or nesting past its bound): such an answer is walked as the
  connector's decoder reads it, and refused only for a repeat.
- **Changed (a bound on the repeat check, every level):** where the gateway's
  strict reader cannot finish a body, the check for a repeated member name
  walks it as encoding/json reads it. That walk now goes no deeper than 10,000
  levels of nested objects and arrays, encoding/json's own bound, so the
  memory the check takes is bounded on every body it reads,
  a peer's request included. A body nested deeper is read by no decoder, and
  the check stops where it goes deeper: a repeat before that point is still
  refused as a repeat; otherwise the path refuses the body as it refuses any
  body it cannot read. So on a request nested more than 10,000 levels deep
  whose only repeat is past that depth, or after it in the body, the answer's
  text is that path's unreadable-body refusal (for coverage eligibility, `400
  parse eligibility request failed: …`, not recorded as message integrity)
  where it was the repeated-member refusal; the status is unchanged.
- **Go API:** `engine.SoRRepeatedMember()` is the read error a connector
  returns for an answer that repeats a member name.

## A request leg's token is verified before the leg is sent (v1.1.0)

- **Changed (requester):** the gateway verifies the token `POST /authorize`
  mints for each request leg before sealing it in: signed by the
  Authorization Framework's key and bound to exactly that leg (its frame,
  operation, correlation, holder, patient and payload hash). A 2xx answer with
  no token, a `null` token or an unsigned one is an authorization failure too.
  Either way the leg is not sent, the caller gets `502 authorization failed`,
  the leg is recorded as this gateway's refusal on `authority`, and the token
  outcome is `failed`.
  - Before, such a leg was sent, and the Hub refused it: the caller got
    `502 hub refused the exchange: authz token verification failed`, and the
    leg was recorded as the Hub's refusal on `authority`.
- **Changed (responder):** an answer whose authorize answer carries no token,
  a `null` one or one with no signature is not sent. What the payer's gateway
  answers is in "A responder verifies its answer token, and attests its system
  answered when it cannot send the answer" above.
  - Before, the answer was sent with the zero token, and the Hub refused it
    (`response leg authorization failed`, delivered). The requester recorded
    that as the Hub's refusal on `authority`.
- **Changed (involved patients):** an involved patient whose authorize answer
  carries no token, or one with no signature, is left out of the involved list
  as `failed`. Before,
  an answer with no token was left out as `unlabelled`. An unsigned token that
  carried its label was put in the list, and the Hub then refused the whole leg
  (`involved patients refused`).

## The coverage refusals for a member the gateway names no patient for drop "system of record" (v1.1.0)

- **Changed (refusal text):** the Da Vinci ingress's no-coverage refusals for a
  member the participant's system of record names no patient for (one it does
  not hold, or holds but cannot name) end in `(this gateway's records name no
  patient for this member)`, where they ended in `(this gateway's system of
  record names no patient for this member)`:
  - CDS Hooks: `412 no coverage to route by: send prefetch.coverage or
    fhirServer (this gateway's records name no patient for this member)`, and,
    with `CDS_FHIR_SERVER_READ=off`, `412 no coverage to route by: send
    prefetch.coverage (this gateway's records name no patient for this member,
    and it does not read fhirServer)`;
  - DTR `$questionnaire-package`: `422 no coverage to route by: send the
    coverage parameter (this gateway's records name no patient for this
    member)`.

  Statuses and conditions are unchanged, and so is each reason's beginning,
  `no coverage to route by:`, the beginning the participant protocol documents
  for these CDS Hooks refusals. `no coverage in request or system of record`
  is unchanged byte for byte. A caller that matches a whole reason text must
  match the new explanation; one that matches the documented beginning is
  unaffected.

## A requester records the Authorization Framework's audit refusal by its rule (v1.1.0)

- **Changed (records and metrics):** when the Authorization Framework cannot
  record its decision, it answers `POST /authorize` with 502
  `{"error":"decision audit failed","rule":"audit"}`. A requester's gateway now
  reads `rule` and records the leg as refused by the Authorization Framework
  on the `audit` rule, where it recorded `other`. That covers the access line's
  `outcome` and `refusal`, `Exchange` (its `outcome`), `ExchangeRefused`,
  `ExchangeRefusedByRule`, `shn.gateway.exchanges` and
  `shn.gateway.refusals`. So a whole Audit Plane outage, which the Framework
  meets before the Hub does, reaches the per-rule `audit` refusal alarm.
  - The rule is read only from the field of a 502, never from the error
    text. A 502 that states no rule, or a rule this gateway does not read,
    and a 500 that states one, are this gateway's refusal on `authority`, as
    is a Framework that cannot be reached: no authority could be obtained. A
    403 stays the authority denial.
  - Your caller's answer is unchanged: the opaque 502 `authorization
    failed`.
  - `LegOutcome` (`failed`), the token outcome (`unavailable`), an answer's
    own authorization error class (`other`) and the involved-patient token
    omissions (`failed`) are unchanged.

## A system of record's answer that repeats a member name is recorded as message integrity on every leg (v1.1.0)

- **Changed (exchange record):** a read or search of the participant's own
  system of record whose answer the gateway knows repeats a member name (a
  search page, or a read the connector reports with `engine.SoRInvalidAnswer`:
  the bundled FHIR connector does, for a member's Patient) was recorded
  `upstream-error`, with no rule: on the federated query a facility answers,
  coverage eligibility, patient-authored DTR and the member binding of a leg
  the gateway answers. A requester's gateway records its own system's failed
  read as before.
  The participant's answer to a forwarded operation that repeats a member name
  was already recorded `refused` by the gateway on `integrity`, with the call
  `malformed`. These reads are now recorded the same way: a body that can be
  read two ways is refused on message integrity, and it was the participant's
  system's answer that was malformed. The refusal and the requester's answer
  are unchanged at every level, for example `502 system of record returned an
  invalid response` for a records search and the patient-match `403` for the
  federated query's patient lookup. An alarm that counts `upstream-error` no
  longer counts it; one that counts refusals on rule `integrity` does.
- **Go API:** `engine.SearchError` and `engine.SoRReadError` gain `Unwrap`,
  which reports an answer that repeats a member name as
  `relay.ErrDuplicateKey`, and an unexported field each, so a struct literal of
  either from another package must name its fields. `engine.SoRInvalidAnswer(body)`
  is the read error for an answer a connector could not read; it reports a
  repeated member name that way too.

## Module 1's origination routes are recorded, and a fidelity refusal names its stage (v1.1.0)

- **Additive (monitoring):** each call a provider's gateway answers on one of
  module 1's `/scenario` routes that originates (builds and sends a request of
  its own) now writes an access line and an exchange record, and reports the
  exchange metrics (EMF and OpenTelemetry), as an ingress call does: direction
  `ingress`, route `scenario-*` (the route's path joined by `-`, such as
  `scenario-uc07-complete`), `sender` this gateway, `exchange` and
  `correlationId` the first leg's the call sent (a refusal may belong to a
  later leg). Its latency is the whole call, a flow of legs: it is on the
  access line (`latencyMs`) but feeds neither `ExchangeLatency` nor the
  exchange duration and stage instruments nor `/internal/stats` latency; the
  exchange count, its outcome and its refusals count as for any exchange.
  A refusal carries its real rule: the Hub's or a peer's,
  `routing` for a coverage naming no payer the network registers,
  `authentication` for a refused `/scenario` caller, `fidelity` for claim facts
  stated for an order the member does not hold. Before, these calls wrote no
  access line and reported no refusal metric. The local routes (`uc06/cancel`,
  `uc07/cancel`, `uc07/pending`, `reset`) are not recorded. `ActiveRequests`
  counts the recorded calls as `ingress`. At `observe`, one diagnostic
  `conformance.result` closes an origination call, counting its legs'
  findings together.
- **Go API (additive):** `engine.ExchangeOriginationRoutes` names the routes
  of `engine.ExchangeRoutes` that module 1's origination calls are recorded
  under.
- **Additive (monitoring):** a fidelity refusal names its stage: the access
  line's `refusal.stage` and the record's `RefusalStage`
  (`recording.RefusalStages`; the engine adds no alias): `build`, SHN refused
  to build the message the refusal is about, because of what the caller
  declared or did not state; `verify`, an edit or message it built failed its
  own verification. The OpenTelemetry `shn.gateway.refusals` instrument gains
  the attribute `shn.refusal.stage` (`none` for every other rule, `build`,
  `verify`, `other`), so every one of its series gains the label
  (`shn_refusal_stage` in Prometheus, `none` off fidelity): a query that
  matches the series' labels exactly adds it, or aggregates it away
  (`sum without (shn_refusal_stage)`). ExchangeRefusedByRule{rule=fidelity} counts fidelity
  refusals at stage verify; build-stage refusals count in ExchangeRefused and
  the OTel refusals instrument with shn.refusal.stage=build. No EMF line's
  dimensions change.
- `deploy/monitoring/prometheus/alerts.yml`'s fidelity and audit rule leaves out
  `shn_refusal_stage="build"`.
- `docs/MONITORING.md`, "Access lines"; `docs/CONFIGURATION.md`, "Access
  lines (always on)"; `STABILITY.md`, "Access lines".

## The evaluation bundle's reference provider checks TLS certificate chains (v1.1.0)

- **Security (evaluation bundle):** the reference provider in `deploy/eval`
  (upstream `br-provider` at its pinned commit) skipped the TLS certificate
  chain check on its outbound HTTPS by default: upstream sets
  `security.ssl-verify` to false, which installs a trust manager that accepts
  any chain. Its connections to the gateway are plain HTTP, but a FHIR server
  a user adds in the provider's settings, and the discovery, UDAP
  registration, token and userinfo calls to it, went through that manager.
  `compose.eval.yml` now sets `SECURITY_SSL_VERIFY: "true"`, so a server's
  chain must reach a root in the image's JVM trust store. A server on a
  private CA needs that CA added to the trust store.

## The evaluation bundle's reference payer builds again (v1.0.0)

- **Fix (evaluation bundle):** `deploy/eval/brpayer/build.sh` failed in
  upstream's frontend stage, on type errors in test files upstream's
  `.dockerignore` leaves out of its build: the script sent the pinned tree
  to Docker on standard input, where `.dockerignore` does not apply. Both
  reference builds (`brpayer/build.sh` and `brprovider/build.sh`) now build
  from the pinned tree written out as a directory, as upstream builds it,
  and pin the images of the build's tools (bun, Python and Maven) by digest.
  The final image's base, the distroless Java runtime, is not pinned, so
  each build takes its current Java patches.

## The validator and evaluation images' JVM trusts no eMudhra emSign root (v1.0.0)

- **Security (deployment):** from v1.0.0, the validator image
  (`deploy/validator`, every IG line) and the evaluation kit's images
  (`deploy/eval`: HAPI, and the reference payer and requester) no longer trust
  the four eMudhra emSign root certificates (emSign Root CA - G1, emSign ECC
  Root CA - G3, emSign Root CA - C1, emSign ECC Root CA - C3) that Oracle's
  Security Alert for CVE-2026-114820 removes from the JRE. Their JRE reads its
  trust store through links to the runtime base's
  `/etc/ssl/certs/java/cacerts`; each build removes the four from that store
  by SHA-256 fingerprint, with no network, and fails if any remains
  (`deploy/validator/truststore/drop-emsign-roots.sh`). The reference payer
  and requester stay the pinned upstream commits' builds, plus that one edit
  (`deploy/validator/truststore/wrap-image.sh`, which also checks the built
  image); their build scripts rebuild an image built before v1.0.0. Every
  other root the base carries is unchanged. An image built from a release
  before v1.0.0 still trusts them: rebuild it from v1.0.0 or later.

## The gateway starts only on a network that accepts wire protocol 2.0.0 (v1.0.0)

- **Start:** the gateway sends wire protocol `2.0.0`'s leg names, so it reads
  discovery once at start and refuses to start unless the descriptor's
  `wireProtocolVersionsAccepted` contains `2.0.0` (with the field absent, unless
  `wireProtocolVersion` is `2.0.0`). The refusal names the network's accepted list
  and the remedy. There is no fallback to `1.1.0`'s names, and no setting skips the
  check.
- A descriptor the gateway cannot fetch or read stops the start, as before, and now
  says it cannot confirm what the network accepts.
- `docs/CONFIGURATION.md`, "The start check".

## The OpenTelemetry instruments are items of the promise, and the EMF cost is published (v1.0.0)

- **Documentation:** `STABILITY.md` lists every OpenTelemetry instrument the
  gateway exports and every attribute they carry, in the evolving
  `monitoring` class, with how their Prometheus names follow from them; the
  examples in `deploy/monitoring/` are outside the promise.
  `docs/MONITORING.md`, "What EMF costs", gives the EMF lines each kind of
  exchange writes, the lines, bytes and ingestion cost per 1,000 exchanges
  for an example payer's and provider's gateway, and the custom-metric
  cap's monthly cost, as estimates at list prices.

## The monitoring examples alert on refusals, certificates, the ingress key and the registrar feed (v1.0.0)

- **New (examples):** the monitoring examples alert on refusals by rule
  (fidelity and audit at one refusal in a 5-minute period, integrity and
  routing at three, in two of three periods), a server or own certificate
  expiring within 21 days, an ingress signing key within an hour of
  expiry, and a registrar feed not read for over five minutes, each with
  promtool unit tests. The dashboard gains refusals by rule and refusing
  party, request and answer sizes, certificate expiry, the ingress signing
  key, the registrar feed's age and peers, and Authorization Framework
  tokens.

## The OpenTelemetry export carries the Go runtime and process metrics (v1.0.0)

- **New (monitoring):** whenever an exporter is on, the OpenTelemetry export
  carries the Go runtime's `go.*` metrics and the process's
  `process.cpu.time`, `process.memory.usage` (Linux) and `process.uptime`, in
  the evolving `monitoring` class. `docs/MONITORING.md`'s catalogue lists
  each under its OpenTelemetry and Prometheus names.
- **Additive (monitoring):** the certificate expiry gauge now covers the
  OTLP collector's certificate and the gateway's own OTLP client and
  metrics-pull certificates.
- `OTEL_GO_X_DEPRECATED_RUNTIME_METRICS` has no effect, and a boot warning
  says so.

## One page for monitoring the gateway (v1.0.0)

- **New (documentation):** [`docs/MONITORING.md`](docs/MONITORING.md) covers
  every way to watch a gateway: `/health`, `/internal/checks` and
  `/internal/stats`, the access lines with per-party queries for CloudWatch
  Logs Insights, Loki and Splunk, CloudWatch EMF, OTLP push and Prometheus
  pull, and the catalogue of every instrument under its OpenTelemetry,
  Prometheus and EMF names. It also covers running several replicas, stopping
  and troubleshooting.

## Exchange metrics: refusals, stages, sizes, requests in flight, conformance and HTTP (v1.0.0)

- **Additive (metrics):** with `METRICS_SERVICE` set, new EMF metrics beside
  the existing ones, which are unchanged byte for byte: `ExchangeRefused` and
  `ExchangeRefusedByRule`, `ExchangeStageLatency`, `RequestBytes`,
  `ResponseBytes`, `ActiveRequests` (the peak each minute),
  `ConformanceFinding` (without the exchange, as `ExchangeRefused`),
  `ValidatorLatency` and `ObserveCheckDropped` (`docs/CONFIGURATION.md`,
  "Exchange metrics"). Each is also an
  OpenTelemetry instrument when an exporter is on, with the standard
  `http.server.request.duration` for every request the main listener serves.
  Each adds EMF lines per call: a leg from the network writes up to 10 more
  when answered and 12 when refused, a call from the participant's own
  system up to 4 and 6, and each `$validate` call and finding one more.
- **Additive (stages):** a call's stages add up to its latency on every leg:
  a leg from the network adds `rest`, the time in none of its seven stages,
  and a call from the participant's own system splits into `network` and
  `self`. A stage the call did not enter is not reported.
- **Go API (additive):** `engine.ExchangeRecord.RequestBytes` and
  `ResponseBytes`; `engine.Config.ActiveRequestMetric`, `FindingMetric`,
  `ValidatorMetric` and `ObserveCheckDroppedMetric` (`engine.FindingMetric`);
  `engine.CheckKinds`, `ConformanceLevels`, `FindingActions`,
  `ValidatorLaneRouting`, `ValidatorLaneCertification`;
  `engine.CertificationLaneUnavailable.Called`; the test helper
  `engine.Gateway.RoutePatternsForTest`. In `telemetry`: the new instruments
  (`Refusals`, `ExchangeStageDuration`, `ActiveRequests`,
  `ExchangeRequestSize`, `ExchangeResponseSize`, `ConformanceFindings`,
  `ValidatorDuration`, `ConformanceChecksDropped`,
  `HTTPServerRequestDuration`); their keys (`KeyRefusalRule`, `KeyRefusalBy`,
  `KeyStage`, `KeyFindingKind`, `KeyAction`, `KeyTolerated`, and the semantic
  convention's `KeyHTTPMethod`, `KeyHTTPStatusCode`, `KeyHTTPRoute`,
  `KeyURLScheme`, `KeyProtocolVersion`, `KeyErrorType`); their closed lists
  (`Stages` and the `Stage*` constants, `Booleans`, `HTTPMethods`,
  `HTTPStatusCodes`, `HTTPErrorTypes`, `URLSchemes`, `ProtocolVersions`,
  `HTTPRoutePatterns`) and `SemconvOther`; `HTTPRoute` and
  `HTTPStatusErrorPairs`; `Attribute.OtherValue`, `Int` and `Optional`,
  `Instrument.Bound`; and `EMF.SampleActiveRequests`.

## Deleting an ingress signing key retires it without a restart (v1.0.0)

- Deleting an ingress signing key's row now retires the key without a restart: every replica that can reach the database drops it from its cache at its next refresh, within about 30 seconds, and refuses bearers signed with it from then on. A replica that cannot reach the database keeps the key until a reload succeeds.

## The ingress key's log lines name the store error's class (v1.0.0)

- The shared-store ingress key's log lines (a failed reload, warm-up, out-of-band reload or refresh) name the store error's class, such as `timeout` or a Postgres error code, rather than its text, which could quote a host, a user or a statement. The refresh line also names its stage: reload, rotate, or a skipped retry.

## Token requests no longer queue behind a hung key write (v1.0.0)

- When the shared store answers reads but writing a new ingress signing key hangs, token requests no longer queue behind the slow write: after a failed attempt a replica grace-signs at once with its current key, and retries the write once per refresh interval. A token request waits at most one store timeout behind the attempt in flight.

## The gateway ships monitoring examples (v1.0.0)

- **New (examples):** `deploy/monitoring/` holds a Grafana dashboard,
  Prometheus alert rules with their promtool unit tests, a compose example
  (Prometheus and Grafana), and OpenTelemetry Collector recipes for Datadog,
  Splunk, New Relic, Azure Monitor, AWS (on the contrib Collector; ADOT
  carries the same components) and Grafana Cloud. They read
  the gateway's metrics as Prometheus stores them from OTLP push or the pull
  listener. The files are examples, outside the stability promise; the
  metric names and attributes they read are inside it.

## The gateway reports its operational state as metrics (v1.0.0)

- **New (monitoring):** the gateway reports its operational state as metrics, in the evolving `monitoring` class, every minute, over EMF (with `METRICS_SERVICE`) and OpenTelemetry. The metrics are:
  - `GatewayInfo`;
  - `CertificateExpiry`, plus `CertificateExpiryMin` for the server and self roles;
  - `IngressKeyRemaining` and `IngressKeyAge`;
  - `DependencyAge` and `DependencyAgeMax`, for the registrar feed;
  - `PeersKnown`;
  - `TokenAcquisition` by outcome, and `TokenLatency`.

  A value the gateway does not know is left out, never reported as 0. A custom trust bundle named by `SSL_CERT_FILE` or `SSL_CERT_DIR` is reported as one trust-root series.

## The ingress signing key rotates an hour early (v1.0.0)

- The shared-store ingress bearer signing key rotates every 23 hours instead of every 24; each key's validity is unchanged (24 h 6 min). A rotation stalled by an unreachable store, or by a store that reads but cannot write the new key, now grace-signs with the current key for an hour before `/oauth/token` answers 503, so it shows in the key's remaining life and age first. The stall's "rotation deferred" log line is written at most once per refresh tick, with a count of the deferred signings and the store error's class rather than its text.

## Every gateway reports its own exchange stats (v1.0.0)

- **New (monitoring):** `GET /internal/stats`, always on, answers with each
  replica's exchanges over the last minute, five minutes and hour, by direction
  and exchange kind: counts by outcome, rate, and latency at P50, P95 and P99,
  with the mergeable sketch they were read from, and the same for the
  exchanges that called the participant's own system, by exchange kind
  ([`docs/CONFIGURATION.md`](docs/CONFIGURATION.md), "Exchange stats"). It
  uses `/internal/checks`' access rule. Nothing per peer is kept.

## The gateway records what its certificate and key metrics read (v1.0.0)

- **Go API (additive):** `engine.Config.CertRecorder`, `AuthzClient` and
  `ConsentClient` (when the two clients are unset, each call uses `Client`);
  `checks.Target.Client`; `pgstore.IngressKeyStore.KeyTimes`
  (`pgstore.IngressKeyTimes`). Every outbound client keeps the transport,
  timeout and redirect policy it had: nothing about how the gateway connects
  changes. The metrics that read them are under "The gateway reports its
  operational state as metrics".

## How long a call from your own system waited on the network (v1.0.0)

- **Additive:** the access line of a call from the participant's own system
  carries `network` (`diagnostics.AccessNetwork`): `latencyMs`, from handing
  each sealed request to the transport for the Hub to reading the Hub's
  answer, or the connection failing, so the Hub's and the recipient's time
  together; and `calls`, the requests handed to the transport. Requests
  outstanding together are timed once, so it is at most the line's own
  `latencyMs`, and the difference is the gateway's own time. Before, such a
  call recorded only its total. It is absent on a leg from the network and
  on a call that handed nothing to the transport for the Hub. No metric
  dimension changes.
- **Go API (additive):** `engine.ExchangeRecord.Network`
  (`engine.NetworkWait`); `diagnostics.AccessLine.Network`
  (`diagnostics.AccessNetwork`).

## A stop has the time it needs, and an audit drain cut short still leaves its count (v1.0.0)

- **Documentation (deployment):** `docs/DEPLOYMENT.md`, "Stopping the gateway", lists
  each step of a stop with its bound: up to 43 s in all, or 32 s without
  `DIAGNOSTIC_SINK_URL`. It asks for a container stop timeout of 45 s, with the
  setting for `docker run`, Docker Compose, Kubernetes and Amazon ECS. CONFIGURATION.md
  understated the budget, as about 17 s at `observe` and about 28 s with diagnostic
  capture, since neither figure counted the in-memory audit outbox's 10 s drain.
- **Fix (deployment):** the bundle and the two evaluation compose files under
  `deploy/` set `stop_grace_period: 45s` on the gateway's service. Docker's default of
  10 s killed a stopping gateway before its drains ended. At `observe` it lost the
  findings of conformance checks still queued.
- **Audit outbox at stop:** an in-memory outbox with records queued reports their
  number `n` as `AuditQueueDepth` before its drain and, when discovery names an audit
  endpoint to send them to, logs
  `gateway: audit: stopping with <n> records in the in-memory outbox`. When the drain
  ends, it logs how many records were sent or refused (each counted, as before) and
  how many were left unsent (counted `AuditAppendDroppedBy{reason=stopped}`, as
  before), or that what was lost is unknown if the outbox cannot be read. A gateway
  killed during the drain therefore leaves the count it began with and no result, so
  its loss reads as unknown rather than as none. Nothing writes to the outbox yet
  (CONFIGURATION.md, "The audit outbox"), so this takes effect once something does.

## The gateway can export its metrics over OpenTelemetry, pushed or scraped (v1.0.0)

- **New (monitoring):** with `OTEL_METRICS_EXPORTER=otlp`, the gateway pushes
  its metrics over OTLP (`http/protobuf` or `grpc`), configured by a closed
  subset of the OpenTelemetry SDK's settings
  ([`docs/CONFIGURATION.md`](docs/CONFIGURATION.md), "OpenTelemetry metrics
  export"). The CloudWatch metrics are unchanged, line for line.
- **New (monitoring):** with `OTEL_METRICS_EXPORTER=prometheus` (alone or
  beside `otlp`), the gateway serves `GET /metrics` for a Prometheus server to
  scrape, on its own listener at `OTEL_EXPORTER_PROMETHEUS_HOST` and
  `OTEL_EXPORTER_PROMETHEUS_PORT` (default `localhost:9464`), never on the
  gateway's own listener. A bind other than this machine's is served only
  over TLS (`METRICS_PULL_TLS_CERT_FILE`, `METRICS_PULL_TLS_KEY_FILE`) with a
  bearer token (`METRICS_PULL_TOKEN`), a client CA
  (`METRICS_PULL_CLIENT_CA_FILE`) or both; without them nothing is served
  there, and `telemetry-config` says so. A scrape without the token gets
  `401`.
- It differs from the OpenTelemetry specification in two documented ways:
  - **Off by default:** `OTEL_METRICS_EXPORTER` unset means `none`, not `otlp`.
  - **A setting that is not valid never stops the gateway, and never sends
    metrics to a guessed place:** a tuning setting falls back to its default,
    a metrics form that is not valid gives way to a valid general form, and a
    destination or credential setting with no valid form left turns OTLP
    export off. Each is named in a boot line and fails the new
    `telemetry-config` check with the new code
    `invalid-telemetry-configuration`.
- **New (monitoring):** `OTEL_CONFIG_FILE` names an OpenTelemetry
  configuration file (`file_format` 1.0) that governs in place of the other
  `OTEL_*` settings. One periodic OTLP reader, one `prometheus/development`
  pull reader (protected by the same `METRICS_PULL_*` settings), the resource and
  views are honoured and build the same export and listener as the settings; what
  the gateway does not support (a second reader of either kind, a `console`
  exporter) is named in a boot line and in `telemetry-config` and turned off on
  its own, and a file that cannot be read or does not parse exports nothing
  ([`docs/CONFIGURATION.md`](docs/CONFIGURATION.md), "The configuration file").
- `/internal/checks` gains `telemetry-config`, present whenever a monitoring
  setting is set, and `telemetry-export`, present while OTLP export is on. Both carry the new result field
  `advisory: true`, which no other check carries. Both, the field and the code
  are in the evolving `monitoring` class.
- The final export runs at stop after the audit outbox drains and the observe
  checks flush, and before the diagnostic publisher's drain; it adds up to 5 s
  to the stop.
- The image carries `/licenses/THIRD_PARTY_NOTICES.md` and the gateway's
  `LICENSE`. The new dependencies are the OpenTelemetry Go SDK and its OTLP
  exporters (v1.47.0), with gRPC and protobuf; its Prometheus exporter
  (v0.69.0), with the Prometheus Go client; and otelconf (v0.27.0), which reads
  the configuration file. The binary links 58 Go modules, 18 of them through
  otelconf (its trace, log and stdout exporters and its propagators, which the
  gateway never runs: none of them has an `init` or sets a global), about 1.2 MB
  in all.

## A gateway other than a provider's that sets an origination profile needs no populate endpoint (v1.0.0)

- **Fix (configuration):** `ORIGINATION_PROFILE=demo` or `provider-data` requires
  `PROVIDER_DTR_POPULATE_URL` on a provider only, since only a provider
  originates. A gateway of any other role that sets a profile used to refuse
  to boot without an endpoint it never calls. It now boots, and builds no
  populate client for the profile. `PROVIDER_DTR_NATIVE=true` still requires
  the endpoint, on any role.
- A provider's gateway is unchanged: a profile without the endpoint still
  refuses to boot.

## The stability promise has tiers (v1.0.0)

- **Documentation (stability promise):** `STABILITY.md`, "Versioning policy",
  puts each item of the binary contract in one of three tiers, set by its class
  ("Tiers at v1.0"):
  - **Full:** only a major release may remove or rename the item, change what
    it means, or narrow its closed set of values. At v1.0 this is the network's
    own surface and guarantees (authentication, authority on each leg,
    consent, routing and addressing, replay, audit, message integrity, and the
    fidelity of the gateway's own edits), discovery, and the Prior
    Authorization workstream's CRD, DTR and PAS pass-through and routing.
  - **Evolving:** a minor release may also remove or rename the item, change
    what it means (its default included), or narrow its closed set of values,
    each with a note here. When a minor release changes an evolving setting's
    default, the gateway logs it at boot in that release, naming the setting and
    its old and new default. A setting still goes through the ignored or removed
    settings tables, so a gateway that still sets it is told, never silently
    changed. At v1.0 this is eligibility, patient-authored DTR, the Federated
    Query Services workstream, modules 1, 2 and 3, and, as before, the items
    inside the promise on the observer event feed and the access line, whose
    clauses widen to these terms; an item outside the promise there stays
    outside. The tiers table names when each evolving class graduates to the
    full terms.
  - **Outside the promise:** `test tooling`, `unsupported` and `SHN-own` items,
    as before.
- A new class, `PA workstream, evolving` (owner `PA workstream`), holds the
  Prior Authorization workstream's eligibility and patient-authored DTR items.
  Five rows move to it: the setting `PAYER_ELIGIBILITY_URL`, the endpoint
  `POST /CoverageEligibilityRequest/$submit`, the observer feed's `legType`
  value `eligibility-ingress`, and the protocol's `transactionType=coverage-eligibility`
  and `transactionType=patient-dtr`.
- A sixth row changes class: `CONFORMANCE_ENFORCEMENT` moves from `module 3` to
  `core`. Its name, its values (`none`, `observe`, `structural`, `strict`) and
  its unset default (`none`) are full; what each level checks is module 3's and
  stays evolving, and `CONFORMANCE_TOLERATE_PEERS` stays `module 3`. No other row
  changes class.
- A section that describes the behavior of an item of an evolving class opens
  with an **Evolving (v1.0)** line naming it.
- Nothing the gateway or the CLI does changes.

## The install bundle passes the conformance level through (v1.0.0)

- **Install bundle:** `deploy/bundle/compose.yml` passes `CONFORMANCE_ENFORCEMENT`
  and `CONFORMANCE_TOLERATE_PEERS` through from whoever runs it, as the
  evaluation bundle does. It sets no default, so the bundle runs at `none` and
  tolerates no payer unless you set them. Before, it passed neither, and the
  level could be set only by editing the file or adding an override file.
- **Install bundle:** it still starts its validator and waits for it to report
  healthy at every level; the gateway sends the validator no payload until a
  level is set (only its connectivity check reaches it). Its `README.md` says so.
- Nothing the gateway does changes.

## A PAS request carries the clinician its order names (v1.0.0)

- **Wire (module 1):** a PAS submission or amendment the gateway builds about
  an order whose `requester` names its clinician carries that clinician's
  records from your system of record: the record the `requester` names, plus
  the Practitioner, PractitionerRole, Organization and Location records it
  references, to two references from the requester, at most 16. The payer
  resolves the order's requester inside the request. Before, such a request
  was refused, by the payer or, on the `provider-data` profile, by the
  gateway's own guard (`502 PAS evidence linkage failed`), after the CRD and
  DTR legs had already gone out.
- **New refusal:** an order naming a clinician whose records your system
  cannot supply, or records outside those bounds, is refused before any leg
  (`422`, naming the reference and the reason), at every conformance level.
  An order naming no requester is unchanged.
- `docs/CONFIGURATION.md`, "Requests the gateway originates", documents the
  carry and the refusals.

## A CRD request's Bundle entries carry absolute fullUrls (v1.0.0)

- **Changed bytes, nothing refused (module 1):** the `draftOrders` Bundle in an
  `order-sign` or `order-select` CRD request the provider's gateway builds
  names each entry by an absolute `urn:uuid:` fullUrl, as FHIR R4 requires of a
  Bundle entry, instead of the order's relative `Type/id`. So does the
  `deviceHistory` prefetch Bundle it builds for an `order-dispatch` request
  when the provider's own search is left out.
- The UUID is a version 5 UUID derived from the request's `hookInstance` and
  the entry's place, never the order's own id, so the same exchange builds the
  same bytes. `selections` and `dispatchedOrders` still name orders as
  `Type/id`, and each order is carried as sent. A payer that resolves orders by
  their type and id is unaffected.

## The image is built for amd64 and arm64, each with an SBOM (v1.0.0)

- **Image:** the published image is an index for `linux/amd64` and
  `linux/arm64`, each image with an attested SPDX SBOM, where v0.63.0's was
  one `linux/amd64` image. Images are signed keyless with cosign; `README.md`
  gives the certificate identity and OIDC issuer to verify a signature with.
- No wire change, and no change to what the gateway sends.

## The gateway knows its own build version (v1.0.0)

- **Binary:** each gateway build carries its version. A release image is
  stamped with its release tag at build (`gateway/Dockerfile`'s
  `GATEWAY_VERSION`). A build without a stamp uses the module version Go
  records (`go install ...@v1.0.0`, or a build from a clone of the tagged
  repository), and otherwise reads as `v0.0.0-unstamped`.
- **`GET /health` (additive):** `version` now reports the build's version when
  `SHN_VERSION` is unset, where it was omitted before. `SHN_VERSION` still
  overrides it.
- The gateway logs its build version at boot, and when `SHN_VERSION` is set to
  something else, the line names both.
- **Binary (additive):** `gateway -version` prints the build's version and where it
  came from (`v1.0.0 stamp`) and exits 0, before it reads any setting, file or
  network.
- No wire change, and no change to what the gateway sends.

## The deprecated Go names stay past v1.0.0 (v1.0.0)

- **Documentation (Go API):** the deprecated `engine` names stay through
  v1.0.0, including those that earlier releases' notes said stay until the
  engine package split or until v1.0.0 (`engine.PendLedger`,
  `engine.InspectNativePASResponse`, `engine.RefusalRules`,
  `engine.Config.ValidatorsByLine` and the rest). So do
  `scenariodriver.Config.ConsoleURL` and `Driver.RunConsoleScenario`, whose doc
  comments said v1.0.0 removes them. A later minor release removes them all,
  naming each in its notes' "Go API" section. Their doc comments say so.
- Nothing the gateway or the CLI does changes.

## A core-only build profile (v1.0.0)

- **Build profile (no compatibility promise):** a build with the `shn_coreonly`
  tag leaves out the engine's workstream zones (the Prior Authorization and
  Federated Query Services adapters and on-behalf building) and the packages
  that assemble the Prior Authorization gateway (`app`, `cmd/gateway`, and
  `connectors/pgstore` while it reaches the Prior Authorization pend ledger and
  continuation store through package `engine`). It compiles only if the
  engine's core names no zone. It must compile in at least one workstream: with
  none, engine init panics.
- The default build, which every release image and `go install` uses, is
  unchanged: the same workstreams, modules, routes and API.
- No configuration or wire change.

## A Go API change is named in the release notes (v1.0.0)

- **Documentation (stability promise):** `STABILITY.md`, "Go APIs are
  SHN-internal", says how a change to the module's Go packages ships after
  v1.0.0, within a major: in a minor release, never a patch, with each
  incompatible change named in that release's notes, in their "Go API"
  section. The Go packages still carry no compatibility promise.
- Nothing the gateway or the CLI does changes.

## The test doubles exist only in a test build (v1.0.0)

- **Go API (no compatibility promise):** the gateway's test doubles and test-only
  seams are now behind the `shn_testdouble` build tag. An untagged build, as every
  release image and `go install` is, no longer links or exports
  `engine.LineFakeValidator`, `engine.NewLineFakeValidator`, `engine.LineFakeCall`,
  `engine.WithFHIRServerTrustForTest`, `engine.EnableIngressForTest` (the ingress
  auth bypass), `relay.ForTest` or `relay.BytesForTest`.
- The module's own tests need `-tags=shn_testdouble`: an untagged `go test ./...`
  of `engine`, `engine/relay` or `app` no longer compiles.
- No configuration or wire change. A released gateway already refused
  `SHN_FAKE_VALIDATOR` at boot, and still does.

## An unset conformance level is `none` (v1.0.0)

- **Behaviour change:** a gateway that leaves `CONFORMANCE_ENFORCEMENT` unset now
  runs at `none`: no payload conformance check runs, no finding is recorded, and
  no validator is needed to boot. Before, an unset level was `observe`, which
  checked every payload, recorded each defect and needed a validator. Payload
  conformance is the participant's opt-in; set `observe`, `structural` or `strict`
  to keep checking.
- Nothing is newly refused. The network rules and the check of a payload this
  gateway itself translated between IG lines refuse at every level, `none`
  included, as before.
- The boot line says when the level came from the unset default:
  `gateway: conformance enforcement=none (CONFORMANCE_ENFORCEMENT unset)`.
- A gateway that sets its level is unchanged.

## The stability promise from v1.0.0 (v1.0.0)

- **Documentation (stability promise):** `STABILITY.md` states the versioning
  policy that binds from v1.0.0, in the tiers below ("The stability promise has
  tiers"). A major release is the only one that may remove or rename a full item,
  change what it means, or narrow its closed set; a minor release may add items and
  values, and may change an evolving item on the evolving terms; a full item is
  deprecated, with a boot warning for at least one minor release, before a major
  release removes it.
- Settings, endpoints, metrics, `/internal/checks` results and their failure codes,
  the protocol (under its own wire protocol version) and the container image are
  covered on those terms, each item in its class's tier.
- The observer event feed and the access line stay evolving surfaces, on the
  evolving terms: a minor release may also remove or rename one of their items,
  change what it means, or narrow a closed set, with a note here. Each one's
  section of `STABILITY.md` says when it graduates; its items then take their
  classes' tiers.
- `/health` and `/internal/checks`: the payload shapes are covered; which checks a
  gateway reports follows its own configuration.
- Diagnostic collection (`DIAGNOSTIC_*`) is test tooling, outside the promise.
- The `shn` CLI, versioned with `shn-sdk`: from `shn-sdk` v1.0.0 the promise covers
  `keygen`, `register`, `login`, `clients`, `revoke`, `rotate` and `doctor`, with
  their flags, exit codes and the key files they write. `eligibility`, `priorauth`,
  `send-test`, `scenario-key` and `scenario-sign` are test tooling.
- Nothing the gateway or the CLI does changes.

## The evaluation bundle passes `CONFORMANCE_TOLERATE_PEERS` through (v1.0.0)

- **Evaluation bundle:** `deploy/eval/compose.eval.yml` passes
  `CONFORMANCE_TOLERATE_PEERS` through from whoever runs it, as it does
  `CONFORMANCE_ENFORCEMENT`. It sets no default, so the bundle tolerates no payer
  unless you set the variable, as any gateway's default.
- **Evaluation bundle:** `deploy/eval/smoke.sh`, SHN's own gate on the bundle,
  runs the gateway strict and tolerates the reference payers it drives, as SHN's
  own provider gateways do. A FHIR profile defect strict would refuse in their
  DTR or PAS answers is recorded as a tolerated finding, and the answer is
  carried. Every other payer and every other check stays strict.
  `payer/smoke.sh` sets neither variable.
- Nothing the gateway does changes.

## The evaluation smoke drives its scenarios with `shn send-test` (v1.0.0)

- **Evaluation bundle:** `deploy/eval/smoke.sh` drives the eight use cases with
  `shn send-test --follow 30`, the command the evaluation README gives you, in
  place of requests of its own. It keeps its `uc01` readiness probe and the
  conformant lane's checks. A scenario still pended after 30 seconds fails it, as
  before.
- **Documentation:** the evaluation README says that UC-06 and UC-07 take the
  attestation's `answer` from the request, which the descriptor's personas do
  not state and the gateway does not supply, and that `shn send-test` states it
  from shn-sdk v1.0.0, the `shn` the README's command needs.
- Nothing the gateway does changes.

## No payer is exempt from the answer checks; a participant tolerates the payers it lists (v1.0.0)

- **Behaviour change:** the reference-payer exemption leaves the gateway. Before, a
  provider gateway on the `provider-data` or `demo` origination lane skipped the check of a
  reference payer's answer to a leg it originated (`validateFHIRPayerIngress`: no validator
  call, no finding, relayed at every level). Every payer's answer is now checked at the
  gateway's level on every lane, and module 1's reference-payer identity set no longer
  reaches the conformance path (`engine.ReferencePayerIdentities` stays, as the set SHN's own
  tolerance lists are pinned to).
- **New setting (additive):** `CONFORMANCE_TOLERATE_PEERS`, `<system>|<value>` payer
  identities, comma-separated (`engine.ParseToleratePeers`; `engine.Config.ConformanceToleratePeers`).
  For a payer's answer, originated or carried (PAS and DTR), from an exchange routed by payer
  identities all listed, a defect the FHIR profile check would refuse is recorded instead and
  the answer carried. Only that check: the content rules (`answer.shape`, `patient.answer`),
  the CDS Hooks answer check, network rules and module 2's bridged certification refuse as
  ever, and a check that could not run is never tolerated. Empty tolerates no one; a malformed entry refuses
  boot; the list is logged at boot (`gateway: conformance tolerated peers=…`).
- **Observer and finding surface (additive):** `ConformanceFinding.Tolerated`
  (`"tolerated": true` on the `conformance.observed` detail), set when tolerance turned a
  refusal into a record. Module 3's.
- SHN's own provider gateways (preview `provider-gw`, `provider-data-gw`,
  `test-provider-gw`, and the Compose two-RI and multiprocess providers) list the reference
  payers until their answers conform. No list is set where a participant runs its own
  gateway: the Kit, a hosted tenant (which cannot set one yet) or the evaluation stack.

## A carried DTR package answer is validated at the provider ingress (v1.0.0)

- **Behaviour change:** a payer's answer to a DTR `$questionnaire-package` request that a
  provider gateway carries for its own participant's system through its Da Vinci ingress is
  `$validate`d at the gateway's conformance level against the operation's output Parameters
  profile (`dtr-qpackage-output-parameters`, which the answer need not declare), as the
  carried PAS answers are. Not at all at `none` or with module 3 absent; recorded at
  `observe`; at `structural` refused for a structural issue and recorded for a deeper one;
  refused at `strict`, where a check that cannot run is refused `500`. A refused answer is
  answered `502` (`ingress validation failed`), recorded as a `fhir-ingress` finding from
  the peer. An empty answer (a `204`, or a `2xx` with no body) has no package to judge and is
  carried as sent.
- **The lines it is judged at are the ones that profile its names.** Every line's output
  profile slices `parameter.name` open, so a line of the other name family leaves the
  package unprofiled and does not judge it. The answer is judged at the lines of the
  line-specific names it uses (`PackageBundle` and `operationOutcome`: 2.0 and 2.1;
  `packagebundle`: 2.2; `outcome` is every line's), the routed line first when it is one of
  them, and is valid when valid at one; an answer using both families must be valid at a
  line of each, and one using neither is judged at every line, where each profile judges
  the absence (2.0.1 requires `PackageBundle`). Module 1's DTR fetch uses the same grouping
  (below).
- Before, an answer that conformed to no DTR line was carried at every level, unless the
  gateway carried it into its own DTR line's names (E-08), which judged it at its names'
  lines. The check now runs before E-08 and whether or not E-08 then renames the answer;
  E-08's own check is gone, so an answer is judged once. A renamed answer is still certified
  at the gateway's line by module 2, refused at every level.
- **What changes for the answers E-08's check refused** (it judged an answer valid at neither
  line of its names at one of them, the routed line, else 2.0):
  - the refusal is `502` (`processing`, `ingress validation failed`, no issues listed), as
    for the carried PAS answers, where it was `422` (`not-supported`, with the issues);
    the issues are to be restored to the refusal;
  - the ingress classifies the answer by its best verdict across both lines, as it does a PAS
    answer across its lines, so one with a structural issue at the line E-08 judged and only
    a deeper one at the other is recorded at `structural`, where it was refused; at `strict`
    it is refused as before;
  - a claim the answer makes (a versioned profile's line, or the lines whose packages define
    a structural marker where it sits) none of whose lines can be judged, for want of a lane
    or a validator that answers, makes the check unavailable: recorded at `structural`,
    refused `500` at `strict`, where the one judged line decided (`422`). `qr-context`
    claims every line, so it alone never does;
  - a line of its names that is neither the routed line nor needed to meet a claim never
    decides when it cannot be judged: where E-08 judged such a line and could not (unavailable: carried at
    `structural`, `500` at `strict`), the other line's verdict now decides, refused `502` at
    `structural` and `strict` when it refuses.
- **Module 1's DTR fetch** judges a payer's package answer at the lines of its names by the
  same grouping, against the profiles the answer declares, as before. A line outside them,
  the routed line or a claimed line included, no longer judges it at all: it neither passes
  it, nor makes the check unavailable when it cannot be judged, nor gives the best verdict.
  An answer such a line passed, left unavailable or found only deeper is now decided by the
  lines of its names (refused at `structural` for a structural issue there); with no lane
  for any of them the check is unavailable (`500` at `strict`), where the routed line judged
  it.
- An unavailable finding names the line that could not be judged (the routed line or a line
  of a claim none of whose lines was judged, else the first line tried), for every payer
  answer judged at candidate lines, PAS included; it named the routed line.
- No reference payer is exempt on the ingress, as for the carried PAS answers. It is payload
  conformance, the participant's own opt-in.

## Each leg is named with its workstream's prefix (v1.0.0)

- **Behaviour change (wire):** the gateway sends every leg's envelope under the leg's
  workstream-prefixed name (`pa.pas-claim`, `fqs.federated-query`), and answers every leg
  under it, where it sent and answered the flat name (`pas-claim`). It reads a request
  under either name. The token's operations do not change.
- The network's Hub forwards each request under the flat name and returns each answer
  under the name its requester used, whatever the descriptor's
  `wireProtocolVersionsAccepted` lists (today `["1.1.0"]`), so a peer on an
  earlier release sends, receives and is answered under the flat names as before. A
  v1.0.0 gateway therefore needs a Hub that reads a leg under either name, as the
  network's Hub does.
- The access line, the observer feed, the metrics and the exchange records keep naming
  each leg by its flat name.

## Module 1 runs only where the participant declares it (v1.0.0)

From shn-gateway v1.0.0, a gateway runs module 1, the `/scenario/*` origination
API, only when `ORIGINATION_PROFILE` declares it (`demo` or `provider-data`). A
provider gateway in front of a system that speaks Da Vinci itself needs no
module 1 configuration.

- **Who is affected:** a provider gateway that calls its own `/scenario/*`
  routes and never set `ORIGINATION_PROFILE`. On v1.0.0 it no longer has those
  routes. To keep using them, set `ORIGINATION_PROFILE=demo`, which is what an
  unset profile meant before. Such a gateway that also sets
  `SCENARIO_CALLERS_FILE` refuses boot until it sets the profile. A gateway
  that already sets the profile, and every payer, facility and PHG gateway, is
  unaffected.
- **Breaking (configuration):** an unset `ORIGINATION_PROFILE` means module 1
  is off, on every role. Before v1.0.0 it meant `demo` for `ROLE=provider`.
  With module 1 off:
  - no `/scenario` route is mounted, and each answers `404`;
  - `PROVIDER_DTR_POPULATE_URL` is no longer required;
  - the Da Vinci ingress and relaying are unchanged.
- **Breaking (configuration):** `SCENARIO_CALLERS_FILE` with no
  `ORIGINATION_PROFILE` refuses boot:
  `gateway: SCENARIO_CALLERS_FILE requires ORIGINATION_PROFILE: with none, module 1 is off and no route serves /scenario`.
- **Behavior change (boot output):**
  - With module 1 off, the boot output says so:
    `gateway: ORIGINATION_PROFILE unset — module 1 is off, and no /scenario route is served`.
  - It names each module 1 setting that is set but unused: `NPI`,
    `PROVIDER_DTR_NATIVE`, `PROVIDER_DTR_POPULATE_URL` and
    `PROVIDER_DTR_POPULATE_TOKEN_URL`. They are still checked at boot as
    before: an invalid `NPI`, or an incomplete `PROVIDER_DTR_*` group, still
    refuses boot.
- **Behavior change (refusal):** the `503` an originating route answered when
  no profile was set
  (`{"error":"origination profile not set: set ORIGINATION_PROFILE (demo|provider-data)"}`)
  is gone, because the route no longer exists.
- **Migrating:**
  - A provider gateway that uses its `/scenario/*` routes sets
    `ORIGINATION_PROFILE` (`demo` keeps v0.63.0's behavior).
  - A provider gateway that only relays its system's own Da Vinci requests may
    drop `ORIGINATION_PROFILE`, `NPI`, and `PROVIDER_DTR_NATIVE` with every
    `PROVIDER_DTR_POPULATE_*` setting (as one group: a partial group still
    refuses boot). It must drop `SCENARIO_CALLERS_FILE`.

## Three retired settings are no longer read, and an unbound amendment's reason moves to `detail` (v1.0.0)

- **Behaviour change (observer feed):** `pend.amendment-unbound` is now one kind.
  The reason an amendment bound no pend this gateway recorded moves from the kind
  (`pend.amendment-unbound:<reason>`) to the event's `detail`. The reason is one of
  a closed set, listed in `STABILITY.md`: `not-pended`, `update-in-progress`,
  `already-decided`, `ledger-unavailable`, `no-prior-claim` or `other-requester`.
  A reader that matched the old kinds reads `kind` and `detail` instead. The event
  is also logged as `gateway: pend.amendment-unbound: leg … correlation …: <reason>`.
- **Behaviour change (configuration):** `PAYER_DAVINCI_PAS_NATIVE`,
  `PAYER_DAVINCI_STRICT_EXTENSIONS` and `SHN_ACCEPT_UNKNOWN_MEMBERS` are no longer
  read. A gateway that sets one, to any value, starts and logs
  `gateway: WARNING: <setting> is set but no longer read: …` once at boot; remove
  it. None of them changed what the gateway relays, refuses or stores:
  - `PAYER_DAVINCI_PAS_NATIVE` was no longer a switch from v0.39.0. Its boot line
    `… is set but is no longer a switch …` is replaced by the warning above.
  - `PAYER_DAVINCI_STRICT_EXTENSIONS` reserved a strict-extensions overlay that
    never had a routing effect.
  - `SHN_ACCEPT_UNKNOWN_MEMBERS` asked for the default, carrying members your
    system of record does not hold. Beside `REQUIRE_KNOWN_MEMBERS=true` it no
    longer refuses to boot: `REQUIRE_KNOWN_MEMBERS` alone decides.
- **Breaking (Go API):** `engine.WithStrictExtensions` is removed. It set a field
  nothing read.
- **Additive (Go API):** `engine.PendAmendmentUnboundReasons`, the closed set of
  `pend.amendment-unbound`'s `detail`.
- A gateway SHN hosts for you no longer renders `PAYER_DAVINCI_STRICT_EXTENSIONS`,
  and the hosted settings no longer offer it.

## The certification evidence's lane qualification backs off (v1.0.0)

- **Behaviour change (load on a validator):** the certification evidence's own qualification
  of a 2.1 or 2.2 validator retries on the gateway's shared backoff, 15 s after the first
  failure doubling to 5 min, each wait drawn from the upper half of its step, where v0.63.0
  retried every 15 s for an hour, then every 5 min. A default lane's first attempt, and each
  re-qualification, also starts after a random part of 15 s. Gateways that a deploy restarts
  together no longer retry one validator in lockstep.
- **Behaviour change (a default lane):** while routing is still retrying a default lane's
  qualification, the evidence posts no corpus of its own. Once routing's attempt passes on the
  same endpoint, for the same line, with the same qualification and within the same budget, that
  pass is the evidence's first qualification; any difference and the evidence runs its own.
  Every re-qualification is the evidence's own run.
- **Behaviour change (verdict text):** while the evidence defers, a default lane's
  `certification validator unavailable` verdict names routing's qualification:
  `…; default lane qualification pending (routing qualifying)`, then `…; default lane
  qualification failed, retrying (routing attempt <n>: <reason>)`, where it read `…; default lane
  qualification pending` or `…; default lane qualification failed, retrying`.
- **Behaviour change (budget):** an attempt lasts up to 120 s, routing's budget, where it lasted
  60 s. The budget bounds an attempt's cost, not what qualifying means: a lane qualifies only by
  passing the readiness corpus, as before. An attempt the budget cuts off is still discarded, never
  resumed.
- **Additive (logs):** `certification_lane_qualification` also logs a change of a failure's
  reason, and carries `attempts` and, on a failure, `next_retry_s`.

## The exchange operations come from the workstreams (v1.0.0)

- `engine.ExchangeOperations` is derived from the compiled-in workstreams, as
  `ExchangeKinds` and `ExchangeRoutes` are: the hooks of the Prior
  Authorization workstream's CDS services, in discovery order, then the
  operations its leg taxonomy declares for a DTR request frame, then `other`.
  Its values are unchanged. Their order is now the derivation's, so
  `order-sign` comes before `order-select`.
- Nothing the gateway relays, refuses, stores or records changes.

## The gateway has an audit outbox (v1.0.0)

- **Additive (store database):** with `SHN_STORE_DATABASE_URL`, your store database
  gains a table, `gw_audit_outbox`, created at start like the gateway's other tables. It
  holds audit records waiting to be sent to the network's Audit Plane, unsigned until
  each send signs them: metadata only, no clinical content, each deleted once the Audit
  Plane accepts or refuses it, at most 10,000 for your holder (one more drops the
  oldest). Without the DSN the outbox is in memory. v1.0.0 writes no record into it yet.
- **Additive (protocol):** the gateway reads the audit endpoint the network's discovery
  names (`endpoints.audit`) at start, and sends its own audit records there, signed with
  the holder key in your `SHN_SECRETS` bundle as each is sent, retrying with backoff. A
  key you rotate (stop the gateway, `shn rotate`, start it) signs what is still
  queued. No setting overrides it (`AUDIT_URL` is still not read).
- **Additive (`/internal/checks`):** a new result, `AUDIT_ENDPOINT`, checks that
  endpoint, and a new failure code, `endpoint-unknown`, says discovery names none (the
  outbox then queues and sends nothing, and the gateway logs it at start).
- **Additive (metrics, with `METRICS_SERVICE`):** `AuditAppendSent`, `AuditAppendRetried`,
  `AuditAppendDroppedBy`, `AuditAppendDropped`, `AuditQueueDepth` and
  `AuditEndpointUnknown`.
- Nothing an exchange sends, refuses or stores changes: queuing a record waits at most
  200 ms for the outbox, and a record that cannot be queued is counted and dropped while
  the exchange goes on as before. See `docs/CONFIGURATION.md`, "The audit outbox".

## A structural marker claims the lines whose IG packages define it (v1.0.0)

- **Behaviour change (fewer `unavailable` answers):** a provider gateway reads
  the structural markers in a payer's answer as claims on lines, and
  `qr-context`, `extension-certificationType` and
  `extension-serviceItemRequestType` each claimed 2.1 alone. A claimed line must
  be judged, so a gateway with no 2.1 validator lane read every answer carrying
  one of them as `unavailable`, whatever its own lines said. The PAS and DTR
  packages of 2.0, 2.1 and 2.2 all define these three where they sit, so each
  now claims every line and decides nothing on its own. Such an answer is
  decided at the lines that judge it. For an answer with a structural defect at
  2.0, at a gateway whose lanes are 2.0 only, by level:
  - `strict`: refused `422` (`ingress validation failed`) instead of `500`
    (`no FHIR validator lane configured for contract line 2.1`; or `validator
    unavailable` when the gateway had a 2.1 lane that did not answer).
  - `structural`: **refused `422`**, where it was recorded as `unavailable` and
    relayed. This is a new refusal: the level the participant chose, applied at
    the line that can now judge the answer.
  - `observe`: recorded as `structural` instead of `unavailable`, and relayed
    as before.
  An answer valid at a judged line is relayed at every level, as before.
- **Behaviour change (claims):** a claim is a set of lines, met when any one of
  them is judged. A versioned `meta.profile` claims its line. A structural
  marker claims the lines whose packages, as the validator lanes load them,
  define it where it sits; `qr-coverage` claims 2.2, as before. No other
  extension claims a line. Together with the qualifying-lane refusal below, an
  answer is unavailable while a claim none of whose lines was judged has a lane
  still qualifying; a claim another of its lines met decides as any line does.
- **Behaviour change (order):** an answer carrying one of the three markers is
  no longer tried at 2.1 before 2.0.
- **Data:** the lines each marker claims come from
  `engine/pa_extension_lines.json`, generated from the PAS and DTR packages each
  line's validator lane loads, never from a marker's name.

## A provider gateway never states a clinician its participant did not (v1.0.0)

From shn-gateway v1.0.0, the requests a provider gateway originates
(`ORIGINATION_PROFILE`) name only a clinician that a source the participant
controls states.

- **Behavior change (configuration):** `NPI` has no default. Before
  v1.0.0, an unset `NPI` stood in as `1234567890`, a placeholder that is not a
  valid NPI, and it reached the CDS Hooks `userId`, attestations and
  eligibility requests.
- **Breaking (configuration):** a set `NPI` must be a ten-digit NPI with a
  valid check digit. Any other value refuses boot:
  `gateway: NPI must be a ten-digit NPI with a valid check digit, got "<value>"`.
- **Behavior change (origination):** the ordering clinician (`userId`) and the
  attesting clinician (an attestation's NPI) come from the first of these that
  names one:
  1. an `npi` the request sends, when scenario caller authentication
     (`SCENARIO_CALLERS_FILE`) verified its caller;
  2. the order's `requester` in the participant's system of record;
  3. the configured `NPI`;
  4. an `npi` the request sends without a verified caller.

  Before v1.0.0, an attestation took a sent `npi` first and the configured
  `NPI` before the record, so the placeholder replaced a clinician the record
  named.
- **Behavior change (refusals):** with no source, the gateway refuses with
  `422` rather than build the request.
  - A CDS Hooks request is refused before any leg:
    `build order-sign request failed: order <ref> names no ordering clinician: …`.
  - An attestation is refused with `attestation names no clinician NPI: …`.
    The single-call UC-06 route and UC-03's oxygen attestation refuse before any
    leg. `/scenario/uc06/complete` refuses before its amendment, and the pend
    stays parked for a corrected request. `/scenario/uc06/start` takes no `npi`,
    so it is refused only when its own order-sign request names no ordering
    clinician.
  - A refusal is recorded as the leg it would have sent failing (`leg.failed`)
    and, where an exchange is recorded, as a fidelity refusal.
- **Behavior change (eligibility):** an eligibility request names the
  configured `NPI` as its provider, and no provider when `NPI` is unset.
- **Behavior change (attestation answers):** a UC-06 or UC-07 request must
  state its answer. Before v1.0.0, a request with no answer was built with
  `"42"` or a default functional-status text. It is now refused:
  `422 answer is required: the request states no answer for questionnaire item <linkId>, …`.
- **Behavior change (sent npi):** an `npi` a UC-06 request sends must be a
  valid NPI, or the request is refused before anything is sent:
  `400 npi must be a ten-digit NPI with a valid check digit`.
- **Additive (Go API):** `scenarioauth.WithCaller` and `scenarioauth.CallerFrom`.
  The gateway's scenario caller authentication puts the caller it verified
  into the request context, and a route reads it with `CallerFrom`.
- **Additive (Go API):** `engine.ValidNPI(npi)` reports whether a value is a
  ten-digit NPI with a valid check digit, the check the boot and a sent `npi`
  apply.
- **Evaluation bundle:** the UC-06 and UC-07 answers are stated by `shn send-test`,
  which `deploy/eval/smoke.sh` drives its scenarios with.
- **Migrating:**
  - A provider gateway whose records name no ordering clinician sets `NPI` to
    its clinician's NPI, or names each order's requester (a `Practitioner`, or
    a `PractitionerRole` and its Practitioner, with a `us-npi` identifier).
  - A gateway that set `NPI` to a value that is not a valid NPI corrects it.
  - A caller of the UC-06 or UC-07 routes states its `answer`.

## A default validator lane retries its qualification until it passes (v1.0.0)

- **Fix:** a default validator lane (`shn-validator-<line>`, when no
  `FHIR_VALIDATE_URL_<line>` is set) qualified once per process. If that one
  attempt failed, for example while several gateways restarted together and
  the validator was busy, the lane stayed unqualified until the gateway
  restarted: the gateway answered a request framed at that line at its own
  line instead, carried messages to a peer at that line rather than building
  them there, and certified no payer answer at it. Now a failed qualification
  is retried, 15 s after the first failure and doubling to every 5 minutes,
  with jitter, until one passes, and the lane serves from then on. A lane boot
  waits for retries inside the 600 s startup budget instead of failing boot on
  its first miss. Each attempt is bounded at 120 s. The log carries a lane's
  state changes, not each attempt: `validator_qualification` gains `attempts`
  and `next_retry_s`.
- **Additive (checks):** `/internal/checks` reports each default lane as
  `validator-lane-<line>`. It reads the lane's state, dials nothing and starts
  no attempt; a `GET` reads it afresh. A lane not yet qualified fails with the new code
  `lane-unqualified`, with a hint naming whether it is on its first attempt or
  retrying, the attempt count and the last failure's kind.
- **Additive (metrics):** the `ValidatorLane` gauge, 1 when a lane is
  qualified and 0 when not, by `line` and `lane` (`routing` or
  `certification`, the latter at every level but `none`), every minute; and
  the `ValidatorLaneAttempt` count of each attempt once the gateway serves, by
  `line`, `lane` and `outcome` (`ready` or `failed`).
- **Behaviour change (refusal at `strict`):** for a payer's answer whose
  routed OR claimed line is a lane still qualifying (configured, not yet
  qualified), the answer's check is unavailable, even if another line reads
  it valid, the routed line included: an answer that claims a line nothing
  has judged yet is not certified by passing at another. Before, another line's valid decided
  it, so the check was more permissive exactly while its lane was down. At
  `observe` and `structural` this is recorded as `unavailable`. At `strict`,
  the participant's opt-in level, the exchange is refused with a 500
  (`validator unavailable`) where it used to pass on another line. This is a
  conformance decision at that level, not a network rule. It lasts only
  while the lane qualifies, which the retry above keeps to minutes after a
  boot, and the two ship together. A decisive line with no lane configured at
  all is decided as before. A line still qualifying that the answer is
  neither routed at nor claims is now listed as `unavailable` among the
  answer's lines rather than left out; it decides nothing.

## A PAS 2.0.1 amendment the gateway builds is one Claim (v1.0.0)

- **Behaviour change (wire):** a payer at PAS 2.0.1 now receives an amendment the gateway
  builds itself at PAS 2.0.1 (an update or resume of a `/scenario/*` flow) as one Claim. The
  update Claim names the submission it amends by `Claim.related[0].claim.identifier`, and the
  request Bundle carries no prior Claim, as PAS 2.0.1's request Bundle allows one Claim.
  Before, the Bundle carried the prior Claim beside the update. The gateway builds it with the
  PAS builders of `github.com/SmartHealthNetwork/shn-sdk` v1.0.0, which it now requires, up
  from v0.62.0.
- An amendment it builds at PAS 2.1.0 or 2.2.1 still carries the prior Claim beside the
  update, as before, whatever the payer's line: also for a PAS 2.0.1 payer, when it is carried
  at the gateway's own line because the two share none, or translated down because the
  participant opted into line translation. An amendment a participant's own system sends is
  carried as sent.

## The DTR populator is a gateway option (v1.0.0)

- **Breaking (Go API):** `engine.Config` no longer has the `Populator` field.
  The DTR populator is the Prior Authorization workstream's own, so it is set
  with that workstream's option, `engine.WithPopulator(p)`, which `engine.New`
  now takes: `func New(cfg Config, opts ...Option) (*Gateway, error)`.
  `engine.Option` is new. A workstream's options are declared with that
  workstream, and `New` applies them, in order, after every workstream's state
  is built and before anything serves.
- **Migrating:** `engine.New(engine.Config{..., Populator: p})` becomes
  `engine.New(engine.Config{...}, engine.WithPopulator(p))`. A call that set no
  populator compiles unchanged and populates as before, through the managed
  populator over the participant's system of record. `WithPopulator(nil)`
  keeps that default, as a nil `Config.Populator` did. A value of type
  `func(engine.Config) (*engine.Gateway, error)` no longer holds `engine.New`.
- Nothing the gateway relays, refuses or stores changes. The managed populator
  still reads the system of record as `New` decorates it, and the native
  populator (`PROVIDER_DTR_NATIVE`, or an operated-populate origination
  profile) is set the same way from config.

## The PA scenario personas have their own package (v1.0.0)

- **Additive (Go API):** package `pa/personas`
  (`github.com/SmartHealthNetwork/shn-gateway/pa/personas`) holds the Prior
  Authorization workstream's synthetic scenario personas: the lanes
  (`LaneConformance`, `LaneProviderData`, `LaneDemo`), `ScenarioPersona` and
  `ScenarioRoster()`, `CanaryTwins`, the demo lane's orders (`DemoOrderTuple`,
  `DemoOrderSet`, `DemoOrderCodes()`, the `DemoDisplay*` and `DemoDx*`
  constants) and the bridging-demo payer identities (`BridgeDemoPayerID`,
  `BridgeRefusePayerID`). It also holds the scenario table the gateway's
  `/scenario/*` routes resolve their members from: `ScenarioMembers(scenario)`
  and `Scenarios()`. It imports no engine package, so
  a driver, a seed or a discovery descriptor names the same members and orders
  without importing the engine. Every member, order and identity is unchanged.
- **Deprecated (Go API):** the `engine` names (`engine.ScenarioRoster`,
  `engine.CanaryTwins`, `engine.DemoOrderCodes` and the rest) stay until a
  later minor release removes them. Each type is the `personas` type, each
  constant and identity the `personas` value, and `engine.CanaryTwins` is the
  same map.
- Nothing the gateway relays, refuses or records changes, and each
  `/scenario/*` route resolves the same member.

## The relay edits are declared by the workstreams (v1.0.0)

- **Additive (Go API):** package `engine/relay` declares the registered edit
  row (`relay.Edit`, with `relay.EditKind` and its five kinds) and three pure
  functions over a set of rows: `relay.ValidateEdits(rows)` refuses an id
  outside the closed set (`relay.EditIDs`) or an id two edits declare, and
  `relay.EditName(rows, id)` and `relay.EditReceivedBy(rows, id)` name an
  edit and the party whose received bytes it changes. They hold no state.
- **Additive (Go API):** package `pa/paedits`
  (`github.com/SmartHealthNetwork/shn-gateway/pa/paedits`) holds the Prior
  Authorization workstream's edits E-01 … E-09 (`paedits.Rows()`, a new slice
  each call). It imports no engine package, so a reader of recorded exchanges
  names an edit without importing the engine.
- The gateway gathers each compiled-in workstream's edits into its registry
  once, refusing an edit on a leg its own workstream's catalog does not
  declare. The registry's content is unchanged: every id, name, transmit, path,
  kind, precondition, authority and disclosure.
- **Deprecated (Go API):** `engine.RelayEditName` and
  `engine.RelayEditReceivedBy` read the same registry through `relay` and stay
  until a later minor release removes them.
- Nothing the gateway relays, refuses, edits or records changes.

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
    `strict` that EOB is written, and at every level that validates the EOB the check is
    recorded as an `unavailable` finding. On `$inquire` each decision stands on its own: one
    EOB not written never keeps the answer's other decisions or their EOBs. Strict still refuses a participant's
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

## Go API notes before v1.0.0

Before v1.0.0, `STABILITY.md` named Go packages partners could depend on across
minor versions. The Go APIs are SHN-internal ([`STABILITY.md`](STABILITY.md),
"Go APIs are SHN-internal"); what earlier releases said is kept here as a
record.

### Supported seams

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

### Internal seams (not for partner use)

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

**`engine.NewNativeResponder` (v1.0.0).** The CDS service id for the order-select
leg and the Store the PAS legs keep their pend ledger in are options, not
parameters. Before:

```go
engine.NewNativeResponder(client, baseURL, crdServiceID, store, clock, opts...)
```

From shn-gateway v1.0.0:

```go
engine.NewNativeResponder(client, baseURL, clock,
	append(opts, engine.WithCRDService(crdServiceID), engine.WithPASStore(store))...)
```

An empty service id and a nil store need no option, as before. Nothing else
changes: the responder forwards the same requests to the same places.

### Unsupported internals

`internal/` packages and `cmd/` binaries are implementation details and may
change without notice between any versions. Do not import `internal/`
directly.

### Go API notes from STABILITY.md's behavior sections

These notes stood in `STABILITY.md` beside the behavior they accompanied,
under the section titles below. They describe Go APIs, which are
SHN-internal, and are kept here as a record.

#### Access lines

**New in v0.58.0.**

- `engine.ReadSystemOfRecord` now returns a wrapper that notes each read on
  the exchange record; a caller that type-asserts its result to the connector
  it was given no longer gets the connector back. `engine.NoSystemOfRecord()`,
  which reads nothing, is returned as it was given.

#### One record per exchange

**New in v0.58.0.** Go API only (additive); nothing a participant sends or
receives changes.

- `engine.Config.ExchangeObserved func(engine.ExchangeRecord)` receives one
  record for every call a gateway answers on a Da Vinci ingress route or for
  a leg it receives from the network, after the answer is written. That
  includes every refusal, the earliest authentication refusal among them, and
  a handler that panics. Nil (the default) records nothing.

#### Native PAS response contract

**Breaking in v0.46.0** (the assembly path is gone):

- `engine.WithPendReQuery` is REMOVED. It configured the pend re-query the poll
  performed; there is no poll.
- `engine.LegResult.ResponseAssembled` is REMOVED. A partner `LegResponder` that
  set it no longer compiles. It marked an answer this gateway had assembled, and
  a relayed answer is never one — delete the assignment; nothing replaces it.

#### A payer that keeps no system of record

**Additive in v0.58.0.**

- **Go API (additive):** `engine.NoSystemOfRecord()` (a `SystemOfRecord` that holds no
  one; pass it as `Config.SoR`) and `engine.ErrNoSystemOfRecord` (the failure of a read
  it cannot answer).

#### Origination profiles

**Behavior change in v0.57.0.**

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

#### Conformance enforcement levels

**Behavior change in v0.53.0: members the system of record does not hold are
carried by default.**

- **Breaking (Go API):** `engine.Config.AcceptUnknownMembers` is removed; use
  `engine.Config.RequireKnownMembers`. Its zero value carries unknown members.

- **Breaking (Go API):** a `LegResponder` on the payer's CRD, DTR, PAS and inquiry
  legs is handed the payer's own binding of the member the request names as its
  subject, not the leg token's subject.

#### The supplemental report on an amendment the gateway builds

**Behavior change in v0.57.0.**

- No configuration changes. Go API: `relay.EditEvidenceSubjectRekey` (`E-06`)
  is added; nothing is removed.

#### Amendments a provider's gateway builds, after a payer's 409

**Behavior change in v0.56.0.**

- No configuration changes. The Go API adds `engine.LegResentEvent`, the new
  event's kind. An observer consumer that switches on `Kind` sees one new kind.

#### A payer's DTR and PAS answers on the legs a provider originates

**Behavior change in v0.55.0.**

- **Go API (additive):** `engine.ReferencePayerIdentities` (the identities above;
  it returns a copy), `engine.ConformanceFinding.DeclaredLine`,
  `engine.ConformanceFinding.Lines` and `engine.LineVerdictSummary`.
  `engine.ConformanceFinding.Verdict` can now be `valid`, on a payer-answer
  finding that is not a defect; the signatures are unchanged.

#### Identifiers for members the system of record does not hold

**Behavior change in v0.55.0.**

- **Go API (additive):** `pgstore.OpenPends` and `pgstore.OpenPend`, which
  list a payer's undecided pended authorizations for an operator. It only
  reads, and never creates or alters the store's schema.

#### Patients an exchange involves

**New in v0.55.0 (additive).**

- **Go API (additive):** `engine.Config.HubAcceptsInvolved`,
  `engine.Config.InvolvedBudget` (zero selects 2 seconds) and
  `engine.Config.InvolvedMetric`; `engine.InvolvedOmittedEvent`.

#### Enrichment of native requests

**Behavior change in v0.54.0: a Da Vinci-native request is carried as sent unless
the participant opts in to enrichment.**

- **Go API (additive):** `engine.Config.EnrichNativeRequests`; its zero value adds
  nothing.

#### Payer eligibility endpoint

Added in v0.54.0.

- **Go API (additive):** `engine.WithEligibilityURL`, a `NativeOption`.

#### Observational source certification

Call `Gateway.Close()` after stopping service and before releasing dependencies.
It cancels observation HTTP work, closes idle connections and joins the worker.
It waits for cooperative callbacks; it cannot forcibly cancel a blocked callback.
The app runner and managed app handlers own this cleanup. Tests may use
`FlushCertificationForTest` as a context-bounded completion barrier, and
`DisableCertificationForTest` for evidence-off comparison runs.

#### Evolving surfaces

These surfaces evolve: fields, event kinds, check names and closed-set values may
be added in minor releases, so decode them tolerantly. The wire surfaces among
them (the observer event feed, `GET /health` and the `/internal/checks` results)
are part of the binary contract on those terms. The Go hooks and packages named
with them (`engine.Config.Observer`, `observer.Hub`, `scenarioauth`,
`scenariodriver`, `fhirseed`, `LegMetric` and `ExchangeObserved` among them) are Go
APIs, and SHN-internal (above). The `/demo/*` endpoints on the observer listener
are test tooling, outside the promise.

New exported engine surface backing these v0.35.0 additions (described in
STABILITY.md, "The observer stream"), each its own release-notes bullet:

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
  releases; decode tolerantly. See STABILITY.md, "One record per exchange". Nil means no record; a
  panic in it is recovered and never reaches the answer (`TestExchangeRecord_Panics`).

#### Optional diagnostic events (evolving)

`engine.Config.Diagnostic` (an `engine.DiagnosticSink`, which is
`func(diagnostics.Event) bool`) is a separate opt-in sink
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
