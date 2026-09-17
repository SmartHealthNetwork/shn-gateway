# Validator readiness fixtures

Synthetic FHIR resources the Kit posts to a packaged HAPI validator before it
counts the child ready (see `../warm.go`). Every file is a copy of the
validator container image's own readiness fixture of the same name, and the
platform repository fences the two sets to byte equality: change the image's
copy and refresh this one in the same change. Per contract line, the four
initialization fixtures (PAS request Bundle, DTR QuestionnaireResponse, PDex
ExplanationOfBenefit, CDex Task) exercise profile resolution; the approved,
denied and pended PAS ClaimResponses are the strict verdict corpus, and the
negative control is derived from the approved fixture at run time by
replacing the nested `extension-reviewActionCode` CodeableConcept with a
Boolean. Line 2.0 fixtures live here; `2.1/` and `2.2/` carry that line's
profile versions.

The two encounter rows that close the corpus use `claim-encounter.json` (a core
Claim carrying the R5 backport `Claim.encounter` extension that references a
contained R4 Encounter — the extension the packaged validator's cross-version
package now defines) unchanged for the positive row, and with the contained
Encounter swapped for a Patient of the same id at run time for the target-type
control, which requires exactly the pinned `claim-encounter-target-errors.json`
outcome. Both files are copies of the image's fixtures like every other file here.
