#!/usr/bin/env bash
# gateway/deploy/eval/brprovider/originate.sh <uc> — POSITIVE proof that the GENERATED
# conformant-lane cert (gencerts.sh) is valid end to end: drive the REAL br-provider
# container to originate one conformant UC through the gateway's Da Vinci ingress, signed
# with its mounted provider-cert.pfx, and assert the leg completes (a CDS Hooks cards
# response — not an auth/transport error).
#
# Mechanism — ported from gateway/scenariodriver/brprovider.go's OriginateThroughBRProvider,
# the same call SHN uses to prove this exact leg against a real HL7-DaVinci/br-provider RI:
#
#   POST {BFF}/api/cds-services/order-select-crd?server=<url-escaped {INGRESS_BASE}/cds-services>
#   Header: X-Bypass-Auth: true   (br-provider's org.hl7.davinci.security.AuthInterceptor /
#     SecurityProperties.bypassHeader — skips the BFF's OWN inbound-request auth check on
#     THIS call only; it does NOT touch the outbound CDS-client JWT br-provider's
#     CdsHooksProxyController signs with its configured cert before relaying to `server`.
#     So a 200 cards response here is genuine end-to-end proof: br-provider's real signing
#     path (provider-cert.pfx, the artifact under test) + the gateway ingress's real bearer
#     verification (ingress-clients.json, the artifact under test) both worked.)
#   Body: a conformant CDS Hooks order-sign request (gateway/scenariodriver/build.go's
#     BuildCRDRequest shape, transcribed here in jq since this is a shell script).
#
# member/code (uc02): Patient MBR-PD-UC02 / HCPCS E0250 "Hospital Bed with Side Rails" —
# sdk/fixtures/providerdata/uc02.json's seeded persona (identifier urn:shn:member|MBR-PD-UC02,
# loaded by evalseed) and gateway/scenariodriver's PersonaOrders["noPA"] code. The
# member must resolve via the gateway's real FHIR SoR ResolvePatient (gateway/engine/
# ingress_crd.go's subject fence: g.cfg.SoR.ResolvePatient searches the seeded HAPI by that
# identifier) — an unseeded/misspelled member 403s "unknown member" here, not 401, so a 403
# means "check the seed", not "check the cert".
# Coverage.payor: CMSPayerIdentity (urn:oid:2.16.840.1.113883.6.300|00001) — routes via
# payer-directory.json to the hosted `conformance-payer`.
#
# This script runs on the HOST (same convention as reject_test.sh / smoke.sh) and reaches
# br-provider's BFF at the host-published port compose.eval.yml maps for the br-provider
# service (127.0.0.1:8082 -> container 8080). Override with BRPROVIDER_BFF_URL if you remap it.
set -euo pipefail

UC="${1:?usage: originate.sh <uc>  (e.g. uc02)}"

BRPROVIDER_BFF_URL="${BRPROVIDER_BFF_URL:-http://127.0.0.1:8082}"
# Config-pinned aud base (PROVIDER_DAVINCI_INGRESS_BASE_URL, compose.eval.yml) — the
# CONTAINER address br-provider's BFF calls out to. Never the host-published gateway port
# (aud mismatch -> 401; see gateway/engine/ingressauth.go's audUnder).
INGRESS_BASE_URL="${INGRESS_BASE_URL:-http://gateway:8080}"

case "$UC" in
  uc02)
    MEMBER="MBR-PD-UC02"
    CODE="E0250"
    DISPLAY="Hospital Bed with Side Rails"
    ;;
  *)
    echo "originate.sh: unsupported uc '${UC}' (only uc02 is wired today — see header comment)" >&2
    exit 2
    ;;
esac

CMS_SYSTEM="urn:oid:2.16.840.1.113883.6.300"
CMS_VALUE="00001"

req_body=$(jq -n \
  --arg member "$MEMBER" --arg code "$CODE" --arg display "$DISPLAY" \
  --arg cmsSystem "$CMS_SYSTEM" --arg cmsValue "$CMS_VALUE" \
  '{
    hook: "order-sign",
    hookInstance: "shn-eval-originate",
    fhirServer: "https://provider.example/fhir",
    context: {
      userId: "Practitioner/p1",
      patientId: $member,
      draftOrders: {
        resourceType: "Bundle",
        type: "collection",
        entry: [
          {
            fullUrl: "urn:uuid:sr1",
            resource: {
              resourceType: "ServiceRequest",
              id: "sr1",
              status: "draft",
              intent: "order",
              code: { coding: [ { system: "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets", code: $code, display: $display } ] },
              subject: { reference: ("Patient/" + $member) },
              insurance: [ { reference: "Coverage/c1" } ]
            }
          }
        ]
      }
    },
    prefetch: {
      patient: { resourceType: "Patient", id: $member },
      coverage: {
        resourceType: "Coverage",
        id: "c1",
        status: "active",
        beneficiary: { reference: ("Patient/" + $member) },
        payor: [ { reference: "#cms-payer" } ],
        contained: [
          {
            resourceType: "Organization",
            id: "cms-payer",
            identifier: [ { system: $cmsSystem, value: $cmsValue } ],
            name: "Centers for Medicare and Medicaid Services"
          }
        ]
      }
    }
  }')

server_param=$(jq -rn --arg s "${INGRESS_BASE_URL}/cds-services" '$s|@uri')
endpoint="${BRPROVIDER_BFF_URL}/api/cds-services/order-select-crd?server=${server_param}"

# Guard the command substitution: a connection error to br-provider's BFF (still booting,
# or wrong port) makes curl exit non-zero, which under `set -e` would abort this script
# BEFORE the diagnostic below — masking a reachability problem as a generic cert failure
# (this is exactly what hid the pfx-permission bug during bring-up). Surface it instead.
resp_file="$(mktemp)"
if ! http_status=$(curl -s -o "$resp_file" -w '%{http_code}' --connect-timeout 5 --max-time 30 \
  -X POST "$endpoint" -H 'Content-Type: application/json' -H 'X-Bypass-Auth: true' -d "$req_body"); then
  echo "✗ conformant ${UC}: could not reach br-provider BFF at ${BRPROVIDER_BFF_URL} (is it up?)" >&2
  rm -f "$resp_file"; exit 1
fi
resp_body=$(cat "$resp_file")
rm -f "$resp_file"

if [ "$http_status" = "200" ] && echo "$resp_body" | jq -e '.cards' >/dev/null 2>&1; then
  echo "✓ conformant ${UC} completed through the ingress (generated cert valid end to end)"
  echo "  cards: $(echo "$resp_body" | jq -c '.cards')"
  exit 0
else
  echo "✗ conformant ${UC} did not complete: HTTP ${http_status}" >&2
  echo "  body: ${resp_body}" >&2
  exit 1
fi
