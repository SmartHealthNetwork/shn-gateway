# Integration guide

Every gateway role requires a real system of record — `FHIR_DATA_URL` is required at
boot; there is no built-in in-process persona stub to fall back on for a no-backend
first run any more. To carry **your** data, point the gateway at your systems through
connectors. This applies to both sides: a provider reads the clinical and coverage data
it originates from; a payer's Da Vinci legs forward to the payer's own system by default
(see [Payer decisioning](#payer-decisioning) — there is no built-in decision policy, and
in-process decisioning is an advanced Go-integration path, not a config-only one).

For the field-by-field reference of every environment variable mentioned
below, see [CONFIGURATION.md](CONFIGURATION.md).

- [The common case — point at your FHIR server (no code)](#the-common-case--point-at-your-fhir-server-no-code)
- [Authenticating to your backend (SMART Backend Services)](#authenticating-to-your-backend-smart-backend-services)
- [Provider-data origination](#provider-data-origination)
- [Native Da Vinci ingress](#native-da-vinci-ingress)
  - [Calling the ingress from your EHR](#calling-the-ingress-from-your-ehr)
- [A non-FHIR backend (custom connector)](#a-non-fhir-backend-custom-connector)
- [Payer decisioning](#payer-decisioning)
- [Native-forward payer mode](#native-forward-payer-mode)
- [Provider DTR population](#provider-dtr-population)
- [Durable claim state](#durable-claim-state)
- [Seed your own FHIR server](#seed-your-own-fhir-server)

---

## The common case — point at your FHIR server (no code)

If your backend exposes **FHIR R4** (Epic, and increasingly Availity /
Surescripts — the CMS-0057 direction), you write **no code**. Set `FHIR_DATA_URL`
to your US Core FHIR base URL and, if it requires authenticated access, the SMART
Backend Services credential block:

```sh
docker run --rm \
  -e SHN_DISCOVERY_URL=https://accounts.shn-preview.org/discovery \
  -e ROLE=provider \
  -e FHIR_VALIDATE_URL=https://your-hapi.example.com/fhir \
  -e FHIR_DATA_URL=https://fhir.your-org.example.com/r4 \
  -e FHIR_TOKEN_URL=https://fhir.your-org.example.com/oauth2/token \
  -e FHIR_CLIENT_ID=shn-gateway \
  -e FHIR_CLIENT_KEY=/etc/shn/client-key.pem \
  -e FHIR_CLIENT_ALG=ES384 \
  -e SHN_SECRETS=/etc/shn/bundle \
  -v "$PWD/provider-bundle:/etc/shn/bundle:ro" \
  -v "$PWD/client-key.pem:/etc/shn/client-key.pem:ro" \
  -p 8080:8080 \
  shn-gateway
```

`FHIR_CLIENT_KEY` is a **path to a mounted PEM file**, not the key text. See
[Authenticating to your backend](#authenticating-to-your-backend-smart-backend-services)
for creating that key pair and registering its public key with your server.

Trust anchors, the Hub/authz/registrar, and the consent/audit/PHG planes are all
resolved from `SHN_DISCOVERY_URL` — nothing else to wire. Payer routing resolves
off your own patients' Coverage by default (`FeedPayerRouter`, see
[CONFIGURATION.md](CONFIGURATION.md#per-role)); set `PAYER_DIRECTORY` only if you
need the static override.

## Authenticating to your backend (SMART Backend Services)

Wherever the gateway connects **out to a server you run** — your FHIR system of
record (`FHIR_CLIENT_*`, above), a Da Vinci payer endpoint in
[native-forward mode](#native-forward-payer-mode) (`PAYER_DAVINCI_*`) or the SDC
`$populate` engine behind `PROVIDER_DTR_POPULATE_URL` (`PROVIDER_DTR_POPULATE_*`) — it
authenticates the same way, as an OAuth2 `client_credentials` client — by default a
**SMART Backend Services** signed JWT assertion (`private_key_jwt`).

The gateway supports two client-auth modes at this edge, exactly one per
credential block. **`private_key_jwt` (asymmetric, preferred):** the gateway signs
each token request with a private key (ES384 or RS384) — register the public key
on that client at your authorization server and point the gateway at your private
key (below); the private key never leaves your environment. **`client_secret_post`
(shared secret):** for authorization servers that can issue only a `client_id` +
`client_secret`, set `*_CLIENT_SECRET` instead of the key/alg pair. The secret's
value goes in the env var directly — it is **not a path**, unlike `*_CLIENT_KEY`.
Prefer `private_key_jwt` whenever your server supports asymmetric registration.

**The client identity is yours, not the network's.** The gateway is just a
client of *your* authorization server, exactly like any other backend
integration. Nothing is issued by the network operator, and the gateway does not
host a JWKS endpoint: your **public** key is registered at your authorization
server, and the **private** key never leaves your environment. Three steps:

1. **Generate an asymmetric key pair** — EC P-384 (for `ES384`) or RSA (for
   `RS384`). For example, with `openssl`:

   ```sh
   # EC P-384 private key — use with ALG=ES384
   openssl ecparam -name secp384r1 -genkey -noout -out client-key.pem
   # the matching public key, to register with your authorization server
   openssl ec -in client-key.pem -pubout -out client-pub.pem
   ```

2. **Register the public key** (`client-pub.pem`) with your authorization server
   as this client's key, and note the **client id** it is registered under. Most
   servers take the public key as a JWK / JWK Set — convert the PEM if yours
   does.

3. **Point the gateway at the private key and that client id.** The
   `PAYER_DAVINCI_*` names are shown here; the `FHIR_CLIENT_*` set is identical:

   ```sh
   -e PAYER_DAVINCI_TOKEN_URL=https://auth.your-backend.example/oauth/token \
   -e PAYER_DAVINCI_CLIENT_ID=<the client id from step 2> \
   -e PAYER_DAVINCI_CLIENT_KEY=/etc/shn/client-key.pem \
   -e PAYER_DAVINCI_CLIENT_ALG=ES384 \
   -v "$PWD/client-key.pem:/etc/shn/client-key.pem:ro"
   ```

Worth checking:

- **`*_CLIENT_KEY` is a file path, not the key text.** Mount the PEM file into
  the container and give its path; an inline PEM string is not read as a key.
- **`aud`.** The gateway sets the assertion's `aud` to your `*_TOKEN_URL`, so
  that must be the exact audience your authorization server expects.
- **Scope.** The gateway requests `*_SCOPE` (default `system/*.read`). Make sure
  it is a scope your server actually grants this client, in the scope syntax it
  expects. `system/*.read` covers the read-only legs (coverage eligibility, CRD,
  DTR); if you turn on PAS native forwarding (`PAYER_DAVINCI_PAS_NATIVE=true`),
  widen it to include the write a claim submission needs.
- **Optional `*_CLIENT_KID`.** Set it only if your server pins a specific key id
  in the assertion header.

## Provider-data origination

If your system of record does not yet speak native Da Vinci CRD/DTR/PAS, set
`ORIGINATION_PROFILE=provider-data`: the gateway itself reads each exchange's
order and clinical data from your FHIR system of record (`FHIR_DATA_URL`) and
originates the full conformant CRD/DTR/PAS exchange on your behalf — there is no
Da Vinci client for you to build. This is the broadest on-ramp: it works before
your systems are Da Vinci-conformant at all, using only FHIR R4 read access to
data you already have.

`ORIGINATION_PROFILE=provider-data` requires `PROVIDER_DTR_POPULATE_URL` — a real
SDC `Questionnaire/$populate` engine. The gateway resolves the payer's DTR
questionnaire by running its prepopulation CQL against your data, rather than by
asking you to answer the questionnaire yourself; that is what makes the
originated request genuinely conformant rather than a canned shape. See
[Provider DTR population](#provider-dtr-population) below for how to point this
at an engine you operate.

## Native Da Vinci ingress

If your EHR or reference implementation is **already** Da Vinci-conformant —
speaking CDS Hooks `order-sign`, `order-select` and `order-dispatch` (CRD),
`Questionnaire/$questionnaire-package` (DTR), and `Claim/$submit` (PAS) natively —
point it at the gateway's own
ingress instead of `provider-data` origination. Your systems call the gateway
directly, inside your own boundary; the gateway forwards your EHR's own request
bytes through to the Hub. It removes `fhirServer` and `fhirAuthorization` (the
payer never gets a route or a credential into your systems, and the gateway
never calls `fhirServer`), and by default adds nothing: send every prefetch value
you want the payer to see. With `ENRICH_NATIVE_REQUESTS=true` it adds the prefetch
values the request leaves out, read from your own system of record — never made up.
See [CDS Hooks prefetch](CONFIGURATION.md#cds-hooks-prefetch) for the rules. The
gateway's `GET /cds-services` lists one service per hook (`shn-order-sign`,
`shn-order-select`, `shn-order-dispatch`); post each CDS Hooks request to the service
for its hook. The payer's answer comes back exactly as the payer sent it.
A `$questionnaire-package` request is carried as your EHR sent it; with
`ENRICH_NATIVE_REQUESTS=true`, your Coverage and Patient are appended when it carries
none. A Coverage a request leaves out is read from your system of record either way,
to choose the payer. A signature inside a message travels untouched;
HTTP-level signatures are not carried, so sign inside the payload when you need an
end-to-end signature. To have values added, the request's patient must be named by the
network member id; see the member id limitation in
[CDS Hooks prefetch](CONFIGURATION.md#cds-hooks-prefetch).

See [`PROVIDER_DAVINCI_INGRESS` and related variables in
CONFIGURATION.md](CONFIGURATION.md#accept-da-vinci-requests-from-a-provider-ehr-provider-optional)
for the full field reference, the SMART Backend Services inbound authentication
model, and the private-integration rule (this ingress is never a public-internet
surface — only the gateway↔Hub leg is).

### Calling the ingress from your EHR

The ingress is the one place where a system **you** run authenticates *to the
gateway* — everywhere else in this guide, the gateway authenticates to you. Two
things are easy to conflate here:

- The **secrets bundle** (`SHN_SECRETS`: `manifest.json`, `sign.key`, `enc.key`)
  is the gateway's own identity toward the SHN network. The gateway uses it to
  authenticate *itself* to the Hub. It is never turned into a bearer token, and
  your EHR or integration engine never touches it.
- **Inbound** requests authenticate against a small **SMART Backend Services**
  authorization server built into the gateway. It issues its own bearers at
  `POST /oauth/token`, and the only clients it trusts are the ones you list in
  `INGRESS_CLIENTS_FILE`. It does not accept tokens from any other issuer, and
  nothing for this side is issued by SHN.

Four steps, all on your side:

1. **Generate a key pair for the calling system** — EC P-384 (`ES384`) or RSA
   (`RS384`), exactly as in
   [Authenticating to your backend](#authenticating-to-your-backend-smart-backend-services)
   step 1. The private key stays with the caller.

2. **Register its public key with the gateway.** Add an entry to the JSON array
   in `INGRESS_CLIENTS_FILE` (the `client_id` is any string you choose) and
   restart the gateway:

   ```json
   [
     {
       "client_id": "my-ehr",
       "alg": "ES384",
       "public_key_pem": "-----BEGIN PUBLIC KEY-----\n...\n-----END PUBLIC KEY-----\n",
       "scopes": ["system/Davinci.write"]
     }
   ]
   ```

   `GET {PROVIDER_DAVINCI_INGRESS_BASE_URL}/.well-known/smart-configuration`
   confirms the ingress is up and reports the exact `token_endpoint` to use.

3. **Exchange a signed assertion for a bearer.** Sign a JWT with the private key
   from step 1 — standard SMART Backend Services / RFC 7523 client
   authentication — and POST it form-encoded to the token endpoint:

   | Claim | What the gateway requires |
   |---|---|
   | header `alg` | the `alg` you registered (`ES384` or `RS384`); anything else is rejected |
   | `iss`, `sub` | both your `client_id` |
   | `aud` | exactly the `token_endpoint` from `smart-configuration`, i.e. `{PROVIDER_DAVINCI_INGRESS_BASE_URL}/oauth/token` — pinned from config, not taken from the request's `Host` |
   | `exp` | required, and at most 5 minutes in the future |
   | `jti` | required and unique — each assertion is accepted once |

   ```sh
   BASE=https://gateway.internal.example   # your PROVIDER_DAVINCI_INGRESS_BASE_URL
   curl -s "$BASE/oauth/token" \
     -d grant_type=client_credentials \
     -d client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer \
     -d client_assertion="$ASSERTION" \
     -d scope=system/Davinci.write
   # → {"access_token":"eyJ…","token_type":"bearer","expires_in":300,"scope":"system/Davinci.write"}
   ```

   `scope` is optional; if you send one it must be among the client's registered
   `scopes`. Bearers live **5 minutes**. Without `SHN_STORE_DATABASE_URL` they are
   signed with a key the gateway generates at startup, so a restart invalidates
   outstanding bearers — fetch one per run rather than caching across sessions; with
   the store DSN set the signing key is shared and outlives a restart.

   A `503` from the token endpoint means the gateway could not complete the issuance —
   not that your credential was refused. It never returns a token, so **retry with a
   newly minted `client_assertion`**. Mint one per request (the `private_key_jwt` norm)
   and a `503` costs you nothing. Re-sending the same assertion may be refused `401` as
   replayed: the gateway cannot always tell whether the one-time-use record for that `jti`
   was written before the failure, so it will not promise you that one back. An
   `invalid_scope` `400` is different — it is decided before the `jti` is recorded, so fix
   the `scope` and retry with the assertion you already hold.

4. **Call the ingress with the bearer:**

   ```sh
   curl -s "$BASE/Claim/\$submit" \
     -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' \
     --data-binary @pas-bundle.json
   ```

   The header is exactly `Authorization: Bearer <token>` (canonical casing, one
   space). A missing or rejected bearer returns `401` with the body
   FHIR `OperationOutcome`, whose issue diagnostic is
   `ingress authentication required`.

   `$submit` and `$questionnaire-package` return `application/fhir+json` for
   successful FHIR resources and locally generated errors. Local errors carry an
   `OperationOutcome` with `issue[].severity`, `code`, and `diagnostics`; status
   codes retain their authentication, subject, routing, and availability meanings.
   A framed upstream refusal keeps the upstream status, body, and media type.
   CDS Hooks routes retain JSON responses, and the token endpoint retains OAuth
   error responses.

**Alternative — present the signed JWT directly.** The ingress also accepts a
registered client's self-signed JWT *as* the bearer, with no token call (the
UDAP B2B form). Same key, same registration; the claims differ slightly:

| Claim | What the gateway requires |
|---|---|
| header `alg` | the `alg` you registered (`ES384` or `RS384`) |
| `iss` | your `client_id` |
| `sub` | optional; if present, must equal `iss` |
| `aud` | the endpoint you are calling (e.g. `{BASE}/Claim/$submit`) or `{BASE}` itself — any URL at or under the configured base |
| `exp` | required, and at most 5 minutes in the future |
| `jti` | required, but not single-use — the JWT is reusable until it expires |

Token-endpoint errors are deliberately generic (`invalid_client`,
`invalid_scope`, `unsupported_grant_type`, `invalid_request`) and never echo key
or signature detail — check the assertion against the tables above rather than
the response body.

## A non-FHIR backend (custom connector)

For a legacy or non-FHIR system of record (HL7v2, X12, SQL, SOAP), implement the
`engine.SystemOfRecord` interface starting from the runnable scaffold. See
[`connectors/scaffold/README.md`](../connectors/scaffold/README.md) for the
step-by-step: copy `scaffold.go`, fill the read methods against your backend
(obtaining the network's opaque patient identifier from `shnsdk.ResolvePCI`), and wire your
connector through the already-public `engine.Config.SoR` seam.

## System-of-record read failures

The built-in FHIR connector distinguishes absent records from failed reads. A valid
empty Patient search keeps the existing unknown-member result. A failed token request,
unreachable server or invalid FHIR response is a backend failure, not evidence that the
patient is missing.

| Backend result | Local HTTP status | Safe message |
|---|---|---|
| Authentication or credential refusal | 502 | `system of record authentication failed` |
| Transport failure, timeout, rate limit or upstream server outage | 503 | `system of record unavailable` |
| Malformed or unusable required response data | 502 | `system of record returned an invalid response` |

FHIR operation endpoints carry these errors in an `OperationOutcome`; ordinary JSON
and CDS Hooks endpoints keep their existing response families. A recipient failure
relayed through the Hub keeps the Hub's generic failure response. Raw backend responses,
URLs, member identifiers and token details are not exposed in these new error messages.
Check backend credentials and service health before changing a patient's identifier.
A 503 does not promise an automatic retry or make repeating a clinical write safe.

Custom connectors can additionally implement `engine.ContextSystemOfRecord`. Its nine
`Context`-suffixed read methods accept the request context and return an error as their
last result. Return a nil error for actual absence and an `engine.SoRReadError` with
`SoRAuthenticationFailed`, `SoRUnavailable` or `SoRInvalidResponse` for a failed read.
Do not include protected backend details in errors. The engine selects this optional
interface automatically, including through its observer wrapper, and stops before
using missing-data defaults or starting further exchange work when a read fails.

`OpenCoverageContext` returns every Coverage record the member has, exactly as your
system returned it (an empty result means none). The gateway decides what several
records mean: when they do not all name one payer, the exchange is refused with
`422 ambiguous coverage for routing` rather than routed on whichever record came
first. The single-record `OpenCoverage` of the built-in FHIR connector answers only when
exactly one record exists. The built-in FHIR connector reads the Coverages with its bounded
patient search (below): every page, within the search bounds; a search over the bounds is a
failed read (`SoRInvalidResponse`), never a partial answer.

This is a change from earlier releases, where `OpenCoverageContext` returned
`([]byte, bool, error)`. A connector that still has the earlier method no longer satisfies
`engine.ContextSystemOfRecord`, so `engine.New` refuses it with
`engine.ErrSystemOfRecordSignature` rather than silently treating its read failures as
absence. To migrate, return every matching Coverage as `[][]byte` (nil or empty for none)
and the error as before.

A connector can also implement `engine.SearchSystemOfRecord` to let the gateway search
your FHIR server for one patient's records (`<type>?patient=Patient/<id>`, exactly as a
CDS Hooks prefetch template asks). The built-in FHIR connector does: it refuses
redirects, follows `next` links only on the configured server and under its base path,
stops on a repeated page, and stops at fixed bounds (10 pages, 200 entries, 4 MiB,
5 seconds). It returns each page's bytes unchanged. The gateway sends the result as a
searchset it writes, however many pages there were: each matching and included record is a
byte-for-byte copy of your server's record, under an entry `fullUrl` the gateway assigns;
your server's links, entry addresses, search messages and `Bundle.total` are not used.

A provider gateway uses the same search to find the coverage an EHR's request leaves
out, to choose the payer, and, with `ENRICH_NATIVE_REQUESTS=true`, to obtain the CDS
Hooks prefetch values (`coverage` and the history keys) it adds; a connector without it
cannot obtain them (see [CDS Hooks prefetch](CONFIGURATION.md#cds-hooks-prefetch)).

A facility gateway answers a clinical data request (CDex) from the same search:
- **Records.** Every record of each requested type whose own date falls within the
  requested dates, exactly as your server returned it (`DiagnosticReport` by
  `effectiveDateTime`, else the end, else the start of `effectivePeriod`;
  `DocumentReference` by `date`). The search is narrowed with the type's `date` search
  parameter (`date=ge<start>&date=le<end>`, each widened by one day so a server comparing
  instants never drops an edge-day record); the gateway still selects each record by its
  own date. Both record types a request may name have that parameter.
- **Bounds.** Each type's search is held to the bounds above (10 pages, 200 entries,
  4 MiB, 5 seconds). More records than that is answered `422 records exceed the per-answer
  bound for <type>`, never a partial answer; a search that runs out of time is `503`.
- **Identity binding.** The gateway reads the member's `Patient` (`PatientFHIRRef`, then
  `ResolveByReference`) to confirm it exists and is that member, but does not send it. It
  sends only a `Patient` it writes itself, with your server's Patient id and the
  `urn:shn:member` identifier, so the requester can resolve your records' patient
  reference. No `Patient` for the member is `404`; a `Patient` your server does not return
  is `502`.
- **Checks.** Each record must name that patient; a record about anyone else, or the same
  record returned twice, stops the answer with `502`.
- **Connectors without search.** A connector without `SearchSystemOfRecord` is read through
  `FacilityRecordsContext` (one record per type, sent as returned).

Return records as your system holds them; do not rewrite their `subject`.

The original `engine.SystemOfRecord` interface is unchanged. Existing implementations
remain source compatible; the adapter cannot reconstruct an error that a legacy
connector discarded, or cancel a legacy call already in progress. Implement the optional
interface to obtain accurate failure classification and request cancellation. Valid
absence of optional clinical facts retains its prior behavior; a failed subsidiary read
does not produce a partial successful clinical context.

## Payer decisioning

A payer gateway answers Da Vinci legs (CRD, DTR, PAS) out of a **content occupant** —
there is no built-in/default decision policy any more, and a `role=payer` gateway with no
occupant configured **refuses to boot**. The published binary's occupant is
**native-forward** (`PAYER_DAVINCI_BASE_URL`, below): every Da Vinci leg forwards to your
own real payer endpoint. There is no config-only way to plug in custom in-process
decisioning. Coverage eligibility is answered separately: by default the engine itself
reads the member's Coverage record, not any occupant; a payer whose system has its own
eligibility endpoint declares it with `PAYER_ELIGIBILITY_URL`, and the request is then
carried there and your answer relayed.

If you want the engine to answer PA legs from your own decision logic instead of
forwarding to a separate Da Vinci endpoint, build a custom binary against the gateway
module and implement `engine.LegResponder` yourself
(`Handle(ctx, leg, corrID, subjectPCI, requestFHIR) (LegResult, error)`), then set it on
`Config.Responder` — this is an in-process Go integration, not something the published
binary's environment variables expose. `Config.Adjudicator` (`shnsdk.Adjudicator` — the
same interface the standalone SDK `shnsdk.Responder` uses) is declared for source
compatibility but is **no longer read by anything**: setting it alone does nothing;
wrap it in your own `LegResponder` implementation if you want the engine to call it. See
[`STABILITY.md`](../STABILITY.md) for the supported `engine` seams.

> **Note:** The `engine.LegResponder` interface is an **internal, unstable 0.x
> seam** — it may change in any minor version. Do not depend on it directly.

## Native-forward payer mode

`PAYER_DAVINCI_BASE_URL` is **required** for `role=payer` — with it unset, boot fails
closed (there is no in-process occupant to fall back to). With it set, the Da Vinci payer
legs (CRD, DTR, PAS submit, PAS update and inquiry) forward to your real partner Da Vinci
endpoint over a SMART-authenticated client. Coverage eligibility forwards the same way only
when you set `PAYER_ELIGIBILITY_URL`; otherwise the gateway answers it from your Coverage
records. `PAYER_DAVINCI_PAS_NATIVE`
still parses (back-compat) but is a no-op: PAS forwarding was never independently
optional-off, since the in-process fallback it used to gate is deleted; setting it
`false` only prints a warning that PAS forwards regardless.

See [CONFIGURATION.md](CONFIGURATION.md#native-forward-payer-mode-payer_davinci_)
for the full field reference, including the exactly-one-mode credential rule, and
[Authenticating to your backend](#authenticating-to-your-backend-smart-backend-services)
to set up the `PAYER_DAVINCI_CLIENT_*` credentials.

The engine continues to own authority enforcement regardless of native-forward
mode: every forwarded leg is still independently authorized, sealed, and audited.
A native-forwarded PAS or DTR answer, and a coverage-eligibility answer when you
declare `PAYER_ELIGIBILITY_URL`, is checked for the patient it names. If your system
returns an answer about a different patient than the request, the engine rejects it
before sealing at enforcement `strict` (a 403, not a sealed foreign-patient leg);
below `strict` it is relayed as sent, and recorded as a finding at `observe` and
`structural`.

## Provider DTR population

On the **provider** side, the DTR leg fills the payer's questionnaire from the
member's clinical data. By default the gateway uses a **managed** populator that
fills a built-in questionnaire from your `FHIR_DATA_URL` system of record (there is
no stub fallback — `FHIR_DATA_URL` is required for every role). To populate
**arbitrary** DTR questionnaires — the real Da
Vinci DTR case, where questionnaires carry CQL expressions the gateway does not
itself evaluate — forward population to an SDC `Questionnaire/$populate` engine
(see [CONFIGURATION.md](CONFIGURATION.md#provider-dtr-population-provider_dtr_)
for the variables that control this). The connector authenticates to that engine
with its own SMART Backend Services credential block (`PROVIDER_DTR_POPULATE_*`),
the same `private_key_jwt` / `client_secret_post` shape as the SoR and payer
connectors, and fails the leg closed if the token endpoint refuses.

A **CMS-0057-conformant** provider runs its own DTR client and points
`PROVIDER_DTR_POPULATE_URL` at it. A provider without a DTR client yet can point it
at a `$populate` CQL engine you operate (for example a HAPI FHIR Clinical Reasoning
server) — the same SDC contract, populated centrally (DTR-as-a-service). Either way
the engine keeps authority: the populated `QuestionnaireResponse` is fenced to the
member it was populated for — a response about a different patient is rejected before
it can reach PAS — then sealed and audited like any other leg.

## Durable claim state

Set `SHN_STORE_DATABASE_URL` to a Postgres DSN to persist in-flight
(pended/resumable) claim state across restarts and replicas, instead of the
default in-memory store.

The same database also shares the ingress signing key and one-time-use records
across replicas (see [DEPLOYMENT.md](DEPLOYMENT.md), "Running more than one replica").

## Conformance enforcement

Your gateway can check the FHIR resources it sends or receives with its `$validate`
endpoint — and, for a payer's CDS Hooks answer, against the CDS Hooks response rules —
along with the message's own consistency (for example, one patient throughout a
request). What the FHIR check covers, per leg:

- A resource is checked against the profiles it declares in its own `meta.profile`,
  and otherwise against its base FHIR R4 definition only.
- A few resources your gateway builds are checked against a named profile: the
  DTR QuestionnaireResponse it sends (the DTR QuestionnaireResponse profile of
  the leg's IG line), a QuestionnaireResponse it populates (base R4
  QuestionnaireResponse), and a PAS inquiry it builds (the PAS inquiry
  request-bundle profile of the line).
- Of a CDS Hooks request, the draft order is validated, and on `order-select` and
  `order-sign` the Coverage too.
- A PAS request carried from your own system (the `$submit` ingress) is not
  `$validate`d by either gateway; only the content checks and the network rules
  apply to it. A PAS submit or update the gateway builds from your records is
  validated: the Bundle for base R4 shape, and its QuestionnaireResponse
  attachments against the DTR profile.
- A payer's DTR questionnaire package is not validated.
- Of coverage eligibility, both gateways validate the request and the answer: the
  answer the payer's gateway builds from its records or, when the payer declares its
  own endpoint (`PAYER_ELIGIBILITY_URL`), the payer system's answer.

A level applies only to what these checks cover. Whether they run, and what a
defect does, is `CONFORMANCE_ENFORCEMENT`, a setting on your own gateway:

- `none`: no payload conformance check runs and no finding is recorded. The message
  relays as sent, apart from the gateway's registered edits (the callback removed,
  prefetch and coverage obtained when the provider opts in with
  `ENRICH_NATIVE_REQUESTS=true`, and payer identity mapping; participant protocol
  §7a.4).
- `observe` (the default when the variable is unset): every check runs and each
  defect is recorded as a finding — it does not stop the message, which is carried
  as sent, apart from the gateway's registered edits. A validator that cannot be
  reached is recorded the same way.
- `structural` (v0.54.0 and later): every check runs; a message whose structure is
  broken is refused as at `strict`, and every other defect is recorded as at
  `observe`. Broken structure is a missing required element, an element the
  resource does not define, a value of the wrong JSON type or one that cannot be
  read, a CDS Hooks answer that cannot be read or lacks a required member, and a
  request or answer the gateway cannot read. A FHIR issue is read by the
  validator's message id: only invariants and a code outside its code list (a code
  the bound value set or code system does not contain, or a code system the
  validator cannot check, licensed ones included), as the validator's recognized
  code-list issues report it, are recorded; every other FHIR profile issue
  (cardinality, fixed and pattern values, slicing, extensions, lengths, any other
  terminology issue, or one the gateway cannot classify) and every fatal issue
  refuses. CDS Hooks summary length,
  topic and selection behavior, another patient in one message, and the content
  business rules are recorded. A validator that cannot be reached is recorded.
- `strict`: a defect refuses the message. A FHIR profile refusal names the
  validator issues it was based on; a CDS Hooks response-rules refusal names the
  rule (and the violating path) as well.

Any other value refuses to boot.

Some checks are not about a payload's conformance and refuse at every level:
authentication, authority (including a token presented with a request other than
the one it was issued for), consent, the patient binding (each gateway identifies
the member the request names by its own system), routing, replay, a repeated
member name in any body, the contract line stamped on an answer's frame, and the
check of a payload this gateway itself translated between IG lines.

**Reading a finding.** At `observe`, `structural` and `strict`, if you run the gateway yourself — through the SHN Kit or a
self-hosted deployment — findings appear in your own gateway log and observer
stream: look for the `conformance:` log line, or the `conformance.observed` event
if you're watching the observer stream. If SHN hosts your gateway, ask your SHN
contact for a finding until partner login ships a self-serve view.

## Seed your own FHIR server

To exercise the gateway against your own FHIR server, seed it with the same
synthetic personas the SHN reference payer recognizes. Two bundles are shipped
as ready-to-POST FHIR transaction Bundles:

- **`seed/provider-personas.json`** — plain-EHR (provider-data) personas:
  self-contained clinical records (Patient, Coverage, Condition, DeviceRequest,
  Observations) for the plain-EHR flows.
- **`seed/conformant-personas.json`** — the conformant-lane Patient roster
  (members `MBR-COVERED`, `MBR-NOTCOVERED`, `MBR-UC06`, `MBR-UC07HCPCS`, `MBR-UC08`).

Load either with a single transaction POST to your FHIR base (run from the repo root):

    curl -X POST -H "Content-Type: application/fhir+json" \
      --data-binary @seed/provider-personas.json \
      https://your-fhir-server/fhir

The reference payer's system of record holds **only** the member ids in these bundles. A
request for any other member is carried by default and bound by the member id and the
Patient it carries (see CONFIGURATION, "Members your system of record does not hold"); the
reference payer answers for that member from its own records, which hold nothing for it.

### Keep the provider-data Observations recent

The provider-data bundle's Observations carry fixed dates. One flow (home-oxygen
DTR pre-population) reads clinical Observations from the last three months, so a
long-committed file can age out. Refresh the dates to "now" before loading:

    jq --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      '(.entry[].resource | select(.resourceType=="Observation")).effectiveDateTime = $now' \
      seed/provider-personas.json > provider-personas.fresh.json

Then POST `provider-personas.fresh.json`. The conformant bundle is Patient-only
(no dated resources) and needs no refresh.

> DTR questionnaire pre-population runs on SHN's operated CQL engine, so your
> server needs only this data. Running native DTR `$populate` on your own server
> would additionally require the CQL libraries — a later, advanced setup.
