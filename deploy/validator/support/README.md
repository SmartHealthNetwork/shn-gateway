# Offline validation support

`shn.fhir.validation-support-1.5.0.tgz` supplies two things the validator lines load from
`file://` beside their IG packages:

- the CMS terminology required when validating the complete PAS response graph (it does not
  replace PAS profiles or change their required bindings);
- the validation closure of the extension canonicals exchanged resources carry that no line's
  own packages define, copied unchanged from the pinned `hl7.fhir.uv.xver-r5.r4` 0.1.0 and
  `hl7.fhir.uv.extensions.r4` 5.3.0-ballot-tc1 packages (29 resources: 18 StructureDefinitions,
  7 ValueSets, 4 CodeSystems), without loading either package whole. Two seeds today:
  - the R5 `Claim.encounter` extension, so that a Claim carrying it validates against the
    extension's own definition, the R5 Encounter profile it targets and everything those two
    reference (25 of the members);
  - the `artifact-versionAlgorithm` extension a DTR Questionnaire carries (for example
    `valueCoding` `http://hl7.org/fhir/version-algorithm#semver`). The 2.2 line loads
    `hl7.fhir.uv.extensions.r4` whole; the 2.0 and 2.1 lines do not, and without this seed
    their validator reports `Terminology_TX_System_Unknown` (an error) for the
    `version-algorithm` system. The seed adds four members: the extension, the `web-source`
    extension its definition carries, the `version-algorithm` ValueSet it binds and the
    complete `version-algorithm` CodeSystem (5 concepts). On 2.2 the four are same-version,
    byte-identical duplicates of the extensions package's own entries, recorded as such in
    that line's closure inventory.

1.3.x and 1.4.x are not used: HAPI caches a package by id and version, and those numbers have
been used before, so an instance that still holds one would never load this content.

The standalone published gateway build context includes every input.

## The closure and how it is derived

`closure.py` is the rule, not prose. From each seed in `sources.json` (`closure.seeds`) it
follows `baseDefinition`, `type.profile`, `binding.valueSet`, element and resource extension
URLs, `ValueSet.compose` systems and included value sets, `CodeSystem.supplements` and
`CodeSystem.valueSet`, and `type.targetProfile` one level deep (the seed's own value type and
that profile's direct reference targets), inside the two archives; it stops at any canonical the
engine core provides. It is version-aware: a `url|version` reference resolves only to an archive
member whose own `version` matches; a pinned version no archive carries is recorded in
`closure-tolerances.json` under `versionFallbacks` (the engine's versioned-URL fallback resolves
such a core-namespace canonical to the loaded definition) and the loaded member is followed; a
reference target the depth cut leaves is recorded under `cutReferenceTargets`; a canonical no
archive carries under `unresolved`. The tolerance record is part of the committed output and
the per-line closure inventories (`tools/contracts/closure/<line>.json`) declare the same holes.

Each member is written to `inputs/closure/` byte-for-byte from the archive entry;
`closure-members.json` lists them (url, version, resource type, file) and `sources.json`
records, per member, the archive, the member path and the SHA-256 of the bytes, beside both
archive digests. `generate.py` refuses a member whose bytes differ from the recorded digest,
and `closure.py` refuses to walk an archive whose digest differs from the recorded one.

## Sources and scope

`sources.json` pins every downloaded input by SHA-256 and records its source URL.

- CMS July 2026 alpha-numeric HCPCS release, updated June 17, 2026: the full public
  use archive, including its record layout and notices. CMS describes releases from
  2020 onward as complete quarterly files. The generator reads every first procedure
  and modifier record and every continuation: 8,725 procedures plus 384 modifiers,
  9,109 distinct codes. The full release includes historical terminated codes;
  their source termination dates and release-relative inactive flags are preserved
  as concept properties. Membership is not a claim that a historical code remains
  billable today. This is CMS Level II, not AMA CPT Level I or ADA CDT. The
  canonical is the exact `http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets`
  used by PAS. The release remains versioned `2026-07`; no current-date network
  fetch occurs during generation or runtime.
- CMS Place of Service database, updated May 2, 2024, downloaded September 10, 2026:
  the complete HTML source table. Its 52 assigned codes are represented; unassigned
  codes/ranges are not valid concepts. The generator verifies coverage of all
  positions 01 through 99, including the explicitly unassigned positions, so an
  accidentally truncated table cannot be called complete.
- `hl7.fhir.uv.xver-r5.r4` 0.1.0 and `hl7.fhir.uv.extensions.r4` 5.3.0-ballot-tc1 from
  packages.simplifier.net: the two archives the closure is derived from. The archives are
  not committed; the 29 copied members are, with their digests.

CMS publishes the HCPCS archive as a public use file. Its original record-layout
copyright notices are retained in the archive. No CPT or CDT dataset is synthesized
or included. These generated FHIR representations preserve the source code identities
and descriptions; they do not determine coverage or payment. The HL7 sources declare
CC0-1.0.

## Reproduction

From this directory, run `python3 generate.py` followed by `python3 test_generate.py`.
Python's standard library is sufficient. Input digest mismatches, unknown record shapes,
duplicate codes and incomplete tables fail. The tar entries are sorted with fixed metadata
and the gzip timestamp is zero. The tests compare regenerated bytes with the committed
package, verify known valid and absent codes, verify every closure member against its
recorded digest, assert the archive holds exactly the two CodeSystems, the 29 closure
members and the manifest, and pin the packaged `version-algorithm` CodeSystem to the
SHA-256 of its archive entry.

To re-derive the closure, place the two archives (digests as recorded) in a directory and run
`SHN_IG_ARCHIVES=<that directory> python3 closure.py`, then `python3 generate.py`. With the
archives present, `test_generate.py` also re-runs the walk and proves every committed member
byte-identical to its archive entry and the tolerance record unchanged; without them that
test skips and the digest checks stand. Live positive and negative verdict controls remain
necessary: generation tests do not certify HAPI behavior.
