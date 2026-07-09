# Contributing

This repository is a **published, byte-for-byte snapshot** of the gateway as it
runs inside the Smart Health Network — it is not built up from pull requests
merged directly here. If you send a PR, we'll read it, but the actual change
lands through the internal platform repo and reaches this repo on the next
publish. Please don't be surprised if we ask you to describe the change instead
of reviewing a diff.

## How to reach us

- **Bugs and feature requests:** open a GitHub issue on this repository.
- **Security issues:** do **not** open a public issue — see
  [`SECURITY.md`](SECURITY.md) for private reporting.
- **Partner integration questions:** open an issue, or use the same channel you
  used to request your developer account.

## Versioning and stability

See [`STABILITY.md`](STABILITY.md) for what's safe to depend on across
versions, and the release history at
[github.com/SmartHealthNetwork/shn-gateway/releases](https://github.com/SmartHealthNetwork/shn-gateway/releases).

---

## Public-docs vocabulary rules

There was no written style guide for this repo's docs before this file. This is
it — hold the line on these when editing any `.md` file here, so future edits
don't drift back into internal-only framing.

1. **No prose "substrate."** Never write "the substrate," "substrate router,"
   "substrate endpoints," or "crosses the substrate" as a description of the
   network. **Do** keep the literal route name where it's a real, live
   endpoint: `POST /substrate/inbound` is correct as written — it's an actual
   path a gateway serves, not a stand-in for the concept. The distinction: is
   the word naming a URL path, or explaining an idea? Only the former is
   allowed.
2. **No "sandbox" as a partner path or concept.** The preview-only synthetic
   environment that word used to describe is being retired as something a
   partner integrates against. Don't reintroduce it as a first-class concept,
   a path a reader can choose, or a framing device for the whole document.
   Preview infrastructure (hostnames like `shn-preview.org`, example
   credentials, etc.) can still appear where it's genuinely the example value
   to use — just don't build the doc's narrative around "this is a preview
   sandbox, everything here is throwaway."
3. **Forward-looking voice, not preview-pinned.** Describe capabilities in
   their intended production form. Don't lead a document with a
   synthetic-data-only / "never real PHI here" banner as its frame — that
   caveat belongs in the access/onboarding flow (where it's genuinely true
   today), not as the lens the whole doc is read through. Be specific about
   what's actually unshipped; don't hedge everything as "just a preview."
4. **Canonical terms, not product-facing names.** Use: **Hub**, **Smart
   Gateway**, **Authorization Framework**, **Federated Query Services**,
   **PHG**, **Global Person Consent**, **Audit Plane**, **holder**, **direct
   operator**. Avoid internal-only synonyms and marketing aliases for these
   same components. ("Smart Health account" is acceptable *only* as
   patient-facing copy for the PHG, never as the technical term in partner
   docs.)

When in doubt, prefer the concrete and specific (a real endpoint, a real env
var, a real error message) over an abstract description of "the network" —
concrete details don't need euphemisms.
