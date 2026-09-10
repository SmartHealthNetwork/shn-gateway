#!/usr/bin/env bash
# FR-G3 repeatable gate: prove the validator loads its Da Vinci IGs OFFLINE and
# that $validate can RESOLVE those profiles. Offline is enforced BY CONSTRUCTION:
# the validator runs on an --internal docker network (no egress), so if any IG — or
# a transitive dependency — were not baked, $validate?profile=<canonical> reports
# "Invalid profile. Failed to retrieve profile with url=...". Pass criterion: all
# four Da Vinci profiles (PAS/DTR/PDex/CDex) RESOLVE with the network isolated. A bare
# OperationOutcome is NOT sufficient — HAPI returns one even when the profile
# silently failed to load. After health, the same strict PAS verdict corpus is
# asserted repeatedly through the historical 100-second first-use window.
#
# Per-line validator images: the Dockerfile bakes one image PER contract line
# (ARG SHN_IG_LINE=2.0|2.1|2.2). This script builds all three and
# probes 2.0 (unchanged default) + 2.2 (the RI-facing line); the 2.1 build is
# asserted but its probe is SKIPPED by default, because three offline-IG-indexing
# boots — not the builds — are what makes this gate expensive to run everywhere.
# Set SHN_VALIDATOR_VERIFY_PROBE_2_1=1 to also probe 2.1.
#
# Readiness is proved through the image's OWN /healthcheck (the binary every task
# definition and compose file runs), not through /metadata alone: the probe reports
# healthy only once the lane's $validate is warm (healthcheck.go), so this gate also
# asserts what that buys — a FRESH lane's first real $validate, on a warmed profile
# and on a never-touched one, answers inside the consumers' 30s client budget
# (FIRST_VALIDATE_BUDGET_SECS). Before the warm-up a cold first call took ~47s and
# every fail-closed consumer timed out.
set -euo pipefail

# Self-locating and self-contained: the build context, the probe fixtures AND the
# SHN-authored IG package the Dockerfile COPYs (shnig/shn.fhir.carry-*.tgz — see
# that directory's README.md for why it is a committed build input) all live in
# this script's own directory. Nothing above it is read, so the gate runs
# unchanged from a bare clone of this repository.
DIR="$(cd "$(dirname "$0")" && pwd)"
CURL="curlimages/curl:8.11.1"
RUN_DIR="$(mktemp -d "${TMPDIR:-/tmp}/validator-verify.XXXXXXXX")"
PREFIX="validator-verify-$(basename "${RUN_DIR}" | tr '[:upper:].' '[:lower:]-')"
NET="${PREFIX}-net"
OWNED=()
NETWORK_CREATED=0

cleanup_all() {
  local ec=$?
  if [ "${#OWNED[@]}" -gt 0 ]; then
    if [ "${ec}" -ne 0 ]; then
      for c in "${OWNED[@]}"; do docker logs "${c}" 2>&1 | tail -100 || true; done
    fi
    for c in "${OWNED[@]}"; do docker rm -f "${c}" >/dev/null 2>&1 || true; done
  fi
  if [ "${NETWORK_CREATED}" = 1 ]; then docker network rm "${NET}" >/dev/null 2>&1 || true; fi
  rm -rf "${RUN_DIR}"
  exit "${ec}"
}
trap cleanup_all EXIT

echo "pulling ${CURL} (host egress only; every validator below stays offline)..."
docker pull "${CURL}" >/dev/null
docker network create --internal "${NET}" >/dev/null
NETWORK_CREATED=1

# build_line LINE IMAGE — builds the per-line sidecar image (network at BUILD
# time only, for the IG package downloads baked into the image).
build_line() {
  local line="$1" image="$2"
  echo "building ${image} (SHN_IG_LINE=${line})..."
  docker build --build-arg "SHN_IG_LINE=${line}" -t "${image}" "${DIR}"
  local base platform
  base="$(awk '/^FROM hapiproject\/hapi@/{print $2; exit}' "${DIR}/Dockerfile")"
  docker image inspect "${image}" >"${RUN_DIR}/image.json"
  platform="$(python3 -c 'import json,sys; i=json.load(open(sys.argv[1]))[0]; print(i["Os"]+"/"+i["Architecture"]+("/"+i["Variant"] if i.get("Variant") else ""))' "${RUN_DIR}/image.json")"
  # BuildKit may retain FROM only in its build cache. Load the immutable base
  # into the image store for inspection, matching the built image's platform.
  docker pull --platform "${platform}" "${base}" >/dev/null
  docker image inspect "${base}" >"${RUN_DIR}/base.json"
  python3 "${DIR}/verify-state.py" image "${RUN_DIR}/base.json" "${RUN_DIR}/image.json" "${line}"
  echo "built line=${line} image=${image} id=$(docker image inspect -f '{{.Id}}' "${image}")"
}

# The consumers' $validate client budget: the gateway's validator client and the
# console's scenario proxy both give a $validate 30s. Every first-call timing below
# must land under it or the readiness signal is not doing its job.
FIRST_VALIDATE_BUDGET_SECS=30

# assert_under_budget LINE WHAT SECS — fails the gate when a fresh lane's first
# $validate for WHAT took SECS >= FIRST_VALIDATE_BUDGET_SECS.
assert_under_budget() {
  local line="$1" what="$2" secs="$3"
  if [ "${secs}" -ge "${FIRST_VALIDATE_BUDGET_SECS}" ]; then
    echo "FAIL: line ${line}: first \$validate of ${what} took ${secs}s on a lane /healthcheck called ready (budget ${FIRST_VALIDATE_BUDGET_SECS}s) — the warm-up did not warm what the lane serves"
    exit 1
  fi
  echo "first \$validate of ${what}: ${secs}s (budget ${FIRST_VALIDATE_BUDGET_SECS}s)"
}

# verify_verdict_window CONTAINER LINE [HEALTH_AT] — run the strict PAS corpus
# immediately after readiness and then at fixed intervals through 100 seconds.
# Each command is an assertion. A failure returns immediately without a retry.
verify_verdict_window() {
  local container="$1" line="$2" health_at="${3:-$SECONDS}"
  local elapsed=0 attempt=1
  while :; do
    echo "strict verdict verification line=${line} attempt=${attempt} minimum_after_health=${elapsed}s actual_after_health=$((SECONDS - health_at))s"
    if ! docker exec "${container}" /healthcheck verify-verdicts --budget 30s; then
      echo "FAIL: line ${line}: strict verdict verification failed on attempt ${attempt}"
      return 1
    fi
    echo "strict verdict verification completed line=${line} attempt=${attempt} actual_after_health=$((SECONDS - health_at))s"
    if [ "${elapsed}" -ge 100 ]; then
      break
    fi
    sleep 5
    elapsed=$((elapsed + 5))
    attempt=$((attempt + 1))
  done
}

# probe_line LINE IMAGE CONTAINER — boots IMAGE on the isolated network and
# proves its Da Vinci profiles resolve, using that line's own probe fixtures
# (testdata/<line>/ for the PAS/DTR pair; the top-level testdata/ PDex+CDex
# probes are line-neutral and reused for every line — see testdata/README.md).
probe_line() {
  local line="$1" image="$2" container="$3"
  local base="http://${container}:8080/fhir"

  local boot=${SECONDS}
  docker create --name "${container}" --network "${NET}" "${image}" >/dev/null
  OWNED+=("${container}")
  docker start "${container}" >/dev/null

  # probe runs a curl helper ON the isolated network (the validator publishes no host port).
  probe() { docker run --rm --network "${NET}" -v "${DIR}/testdata:/golden:ro" "${CURL}" --connect-timeout 4 --max-time 30 "$@"; }

  echo "waiting for ${container} readiness (line ${line}; ISOLATED; PID-1 warm-up budget 600s)..."
  local ready=0
  while [ "$((SECONDS - boot))" -lt 600 ]; do
    if docker exec "${container}" /healthcheck >/dev/null 2>&1; then ready=1; break; fi
    if [ "$(docker inspect -f '{{.State.Running}}' "${container}")" != true ]; then break; fi
    sleep 1
  done
  if [ "${ready}" != 1 ] || [ "$((SECONDS - boot))" -ge 600 ]; then
    echo "FAIL: line ${line}: no readiness inside startup budget"
    docker exec "${container}" /healthcheck || true
    exit 1
  fi
  local health_at=${SECONDS}
  echo "start-to-ready line=${line} elapsed=$((SECONDS - boot))s"
  verify_verdict_window "${container}" "${line}" "${health_at}"
  docker cp "${container}:/tmp/shn-validator-warm" "${RUN_DIR}/warm.json"
  python3 "${DIR}/verify-state.py" ready "${line}" <"${RUN_DIR}/warm.json"
  docker logs "${container}" >"${RUN_DIR}/warm-before.log" 2>&1
  python3 "${DIR}/verify-state.py" logs "${line}" <"${RUN_DIR}/warm-before.log"
  for _ in $(seq 1 6); do docker exec "${container}" /healthcheck; done
  echo "OK: line ${line} observer ready and supervisor marker persisted"

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
    local t0 t1
    t0=$(date +%s)
    probe -sS --fail-with-body -X POST "${base}/${rt}/\$validate?profile=${canon}" \
      -H 'Content-Type: application/fhir+json' --data-binary "@/golden/${file}" \
    | python3 "${DIR}/verify-profile.py" "${canon}"
    t1=$(date +%s)
    assert_under_budget "${line}" "${rt} (${canon})" "$((t1 - t0))"
  done

  # The consumer-facing property: a profile the warm-up never touched (US Core
  # Patient — in every line's package set, in no warm-up row) still answers inside
  # the budget on a fresh lane, because the engine init the warm-up paid for is the
  # ~40s of the cold cost; a single profile's snapshot is seconds. A bare Patient is
  # enough: the assertion is the latency and that an OperationOutcome came back,
  # not the verdict.
  local t0 t1
  t0=$(date +%s)
  probe -s -X POST "${base}/Patient/\$validate?profile=http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient" \
    -H 'Content-Type: application/fhir+json' --data '{"resourceType":"Patient"}' \
  | python3 -c '
import json,sys
oo=json.load(sys.stdin)
if oo.get("resourceType")!="OperationOutcome": sys.exit("FAIL: non-OperationOutcome for the cold-profile Patient validate call")
print("OK: cold-profile Patient validate call answered")
'
  t1=$(date +%s)
  assert_under_budget "${line}" "Patient (us-core-patient, never warmed)" "$((t1 - t0))"
  docker logs "${container}" >"${RUN_DIR}/warm-after.log" 2>&1
  python3 "${DIR}/verify-state.py" logs "${line}" <"${RUN_DIR}/warm-after.log"
  if grep -Ei 'HikariPool.*(timeout|timed out|exhaust)|OutOfMemoryError|Java heap space' "${RUN_DIR}/warm-after.log"; then
    echo "FAIL: line ${line}: pool or heap exhaustion"; exit 1
  fi
  echo "line ${line}: VALIDATOR OFFLINE VERIFY OK"
  docker rm -f "${container}" >/dev/null 2>&1 || true
}

# 2.0: unchanged default — build + probe, same bar as before the matrix.
build_line 2.0 "${PREFIX}:2.0"
bash "${DIR}/verify-process.sh" "${PREFIX}:2.0"
probe_line 2.0 "${PREFIX}:2.0" "${PREFIX}-2.0"

# 2.1: build asserted (proves the line's package set is baked correctly); probe
# SKIPPED by default to keep this gate's wall-time bounded (three
# offline-IG-indexing boots is the expensive part, not the build). Set
# SHN_VALIDATOR_VERIFY_PROBE_2_1=1 to also probe it locally.
build_line 2.1 "${PREFIX}:2.1"
if [ "${SHN_VALIDATOR_VERIFY_PROBE_2_1:-0}" = "1" ]; then
  probe_line 2.1 "${PREFIX}:2.1" "${PREFIX}-2.1"
else
  echo "line 2.1: build OK, probe SKIPPED (set SHN_VALIDATOR_VERIFY_PROBE_2_1=1 to probe locally)"
fi

# 2.2: the RI-facing line — build + probe, same bar as 2.0.
build_line 2.2 "${PREFIX}:2.2"
probe_line 2.2 "${PREFIX}:2.2" "${PREFIX}-2.2"

echo "VALIDATOR MATRIX OFFLINE VERIFY OK (2.0 + 2.2 probed; probe_2_1=${SHN_VALIDATOR_VERIFY_PROBE_2_1:-0}; strict_verdicts_through=100s; images=${PREFIX}:2.0|2.1|2.2)"
