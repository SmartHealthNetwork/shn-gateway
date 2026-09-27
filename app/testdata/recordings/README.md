# Validator lane recordings (default-lane qualification, certification warm-up)

What a real HAPI validator lane answered, captured on 2026-09-27 from the three local lanes
`make validate` boots (the 2.0, 2.1 and 2.2 contract lines, each with its line's IG closure and
the shared validation-support package; HAPI FHIR 8.10.0), through an exact-bytes recording proxy.
The requests are the validator code's own: the gateway's default-lane qualifier and its boot
certification warm-up ran against each lane. Every request body is a committed synthetic fixture.
No participant or patient data.

- `lane-2.0-asked-2.1.json`, `lane-2.0-asked-2.2.json`, `lane-2.1-asked-2.0.json`,
  `lane-2.1-asked-2.2.json`, `lane-2.2-asked-2.0.json`, `lane-2.2-asked-2.1.json`: a lane
  qualified as a line it does not serve (`lane-<lane>-asked-<line>`). The qualifier's four
  initialization rows for the asked line and its first decision row (row 5 of 42, the versioned
  approved ClaimResponse at the asked line's PAS version), which the lane refuses, so the
  qualification stops there. The refusals are the lane's own: `processing` with the message id
  `Validation_VAL_Profile_Unknown` (the asked PAS version is not loaded), and on the 2.1 and 2.2
  lanes asked for 2.0 also `Validation_VAL_Profile_Minimum` (`ClaimResponse.request`). The
  qualifier first asked each lane `GET /fhir/metadata`; that answer is not repeated here: it is
  `lane-<lane>-metadata.json` beside the validator code
  (`../../internal/lanequalify/testdata/recordings/`), whose resource type and FHIR version the
  captured answers were checked to equal.
- `lane-2.0-certify-warm.json`, `lane-2.1-certify-warm.json`, `lane-2.2-certify-warm.json`: the
  boot certification warm-up's one request per line (the line's synthetic PAS request bundle
  against the versioned request-bundle profile) and the lane's answer. The lanes carry no X12
  terminology, so each answer holds errors: the warm-up reads only that an answer came.

The same run also qualified each lane as its own line; those answers were checked to equal the
committed `lane-<line>-warm.json` and `lane-<line>-metadata.json` beside the validator code, which
the success rows replay.

Scrub: answers are stored decoded (a client that asked for gzip got it); headers are kept only
where a client reads them (`Content-Type`); `Date`, `X-Request-Id`, `X-Powered-By`,
`Content-Location` and `Location` are dropped. A request answered the same way every time is kept
once. Besides IG and terminology canonical URLs, the recordings name only addresses the fixtures
carry: `shn.example`, `example.org` and `smarthealth.network`. Nothing else is changed.
