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
record (`FHIR_CLIENT_*`, above) or a Da Vinci payer endpoint in
[native-forward mode](#native-forward-payer-mode) (`PAYER_DAVINCI_*`) — it
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
speaking CDS Hooks order-select (CRD), `Questionnaire/$questionnaire-package`
(DTR), and `Claim/$submit` (PAS) natively — point it at the gateway's own
ingress instead of `provider-data` origination. Your systems call the gateway
directly, inside your own boundary; the gateway resolves and inlines the
payer's prefetch from your system of record (non-aggregating; no callback to
your systems) and forwards the conformant request through to the Hub.

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
   `scopes`. Bearers live **5 minutes** and are signed with a key the gateway
   generates at startup, so a restart invalidates outstanding bearers — fetch one
   per run rather than caching across sessions.

4. **Call the ingress with the bearer:**

   ```sh
   curl -s "$BASE/Claim/\$submit" \
     -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' \
     --data-binary @pas-bundle.json
   ```

   The header is exactly `Authorization: Bearer <token>` (canonical casing, one
   space). A missing or rejected bearer returns `401` with the body
   `{"error":"ingress authentication required"}`. If you see a different 401
   body, something in front of the gateway is answering, not the gateway.

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
(deriving the patient identifier via `shnsdk.ResolvePCI`), and wire your
connector through the already-public `engine.Config.SoR` seam.

## Payer decisioning

A payer gateway answers Da Vinci legs (CRD, DTR, PAS) out of a **content occupant** —
there is no built-in/default decision policy any more, and a `role=payer` gateway with no
occupant configured **refuses to boot**. The published binary's occupant is
**native-forward** (`PAYER_DAVINCI_BASE_URL`, below): every Da Vinci leg forwards to your
own real payer endpoint. There is no config-only way to plug in custom in-process
decisioning — coverage eligibility is answered separately, by the engine itself reading
the member's Coverage record, not by any occupant.

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
closed (there is no in-process occupant to fall back to). With it set, **all five** Da
Vinci payer legs (eligibility, CRD, DTR, PAS submit, PAS update) forward to your real
partner Da Vinci endpoint over a SMART-authenticated client. `PAYER_DAVINCI_PAS_NATIVE`
still parses (back-compat) but is a no-op: PAS forwarding was never independently
optional-off, since the in-process fallback it used to gate is deleted; setting it
`false` only prints a warning that PAS forwards regardless.

See [CONFIGURATION.md](CONFIGURATION.md#native-forward-payer-mode-payer_davinci_)
for the full field reference, including the exactly-one-mode credential rule, and
[Authenticating to your backend](#authenticating-to-your-backend-smart-backend-services)
to set up the `PAYER_DAVINCI_CLIENT_*` credentials.

The engine continues to own authority enforcement regardless of native-forward
mode: every forwarded leg is still independently authorized, sealed, and audited.
The outbound subject fence (`fenceResponseSubject`) applies to all
native-forwarded responses — including PAS submit/update when
`PAYER_DAVINCI_PAS_NATIVE=true`. If the partner returns a response about a
different patient than the request, the engine rejects it before sealing (a
403, not a sealed foreign-patient leg).

## Provider DTR population

On the **provider** side, the DTR leg fills the payer's questionnaire from the
member's clinical data. By default the gateway uses a **managed** populator that
fills a built-in questionnaire from your `FHIR_DATA_URL` system of record (there is
no stub fallback — `FHIR_DATA_URL` is required for every role). To populate
**arbitrary** DTR questionnaires — the real Da
Vinci DTR case, where questionnaires carry CQL expressions the gateway does not
itself evaluate — forward population to an SDC `Questionnaire/$populate` engine
(see [CONFIGURATION.md](CONFIGURATION.md#provider-dtr-population-provider_dtr_)
for the two variables that control this).

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

The reference payer recognizes **only** the member ids in these bundles; a request
for any other member is rejected by the gateway as an unknown member.

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
