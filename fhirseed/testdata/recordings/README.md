# Data server recordings

What a real multitenant HAPI data server answered this package's validator warm-up
(`WarmValidate`), captured on 2026-09-26 (local time; timestamps inside the answers are UTC,
2026-09-27) through an exact-bytes recording proxy from the data-plane HAPI that `make validate`
boots (HAPI FHIR 8.10.0, URL-partitioned multitenant), freshly booted. The request body is the
fixed US Core Patient in `warm.go`. No participant or patient data.

- `warm-default.json`: `WarmValidate("DEFAULT")` twice on the freshly booted server. The first
  `$validate` after boot took 1.84 s and the second 29 ms. The capture measured those times; a
  replay carries bytes, not timing, so a test that needs the slow first answer adds the delay
  itself and says so. The two answers are byte-identical: 200 with an OperationOutcome holding
  one warning (`dom-6`: the resource has no narrative). A cold server gives the same answer as
  a warm one, only later.
- `warm-unknown-tenant.json`: `WarmValidate("nosuchtenant")`, a tenant the server has no
  partition for. The server answered 200 with the same OperationOutcome: a type-level
  `$validate` validates the posted resource and does not resolve the partition. So the warm-up
  warms the server whatever the tenant, and its success says nothing about the tenant; the
  seeder's next request writes to the tenant and fails there if the tenant is missing.

Scrub: answers are stored decoded (the client asked for gzip); headers are kept only where a
client reads them (`Content-Type`); `Date`, `X-Request-Id` and `X-Powered-By` are dropped. These
answers name no host. Every exchange is kept in capture order and none is marked `repeat`, so a
replay allows exactly as many warm-ups as the capture made. Nothing else is changed.
