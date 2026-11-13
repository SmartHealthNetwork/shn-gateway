# SHN Smart Gateway — config-only deployment bundle (FR-G3)

This bundle is the **shape of the recommended install unit**: the gateway plus a
**co-located IG-loaded `$validate` sidecar** in one config-only deployment. There is no
separate validator host, and because `$validate` needs the full (PHI-bearing) resource,
the validator runs **inside your own boundary** — PHI is never sent to an SHN-operated
validator (OWD-G9).

The validator runs, but the gateway sends it no payload (no `$validate` call) until you
choose a conformance level; only the gateway's connectivity check, at boot and on
`/internal/checks`, reaches it. Unset, `CONFORMANCE_ENFORCEMENT` is `none`: no payload
conformance check runs and no finding is recorded. To turn the checks on, set
`CONFORMANCE_ENFORCEMENT` to `observe`, `structural` or `strict` (see
`docs/CONFIGURATION.md` in the gateway repo). At every level, startup waits for the
validator to report healthy.

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
- On Linux, that bundle readable by the gateway's uid/gid `65532`, which `shn register`
  does not do for you: `sudo chgrp -R 65532 <dir> && sudo chmod 2750 <dir> && chmod 0640 <dir>/*`
  (the [gateway README](../../README.md#6-going-to-production), "Run as non-root").

## Run
```bash
export SHN_DISCOVERY_URL=https://accounts.shn-preview.org/discovery
export ROLE=provider                   # provider | payer | facility | phg
export SHN_SECRETS=/abs/path/to/bundle # the `shn register -out` dir
# optional: check payloads (unset is none, which checks none)
# export CONFORMANCE_ENFORCEMENT=observe # observe | structural | strict
docker compose -f compose.yml up --build
```
The gateway waits for the validator to report healthy (first boot indexes the IGs and
can take several minutes), then serves its role surface and joins the network. To point
at production later, change only `SHN_DISCOVERY_URL`.

The validator starts one Java process and warms its synthetic validation profiles sequentially
before reporting ready. Health probes only observe readiness; they do not submit work.
A failed or uncertain warm-up remains unhealthy until you restart the validator. Capture its
logs first, then restart it with the same Compose project settings:

```bash
docker compose -f compose.yml logs validator
docker compose -f compose.yml restart validator
```

The new process must complete fresh warm-up even if its temporary marker survived the restart.
Compose does not automatically replace an unhealthy validator.

## What runs
- **validator** — the HAPI FHIR 8.12.0 engine on distroless Java with US Core 6.1.0 + Da Vinci CRD/DTR/PAS 2.0.1 +
  CDex 2.1.0 + HRex 1.1.0 + SDC 3.0.0 loaded; serves per-message `$validate` (FR-G29),
  which the gateway calls at `observe`, `structural` or `strict`. At `strict` a check that
  cannot run is refused; at `observe` and `structural` it is recorded as unavailable.
- **gateway** — the Smart Gateway image; `FHIR_VALIDATE_URL` points at the local sidecar
  by default. `CONFORMANCE_ENFORCEMENT` and `CONFORMANCE_TOLERATE_PEERS` pass through
  from your environment; both are unset by default. Add connector env (`FHIR_DATA_URL`,
  `PAYER_DAVINCI_*`, `PROVIDER_DTR_*`, `SHN_STORE_DATABASE_URL`, …) per the gateway
  README to reach your own systems; the conformant common case needs none. Its service
  sets `stop_grace_period: 45s`: a gateway's stop takes up to 44 s
  ([DEPLOYMENT.md](../../docs/DEPLOYMENT.md#stopping-the-gateway), "Stopping the
  gateway"), and Docker's default of 10 s would cut it. Run the gateway any other way and
  give its container the same 45 s.
