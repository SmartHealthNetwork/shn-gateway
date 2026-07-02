# SHN Smart Gateway — config-only deployment bundle (FR-G3)

This bundle is the **shape of the recommended install unit**: the gateway plus a
**co-located IG-loaded `$validate` sidecar** in one config-only deployment. There is no
separate validator host, and because `$validate` needs the full (PHI-bearing) resource,
the validator runs **inside your own boundary** — PHI is never sent to an SHN-operated
validator (OWD-G9).

> **Status — config-only install from the published gateway repo.** Clone the
> `shn-gateway` repo and `docker compose up --build` from this directory: both images
> build from the repo itself (`build:`), so no separate source tree is needed. A
> *no-build* experience (pre-published registry images, `build:`→`image:`) is the only
> remaining packaging step (deferred — the env-var contract below does not change when
> it lands).

## Prerequisites
- Docker + docker compose.
- A registration bundle from `shn register -out <dir>` (your holder identity; keys are
  generated client-side and never leave you).

## Run
```bash
export SHN_DISCOVERY_URL=https://accounts.shn-preview.org/discovery
export ROLE=provider                   # provider | payer | facility | phg
export SHN_SECRETS=/abs/path/to/bundle # the `shn register -out` dir
docker compose -f compose.yml up --build
```
The gateway waits for the validator to report healthy (first boot indexes the IGs and
can take several minutes), then serves its role surface and joins the network. To point
at production later, change only `SHN_DISCOVERY_URL`.

## What runs
- **validator** — `hapiproject/hapi` with US Core 6.1.0 + Da Vinci CRD/DTR/PAS 2.0.1 +
  PDex 2.1.0 + SDC 3.0.0 loaded; serves fail-closed per-message `$validate` (FR-G29).
- **gateway** — the Smart Gateway image; `FHIR_VALIDATE_URL` points at the local sidecar
  by default. Add connector env (`FHIR_DATA_URL`, `PAYER_DAVINCI_*`, `PROVIDER_DTR_*`,
  `SHN_STORE_DATABASE_URL`, …) per the gateway README to reach your own systems; the
  conformant common case needs none.
