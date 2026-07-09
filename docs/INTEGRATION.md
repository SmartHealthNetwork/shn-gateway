# Integration guide

By default the gateway uses a built-in **synthetic stub** for its system of
record — which is why a first run works with no backend at all. To carry
**your** data, point the gateway at your systems through connectors. This
applies to both sides: a provider reads the clinical and coverage data it
originates from; a payer reads the records its decisioning evaluates.

For the field-by-field reference of every environment variable mentioned
below, see [CONFIGURATION.md](CONFIGURATION.md).

- [The common case — point at your FHIR server (no code)](#the-common-case--point-at-your-fhir-server-no-code)
- [Provider-data origination](#provider-data-origination)
- [Native Da Vinci ingress](#native-da-vinci-ingress)
- [A non-FHIR backend (custom connector)](#a-non-fhir-backend-custom-connector)
- [Payer decisioning](#payer-decisioning)
- [Native-forward payer mode](#native-forward-payer-mode)
- [Provider DTR population](#provider-dtr-population)
- [Durable claim state](#durable-claim-state)

---

## The common case — point at your FHIR server (no code)

If your backend exposes **FHIR R4** (Epic, and increasingly Availity /
Surescripts — the CMS-0057 direction), you write **no code**. Set `FHIR_DATA_URL`
to your US Core FHIR base URL and, if it requires authenticated access, the SMART
Backend Services quad:

```sh
docker run --rm \
  -e SHN_DISCOVERY_URL=https://accounts.shn-preview.org/discovery \
  -e ROLE=provider \
  -e FHIR_VALIDATE_URL=https://your-hapi.example.com/fhir \
  -e FHIR_DATA_URL=https://fhir.your-org.example.com/r4 \
  -e FHIR_TOKEN_URL=https://fhir.your-org.example.com/oauth2/token \
  -e FHIR_CLIENT_ID=shn-gateway \
  -e FHIR_CLIENT_KEY="$(cat client-key.pem)" \
  -e FHIR_CLIENT_ALG=ES384 \
  -e SHN_SECRETS=/etc/shn/bundle \
  -v "$PWD/provider-bundle:/etc/shn/bundle:ro" \
  -p 8080:8080 \
  shn-gateway
```

Trust anchors, the Hub/authz/registrar, and the consent/audit/PHG planes are all
resolved from `SHN_DISCOVERY_URL` — nothing else to wire. Payer routing resolves
off your own patients' Coverage by default (`FeedPayerRouter`, see
[CONFIGURATION.md](CONFIGURATION.md#per-role)); set `PAYER_DIRECTORY` only if you
need the static override.

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

## A non-FHIR backend (custom connector)

For a legacy or non-FHIR system of record (HL7v2, X12, SQL, SOAP), implement the
`engine.SystemOfRecord` interface starting from the runnable scaffold. See
[`connectors/scaffold/README.md`](../connectors/scaffold/README.md) for the
step-by-step: copy `scaffold.go`, fill the read methods against your backend
(deriving the patient identifier via `shnsdk.ResolvePCI`), and wire your
connector through the already-public `engine.Config.SoR` seam.

## Payer decisioning

A payer gateway decides eligibility and prior authorization via an **Adjudicator**.
The default reads coverage and clinical facts from its system of record (the
stub, or your `FHIR_DATA_URL`) and applies a built-in decision policy. To run
**your own** coverage and medical-necessity policy, inject a custom
`shnsdk.Adjudicator` through the `engine.Config.Adjudicator` seam — the same
interface the SDK responder uses, so one implementation works in both. See
[`STABILITY.md`](../STABILITY.md) for the supported `engine` seams.

> **Note:** The `engine.LegResponder` interface is an **internal, unstable 0.x
> seam** — it may change in any minor version. Do not depend on it directly.
> `engine.Config.Adjudicator` is the **supported** partner injection point and
> will remain stable across minor versions.

## Native-forward payer mode

Setting `PAYER_DAVINCI_BASE_URL` switches the payer gateway into **native-forward
mode**: the three read-only Da Vinci legs (coverage eligibility, CRD order-select,
and DTR questionnaire fetch) are forwarded to a real partner Da Vinci endpoint
instead of being adjudicated by the built-in fallback. PAS (the claim submission
legs) also forward when `PAYER_DAVINCI_PAS_NATIVE=true` is set; without it, PAS
falls back to the built-in adjudicator. A gateway with `PAYER_DAVINCI_BASE_URL` set
and `PAYER_DAVINCI_PAS_NATIVE=true` is **fully native**: all five payer legs
forward to your partner. A payer Store (`SHN_STORE_DATABASE_URL` or holdersim) is
required when `PAYER_DAVINCI_PAS_NATIVE=true` — `build()` fails at startup otherwise.

See [CONFIGURATION.md](CONFIGURATION.md#native-forward-payer-mode-payer_davinci_)
for the full field reference, including the all-or-nothing credential rule.

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
fills a built-in questionnaire from the system of record (the stub, or your
`FHIR_DATA_URL`). To populate **arbitrary** DTR questionnaires — the real Da
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
