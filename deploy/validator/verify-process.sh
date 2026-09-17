#!/usr/bin/env bash
# FR-G3 Linux acceptance: the real image binary is PID 1; only its child is controlled.
# Usage: verify-process.sh IMAGE (built by this directory's Dockerfile).
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE="${1:?supply the built validator image}"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/validator-process.XXXXXXXX")"
PREFIX="validator-process-$(basename "${TMP}" | tr '[:upper:].' '[:lower:]-')"
OWNED=()
cleanup() {
  local ec=$?
  if [ "${#OWNED[@]}" -gt 0 ]; then
    if [ "${ec}" -ne 0 ]; then
      for c in "${OWNED[@]}"; do docker logs "${c}" 2>&1 | tail -80 || true; done
    fi
    for c in "${OWNED[@]}"; do docker rm -f "${c}" >/dev/null 2>&1 || true; done
  fi
  rm -rf "${TMP}"
  exit "${ec}"
}
trap cleanup EXIT
PYTHONDONTWRITEBYTECODE=1 python3 "${DIR}/test_verify_state.py"
(cd "${DIR}" && GOWORK=off go test ./testdata/process-child -count=1)
ARCH="$(docker image inspect -f '{{.Architecture}}' "${IMAGE}")"
(cd "${DIR}" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="${ARCH}" go build -trimpath -o "${TMP}/child" ./testdata/process-child)
chmod 755 "${TMP}" "${TMP}/child"
JAVA=(java --class-path /app/main.war '-Dloader.path=main.war!/WEB-INF/classes/,main.war!/WEB-INF/,/app/extra-classes' org.springframework.boot.loader.PropertiesLauncher)
C="${PREFIX}-resolve"
docker create --name "${C}" --network none -v "${TMP}/child:/process-child:ro" --entrypoint /process-child "${IMAGE}" resolve-java >/dev/null
OWNED+=("${C}")
docker start -a "${C}" >"${TMP}/java-path"
JAVA_EXECUTABLE="$(cat "${TMP}/java-path")"
[[ "${JAVA_EXECUTABLE}" = /*/java ]] || { echo 'FAIL: unresolved Java executable'; exit 1; }
start() {
  C="${PREFIX}-$1"; shift
  docker create --name "${C}" --network none -v "${TMP}/child:/process-child:ro" -v "${TMP}/child:${JAVA_EXECUTABLE}:ro" -v "${DIR}/testdata:/process-fixtures:ro" \
    -e 'PROCESS_LITERAL=value with spaces;$literal' "$@" --entrypoint /healthcheck "${IMAGE}" \
    supervise "${JAVA[@]}" 'argument with spaces' '$literal;unchanged' >/dev/null
  OWNED+=("${C}")
  docker start "${C}" >/dev/null
  await_status
}
request() { docker exec "${C}" /process-child request "$1"; }
public_request() { docker exec "${C}" /process-child request-public "$1"; }
assert_complete_posts() {
  request status | python3 -c 'import json,sys;s=json.load(sys.stdin);assert s["posts"]==["Bundle","QuestionnaireResponse","ExplanationOfBenefit","Task"]+["ClaimResponse"]*30+["Bundle"]*4+["Claim"]*2+["ClaimResponse"]*2 and s["maximum"]==1;assert len(s["profiles"])==len(s["posts"]);assert s["profiles"][-2:]==["http://hl7.org/fhir/us/davinci-pas/StructureDefinition/profile-claimresponse|9.9.9","https://example.org/fhir/StructureDefinition/unavailable-profile"]'
}
await_status() {
  local i
  for i in $(seq 1 100); do
    if request status >"${TMP}/status.json" 2>/dev/null; then return; fi
    sleep .1
  done
  echo 'FAIL: controlled child did not start'; return 1
}
await_ready() {
  local i
  for i in $(seq 1 100); do
    if docker exec "${C}" /healthcheck >/dev/null 2>&1; then return; fi
    sleep .1
  done
  echo 'FAIL: worker never became ready'; return 1
}
await_stopped() {
  local expected="$1" i
  for i in $(seq 1 250); do
    if [ "$(docker inspect -f '{{.State.Running}}' "${C}")" = false ]; then
      [ "$(docker inspect -f '{{.State.ExitCode}}' "${C}")" = "${expected}" ]; return
    fi
    sleep .1
  done
  echo 'FAIL: supervisor did not stop within allowance'; return 1
}
assert_cold() {
  request status >"${TMP}/status.json"
  python3 - "${TMP}/status.json" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]))
assert s['parent']==1 and s['pid']>1 and s['uid']==65532
assert s['args']==['--class-path','/app/main.war','-Dloader.path=main.war!/WEB-INF/classes/,main.war!/WEB-INF/,/app/extra-classes','org.springframework.boot.loader.PropertiesLauncher','argument with spaces','$literal;unchanged','--server.address=127.0.0.1','--server.port=18080'] and s['cwd']=='/app'
assert 'PROCESS_LITERAL=value with spaces;$literal' in s['env']
assert s['initial']['state']=='waiting-for-metadata' and not s['initial']['warm']
assert not s['posts']
PY
  if docker exec "${C}" /healthcheck >/dev/null 2>&1; then echo 'FAIL: cold probe ready'; return 1; fi
  if public_request status >/dev/null 2>&1; then echo 'FAIL: cold public forwarding'; return 1; fi
}
start normal
assert_cold
# A second actual supervisor must refuse before a marker write or child launch.
request marker >"${TMP}/before.json"
if docker exec "${C}" /healthcheck supervise /process-child sentinel >/dev/null 2>&1; then
  echo 'FAIL: admitted second supervisor'; exit 1
fi
request marker >"${TMP}/after.json"
cmp "${TMP}/before.json" "${TMP}/after.json"
if docker cp "${C}:/tmp/second-child-launched" "${TMP}/sentinel" >/dev/null 2>&1; then
  echo 'FAIL: second supervisor launched a child'; exit 1
fi
request metadata-on
# Hold Bundle until independent real probe processes have observed it concurrently.
for _ in $(seq 1 100); do
  request status >"${TMP}/status.json"
  if python3 -c 'import json,sys;s=json.load(open(sys.argv[1]));sys.exit(0 if s["active"]==1 else 1)' "${TMP}/status.json"; then break; fi
  sleep .1
done
pids=()
for _ in $(seq 1 6); do docker exec "${C}" /healthcheck >"${TMP}/probe-$_.log" 2>&1 & pids+=("$!"); done
for pid in "${pids[@]}"; do if wait "${pid}"; then echo 'FAIL: ready while Bundle held'; exit 1; fi; done
request status | python3 -c 'import json,sys;s=json.load(sys.stdin);assert s["posts"]==["Bundle"] and s["active"]==s["maximum"]==1'
if public_request status >/dev/null 2>&1; then echo 'FAIL: public forwarding while Bundle held'; exit 1; fi
request release
await_ready
public_request status >/dev/null
LINE="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "${C}" | sed -n 's/^SHN_IG_LINE=//p')"
request marker >"${TMP}/ready.json"
python3 "${DIR}/verify-state.py" ready "${LINE}" <"${TMP}/ready.json"
for _ in $(seq 1 6); do docker exec "${C}" /healthcheck; done
assert_complete_posts
# An actual restart retains the writable container layer, including /tmp.
docker restart -t 25 "${C}" >/dev/null
await_status
assert_cold
request marker >"${TMP}/restarted.json"
python3 - "${TMP}/ready.json" "${TMP}/restarted.json" <<'PY'
import json,sys
a,b=[json.load(open(p)) for p in sys.argv[1:]]
assert a['key']!=b['key'] and b['state']=='waiting-for-metadata' and not b['warm']
PY
request metadata-on
request release
await_ready
assert_complete_posts
docker kill --signal TERM "${C}" >/dev/null
await_stopped 0
docker logs "${C}" 2>&1 | grep 'child received terminated' >/dev/null
# Child exit is the supervisor/container exit (the parent waits and reaps).
start child-exit
request exit >/dev/null 2>&1 || true
await_stopped 7
# Uncertain validation must leave the child live but terminally unready.
start uncertain -e PROCESS_DROP_RESPONSE=1
request metadata-on
request release
for _ in $(seq 1 100); do
  request marker >"${TMP}/failed.json"
  if python3 -c 'import json,sys;sys.exit(0 if json.load(open(sys.argv[1]))["state"]=="failed" else 1)' "${TMP}/failed.json"; then break; fi
  sleep .1
done
python3 -c 'import json,sys;assert json.load(open(sys.argv[1]))["state"]=="failed"' "${TMP}/failed.json"
for _ in $(seq 1 6); do if docker exec "${C}" /healthcheck >/dev/null 2>&1; then exit 1; fi; done
request status | python3 -c 'import json,sys;s=json.load(sys.stdin);assert s["posts"]==["Bundle"] and s["active"]==0'
docker kill --signal TERM "${C}" >/dev/null
await_stopped 0
# Real 20s shutdown bound; Docker does not inject a kill for this assertion.
start forced -e PROCESS_IGNORE_SIGNAL=1
t0=$(date +%s)
docker kill --signal TERM "${C}" >/dev/null
await_stopped 137
elapsed=$(( $(date +%s) - t0 ))
[ "${elapsed}" -ge 19 ] && [ "${elapsed}" -lt 25 ]
docker logs "${C}" 2>&1 | grep 'child received terminated' >/dev/null
# Valid prefix and PATH lookup, followed by a real exec-format failure. No shell fallback.
printf 'invalid executable format\n' >"${TMP}/invalid-java"
chmod 755 "${TMP}/invalid-java"
C="${PREFIX}-exec-failure"
docker create --name "${C}" --network none -v "${TMP}/invalid-java:${JAVA_EXECUTABLE}:ro" --entrypoint /healthcheck "${IMAGE}" supervise "${JAVA[@]}" >/dev/null
OWNED+=("${C}")
docker start "${C}" >/dev/null
await_stopped 1
docker logs "${C}" 2>&1 | grep "supervisor: child launch failed" >/dev/null
echo "VALIDATOR LINUX PROCESS VERIFY OK image=${IMAGE} forced_shutdown=${elapsed}s"
