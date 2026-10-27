# Provider evaluation bundle

> **EVALUATION ONLY — NOT A PRODUCTION DEPLOYMENT.**
> This bundle exists so you can watch all eight prior-authorization use cases run
> end to end, today, without integrating any of your own systems first. It ships
> a pre-seeded FHIR server and a reference Da Vinci requester alongside the real
> Smart Gateway image. When you're ready to connect your own systems, see
> [Production cutover](#production-cutover) below — the gateway you're running
> here is the same gateway you run in production; only what's plugged into it
> changes.

## What this runs

Four containers, wired together with one shared cert volume:

| Service | Role |
|---|---|
| `hapi` | A FHIR server, pre-loaded with the US Core, SDC and Da Vinci CDex, HRex, CRD, DTR and PAS implementation guides, standing in for your provider system of record and validating what the gateway sends, its QuestionnaireResponses against DTR's profile included. It carries the 2.0 line's guides: a payer self-test against a payer on another line sees the gateway's QuestionnaireResponse profile reported as unknown (a finding at `observe`, a refusal at `strict`). Seeded on boot with realistic member and order data. |
| `gateway` | The real `shn-gateway` image, configured with `ROLE=provider`, pointed at the seeded `hapi` for both data and DTR questionnaire population. |
| `br-provider` | A reference Da Vinci requester application (HL7-DaVinci's `br-provider`, the pinned upstream commit's build, plus SHN's trust-store edit: four emSign roots removed) that originates prior-authorization requests through the gateway's Da Vinci ingress, so you can see the conformant lane work with a real client, not just synthetic calls. |
| `gencerts` | A one-shot init step that generates the certificate pair `br-provider` and the gateway's ingress use to authenticate each other. Generated fresh on first boot — no key material is ever committed to this repository. |

The gateway originates through the real Hub to a hosted evaluation payer
(`conformance-payer`), so every exchange is a genuine round trip, not a local
loopback: real authorization per leg, a real audit trail, real profile
validation.

> **Two ways personas get loaded.** The Docker eval stack seeds its bundled HAPI
> automatically (`evalseed`) — you don't load anything. To seed **your own**
> external FHIR server instead, download the seed bundles at the repo root
> (`seed/provider-personas.json` and `seed/conformant-personas.json`) and POST
> them (see
> [docs/INTEGRATION.md → Seed your own FHIR server](../../docs/INTEGRATION.md#seed-your-own-fhir-server)).

## Prerequisite: an SHN developer account

You need an **approved SHN developer account** before running this bundle.
Request one at `https://developers.shn-preview.org` (no account needed to
submit the request). Once approved, register a provider client and download
your bundle:

```sh
shn register --accounts https://accounts.shn-preview.org \
  --role provider --name my-org --base-url https://my-org.example.com \
  -out ./my-provider-bundle
```

Keep the resulting directory — its path is what you'll pass as `SHN_SECRETS`
below. Keys are generated client-side and never leave your machine.

## Run it — three steps

**Step 0 — create the caller that drives the scenarios** (once). The gateway runs
the eight use cases through its `/scenario` routes, and it admits a request there
only from a caller your bundle names, signed with that caller's own key. From
this directory:

```sh
shn scenario-key --bundle /abs/path/to/my-provider-bundle
```

This writes the caller's key to `./scenario-caller-send-test.sign`, readable by
you only, and adds the caller to `scenario-callers.json` in your bundle, which
the gateway reads at boot. The key is a credential: keep it out of the bundle
and out of version control. To replace it, run the command again with `--force`
and restart the gateway. A bundle without `scenario-callers.json` stops the
gateway at boot, naming the file.

**Step 1 — build the reference requester.** This clones and builds
`br-provider` from pinned upstream source; there's no published image to pull.
The image `br-provider:43a4806` is the pinned upstream commit's build, plus SHN's
trust-store edit (four emSign roots removed): one layer, its JVM trust store
without the four eMudhra emSign root certificates of CVE-2026-114820, checked in
the built image (`../validator/truststore/wrap-image.sh`). The upstream build
alone is kept as `br-provider:43a4806-upstream`.

The upstream build is upstream's own Dockerfile, built from the pinned commit's
tree as upstream builds it, with upstream's `.dockerignore` applied. It pins the
images of the build's tools (bun, Python and Maven) by digest, so a new release
of one cannot break the build, though the docs stage's pip install of
mkdocs-material names no version and still floats. The final image's base,
the distroless Java 17 runtime, is not pinned, so each build takes its current
Java patches.

The clone is kept in `/tmp/br-provider` (set `BRPROVIDER_CLONE_DIR` to keep it
elsewhere). If your system's `/tmp` cleanup has emptied or pruned it, the
script moves it aside to `/tmp/br-provider.broken-<time>-<pid>-<n>` and clones
again; delete that directory once no build is using it.

```sh
bash brprovider/build.sh build
```

**Step 2 — bring up the bundle**, pointing `SHN_SECRETS` at the bundle
directory from the prerequisite step:

```sh
SHN_SECRETS=/abs/path/to/my-provider-bundle docker compose -f compose.eval.yml up --build
```

Once everything reports ready, the gateway is listening at
`http://127.0.0.1:8080` and the reference requester's UI at
`http://127.0.0.1:8082`.

### Drive the scenarios

Run all eight use cases, signed as the caller from step 0:

```sh
shn send-test --gateway http://127.0.0.1:8080 --discovery https://accounts.shn-preview.org \
  --scenario-key ./scenario-caller-send-test.sign --bundle /abs/path/to/my-provider-bundle
```

Use `shn` from shn-sdk v1.0.0 or later: it states the UC-06 and UC-07 attestation answers,
which this gateway requires, and an earlier `shn` fails both scenarios with
`422 answer is required`.

**A pend is a valid answer.** The evaluation payer pends some prior authorizations
when they're submitted and decides them later, as a real payer may. A
scenario route answers with the payer's first answer: for a pend, that's
`"decision":"pended"` plus a `continuation`. `shn send-test` then follows each pend to
the payer's decision through the gateway's `POST /scenario/pa/inquire`, for up to 60
seconds per scenario (`--follow <seconds>`; `--follow 0` reports each pend as the
payer answered it). A scenario still pended at that bound is reported as pended, with
its continuation, and is neither a pass nor a failure. A scenario the gateway refuses is
reported with its HTTP status and the refusal's body, which says why. Its exit status is 0 when every
scenario passed, 1 when one failed or the run could not start, 2 when the command was
misused, and 3 when none failed but one was still pended (the run then ends
`SEND-TEST PENDED`). To
follow a pend yourself, post `{"continuation":"<id>"}` to `/scenario/pa/inquire`, or
ask a scenario route to follow the decision before it answers with `?wait=<seconds>`
(at most 30). The query is part of what the caller's signature covers.

To call one route with any HTTP client, `shn scenario-sign` prints the
`X-Holder-Assertion` header that signs it. An assertion is for one request,
valid for a minute, and the body you send must be the body you signed. Each
route takes the persona the network's descriptor states for it, which
`persona_body.sh` reads. UC-06 and UC-07 also take the attestation's free-text
`"answer"`, which the descriptor does not state; a request without one is refused (422):

```sh
curl -fsS https://accounts.shn-preview.org/discovery -o discovery.json
body=$(bash persona_body.sh discovery.json uc01 provider-data 0 '{"branch":"covered"}')
curl -fsS -X POST http://127.0.0.1:8080/scenario/uc01 -H 'Content-Type: application/json' \
  -H "X-Holder-Assertion: $(shn scenario-sign --key ./scenario-caller-send-test.sign \
      --bundle /abs/path/to/my-provider-bundle POST /scenario/uc01 "$body")" \
  --data-raw "$body"
```

A request that is not signed, or signed by a caller the bundle does not name, is
refused `401 {"error":"unauthorized caller"}`.

### First-run cost — be aware before you start

The first run is genuinely slow; subsequent runs are fast because Docker
caches the layers.

- **`br-provider/build.sh` builds from source** (a multi-stage build: frontend,
  docs, and a Java server) — expect several minutes the first time.
- **The seeded FHIR server's first boot can take up to ~20 minutes** — it's
  indexing the implementation guides and generating its internal snapshots.
  The `seed` step waits for it and will not fail early; let it run.

Once both are built once and the FHIR server has booted once, later
`docker compose up` runs come back in seconds.

## Point it at your own payer (payer self-test)

By default this bundle originates to the hosted `conformance-payer`. To route the
traffic to **your own** payer instead, set `PAYER_HOLDER_ID` to your payer's holder id:

```sh
SHN_SECRETS=/abs/path/to/my-provider-bundle PAYER_HOLDER_ID=my-payer-holder-id \
  docker compose -f compose.eval.yml up --build
```

`PAYER_HOLDER_ID` only changes the **route**. Two things must still be true, or the leg
is denied before it ever reaches your payer:

- **`SHN_SECRETS` here is still a _provider_ bundle** — not your payer bundle. This gateway
  is `ROLE=provider`; it *originates* the request, and the Authorization Framework binds
  authority to the caller's **registered role**. A payer bundle can't originate a provider
  leg — that authorization is denied, and the ingress returns
  `HTTP 502 {"error":"authorization denied"}`. A payer self-test therefore needs **two**
  registrations — a `--role provider` bundle here, and a `--role payer` bundle for your payer.
- **Your payer must be up and reachable.** A payer is a Hub-*dialed* responder: it must be
  registered under a public `baseURL` and running before you originate, or the Hub has nothing
  to dial. Run it with the payer evaluation bundle — see [`payer/README.md`](payer/README.md).

## Pended decisions in this bundle

A real payer **pends** a prior-authorization request it cannot decide at once, and the pend
lasts hours or days — the case the Da Vinci PAS pended model and the CMS turnaround rules are
built around. The payers this bundle exercises are configured to resolve a pend after a few
seconds instead: the hosted evaluation payer answers a pended request with a pended response
and turns it into a decision about three seconds later, and the reference payer in the payer
bundle does the same (`PAS_PENDED_RESOLUTION_DELAY_SECONDS: "3"` in
`payer/compose.eval.payer.yml`). That is a test-speed setting, not a model of a payer.

The consequence for your client: if every pend you ever see resolves inside one interaction,
your "still pended, check again later" path is never exercised, and a client that waits a pend
out will work here and fail against a real payer. To exercise that path, run the payer bundle
with the delay raised — a pend is then held for that long, and an inquiry for the claim returns
the pended state until it resolves:

```yaml
# payer/compose.eval.payer.yml — hold a pend for ten minutes instead of three seconds
PAS_PENDED_RESOLUTION_DELAY_SECONDS: "600"
```

The hosted evaluation payer's delay is fixed by SHN; to hold a pend, use your own payer bundle.

## The ordering clinician

A CRD request and an attestation the gateway builds for your provider name the clinician who
ordered the service, in the CRD request's `userId` and in the attestation. The clinician comes
from your own records first (the order's requester), and the gateway's `NPI` setting fills in
only where they name none; it has no default, and a CRD request or attestation no source names a
clinician for is refused. An eligibility request names `NPI` as its provider when it is set, and
no provider otherwise. The bundle's test records name no ordering clinician on most orders, so
the bundle sets `NPI` to a synthetic clinician NPI (`1000000020`, valid check digit). With your
own records, name each order's requester (a `Practitioner` with its NPI), or set `NPI` to your own
clinician's NPI. The UC-06 and UC-07 attestations take their answer from the request, and the
gateway supplies none: `shn send-test` states the clinician's functional status for UC-06 and the
patient's for UC-07.

## Conformance enforcement in this bundle

The bundle passes `CONFORMANCE_ENFORCEMENT` through from your own environment, and sets
nothing itself — so with nothing set the gateway runs the published default of the release
you downloaded: `none` from v1.0.0, which checks no payload and records nothing, and
`observe` before it. Set `observe` to have the gateway check and record. At
`observe` every message is checked against its FHIR profile — and a payer's CDS Hooks answer
against the CDS Hooks response rules — each defect is recorded as a finding in the gateway's
log and observer stream, and the message relays as sent, apart from the gateway's registered
edits (the callback removed, prefetch and coverage obtained where the provider opts in with
`ENRICH_NATIVE_REQUESTS=true`, and payer identity mapping):
nothing is refused for conformance.
Network rules refuse at every level regardless: authentication, authority, consent, routing, replay, message integrity, and
the check of a payload this gateway itself translated between IG lines.

That default is deliberate. This is your evaluation, run on your machine, against your own
systems and — with `PAYER_HOLDER_ID` above — your own payer. A refusal produced by a value we
shipped inside a bundle you operate would read as a verdict on your conformance when all it
states is how we configured the bundle.

To run no payload check at all, set the level yourself to `none`: nothing is checked, no
finding is recorded, and every message relays as sent, apart from the gateway's registered
edits.

```sh
SHN_SECRETS=/abs/path/to/my-provider-bundle CONFORMANCE_ENFORCEMENT=none \
  docker compose -f compose.eval.yml up --build
```

Set `strict` instead if you want a message with a defect refused. `none`, `observe`,
`structural` and `strict` are the only accepted values; any other refuses to boot. It is the same setting on
the gateway you run in production — see [CONFIGURATION.md](../../docs/CONFIGURATION.md).

The bundle passes `CONFORMANCE_TOLERATE_PEERS` through the same way, and sets nothing itself,
so it tolerates no payer. If you list a payer there (identities as `<system>|<value>`,
comma-separated), a DTR or PAS answer from it that your level would refuse for a FHIR profile
defect is carried instead, and the defect is recorded as a tolerated finding. Every other check,
and every network rule, still refuses. See [CONFIGURATION.md](../../docs/CONFIGURATION.md).

## Production cutover

Everything in this bundle other than the gateway itself — `hapi`,
`br-provider`, and `gencerts` — is evaluation scaffolding standing in for
systems you already have. Moving to production means:

1. Start from **`gateway/deploy/bundle/`** instead of this directory — that's
   the actual production install unit (the gateway plus your own co-located
   validator, nothing else).
2. Point `FHIR_DATA_URL` and `PROVIDER_DTR_POPULATE_URL` at **your own**
   systems of record instead of the seeded `hapi` container.
3. Drop `hapi`, `br-provider`, and `gencerts` entirely — they don't exist in
   `gateway/deploy/bundle/`, and nothing in the gateway depends on them.
4. Everything else about how the gateway is configured and how it exchanges
   data through the Hub is unchanged. There is no separate "production
   gateway" to learn.

See the main [gateway README](../../README.md) for the full environment
variable reference and integration guide.
