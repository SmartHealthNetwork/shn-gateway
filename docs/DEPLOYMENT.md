# Deploying the Smart Gateway

The gateway is a stateless container. It runs as an unprivileged user
(`65532:65532`, distroless) and listens on **port 8080** by default.

Because it runs unprivileged it cannot bind ports below 1024. Do not try to make
it listen on 443 directly — publish it on 443 at your load balancer, or set
`PORT` to a high port such as `8443`. Both are one-line changes.

## Two supported topologies

### 1. Terminate TLS at your load balancer (recommended)

The common production shape. Your ALB / NLB / ingress controller holds the
certificate and forwards to the container.

```
Internet ──https:443──▶ ALB (ACM cert) ──http:8080──▶ gateway container
```

- Target group protocol HTTP, port 8080.
- Health check path: `/health`.
- The address you register (`--base-url`) is the **load balancer's** public
  https URL, not the container's.

### 2. Terminate TLS inside the container

For hops that must not carry plaintext even inside your own network — for
example an EHR or interface engine pushing PHI into the Da Vinci ingress, which
is authenticated with short-lived bearer tokens. Mount a certificate and set
both variables:

```
-e PORT=8443 \
-e TLS_CERT_FILE=/etc/shn/tls/tls.crt \
-e TLS_KEY_FILE=/etc/shn/tls/tls.key \
-v /your/certs:/etc/shn/tls:ro \
-p 8443:8443
```

- Both variables are required together. Setting one without the other is a
  startup error — the gateway will not fall back to plaintext.
- TLS 1.2 is the minimum accepted version.
- A bad or unreadable certificate fails at startup with a clear message, before
  the gateway reports that it is listening.
- The certificate is read once at startup. To rotate, restart the container.
- Files must be readable by uid `65532`.
- With TLS enabled the listener also offers HTTP/2 via ALPN (standard Go
  behavior); the plain-HTTP default speaks HTTP/1.1 only. All gateway routes
  are ordinary request/response, so both protocols behave identically.

On success the startup line names the scheme:

```
gateway: role=provider holder=<id> listening on https://0.0.0.0:8443
```

## Your registered address must be https

Registration **rejects** any `baseURL` that is not a publicly resolvable https
URL — this is enforced, not advisory. A gateway fronted by plain http cannot be
registered at all. Private, loopback, and link-local addresses are refused too.

This applies to responders (payer, facility) that receive delivered requests.
Originator-only participants are never dialed, but the URL must still validate.

## What is already protected without any TLS configuration

TLS protects the hop. It is not what protects the payload:

- **Payloads are encrypted end to end** to the recipient's key. Intermediaries
  route on metadata and cannot read the contents.
- **Every inbound delivery is authenticated** with a signed, single-use,
  short-TTL assertion that is verified before the payload is touched. This
  verification has no "off" state — a gateway that cannot verify it will not
  start.
- **Authority is carried separately** from the channel, in a bound token
  checked per leg. Neither substitutes for the other.

So the case for configuring TLS is defense in depth and your own network policy
— not payload confidentiality on the delivery path, which does not depend on it.

## Running more than one replica

**The Da Vinci ingress currently supports a single reachable instance.**

The ingress authorization server signs its bearer tokens with a key generated in
memory at startup. With two or more replicas behind a load balancer, a client
that obtains a token from one replica and presents it to another will be
rejected, and restarts invalidate outstanding tokens.

If you use the ingress push routes (`/cds-services/{id}`,
`/Questionnaire/$questionnaire-package`, `/Claim/$submit`), run a single
reachable instance for now. Shared-key token signing is planned; please tell us
if multi-replica ingress is a requirement for your deployment so we can
sequence it.

Deployments that do not use the ingress push routes are not affected by this.

## Checklist

- [ ] Container runs as `65532:65532`; certificate files readable by that uid
- [ ] TLS terminated at the load balancer, or `TLS_CERT_FILE` + `TLS_KEY_FILE` set
- [ ] `PORT` matches what the load balancer targets
- [ ] Health check on `/health`
- [ ] Registered `--base-url` is the public https address, and does not redirect
- [ ] Single reachable instance if the Da Vinci ingress routes are in use
