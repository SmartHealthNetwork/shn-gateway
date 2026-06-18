# shn-gateway

The **SHN Smart Gateway** is a partner-deployable component that carries a
holder's exchanges across organizational boundaries behind the SHN Hub, with
sealed-leg authority. The substrate is workflow-general; the first workflow
delivered on it is Da Vinci prior authorization. The canonical private source
lives in the SHN substrate repository; this repository is a published snapshot.

## Install

This repository is **private**. Set `GOPRIVATE` so the Go toolchain does not
attempt the public proxy, and ensure your Git client can authenticate to
GitHub (a personal access token with `repo` scope, or an SSH key added to
your GitHub account):

```sh
export GOPRIVATE=github.com/SmartHealthNetwork/shn-gateway
```

Then either install the binary directly:

```sh
go install github.com/SmartHealthNetwork/shn-gateway/cmd/gateway@v0.1.0
```

Or build the Docker image from this repository's standalone Dockerfile
(context is this module; `shn-sdk` resolves from the public Go proxy):

```sh
docker build -t shn-gateway .
```

## Get a bundle

Before running the gateway, obtain a key bundle by registering with the SHN
Accounts service. Use the `shn register` command from the public
[`shn-sdk`](https://github.com/SmartHealthNetwork/shn-sdk) CLI:

```sh
go install github.com/SmartHealthNetwork/shn-sdk/cmd/shn@latest
shn register --out /etc/shn/bundle
```

`shn register` generates your keys client-side, performs Trust-gated
admission, and writes the bundle — `manifest.json` plus key files — that the
gateway loads via `shnsdk.LoadBundle`. The holder ID is server-assigned.

## Run

The gateway is **config-only**: point it at the SHN discovery endpoint, set
your role, and give it the bundle directory. No other configuration is
required for a standard deployment.

| Env var | Required | Description |
|---|---|---|
| `SHN_DISCOVERY_URL` | yes | SHN discovery endpoint — resolves substrate endpoints and trust anchors |
| `ROLE` | yes | `provider`, `payer`, `facility`, or `phg` |
| `SHN_SECRETS` | yes | Path to the directory written by `shn register` |
| `SHN_STORE_DATABASE_URL` | no | Postgres DSN for durable claim-state storage (default: in-memory) |
| `FHIR_DATA_URL` | no | FHIR server base URL for your system of record (default: built-in stub) |
| `PORT` | no | Listening port (default: `8080`) |

Minimal `docker run` example:

```sh
docker run --rm \
  -e SHN_DISCOVERY_URL=https://accounts.example.shn.health/discovery \
  -e ROLE=provider \
  -e SHN_SECRETS=/etc/shn/bundle \
  -v /your/bundle/dir:/etc/shn/bundle:ro \
  -p 8080:8080 \
  shn-gateway
```

### Nonroot image

The Docker image runs as **uid/gid 65532** (distroless nonroot). A
self-mounted bundle directory and its secret files must be readable by
gid 65532. Use group-readable permissions: `0640` for files, `0750` for
directories, with group ownership set to `65532`. Our provisioning tooling
applies these permissions automatically.

## Further reading

- `STABILITY.md` — versioning and supported seam contract
- [`shn-sdk`](https://github.com/SmartHealthNetwork/shn-sdk) — the public
  participant SDK (`shn register`, wire vectors, participant protocol)
- `connectors/` — override seams for your system of record, durable store,
  and FHIR SMART auth
