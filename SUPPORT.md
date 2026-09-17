# Support

## Where to ask

| Need | Where |
|---|---|
| A bug in the gateway, or a question about configuring or running it | A GitHub issue on this repository |
| A security vulnerability | **Not** an issue — see [`SECURITY.md`](SECURITY.md) |
| A developer account, client registration, or access to the preview network | The channel you used to request your developer account; if you don't have one yet, open an issue and we'll point you at it |
| Which seams are safe to depend on across versions | [`STABILITY.md`](STABILITY.md) first, then an issue |

## What to include in an issue

- The gateway version: the image tag you run, or the
  `github.com/SmartHealthNetwork/shn-gateway` line in your `go.mod`.
- The startup log through the first `listening on` line — it records the role,
  the holder id and the listen address, plus any configuration warning printed
  before it, which resolve most "it won't start" reports (see the
  [troubleshooting table](README.md#7-troubleshooting)).
- For a failed population, the one-line `gateway: populate_failure` record the
  gateway writes: it names the failing boundary without carrying any payload.
- What you sent and what came back, with **synthetic data only**. The preview
  network never carries real patient information, and neither should an issue.
  The `populate_failure` records are payload-free by design; other log lines are
  not covered by that guarantee, so redact before attaching them.

## What to expect

This is a preview network and a pre-1.0 module: there is no support contract,
no uptime guarantee and no response-time commitment. Issues are read and
answered by the people who build the gateway; a confirmed defect becomes an
internal change with a test and ships in the next version (see
[`CONTRIBUTING.md`](CONTRIBUTING.md) for how releases work). Questions that turn
out to be documentation gaps get fixed in the documentation.
