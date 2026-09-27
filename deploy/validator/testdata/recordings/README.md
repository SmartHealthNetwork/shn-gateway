# Validator lane recordings

What a real HAPI validator lane answered, captured on 2026-09-26 from the three local lanes
`make validate` boots (the 2.0, 2.1 and 2.2 contract lines, each with its line's IG closure and
the shared validation-support package), through an exact-bytes recording proxy. No participant
or patient data.

- `lane-2.0-warm.json`, `lane-2.1-warm.json`, `lane-2.2-warm.json`: the full readiness corpus
  (42 `$validate` requests per line) and each answer. The requests are the validator code's own:
  its readiness warm-up ran against each lane, and then its verification pass; every request
  body is a committed synthetic fixture. A request whose last answers were byte-identical is
  kept once and marked `repeat`. An earlier, different answer stays in order: on the 2.2 lane
  the first validation of each of two ClaimResponse profile forms (versioned and unversioned)
  reported extra `SLICING_CANNOT_BE_EVALUATED` issues (6 issues, then 3 on every later
  request), the lane still warming.
- The verification pass asked 20 of those requests and got, on every line, exactly the settled
  answer the warm-up recorded; the warm recording therefore serves it too, and no separate file
  is kept.
- `lane-2.0-metadata.json`, `lane-2.1-metadata.json`, `lane-2.2-metadata.json`: each lane's
  answer to `GET /fhir/metadata`, a CapabilityStatement, from a request issued by hand with curl.
- `lane-2.0-errors.json`, `lane-2.1-errors.json`, `lane-2.2-errors.json`: three requests issued by
  hand with curl that each lane refuses, and its refusal: `$validate` on an unknown resource
  type (404, HAPI-0302), `$validate` of a truncated JSON body (400, HAPI-0450) and a `GET` of a
  path that names no resource type (404, HAPI-0302). The request bodies are written for the
  purpose (`{"resourceType":"Patient","id":"x"}` and its truncation). Every recorded `$validate`
  in the warm files answers 200, negative controls included; these refusals are the only
  non-200 answers the lanes were seen to send.

Scrub: answers are stored decoded (a client that asked for gzip got it); headers are kept only
where a client reads them (`Content-Type`); `Date`, `X-Request-Id`, `X-Powered-By` and
`Content-Location` are dropped. The CapabilityStatement's `rest` listing (about 2 MB of resource
types and operations, which no client reads) is replaced with `[]` and declared in `scrubbed`
with the captured answer's size and SHA-256, so a replay does not carry the answer's real size
(the metadata size bound has its own tests). The rest of each CapabilityStatement is kept as the
lane sent it, including its generated `id`, its build `date` and its `implementation.url`, the
local address the proxy reached that lane at (`localhost:8087`, `localhost:8086` and
`localhost:8098` for 2.0, 2.1 and 2.2). Besides those, the XHTML namespace of the statement's
narrative, and IG and terminology canonical URLs, the recordings (requests and answers) name only
addresses the fixtures carry: `example.org` (a deliberately unavailable profile),
`shn.example`, `localhost:8081` and `smarthealth.network`. Nothing else is changed. The same
files sit beside each copy of the validator code, which a fence keeps identical.
