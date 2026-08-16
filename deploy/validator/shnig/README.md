# `shn.fhir.carry` — the SHN-authored IG package

`shn.fhir.carry-1.0.0.tgz` in this directory is a FHIR NPM package carrying the
two SHN-authored StructureDefinitions (`shn-carried-content`,
`shn-loss-report`) that appear on the wire when an exchange crosses contract
lines. Every `hapi-<line>` stage of `../Dockerfile` `COPY`s it and loads it
from `file://`, so `$validate` can resolve those canonicals offline.

## Why it is committed, when nothing else here is

Every other IG this validator bakes is fetched from Simplifier at build time.
This one is SHN-authored and not published to a public registry, so there is
nothing to fetch. And the `gateway` module is published **standalone** — its
`deploy/bundle/compose.yml` is the documented partner install unit and builds
`deploy/validator/` from the published tree itself. A build input that only
exists on a maintainer's machine makes that install unit unbuildable for
everyone else, which is precisely what happened before this file was committed.

So the artifact ships here, as a build input, exactly like the pinned base
image digest above it in the Dockerfile.

## What keeps it honest

A committed binary is only trustworthy if it is reproducible and pinned:

- **Pinned version.** `1.0.0`, from the platform's IG-pin manifest. The
  filename is part of the Dockerfile's `COPY` lines and the runtime
  `hapi.fhir.implementationguides.shnig.*` settings, so a version move is a
  visible, reviewable edit in several places at once — never a silent swap.
- **Byte-reproducible.** The builder writes sorted tar entries with fixed
  mtimes and no gzip timestamp, so the same version always produces identical
  bytes.
- **Drift-checked on every build gate.** Upstream's hermetic test gate rebuilds
  the package from source and asserts it is byte-identical to this file. A
  source edit that is not re-copied here fails loudly.

That combination is what keeps the shared validator image immutable-by-digest
rather than a mutable shared component (OWD-G7): the bytes here are a pinned,
verifiable function of a pinned version — not a snapshot someone dropped in.

## Regenerating it

It is generated, not hand-authored. Rebuild it from the platform repository's
IG-package builder and copy the result over this file; do not edit the tarball
in place.
