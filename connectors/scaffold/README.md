# Tier-3 connector scaffold (`SystemOfRecord`)

Most partners never touch this directory. Pick your path:

## Path 1 — Configure our prebuilt image (the common case, no code)

If your backend exposes **FHIR R4** (Epic, and increasingly Availity / Surescripts —
the CMS-0057 direction), you do **not** write a connector. Deploy the published
`shn-gateway` image and point its built-in generic FHIR connector at your endpoint:

- `FHIR_DATA_URL` — your US Core FHIR base URL.
- The SMART Backend Services quad (`FHIR_TOKEN_URL`, client id, key, scope) — if your
  FHIR server requires authenticated access.

Everything else (trust anchors, Hub/authz/registrar, the consent/audit/PHG planes) is
resolved from `SHN_DISCOVERY_URL`. No clone, no build.

## Path 2 — Clone & customize (only if no built-in connector fits)

For a legacy/non-FHIR backend (HL7v2, X12, SQL, SOAP), implement the
`engine.SystemOfRecord` interface (6 read methods) starting from `scaffold.go`:

1. Copy `scaffold.go`, give your type a backend handle (DB pool, SOAP/X12 client).
2. Replace each `// TODO(partner):` body with a read against your system of record.
   `ResolvePatient` must derive the PCI via `shnsdk.ResolvePCI(member, birthDate, family)`
   (AI-5) — do not invent your own subject identifier.
3. Wire your connector in **either** of two identical-seam ways:
   - **Edit the selection** in `app/app.go` (the `if cfg.FHIRDataURL == ""` `sor`/`store` assignment in `build()`), or
   - **Construct the engine directly** in your own `main`:
     `engine.New(engine.Config{Role: ..., SoR: yourconnector.New(...), ...}).Handler()`.

Both wire the same already-public seam `engine.Config.SoR`. An automated override test
proves the seam is genuinely overridable — it injects this scaffold as the provider
`SystemOfRecord` and asserts the scaffold's own clinical data surfaces end-to-end.

## Why this scaffold is not config-selectable

`scaffold` carries demo persona data and stub bodies, so it is deliberately **not** in the
image's connector switch — a template must never be selectable in production. Real vendor
connectors are the opposite: they are meant to ship in the image and be selected by config
(the planned connector-registry track). This scaffold exists to make the seam they plug into
easy to start from and proven to work.
