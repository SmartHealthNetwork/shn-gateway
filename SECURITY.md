# Security policy

## Reporting a vulnerability

Please report suspected vulnerabilities **privately** — do not open a public
issue. Use GitHub's private vulnerability reporting for this repository:
[github.com/SmartHealthNetwork/shn-gateway/security/advisories/new](https://github.com/SmartHealthNetwork/shn-gateway/security/advisories/new).
If that isn't available to you, open a regular issue that says only that you
have a security report to make — no details — and a maintainer will open a
private advisory on your behalf and continue there.

Include what you'd normally include in a report: the affected version, a
description of the issue, and — if you have one — a minimal reproduction. We'll
acknowledge receipt, investigate, and coordinate a fix and disclosure timeline
with you.

## Scope

This policy covers the code in this repository: the published `shn-gateway`
snapshot (a byte-for-byte copy of the gateway as it runs in the Smart Health
Network). Reports about the Smart Health Network's hosted services (the Hub,
Authorization Framework, registrar, and related trust-plane infrastructure) are
welcome through the same advisory link — there is no separate public channel
for them — and are routed to the right people internally.

## Our posture in one line

You hold your own keys and run your own gateway inside your own boundary —
your PHI never leaves it and is never sent to SHN, so most classes of "data
exposure" report are about your own deployment's configuration; we still want
to hear about anything in the gateway's code or defaults that could put that
guarantee at risk.
