# Offline validation support

`shn.fhir.validation-support-1.4.0.tgz` supplies three things the validator lines load from
`file://` beside their IG packages:

- the CMS terminology used by PAS response and US Core Condition validation (it does not
  replace PAS profiles or change their required bindings);
- the first SHN-maintained release (`1.0.0`) of the local
  `urn:shn:clinical-context` CodeSystem used by the lumbar workflow;
- the validation closure of the cross-version canonicals SHN-built resources carry — today
  the R5 `Claim.encounter` extension — copied unchanged from the pinned
  `hl7.fhir.uv.xver-r5.r4` 0.1.0 and `hl7.fhir.uv.extensions.r4` 5.3.0-ballot-tc1 packages
  (25 resources: 16 StructureDefinitions, 6 ValueSets, 3 CodeSystems), so that a Claim
  carrying the extension validates against the extension's own definition, the R5 Encounter
  profile it targets and everything those two reference, without loading either package
  whole.

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

`sources.json` pins every downloaded input by SHA-256 and records its source URL. Its
separate `local` entry pins SHN's authored CodeSystem input and identifies the source
snapshot, public constants and consumers that define the current convention. That
snapshot is provenance for this authored release, not an upstream clinical authority.

The local release is flat, case-sensitive and complete for exactly three concepts:
`conservative-therapy-weeks` is a quantity of completed conservative-therapy weeks;
`neuro-deficit` is a Boolean progressive neurological-deficit flag; and
`patient-reported-required` is a Boolean workflow requirement for a patient-reported
functional-status attestation, not the attestation act. Each participant supplies
its own facts and maps its own system to these concepts; missing facts remain missing.
The release does not set a clinical threshold, identify a suitable LOINC code, or
establish clinical equivalence. Source `Coding.version` remains absent. The local
CodeSystem is SHN-authored under Apache-2.0 (`LICENSE`); the original CMS notices
remain in their archives, and the copied HL7 sources retain their CC0-1.0 terms.

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
- CMS April 1, 2026 ICD-10-CM Code Descriptions in Tabular Order archive:
  all 98,186 entries (23,467 headers and 74,719 diagnosis codes). Its included
  format documents define flag 0 as a header, not valid for HIPAA-covered
  transactions, and flag 1 as valid for those transactions. Both types remain
  present. The independent codes file must exactly equal the flag-1 keys and
  full descriptions. The generator preserves the full description as display,
  abbreviated description as an English designation, release-specific order as
  `cmsOrder`, and the exact source flag as `cmsValidForHIPAATransactions`.
  Membership alone does not make a header a valid diagnosis code. No hierarchy,
  FHIR abstract status, billability, exclusions or clinical rules are inferred.
  `content=complete` describes membership of this full official release, not a
  claim to encode every tabular annotation or to supply every US Core binding.
  The [FHIR R4 ICD representation](https://hl7.org/fhir/R4/icd.html) requires
  decimal notation: three-character keys remain unchanged; longer keys receive
  a period after the third character. This also preserves the source's `QA`
  entries; a second-character-digit assumption would discard official rows.
  The canonical remains `http://hl7.org/fhir/sid/icd-10-cm`. The explicit release
  identifier `2026-04-01` identifies the [CMS April FY2026 release](https://www.cms.gov/medicare/coding-billing/icd-10-codes),
  applicable April–September 2026; it is not a claimed CMS version string or
  a change to source Coding.version. Original archive, documentation and notices
  are retained unchanged. This ICD-10-CM representation is not WHO ICD-10,
  ICD-10-PCS, ICD-9-CM or a replacement for missing SNOMED/LOINC/CPT content.
- CMS Place of Service database, updated May 2, 2024, downloaded September 10, 2026:
  the complete HTML source table. Its 52 assigned codes are represented; unassigned
  codes/ranges are not valid concepts. The generator verifies coverage of all
  positions 01 through 99, including the explicitly unassigned positions, so an
  accidentally truncated table cannot be called complete.
- `hl7.fhir.uv.xver-r5.r4` 0.1.0 and `hl7.fhir.uv.extensions.r4` 5.3.0-ballot-tc1 from
  packages.simplifier.net: the two archives the closure is derived from. The archives are
  not committed; the 25 copied members are, with their digests.

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
recorded digest, and assert the archive holds exactly the three CMS CodeSystems, this
one SHN CodeSystem, the 25 closure members and the manifest. `prior-resource-sha256.json`
pins all 28 resource bytes from support 1.3.0; only the new local member and package
metadata may differ in 1.4.0.

To re-derive the closure, place the two archives (digests as recorded) in a directory and run
`SHN_IG_ARCHIVES=<that directory> python3 closure.py`, then `python3 generate.py`. With the
archives present, `test_generate.py` also re-runs the walk and proves every committed member
byte-identical to its archive entry and the tolerance record unchanged; without them that
test skips and the digest checks stand. Live positive and negative verdict controls remain
necessary: generation tests do not certify HAPI behavior.
