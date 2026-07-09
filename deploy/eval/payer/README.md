# Payer responder evaluation bundle

> **This is a deploy artifact, not a laptop loopback.** A payer is a
> **Hub-dialed responder**: when a provider originates a request, the Hub
> `POST`s it to the payer's registered `baseURL + /substrate/inbound`. The
> registrar refuses to register a `127.0.0.1` or other private/loopback
> `baseURL`. That means this bundle only receives real traffic once it is
> running behind a **publicly reachable https endpoint** and has been
> registered under that public URL — see [Reachability](#reachability) below
> before you expect an end-to-end result. Running it purely on
> `127.0.0.1` will boot and serve locally, but the Hub has nothing to dial.

> **EVALUATION ONLY — NOT A PRODUCTION DEPLOYMENT.**
> This bundle ships the real Smart Gateway image (`ROLE=payer`) alongside a
> reference Da Vinci payer implementation (HL7-DaVinci's `br-payer`, built
> from pinned upstream source) so you can see every prior-authorization use
> case adjudicated by a genuine Da Vinci-conformant decisioning engine
> instead of the gateway's built-in canned responses. When you're ready to
> connect your own adjudication system, see
> [Production cutover](#production-cutover) below — the gateway you're
> running here is the same gateway you run in production; only what's
> plugged into it changes.

## What this runs

Three containers:

| Service | Role |
|---|---|
| `gateway` | The real `shn-gateway` image, configured with `ROLE=payer`, native-forwarding all CRD and PAS decisioning to `br-payer`. |
| `br-payer` | A reference Da Vinci payer application (HL7-DaVinci's `br-payer`, built from pinned upstream source) that adjudicates CRD hooks and PAS `$submit` requests conformantly, standing in for your own decisioning system. |
| `validator` | A FHIR server pre-loaded with the US Core + Da Vinci PAS implementation guides, used only for runtime `$validate` of outbound resources — it holds no data. |

There is no Store and no Postgres in this bundle. A PAS-native payer holds
no durable state of its own between requests; the gateway boots on its
in-memory stub (`gateway/app/app.go`, the same code path production runs
when `FHIR_DATA_URL` is unset). If your own adjudication system needs
durable state, that lives in *it*, not in the gateway.

## Prerequisite: an SHN developer account

You need an **approved SHN developer account** before running this bundle.
Request one at `https://developers.shn-preview.org` (no account needed to
submit the request). Once approved, register a payer client **against your
public baseURL** and download your bundle:

```sh
shn register --accounts https://accounts.shn-preview.org \
  --role payer --name my-org --base-url https://my-payer.example.com \
  -out ./my-payer-bundle
```

Keep the resulting directory — its path is what you'll pass as
`SHN_SECRETS` below. Keys are generated client-side and never leave your
machine. The `--base-url` you register **must** be the same public
endpoint the gateway is reachable at when the Hub dials it — see below.

## Reachability

Pick one of the two options below before you register — the Hub only ever
calls the `baseURL` you registered. You need that URL to
resolve to this bundle's `gateway` container (port 8085 by default) before
any end-to-end test can succeed.

**Option A — deploy to your cloud.** Run this bundle on a host or cluster
you already operate, behind your normal ingress/TLS termination, at a
stable public hostname. This is the production-like path: register once,
run repeatedly, tear down and redeploy without changing the registered URL.

**Option B — a public tunnel, for a quick eval.** Run the bundle locally
and expose it with a tunnel, e.g.:

```sh
cloudflared tunnel --url http://127.0.0.1:8085
# or: ngrok http 8085
```

Register with the tunnel's `https://` URL as `--base-url`. **The tunnel URL
is ephemeral** — most free tunnels mint a new hostname every run, and the
old registration stops resolving. Re-run `shn register` (and re-download
the bundle) each time you restart the tunnel.

Either way, until the registered `baseURL` is reachable and matches what's
running, this bundle will boot and serve locally but the Hub cannot reach
it — that's expected, not a bug.

## Run it — two steps

**Step 1 — build the reference payer.** This clones and builds `br-payer`
from pinned upstream source; there's no published image to pull.

```sh
bash ../brpayer/build.sh build
```

**Step 2 — bring up the bundle**, pointing `SHN_SECRETS` at the bundle
directory from the prerequisite step:

```sh
SHN_SECRETS=/abs/path/to/my-payer-bundle docker compose -f compose.eval.payer.yml up --build
```

Once everything reports ready, the gateway is listening at
`http://127.0.0.1:8085` and `br-payer` at `http://127.0.0.1:8081`. If
you're using Option A or B above, this is also the point at which your
public endpoint starts serving real traffic.

### First-run cost — be aware before you start

The first run is genuinely slow; subsequent runs are fast because Docker
caches the layers.

- **`brpayer/build.sh` builds from source** (a HAPI FHIR JPA server build) —
  expect several minutes the first time.
- **The validator's first boot can take a while** — it's indexing the
  implementation guides and generating its internal snapshots.

Once both are built once, later `docker compose up` runs come back in
seconds.

## Decisioning options

This bundle ships one default, but the gateway supports three ways to
answer CRD hooks and PAS submissions:

1. **Local `br-payer` (this bundle's default).** Native-forward to the
   co-bundled reference implementation — useful to see conformant
   Da Vinci decisioning behavior without standing up anything of your own.
2. **Native-forward to your own Da Vinci endpoint.** Point
   `PAYER_DAVINCI_BASE_URL` / `PAYER_DAVINCI_CDS_BASE_URL` (and the
   `PAYER_DAVINCI_*_SERVICE_ID` / `*_HOOK` pair) at your production CRD and
   PAS services instead of `br-payer`, and drop the `br-payer` service
   entirely.
3. **A custom Adjudicator.** If your decisioning doesn't speak Da Vinci CRD
   hooks or PAS natively, implement `engine.Config.Adjudicator` in a
   gateway build of your own and wire it in place of the native-forward
   path. See the main [gateway README](../../../README.md) for the
   interface.

## Production cutover

`br-payer` and `validator` are evaluation scaffolding standing in for
systems you already have. Moving to production means:

1. Start from **`gateway/deploy/bundle/`** instead of this directory —
   that's the actual production install unit (the gateway plus your own
   co-located validator, nothing else).
2. Point `PAYER_DAVINCI_BASE_URL` / `PAYER_DAVINCI_CDS_BASE_URL` at **your
   own** Da Vinci endpoint (decisioning option 2 above), or wire a custom
   `engine.Config.Adjudicator` (option 3).
3. Drop `br-payer` and `validator` entirely — they don't exist in
   `gateway/deploy/bundle/`, and nothing in the gateway depends on them.
4. Everything else about how the gateway is configured and how it
   exchanges data through the Hub is unchanged. There is no separate
   "production gateway" to learn.

See the main [gateway README](../../../README.md) for the full environment
variable reference and integration guide.
