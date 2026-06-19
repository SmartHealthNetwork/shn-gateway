# shn-gateway

The **SHN Smart Gateway** is the software a participant runs to join the Smart
Health Network. Each organization — a provider, a payer, or another participant —
deploys its own gateway. The gateway holds that organization's keys and exchanges
healthcare data with other participants through the **SHN Hub**, with every leg
independently authorized and end-to-end encrypted. The network is
workflow-general; the first workflow delivered on it is Da Vinci prior
authorization.

The gateway is **config-only**: a single published image, pointed at the SHN
discovery endpoint, with your registration bundle mounted. The common case needs
no code.

This guide takes two organizations — a **provider** and a **payer** — from
nothing to a real prior-authorization payload flowing between them through the
live SHN Hub.

- [1. Prerequisites](#1-prerequisites)
- [2. Install the gateway](#2-install-the-gateway)
- [3. Register and get your bundle](#3-register-and-get-your-bundle)
- [4. Configure](#4-configure)
- [5. Run](#5-run)
- [6. Exchange a payload through the Hub (end to end)](#6-exchange-a-payload-through-the-hub-end-to-end)
- [7. Integrate your internal systems](#7-integrate-your-internal-systems)
- [8. Roles and endpoints reference](#8-roles-and-endpoints-reference)
- [9. Troubleshooting](#9-troubleshooting)
- [10. Further reading](#10-further-reading)

> **This guide targets the SHN preview sandbox.** SHN is currently in a preview
> phase, served at `shn-preview.org` and seeded with synthetic personas (Linda
> Johansson et al.) so partners can integrate against a live network. **While in
> preview, send only synthetic data — never production PHI.** Every member id and
> DOB below is fabricated test data. When SHN reaches production you point the
> same gateway at the production substrate by changing `SHN_DISCOVERY_URL` —
> nothing else changes.

---

## 1. Prerequisites

- **Docker** (recommended) or a **Go 1.23+ toolchain** to run the binary.
- **Access to this repository.** It is private; you must be an invited
  collaborator with a GitHub personal access token (`repo` scope) or an SSH key
  on your account.
- **A target substrate.** This guide uses the preview sandbox at `shn-preview.org`.
  Its discovery endpoint is `https://accounts.shn-preview.org/discovery` — a single
  anchor that resolves every other URL (Hub, Authorization Framework, registrar,
  trust anchors). If you run a private SHN deployment, substitute your own apex
  throughout; the discovery descriptor is the source of truth for the live URLs.
- **A sandbox invite.** The sandbox is invite-gated. Submit the **Request
  access** form at `https://developers.shn-preview.org` (no account needed).
  When approved you receive an invite email with a temporary password; sign in
  at the portal and set your password before registering below.
- **A public https endpoint, if you run a responder.** A payer (or any role that
  *receives* exchanges) must be reachable by the Hub. See
  [§6 the payer side](#the-payer-side--receive-and-adjudicate). A provider that
  only *originates* does not need this.

Confirm the sandbox is reachable:

```sh
curl https://accounts.shn-preview.org/discovery
```

You should get a JSON descriptor listing `endpoints`, `sandboxResponders`, and
`sandboxPersonas`.

---

## 2. Install the gateway

Set `GOPRIVATE` so the Go toolchain does not attempt the public proxy for this
private module, and make sure your Git client can authenticate to GitHub:

```sh
export GOPRIVATE=github.com/SmartHealthNetwork/shn-gateway
```

**Option A — Docker image (recommended).** Clone this repository and build the
standalone image (build context is this module; the public `shn-sdk` resolves
from the standard Go proxy):

```sh
git clone https://github.com/SmartHealthNetwork/shn-gateway.git
cd shn-gateway
docker build -t shn-gateway .
```

**Option B — Go binary:**

```sh
go install github.com/SmartHealthNetwork/shn-gateway/cmd/gateway@v0.2.0
```

The image runs as a non-root user (uid/gid 65532, distroless) — see
[§5](#run-as-non-root) for the bundle permissions that implies.

---

## 3. Register and get your bundle

Before the gateway can run it needs a **key bundle** — your client identity in
SHN. You obtain it by registering with the SHN Accounts service using the `shn`
CLI from the public [`shn-sdk`](https://github.com/SmartHealthNetwork/shn-sdk).
Keys are generated **client-side**; your private keys never leave your machine.

Install the CLI:

```sh
# macOS / Linux — detects platform, verifies the published SHA-256, installs to ~/.local/bin
curl -fsSL https://developers.shn-preview.org/install.sh | sh

# …or with a Go toolchain:
go install github.com/SmartHealthNetwork/shn-sdk/cmd/shn@latest
```

Log in once (token caches at `~/.shn/credentials`), then register **one client
per organization**, with the `--role` that matches how you participate
(`provider`, `payer`, `facility`, or `phg` — see
[§8](#8-roles-and-endpoints-reference)). The holder id is **server-assigned** and
printed on success — you'll need it to address exchanges.

**Provider** (originates exchanges):

```sh
shn login --accounts https://accounts.shn-preview.org

shn register --accounts https://accounts.shn-preview.org \
  --role provider --name acme-clinic --base-url https://acme-clinic.example.com \
  -out ./provider-bundle
# → Registered acme-clinic-7f3a. Keys in ./provider-bundle.
```

**Payer** (receives and adjudicates exchanges):

```sh
shn register --accounts https://accounts.shn-preview.org \
  --role payer --name acme-health --base-url https://gateway.acme-health.example.com \
  -out ./payer-bundle
# → Registered acme-health-2b9c. Keys in ./payer-bundle.
```

The `-out` directory holds the bundle the gateway loads: `manifest.json` (your
holder id, role, public keys, baseURL) plus the private key files.

> **`--base-url` differs by role — this is the key distinction.** It must be an
> **https URL that publicly resolves** (the registrar rejects private, loopback,
> link-local, and unresolvable addresses).
> - A **provider that only originates** is never dialed by the Hub — use any
>   https URL you control, e.g. your website.
> - A **responder** (`payer`, `facility`, `phg`) **is** dialed: the Hub POSTs to
>   `{baseURL}/substrate/inbound`, so baseURL must be the real, public,
>   TLS-terminated address where your gateway listens, and must not redirect on
>   that path.

List or revoke clients anytime:

```sh
shn clients --accounts https://accounts.shn-preview.org
shn revoke acme-clinic-7f3a --accounts https://accounts.shn-preview.org
```

---

## 4. Configure

The gateway is configured entirely by environment variables. `SHN_DISCOVERY_URL`
resolves almost everything else; in a standard sandbox deployment you set only a
handful of variables.

### Required (every role)

| Env var | Description |
|---|---|
| `SHN_DISCOVERY_URL` | The single anchor — resolves substrate endpoints and trust anchors. Sandbox: `https://accounts.shn-preview.org/discovery` |
| `ROLE` | `provider`, `payer`, `facility`, or `phg`. Must match the role you registered. |
| `SHN_SECRETS` | Path to the bundle directory written by `shn register -out`. |

### Validation (required to boot — FR-36)

The gateway **refuses to start without a FHIR validator** — every resource is
validated at the edge before it crosses the substrate. The sandbox discovery
descriptor does **not** advertise a validator, so you must supply one:

| Env var | Description |
|---|---|
| `FHIR_VALIDATE_URL` | A FHIR `$validate` endpoint (a HAPI server with the Da Vinci CRD/DTR/PAS + US Core IGs loaded). The production path. |
| `SHN_FAKE_VALIDATOR` | Set to `1` to use a no-op validator. **Dev only** — skips real profile validation. Use for a first wiring smoke test; never in production. |

If neither is set (and discovery advertises none), the gateway exits with
`refusing to run without per-message validation (FR-36)`.

### Per-role

| Env var | Applies to | Description |
|---|---|---|
| `COUNTERPART_ID` | provider | The holder id you originate to. Set it to your counterpart payer's server-assigned id (or `payer` for the built-in sandbox responder). Required to originate. |

Responder roles (`payer`/`facility`/`phg`) need no `COUNTERPART_ID`: they receive
at `POST /substrate/inbound` and reply to whoever the Hub delivers from. A payer
adjudicates with built-in sandbox decision logic out of the box — plug in your
own via [§7](#7-integrate-your-internal-systems).

### Networking

| Env var | Description |
|---|---|
| `PORT` | Listening port. Default `8080`. |
| `HOST` | Bind address. Default `0.0.0.0`. |

### Connect your system of record (optional — see [§7](#7-integrate-your-internal-systems))

| Env var | Description |
|---|---|
| `FHIR_DATA_URL` | FHIR R4 base URL for your system of record. **Omit to use the built-in synthetic stub** (carries the sandbox personas, so you can run end to end with no backend). |
| `FHIR_TOKEN_URL` | SMART Backend Services token endpoint, if your FHIR server requires authenticated access. Requires the client quad below. |
| `FHIR_CLIENT_ID` | SMART client id. |
| `FHIR_CLIENT_KEY` | SMART client private key (PEM). |
| `FHIR_CLIENT_ALG` | `ES384` or `RS384`. |
| `FHIR_CLIENT_SCOPE` | Requested scope. Default `system/*.read`. |
| `FHIR_CLIENT_KID` | Key id for the client assertion JWK, if your server requires it. |
| `SHN_STORE_DATABASE_URL` | Postgres DSN for durable claim-state storage. Omit for in-memory (non-durable across restarts). |

### Advanced overrides (rarely needed)

Each substrate endpoint and trust-anchor key URL is resolved from discovery by
default; set the matching variable only to override (e.g. when the gateway runs
inside the substrate's own network): `AUTHZ_URL`, `HUB_URL`, `CONSENT_URL`,
`AUDIT_URL`, `PHG_URL`, `REGISTRAR_URL`, `FHIR_VALIDATE_URL`, `AUTHZ_PUBKEY_URL`,
`HUB_TRANSPORT_KEY_URL`. Explicit env always wins over discovery. `NPI` overrides
the organization NPI stamped into a provider's originated requests (defaults to a
synthetic sandbox placeholder).

---

## 5. Run

Each organization launches its own gateway with the bundle from
[§3](#3-register-and-get-your-bundle). Both examples below use the dev validator
(`SHN_FAKE_VALIDATOR=1`) and the built-in synthetic stub for a first run — no
validator host, no backend. For production, drop `SHN_FAKE_VALIDATOR=1`, set
`FHIR_VALIDATE_URL`, and set `FHIR_DATA_URL`
([§7](#7-integrate-your-internal-systems)).

**Provider** — boots ready to originate to its counterpart:

```sh
docker run --rm \
  -e SHN_DISCOVERY_URL=https://accounts.shn-preview.org/discovery \
  -e ROLE=provider \
  -e COUNTERPART_ID=acme-health-2b9c \
  -e SHN_FAKE_VALIDATOR=1 \
  -e SHN_SECRETS=/etc/shn/bundle \
  -v "$PWD/provider-bundle:/etc/shn/bundle:ro" \
  -p 8080:8080 \
  shn-gateway
```

**Payer** — boots as a responder listening for Hub deliveries:

```sh
docker run --rm \
  -e SHN_DISCOVERY_URL=https://accounts.shn-preview.org/discovery \
  -e ROLE=payer \
  -e SHN_FAKE_VALIDATOR=1 \
  -e SHN_SECRETS=/etc/shn/bundle \
  -v "$PWD/payer-bundle:/etc/shn/bundle:ro" \
  -p 8080:8080 \
  shn-gateway
```

Each gateway boots, loads its bundle, resolves the Hub/authz/registrar and trust
anchors from discovery, and listens on `:8080`. Running the binary instead of the
image is the same env, e.g. `ROLE=payer SHN_SECRETS=./payer-bundle … gateway`.

### Run as non-root

The Docker image runs as **uid/gid 65532** (distroless nonroot). A self-mounted
bundle directory and its key files must be readable by gid 65532: use `0640`
for files and `0750` for the directory, with group ownership set to `65532`.
SHN's provisioning tooling applies these automatically; if you mount your own
bundle, set them yourself:

```sh
chgrp -R 65532 ./payer-bundle && chmod 750 ./payer-bundle && chmod 640 ./payer-bundle/*
```

---

## 6. Exchange a payload through the Hub (end to end)

Two gateways let two organizations exchange directly. Here a **payer** stands up
its gateway as a responder, and a **provider** stands up its gateway and
originates a prior authorization to that payer — the request travels
provider → Hub → payer, and the decision comes back the same way. Neither party
sees the other's keys or network; the Hub routes sealed envelopes between them.

### The payer side — receive and adjudicate

A payer-role gateway serves `POST /substrate/inbound`: the Hub delivers sealed
exchanges there, and the gateway authenticates the Hub, decrypts, **adjudicates**,
and returns a sealed response. Out of the box it uses the built-in sandbox
decision logic — it approves the lumbar-MRI prior auth (UC-03), pends the
amend scenario (UC-04), denies UC-08, and answers eligibility for the seeded
members. Plug in your own decisioning via [§7](#7-integrate-your-internal-systems).

Because the Hub must reach the payer, the payer's registered `--base-url` has to
be a **publicly resolvable https endpoint** that fronts the gateway's `:8080`.
For a real deployment that's your service's address behind TLS; to test from a
laptop, expose the local gateway with a tunnel and register that https URL:

```sh
# e.g. with ngrok — then register --base-url with the https URL it prints
ngrok http 8080
```

Start the payer gateway ([§5](#5-run)). The Hub will now POST deliveries to
`{your-base-url}/substrate/inbound`.

### The provider side — originate

Start the provider gateway with `COUNTERPART_ID` set to the **payer's holder id**
from [§3](#3-register-and-get-your-bundle) (`acme-health-2b9c` in this guide), as
in [§5](#5-run). Then originate a **coverage-eligibility** exchange (UC-01) for a
seeded persona:

```sh
# Covered persona → routes provider → Hub → your payer → back.
curl -s -X POST localhost:8080/scenario/uc01 \
  -H 'Content-Type: application/json' \
  -d '{"branch":"covered"}'
# → {"covered":true,"reason":""}
```

Originate the full **prior-authorization** chain (CRD → DTR → PAS, UC-03):

```sh
curl -s -X POST localhost:8080/scenario/uc03 \
  -H 'Content-Type: application/json' -d '{}'
# → {"outcome":"approved","preAuthRef":"…"}
```

A `covered:true` / `approved` response means the full loop worked: your provider
gateway built and **validated** the request, **authorized** the leg with the
Authorization Framework, **sealed** it, sent it to the **Hub**, the Hub routed it
to your payer gateway, the payer **adjudicated** and sealed a response, and your
provider gateway **verified and decrypted** it. Two independent organizations just
completed a prior authorization through the substrate.

The provider serves the `POST /scenario/uc01` … `/scenario/uc08` originate
endpoints (UC-06/UC-07 add `/start`, `/complete`, `/cancel` sub-routes for their
multi-step flows). For the full persona matrix — including how the clinical
*answers* drive approve vs. pend vs. deny — drive the exchange from the `shn` CLI
or the SDK; see the SANDBOX guide in
[`shn-sdk`](https://github.com/SmartHealthNetwork/shn-sdk).

### Quick single-machine smoke (built-in sandbox payer)

To confirm a provider gateway works **without** standing up a payer, point it at
the sandbox's built-in `payer` responder — set `COUNTERPART_ID=payer` and
originate. No payer deployment, no public endpoint required:

```sh
# provider gateway running with COUNTERPART_ID=payer
curl -s -X POST localhost:8080/scenario/uc01 \
  -H 'Content-Type: application/json' -d '{"branch":"covered"}'
# → {"covered":true,"reason":""}
```

---

## 7. Integrate your internal systems

By default the gateway uses a built-in **synthetic stub** for its system of
record (the sandbox personas) — which is why [§6](#6-exchange-a-payload-through-the-hub-end-to-end)
works with no backend. To carry **your** data, point the gateway at your systems
through connectors. This applies to both sides: a provider reads the clinical and
coverage data it originates from; a payer reads the records its decisioning
evaluates.

### The common case — point at your FHIR server (no code)

If your backend exposes **FHIR R4** (Epic, and increasingly Availity /
Surescripts — the CMS-0057 direction), you write **no code**. Set `FHIR_DATA_URL`
to your US Core FHIR base URL and, if it requires authenticated access, the SMART
Backend Services quad:

```sh
docker run --rm \
  -e SHN_DISCOVERY_URL=https://accounts.shn-preview.org/discovery \
  -e ROLE=provider -e COUNTERPART_ID=acme-health-2b9c \
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
resolved from `SHN_DISCOVERY_URL` — nothing else to wire.

### Payer decisioning

A payer gateway decides eligibility and prior-auth via an **Adjudicator**. The
default reads coverage and clinical facts from its system of record (the stub, or
your `FHIR_DATA_URL`) and applies the sandbox criteria. To run **your own**
coverage and medical-necessity policy, inject a custom `shnsdk.Adjudicator`
through the `engine.Config.Adjudicator` seam — the same interface the SDK
responder uses, so one implementation works in both. See
[`STABILITY.md`](STABILITY.md) for the supported `engine` seam.

> **Note:** The `engine.LegResponder` interface is an **internal, unstable 0.x
> seam** — it may change in any minor version. Do not depend on it directly.
> `engine.Config.Adjudicator` is the **supported** partner injection point and
> will remain stable across minor versions.

#### Native-forward payer mode (`PAYER_DAVINCI_*`)

Setting `PAYER_DAVINCI_BASE_URL` switches the payer gateway into **native-forward
mode**: the three read-only Da Vinci legs (coverage eligibility, CRD order-select,
and DTR questionnaire fetch) are forwarded to a real partner Da Vinci endpoint
instead of being adjudicated by the sandbox fallback. PAS (the claim submission
legs) also forward when `PAYER_DAVINCI_PAS_NATIVE=true` is set; without it, PAS
falls back to the sandbox adjudicator. A gateway with `PAYER_DAVINCI_BASE_URL` set
and `PAYER_DAVINCI_PAS_NATIVE=true` is **fully native**: all five payer legs
forward to your partner. A payer Store (`SHN_STORE_DATABASE_URL` or holdersim) is
required when `PAYER_DAVINCI_PAS_NATIVE=true` — `build()` fails at startup otherwise.

| Env var | Description |
|---|---|
| `PAYER_DAVINCI_BASE_URL` | Base URL of the partner Da Vinci payer (e.g. `https://api.payer.example/davinci`). Setting this enables native-forward mode. |
| `PAYER_DAVINCI_TOKEN_URL` | SMART Backend Services token endpoint for the partner. Required if the partner requires authentication. |
| `PAYER_DAVINCI_CLIENT_ID` | SMART client id for the partner. Required when `PAYER_DAVINCI_TOKEN_URL` is set. |
| `PAYER_DAVINCI_CLIENT_KEY` | SMART client private key (PEM path or inline PEM). Required when `PAYER_DAVINCI_TOKEN_URL` is set. |
| `PAYER_DAVINCI_CLIENT_ALG` | `ES384` or `RS384`. Required when `PAYER_DAVINCI_TOKEN_URL` is set. |
| `PAYER_DAVINCI_SCOPE` | Requested scope. Default `system/*.read`. |
| `PAYER_DAVINCI_CLIENT_KID` | Key id for the client assertion JWK, if the partner requires it. |
| `PAYER_DAVINCI_PAS_NATIVE` | `true` to forward PAS submit/update legs to the partner's `/Claim/$submit`. Default `false` (sandbox PAS fallback). Requires a payer Store. |

**All-or-nothing rule:** if `PAYER_DAVINCI_TOKEN_URL` is set, then
`PAYER_DAVINCI_CLIENT_ID`, `PAYER_DAVINCI_CLIENT_KEY`, and
`PAYER_DAVINCI_CLIENT_ALG` must also be set — a partial credential block is a
hard startup error (a likely misconfig). Setting `PAYER_DAVINCI_BASE_URL` alone
(no token URL) is valid and forwards to the partner **unauthenticated** — the
gateway logs a warning on startup to make this mode visible.

The engine continues to own authority enforcement: every forwarded leg is still
independently authorized, sealed, and audited through the substrate. The (C)
outbound subject fence (`fenceResponseSubject`) applies to all native-forwarded
responses — including PAS submit/update when `PAYER_DAVINCI_PAS_NATIVE=true`. If
the partner returns a response about a different patient than the request, the
engine rejects it before sealing (a 403, not a sealed foreign-patient leg).

### Provider DTR population (`PROVIDER_DTR_*`)

On the **provider** side, the DTR leg fills the payer's questionnaire from the
member's clinical data. By default the gateway uses a **managed** populator that
fills the sandbox lumbar-MRI questionnaire from the system of record (the stub, or
your `FHIR_DATA_URL`). To populate **arbitrary** DTR questionnaires — the real Da
Vinci DTR case, where questionnaires carry CQL expressions the gateway does not
itself evaluate — forward population to an SDC `Questionnaire/$populate` engine:

| Env var | Description |
|---|---|
| `PROVIDER_DTR_NATIVE` | `true` to forward DTR population to an SDC `$populate` engine instead of the managed sandbox populator. Default `false`. |
| `PROVIDER_DTR_POPULATE_URL` | The SDC `Questionnaire/$populate` endpoint. Required when `PROVIDER_DTR_NATIVE=true`. |

A **CMS-0057-conformant** provider runs its own DTR client and points
`PROVIDER_DTR_POPULATE_URL` at it. A provider without a DTR client yet can point it
at a `$populate` CQL engine you operate (for example a HAPI FHIR Clinical Reasoning
server) — the same SDC contract, populated centrally (DTR-as-a-service). Either way
the engine keeps authority: the populated `QuestionnaireResponse` is fenced to the
member it was populated for — a response about a different patient is rejected before
it can reach PAS — then sealed and audited like any other leg.

### Durable claim state

Set `SHN_STORE_DATABASE_URL` to a Postgres DSN to persist in-flight
(pended/resumable) claim state across restarts and replicas, instead of the
default in-memory store.

### A non-FHIR backend (custom connector)

For a legacy or non-FHIR system of record (HL7v2, X12, SQL, SOAP), implement the
`engine.SystemOfRecord` interface starting from the runnable scaffold. See
[`connectors/scaffold/README.md`](connectors/scaffold/README.md) for the
step-by-step: copy `scaffold.go`, fill the read methods against your backend
(deriving the patient identifier via `shnsdk.ResolvePCI`), and wire your
connector through the already-public `engine.Config.SoR` seam.

---

## 8. Roles and endpoints reference

Your `--role` (at registration) and `ROLE` (at run) must match. Each role serves
a different surface:

| Role | What it does | HTTP surface it serves |
|---|---|---|
| `provider` | Originates exchanges (eligibility, prior auth) | `POST /scenario/uc01…uc08` (+ UC-06/07 sub-routes) |
| `payer` | Responds to inbound exchanges; serves the CMS-0057 Patient Access API | `POST /substrate/inbound`; `GET /metadata`, `GET /ExplanationOfBenefit[/{id}]` |
| `facility` | Responds to inbound federated-query exchanges | `POST /substrate/inbound` |
| `phg` | Patient Health Gateway — responds to inbound patient-directed exchanges | `POST /substrate/inbound` |

The provider scenario endpoints are an **operator surface**, not a public one —
keep them off the public internet (the Hub never calls them; only your own
tooling does). The responder `/substrate/inbound` is the one the Hub calls, and
it authenticates every delivery via the `X-Hub-Assertion` header before reading
the body.

---

## 9. Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `refusing to run without per-message validation (FR-36)` at startup | No validator configured and discovery advertises none. Set `FHIR_VALIDATE_URL`, or `SHN_FAKE_VALIDATOR=1` for a dev smoke. |
| `fetch discovery: …` / `fetch hub transport key: …` at startup | `SHN_DISCOVERY_URL` unreachable, or the gateway can't reach the substrate hosts it resolves. Confirm `curl $SHN_DISCOVERY_URL` works from the gateway's network. |
| Provider originate returns `recipient "…" not in registry` | `COUNTERPART_ID` isn't a registered holder, or the registrar feed hasn't propagated yet. Confirm the counterpart appears in `shn clients` / the `/holders` feed. |
| Provider originate returns `502` (routing / response sender mismatch) | The counterpart isn't reachable or didn't respond as itself. Check the payer gateway is running and its registered `--base-url` resolves publicly to it. |
| Payer never receives a delivery | The Hub can't reach the payer's `--base-url`. It must be a public https endpoint fronting `:8080` and must not redirect on `/substrate/inbound`. Re-check the tunnel/load balancer. |
| Payer returns `403 missing or invalid hub assertion` | The request didn't come from the Hub (e.g. a direct curl to `/substrate/inbound`). Only the Hub can deliver; originate from a provider instead. |
| Scenario call returns `400 unknown branch` / `unknown member` | The originate body must be `{"branch":"covered"}` or `{"branch":"notcovered"}`; the member must exist in the active system of record (the stub has the sandbox personas). |
| Permission-denied reading the bundle (Docker) | The mounted bundle isn't readable by gid 65532. Apply `chgrp -R 65532` + `0750`/`0640` (see [§5](#run-as-non-root)). |
| Registration rejected with `invalid baseURL: …` | `--base-url` must be a publicly resolvable https URL — not private/loopback/link-local. See [§3](#3-register-and-get-your-bundle). |
| `egress validation failed` (422) on originate | The resource your connector produced doesn't conform to its IG profile. Check the `issues` array in the response; fix the data your `SystemOfRecord` returns. |

---

## 10. Further reading

- [`STABILITY.md`](STABILITY.md) — versioning policy and the supported seam contract.
- [`connectors/scaffold/README.md`](connectors/scaffold/README.md) — the custom
  `SystemOfRecord` connector walkthrough.
- [`shn-sdk`](https://github.com/SmartHealthNetwork/shn-sdk) — the public
  participant SDK and `shn` CLI. Its `docs/SANDBOX.md` (Discover → Register →
  Build → Run → Validate) and `docs/PARTICIPANT_PROTOCOL.md` (the
  language-neutral wire contract) are the canonical participant references; this
  gateway implements that same contract.
