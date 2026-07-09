#!/usr/bin/env bash
# gateway/deploy/eval/brprovider/reject_test.sh — prove the ingress enforces auth.
#
# With the eval-bundle gateway up (PROVIDER_DAVINCI_INGRESS=1, INGRESS_CLIENTS_FILE
# registered — compose.eval.yml), a request bearing NO / a wrong bearer to a Da Vinci
# ingress route must be 401 — never silently accepted.
#
# /Claim/$submit is handlePASIngress (gateway/engine/gateway.go, mounted only when
# g.cfg.IngressEnabled), which calls g.ingressAuthOK(r) FIRST and 401s before any body
# parsing (gateway/engine/ingress.go). So 401 here proves the route exists AND is
# auth-gated — a 404 would mean the ingress never mounted (a config regression), not
# "correctly rejected".
set -euo pipefail

GATEWAY_URL="${GATEWAY_URL:-http://127.0.0.1:8080}"

code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 15 -X POST \
  "${GATEWAY_URL}/Claim/\$submit" -H 'Content-Type: application/fhir+json' -d '{}')
[ "$code" = "401" ] && echo "✓ ingress rejects unauthenticated ($code)" || { echo "✗ ingress did not reject: $code"; exit 1; }

# Wrong bearer (garbage token, not a registered client's signature) must ALSO 401 — the
# no-bearer case alone would not catch an ingress that fails open on an unparseable/
# unverifiable Authorization header.
code2=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 15 -X POST \
  "${GATEWAY_URL}/Claim/\$submit" -H 'Content-Type: application/fhir+json' \
  -H 'Authorization: Bearer not-a-real-jwt' -d '{}')
[ "$code2" = "401" ] && echo "✓ ingress rejects wrong bearer ($code2)" || { echo "✗ ingress did not reject wrong bearer: $code2"; exit 1; }
