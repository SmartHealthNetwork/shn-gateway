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

# The pinned curl helper is pulled once (host egress only; every validator below
# stays offline); a host that already holds the pinned tag runs the proof without
# registry access.
if docker image inspect "${CURL}" >/dev/null 2>&1; then
  echo "using local ${CURL}"
else
  echo "pulling ${CURL} (host egress only; every validator below stays offline)..."
  docker pull "${CURL}" >/dev/null
fi
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

  local arch
  arch="$(docker image inspect -f '{{.Architecture}}' "${image}")"
  (cd "${DIR}" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" go build -trimpath -o "${RUN_DIR}/runtime-probe" ./testdata/process-child)
  chmod 755 "${RUN_DIR}" "${RUN_DIR}/runtime-probe"
  PYTHONDONTWRITEBYTECODE=1 python3 "${DIR}/test_verify_admission.py"
  local create_args=(--name "${container}" --network "${NET}" -v "${RUN_DIR}/runtime-probe:/runtime-probe:ro")
  if [ "${line}" = 2.1 ]; then
    # A real Spring file-source conflict must lose to the owned CLI suffix.
    # This is explicit verifier configuration, not a measured production sample.
    printf 'server.port=19090\nserver.address=0.0.0.0\n' >"${RUN_DIR}/binding-conflict.properties"
    chmod 644 "${RUN_DIR}/binding-conflict.properties"
    create_args+=(-v "${RUN_DIR}/binding-conflict.properties:/binding-conflict.properties:ro" -e SPRING_CONFIG_ADDITIONAL_LOCATION=file:/binding-conflict.properties)
  fi
  local boot=${SECONDS}
  docker create "${create_args[@]}" "${image}" >/dev/null
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
  docker exec "${container}" /runtime-probe observe-runtime >"${RUN_DIR}/runtime-${line}.json"
  python3 "${DIR}/verify_admission.py" <"${RUN_DIR}/runtime-${line}.json"
  cat "${RUN_DIR}/runtime-${line}.json"
  # A same-network peer can reach public 8080 but cannot bypass admission at 18080.
  local private_status
  # One curl process in one peer container proves public reachability first;
  # --fail-early prevents an earlier DNS/HTTP failure being hidden by the next URL.
  if probe --fail-early -fsS "${base}/metadata" --next --connect-timeout 4 --max-time 30 -sS -v "http://${container}:18080/fhir/metadata" >"${RUN_DIR}/peer-public-${line}.json" 2>"${RUN_DIR}/private-${line}.stderr"; then
    private_status=0
  else
    private_status=$?
  fi
  # This earliest ordinary sample precedes all verifier cache seeding and must
  # retain the historical localhost worker authority throughout its full JSON.
  python3 "${DIR}/verify_admission.py" peer "${private_status}" "${RUN_DIR}/private-${line}.stderr" "${RUN_DIR}/peer-public-${line}.json"
  verify_verdict_window "${container}" "${line}" "${health_at}"
  docker cp "${container}:/tmp/shn-validator-warm" "${RUN_DIR}/warm.json"
  python3 "${DIR}/verify-state.py" ready "${line}" <"${RUN_DIR}/warm.json"
  docker logs "${container}" >"${RUN_DIR}/warm-before.log" 2>&1
  python3 "${DIR}/verify-state.py" logs "${line}" <"${RUN_DIR}/warm-before.log"
  # The composition's cold cost is a declared number: the package cache indexes
  # exactly the StructureDefinitions the manifest declares for this line
  # (indexed-counts.json, generated from tools/contracts/manifest.json). A
  # different count is a composition change that must re-measure and re-declare.
  local indexed declared
  indexed=$(grep -c 'Indexing StructureDefinition' "${RUN_DIR}/warm-before.log" || true)
  declared=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["lines"][sys.argv[2]])' "${DIR}/indexed-counts.json" "${line}")
  if [ "${indexed}" != "${declared}" ]; then
    echo "FAIL: line ${line}: the lane indexed ${indexed} StructureDefinitions at boot, the manifest declares ${declared} — a composition change re-measures and re-declares its cold cost (tools/contracts/manifest.json lines.${line}.indexedStructureDefinitions)"
    exit 1
  fi
  echo "indexed StructureDefinitions line=${line} count=${indexed} (declared ${declared})"
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

  # Exact-version and core-first witnesses of the engine's resolution rules: the
  # lane's own us-core-patient version validates; the other line's version is
  # refused as an unknown profile (the package cache has no version fallback);
  # a core canonical pinned to a version nothing carries still validates (core
  # is version-blind for hl7.org canonicals). The lane's US Core version is
  # read from the container's own configuration, never spelled here.
  local uscore other
  uscore=$(docker inspect "${container}" --format '{{range .Config.Env}}{{println .}}{{end}}' | awk -F= -v k="hapi.fhir.implementationguides.uscore.version" '$1==k{print substr($0, length(k)+2)}')
  [ -n "${uscore}" ] || { echo "FAIL: line ${line}: could not read the lane's US Core version from its own env"; exit 1; }
  if [ "${uscore}" = "6.1.0" ]; then other=7.0.0; else other=6.1.0; fi
  witness() { # LABEL MODE RESOURCE_TYPE BODY
    probe -s -X POST "${base}/$3/\$validate" -H 'Content-Type: application/fhir+json' --data "$4" \
      | python3 "${DIR}/verify-witness.py" "$2" "$1"
  }
  local patient='{"resourceType":"Patient","id":"w","identifier":[{"system":"urn:shn:probe","value":"w"}],"name":[{"family":"Witness","given":["W"]}],"gender":"female"'
  witness "line ${line} us-core-patient|${uscore} (the lane's version)" clean Patient "${patient},\"meta\":{\"profile\":[\"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient|${uscore}\"]}}"
  witness "line ${line} us-core-patient|${other} (another line's version)" unknown-profile Patient "${patient},\"meta\":{\"profile\":[\"http://hl7.org/fhir/us/core/StructureDefinition/us-core-patient|${other}\"]}}"
  witness "line ${line} Patient|9.9.9 (core canonical, version-blind)" clean Patient "${patient},\"meta\":{\"profile\":[\"http://hl7.org/fhir/StructureDefinition/Patient|9.9.9\"]}}"
  # The copied R5 Encounter profile (the support package's derived closure) pins
  # alternate-reference on two of its Reference elements at 5.2.0, a version no
  # lane loads; the loaded definition is 5.3.0-ballot-tc1 (the support package's
  # copy on 2.0/2.1, the extensions package on 2.2). The engine's versioned-url
  # fallback resolves the core-namespace canonical to the loaded definition: an
  # Encounter declaring profile-Encounter with that slice populated is NOT
  # refused, and a string in the Reference's place is rejected — both halves
  # witnessed here, so the tolerance reason in the manifest describes what the
  # engine does.
  local encounter='{"resourceType":"Encounter","id":"w","meta":{"profile":["http://hl7.org/fhir/5.0/StructureDefinition/profile-Encounter"]},"status":"finished","class":{"system":"http://terminology.hl7.org/CodeSystem/v3-ActCode","code":"AMB"},"basedOn":[{"reference":"ServiceRequest/w","extension":[{"url":"http://hl7.org/fhir/StructureDefinition/alternate-reference",'
  witness "line ${line} profile-Encounter with alternate-reference populated (not refused)" clean Encounter "${encounter}\"valueReference\":{\"reference\":\"CarePlan/w\"}}]}]}"
  witness "line ${line} profile-Encounter with alternate-reference mistyped (validated against the loaded definition)" type-refused Encounter "${encounter}\"valueString\":\"not-a-reference\"}]}]}"
  if [ "${line}" = "2.2" ]; then
    # DTR 2.2.0 pins version-specific extension canonicals (|5.3.0-ballot-tc1);
    # the golden that uses them must validate clean, not merely resolve its profile.
    probe -sS --fail-with-body -X POST "${base}/QuestionnaireResponse/\$validate?profile=http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/dtr-questionnaireresponse" \
      -H 'Content-Type: application/fhir+json' --data-binary "@/golden/2.2/questionnaireresponse-autofill.json" \
      | python3 "${DIR}/verify-witness.py" clean "line 2.2 DTR QuestionnaireResponse golden with |5.3.0-ballot-tc1 references"
  fi
  # Ordinary metadata is globally cached by HAPI. Preserve this late sample
  # before any no-cache probe can replace the worker/observer's cached statement.
  probe -fsS -H 'Host: original.example:18089' "${base}/metadata" >"${RUN_DIR}/metadata-host-${line}.json"
  python3 "${DIR}/verify_admission.py" metadata "${RUN_DIR}/metadata-host-${line}.json"
  # Fresh derivation must reflect each distinct incoming authority exactly.
  probe -fsS -H 'Cache-Control: no-cache' -H 'Host: original.example:18089' "${base}/metadata" >"${RUN_DIR}/metadata-fresh-a-${line}.json"
  python3 "${DIR}/verify_admission.py" metadata "${RUN_DIR}/metadata-fresh-a-${line}.json" 'http://original.example:18089/fhir'
  probe -fsS -H 'Cache-Control: no-cache' -H 'Host: alternate.example:18090' "${base}/metadata" >"${RUN_DIR}/metadata-fresh-b-${line}.json"
  python3 "${DIR}/verify_admission.py" metadata "${RUN_DIR}/metadata-fresh-b-${line}.json" 'http://alternate.example:18090/fhir'
  # Both sides bypass cached metadata, so equality witnesses Java's actual
  # forwarded-header derivation without adding any header policy to forwarding.
  probe -fsS -H 'Cache-Control: no-cache' -H 'Host: original.example:18089' -H 'X-Forwarded-Host: forwarded.example:18443' -H 'X-Forwarded-Proto: https' "${base}/metadata" >"${RUN_DIR}/metadata-forwarded-${line}.json"
  docker run --rm --network "container:${container}" "${CURL}" --connect-timeout 4 --max-time 30 -fsS -H 'Cache-Control: no-cache' -H 'Host: original.example:18089' -H 'X-Forwarded-Host: forwarded.example:18443' -H 'X-Forwarded-Proto: https' 'http://127.0.0.1:18080/fhir/metadata' >"${RUN_DIR}/metadata-private-${line}.json"
  python3 "${DIR}/verify_admission.py" forwarded "${RUN_DIR}/metadata-forwarded-${line}.json" "${RUN_DIR}/metadata-private-${line}.json"
  probe -fsS -D - -H 'Host: original.example:18089' -H 'Content-Type: application/fhir+json' --data-binary '{"resourceType":"Patient"}' "${base}/Patient" >"${RUN_DIR}/location-${line}.response"
  python3 "${DIR}/verify_admission.py" location "${RUN_DIR}/location-${line}.response"
  echo "actual public Host/base, forwarded-header equivalence, Location and peer isolation verified line=${line}"
  docker logs "${container}" >"${RUN_DIR}/warm-after.log" 2>&1
  python3 "${DIR}/verify-state.py" logs "${line}" <"${RUN_DIR}/warm-after.log"
  # Every "Found multiple package versions" HAPI logged must name a canonical the
  # closure inventory already records as carried by two loaded packages
  # (known-collisions.json, generated from tools/contracts/closure/<line>.json);
  # anything else is the nondeterministic resolution the closure gate excludes.
  python3 "${DIR}/verify-collisions.py" "${line}" <"${RUN_DIR}/warm-after.log"
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
