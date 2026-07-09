#!/usr/bin/env sh
# gateway/deploy/eval/brprovider/gencerts.sh — generate the conformant-lane UDAP crypto.
#
# Runs as a compose init service (image: alpine/openssl) BEFORE gateway + br-provider start
# (depends_on: condition: service_completed_successfully). Emits into $SECRETS (a shared
# compose volume): provider-cert.pfx (br-provider's outbound CDS-client signing identity) +
# ingress-clients.json (the gateway ingress's inbound client registration). client_id ==
# br-provider's SECURITY_EXTERNAL_BASE_URL == the ingress registration's client_id, so the
# same RS384 keypair serves both sides of the UDAP B2B handshake.
#
# Design: generate-at-first-boot, no private key committed, no go-pkcs12 dependency
# (openssl emits the .pfx; Go stdlib cannot). Idempotent: a second run finds
# provider-cert.pfx already present and exits without regenerating (a fresh boot into an
# already-populated volume is a no-op, not a key rotation).
#
# NOTE: this container has no python3 (alpine/openssl is openssl-only), so ingress-clients.json
# is built with printf/awk instead of a JSON library. The emitted shape MUST stay byte-identical
# in SCHEMA to what gateway/app/app.go's loadIngressClients unmarshals into:
#   {client_id string, alg string, public_key_pem string, scopes []string}
# public_key_pem is the SubjectPublicKeyInfo (PKIX) PEM — `openssl x509 ... -pubkey -noout` —
# NOT the certificate PEM; jwt.ParseRSAPublicKeyFromPEM parses PKIX first.
set -eu

SECRETS="${SECRETS:-/secrets}"
CLIENT_ID="${BRPROVIDER_CLIENT_ID:-http://br-provider:8080}"

mkdir -p "$SECRETS"

# Idempotency guard: a re-run against an already-populated volume (e.g. a compose restart
# that didn't clear the certs volume) must NOT regenerate — br-provider and the gateway would
# then disagree on which keypair is live until both are also restarted.
[ -f "$SECRETS/provider-cert.pfx" ] && { echo "certs present"; exit 0; }

openssl req -x509 -newkey rsa:2048 -sha256 -days 365 -nodes \
  -keyout "$SECRETS/provider-key.pem" -out "$SECRETS/provider-cert.pem" -subj "/CN=br-provider"

# PKCS12 for br-provider's CertificateHolder (single alias "provider", RSA key + self-signed
# cert) — this is the artifact br-provider's SECURITY_CERT_FILE mounts.
openssl pkcs12 -export -inkey "$SECRETS/provider-key.pem" -in "$SECRETS/provider-cert.pem" \
  -out "$SECRETS/provider-cert.pfx" -passout pass:udap-test -name provider

# br-provider reads this .pfx as a non-root uid (65532) from the shared `certs` NAMED volume,
# where Linux uid/mode are enforced strictly (a host bind mount is uid-remapped and hides this).
# openssl writes key-bearing files 0600/root, so without this the CertificateHolder bean dies
# with "Permission denied" and br-provider crash-loops. The cert is synthetic, per-run, and
# never leaves the loopback compose network, so world-readable inside the volume is fine.
chmod 0644 "$SECRETS/provider-cert.pfx"

# The SubjectPublicKeyInfo (PKIX) PEM the gateway ingress registers via loadIngressClients.
openssl x509 -in "$SECRETS/provider-cert.pem" -pubkey -noout > "$SECRETS/provider-pub.pem"

# Build ingress-clients.json WITHOUT python3 (alpine/openssl has no interpreter): fold the
# multi-line PEM into literal "\n" escapes with awk, then printf a single-element JSON array.
# Field names (client_id/alg/public_key_pem/scopes) match gateway/app/app.go's
# loadIngressClients json tags exactly.
PUB=$(awk '{printf "%s\\n", $0}' "$SECRETS/provider-pub.pem")
printf '[{"client_id":"%s","alg":"RS384","public_key_pem":"%s","scopes":["system/*.read"]}]\n' \
  "$CLIENT_ID" "$PUB" > "$SECRETS/ingress-clients.json"

echo "generated conformant-lane crypto in $SECRETS"
