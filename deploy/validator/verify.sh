#!/usr/bin/env bash
# FR-G3 repeatable gate: prove the validator loads its Da Vinci IGs OFFLINE and
# that $validate can RESOLVE those profiles. Offline is enforced BY CONSTRUCTION:
# the validator runs on an --internal docker network (no egress), so if any IG — or
# a transitive dependency — were not baked, $validate?profile=<canonical> reports
# "Invalid profile. Failed to retrieve profile with url=...". Pass criterion: all
# four Da Vinci profiles (PAS/DTR/PDex/CDex) RESOLVE with the network isolated. A bare
# OperationOutcome is NOT sufficient — HAPI returns one even when the profile
# silently failed to load.
#
# Per-line validator images: the Dockerfile bakes one image PER contract line
# (ARG SHN_IG_LINE=2.0|2.1|2.2). This script builds all three and
# probes 2.0 (unchanged default) + 2.2 (the RI-facing line); the 2.1 build is
# asserted but its probe is SKIPPED by default, because three offline-IG-indexing
# boots — not the builds — are what makes this gate expensive to run everywhere.
# Set SHN_VALIDATOR_VERIFY_PROBE_2_1=1 to also probe 2.1.
set -euo pipefail

# Self-locating and self-contained: the build context, the probe fixtures AND the
# SHN-authored IG package the Dockerfile COPYs (shnig/shn.fhir.carry-*.tgz — see
# that directory's README.md for why it is a committed build input) all live in
# this script's own directory. Nothing above it is read, so the gate runs
# unchanged from a bare clone of this repository.
DIR="$(cd "$(dirname "$0")" && pwd)"
CURL="curlimages/curl:8.11.1"
NET="shn-validator-verify-net"

cleanup_all() {
  docker rm -f shn-validator-verify-2.0 shn-validator-verify-2.1 shn-validator-verify-2.2 >/dev/null 2>&1 || true
  docker network rm "${NET}" >/dev/null 2>&1 || true
}
trap cleanup_all EXIT

echo "pulling ${CURL} (host egress only; every validator below stays offline)..."
docker pull "${CURL}" >/dev/null

cleanup_all
docker network create --internal "${NET}" >/dev/null   # --internal == no egress, by construction

# build_line LINE IMAGE — builds the per-line sidecar image (network at BUILD
# time only, for the IG package downloads baked into the image).
build_line() {
  local line="$1" image="$2"
  echo "building ${image} (SHN_IG_LINE=${line})..."
  docker build --build-arg "SHN_IG_LINE=${line}" -t "${image}" "${DIR}"
}

# probe_line LINE IMAGE CONTAINER — boots IMAGE on the isolated network and
# proves its Da Vinci profiles resolve, using that line's own probe fixtures
# (testdata/<line>/ for the PAS/DTR pair; the top-level testdata/ PDex+CDex
# probes are line-neutral and reused for every line — see testdata/README.md).
probe_line() {
  local line="$1" image="$2" container="$3"
  local base="http://${container}:8080/fhir"

  docker run -d --name "${container}" --network "${NET}" "${image}" >/dev/null

  # probe runs a curl helper ON the isolated network (the validator publishes no host port).
  probe() { docker run --rm --network "${NET}" -v "${DIR}/testdata:/golden:ro" "${CURL}" "$@"; }

  echo "waiting for ${base}/metadata (line ${line}; network ISOLATED; IG indexing on first boot may take several minutes)..."
  local ready=0
  for _ in $(seq 1 240); do
    if [ "$(probe -s -o /dev/null -w '%{http_code}' "${base}/metadata")" = "200" ]; then ready=1; break; fi
    sleep 5
  done
  if [ "${ready}" != "1" ]; then
    echo "FAIL: line ${line} metadata never served offline (baked IGs/deps did not load)"
    docker logs "${container}" | tail -60
    exit 1
  fi

  # (golden | resourceType | Da Vinci canonical) — these canonicals must resolve
  # against this line's baked IGs; the check is profile RESOLUTION, not a bare
  # 200. The PAS/DTR entries use that line's own vendored golden (testdata/<line>/
  # for 2.1/2.2; the top-level testdata/ pair for 2.0, unchanged); the PDex/CDex
  # entries are line-neutral (single native line each) and always the top-level copy.
  local pas_dtr_dir="."
  if [ "${line}" = "2.1" ] || [ "${line}" = "2.2" ]; then
    pas_dtr_dir="${line}"
  fi
  local probes=(
    "${pas_dtr_dir}/claim-bundle.json|Bundle|http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-pas-request-bundle"
    "${pas_dtr_dir}/questionnaireresponse-autofill.json|QuestionnaireResponse|http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-questionnaireresponse"
    "eob-approved.json|ExplanationOfBenefit|http://hl7.org/fhir/us/davinci-pdex/StructureDefinition/pdex-priorauthorization"
    "cdex-task-data-request.json|Task|http://hl7.org/fhir/us/davinci-cdex/StructureDefinition/cdex-task-data-request"
  )
  for p in "${probes[@]}"; do
    IFS='|' read -r file rt canon <<<"${p}"
    probe -s -X POST "${base}/${rt}/\$validate?profile=${canon}" \
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
  echo "line ${line}: VALIDATOR OFFLINE VERIFY OK"
  docker rm -f "${container}" >/dev/null 2>&1 || true
}

# 2.0: unchanged default — build + probe, same bar as before the matrix.
build_line 2.0 shn-validator:dev
probe_line 2.0 shn-validator:dev shn-validator-verify-2.0

# 2.1: build asserted (proves the line's package set is baked correctly); probe
# SKIPPED by default to keep this gate's wall-time bounded (three
# offline-IG-indexing boots is the expensive part, not the build). Set
# SHN_VALIDATOR_VERIFY_PROBE_2_1=1 to also probe it locally.
build_line 2.1 shn-validator:2.1
if [ "${SHN_VALIDATOR_VERIFY_PROBE_2_1:-0}" = "1" ]; then
  probe_line 2.1 shn-validator:2.1 shn-validator-verify-2.1
else
  echo "line 2.1: build OK, probe SKIPPED (set SHN_VALIDATOR_VERIFY_PROBE_2_1=1 to probe locally)"
fi

# 2.2: the RI-facing line — build + probe, same bar as 2.0.
build_line 2.2 shn-validator:2.2
probe_line 2.2 shn-validator:2.2 shn-validator-verify-2.2

echo "VALIDATOR MATRIX OFFLINE VERIFY OK (2.0 + 2.2 probed; 2.1 build-asserted)"
