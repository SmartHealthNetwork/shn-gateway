# Validator lane recordings (certification)

What a real HAPI validator lane answered to certification requests, captured on 2026-09-27 from
three local HAPI FHIR 8.10.0 validator lanes, one each for the 2.0, 2.1 and 2.2 contract lines,
through an exact-bytes recording proxy. Each lane runs the digest-pinned engine and backported WAR
the Smart Gateway's `deploy/validator` builds on, loaded with its line's IG closure and the shared
validation-support package. The requests are the validator code's own: the certification client
(`NewCertificationOperationValidator`) sent each one. No participant or patient data.

- `lane-2.0-certify-literal.json`, `lane-2.1-certify-literal.json`,
  `lane-2.2-certify-literal.json`: the literal `{"resourceType":"Claim"}` against the profile
  `profile` and against no profile. Each lane finds it invalid: eight
  `Validation_VAL_Profile_Minimum` errors from the core Claim profile, and with `profile` also
  `Validation_VAL_Profile_Unknown` for that profile. No lane calls it valid.
- `lane-2.0-certify-collect.json`, `lane-2.1-certify-collect.json`,
  `lane-2.2-certify-collect.json`: what the collector asks a lane, at that lane's line (the
  versioned PAS Claim or request-bundle profile), for five payloads, each asked twice and
  answered identically: the literal `{"resourceType":"Claim"}`, a Claim whose `status` is
  `PRIVATE-SYNTHETIC-SENTINEL` (the lane refuses to parse it and answers 400, quoting the value in
  its diagnostics), and the synthetic PAS request bundle of each of the three lines. Every
  answer holds errors (the lanes carry no X12 terminology); none of these payloads is valid on
  any lane.

Scrub: answers are stored decoded (a client that asked for gzip got it); headers are kept only
where a client reads them (`Content-Type`); `Date`, `X-Request-Id`, `X-Powered-By`,
`Content-Location` and `Location` are dropped. A request answered the same way every time is kept
once and marked `repeat`. The sentinel value and the literal Claim are request bodies written for
the purpose. Besides IG and terminology canonical URLs, the recordings name only addresses the
fixtures carry: `shn.example` and `smarthealth.network`. Nothing else is changed.
