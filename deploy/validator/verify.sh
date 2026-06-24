#!/usr/bin/env bash
# FR-G3 repeatable gate: prove the validator loads its Da Vinci IGs OFFLINE and
# that $validate can RESOLVE those profiles. Offline is enforced BY CONSTRUCTION:
# the validator runs on an --internal docker network (no egress), so if any IG — or
# a transitive dependency — were not baked, $validate?profile=<canonical> reports
# "Invalid profile. Failed to retrieve profile with url=...". Pass criterion: all
# four Da Vinci profiles (PAS/DTR/PDex/CDex) RESOLVE with the network isolated. A bare
# OperationOutcome is NOT sufficient — HAPI returns one even when the profile
# silently failed to load.
set -euo pipefail

# Self-locating: the build context and the probe fixtures are this script's own
# directory, so the gate runs unchanged from the monorepo OR a bare snapshot clone.
DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE="shn-validator:dev"
NET="shn-validator-verify-net"
CONTAINER="shn-validator-verify"
CURL="curlimages/curl:8.11.1"
BASE="http://${CONTAINER}:8080/fhir"

cleanup() {
  docker rm -f "${CONTAINER}" >/dev/null 2>&1 || true
  docker network rm "${NET}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "building ${IMAGE}..."
docker build -t "${IMAGE}" "${DIR}"
docker pull "${CURL}" >/dev/null   # host egress only; the validator stays offline below

cleanup
docker network create --internal "${NET}" >/dev/null   # --internal == no egress, by construction
docker run -d --name "${CONTAINER}" --network "${NET}" "${IMAGE}" >/dev/null

# probe runs a curl helper ON the isolated network (the validator publishes no host port).
probe() { docker run --rm --network "${NET}" -v "${DIR}/testdata:/golden:ro" "${CURL}" "$@"; }

echo "waiting for ${BASE}/metadata with the network ISOLATED (IG indexing on first boot may take several minutes)..."
ready=0
for _ in $(seq 1 240); do
  if [ "$(probe -s -o /dev/null -w '%{http_code}' "${BASE}/metadata")" = "200" ]; then ready=1; break; fi
  sleep 5
done
[ "${ready}" = "1" ] || { echo "FAIL: metadata never served offline (baked IGs/deps did not load)"; docker logs "${CONTAINER}" | tail -60; exit 1; }

# (golden | resourceType | Da Vinci canonical) — these canonicals must resolve on
# the 8-IG HAPI; the check is profile RESOLUTION, not a bare 200.
probes=(
  "claim-bundle.json|Bundle|http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle"
  "questionnaireresponse-autofill.json|QuestionnaireResponse|http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-questionnaireresponse"
  "eob-approved.json|ExplanationOfBenefit|http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/pdex-priorauthorization"
  "cdex-task-data-request.json|Task|http://hl7.org/fhir/us/davinci-cdex/StructureDefinition/cdex-task-data-request"
)
for p in "${probes[@]}"; do
  IFS='|' read -r file rt canon <<<"${p}"
  probe -s -X POST "${BASE}/${rt}/\$validate?profile=${canon}" \
    -H 'Content-Type: application/fhir+json' --data-binary "@/golden/${file}" \
  | python3 -c '
import json,sys
canon=sys.argv[1]; oo=json.load(sys.stdin)
if oo.get("resourceType")!="OperationOutcome": sys.exit("FAIL: non-OperationOutcome for %s"%canon)
for i in oo.get("issue",[]):
    if "failed to retrieve profile" in i.get("diagnostics","").lower():
        sys.exit("FAIL: profile NOT resolved offline (IG/dep not baked): %s"%canon)
print("OK: resolved offline -> %s"%canon)
' "${canon}"
done
echo "VALIDATOR OFFLINE VERIFY OK (PAS/DTR/PDex/CDex profiles resolved with the network isolated)"
