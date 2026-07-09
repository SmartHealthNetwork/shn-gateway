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
| `hapi` | A FHIR server, pre-loaded with the US Core + Da Vinci CDex/HRex/PAS implementation guides, standing in for your provider system of record. Seeded on boot with realistic member and order data. |
| `gateway` | The real `shn-gateway` image, configured with `ROLE=provider`, pointed at the seeded `hapi` for both data and DTR questionnaire population. |
| `br-provider` | A reference Da Vinci requester application (HL7-DaVinci's `br-provider`, built from pinned upstream source) that originates prior-authorization requests through the gateway's Da Vinci ingress, so you can see the conformant lane work with a real client, not just synthetic calls. |
| `gencerts` | A one-shot init step that generates the certificate pair `br-provider` and the gateway's ingress use to authenticate each other. Generated fresh on first boot — no key material is ever committed to this repository. |

The gateway originates through the real Hub to a hosted evaluation payer
(`conformance-payer`), so every exchange is a genuine round trip, not a local
loopback: real authorization per leg, a real audit trail, real profile
validation.

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

## Run it — two steps

**Step 1 — build the reference requester.** This clones and builds
`br-provider` from pinned upstream source; there's no published image to pull.

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
