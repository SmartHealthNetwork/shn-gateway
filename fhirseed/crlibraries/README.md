# DTR prepopulation Libraries (operated-CQL `$populate` fixtures)

These FHIR `Library` resources are the prepopulation CQL the SHN-operated clinical-reasoning
engine (HAPI CR, the provider tenant) resolves **from its store** when running
`Questionnaire/$populate` for the HomeOxygen and HomeHealthAssessment DTR questionnaires. They are
installed into the provider HAPI **DEFAULT** partition when the live conformance gate's stack
comes up, because HAPI rejects a `Library` in a tenant partition (`HAPI-1318: Resource type
Library can not be partitioned`).

- `Library-HomeOxygenDispatchPrepopulation.json` — the HomeOxygen prepop logic
  (`ArterialOxygenSaturation`, `ArterialPartialPressureOfOxygen`, …). Depends on `DTRHelpers`
  (+ `FHIRHelpers`, which HAPI CR bundles internally — not stored).
- `Library-HomeHealthAssessmentPrepopulation.json` — the HomeHealthAssessment prepop stub (UC-04
  order-select lane). The questionnaire is adaptive with 0 CQL expression items (measured), so
  the CQL body is a minimal context-only stub — `$populate` auto-pops nothing; attestation answers
  trace directly to the seeded ServiceRequest. Depends on `DTRHelpers`.
- `Library-DTRHelpers.json` — shared DTR helper functions (`ObservationLookBack`, …).
- `Library-BasicPatientInfoPrepopulation.json` — patient demographic prepop.

**Provenance (MIT).** Cribbed verbatim from the pinned HL7-DaVinci/br-payer reference
implementation `a8bece4` (`library/HomeOxygenDispatch/`, `library/dtr/`,
`library/HomeHealthAssessment/`). The only edit: the source `content` points `url` at a sibling
`.cql` file (br-payer loads it from disk at build time); here the CQL is embedded as base64
`content.data` so the resource is self-contained when PUT standalone. HAPI CR compiles the
`text/cql` → ELM at runtime (no precompiled ELM needed; the provider HAPI has
`hapi.fhir.cr.enabled: true`).

**Honesty.** These are the payer's REAL prepopulation CQL — the de-risk's crux is that the operated
engine runs THIS CQL against the provider's seeded observations (HomeOxygen) or the attestation
answers trace to the seeded order (HomeHealthAssessment), not a canned answer book. Synthetic
fixtures only; no PHI.
