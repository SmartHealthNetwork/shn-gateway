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
ARCH="$(docker image inspect -f '{{.Architecture}}' "${IMAGE}")"
(cd "${DIR}" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="${ARCH}" go build -trimpath -o "${TMP}/child" ./testdata/process-child)
chmod 755 "${TMP}" "${TMP}/child"
start() {
  C="${PREFIX}-$1"; shift
  docker create --name "${C}" --network none -v "${TMP}/child:/process-child:ro" -v "${DIR}/testdata:/process-fixtures:ro" \
    -e 'PROCESS_LITERAL=value with spaces;$literal' "$@" --entrypoint /healthcheck "${IMAGE}" \
    supervise /process-child 'argument with spaces' '$literal;unchanged' >/dev/null
  OWNED+=("${C}")
  docker start "${C}" >/dev/null
  await_status
}
request() { docker exec "${C}" /process-child request "$1"; }
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
assert s['args']==['argument with spaces','$literal;unchanged'] and s['cwd']=='/app'
assert 'PROCESS_LITERAL=value with spaces;$literal' in s['env']
assert s['initial']['state']=='waiting-for-metadata' and not s['initial']['warm']
assert not s['posts']
PY
  if docker exec "${C}" /healthcheck >/dev/null 2>&1; then echo 'FAIL: cold probe ready'; return 1; fi
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
request release
await_ready
LINE="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "${C}" | sed -n 's/^SHN_IG_LINE=//p')"
request marker >"${TMP}/ready.json"
python3 "${DIR}/verify-state.py" ready "${LINE}" <"${TMP}/ready.json"
for _ in $(seq 1 6); do docker exec "${C}" /healthcheck; done
request status | python3 -c 'import json,sys;s=json.load(sys.stdin);assert s["posts"]==["Bundle","QuestionnaireResponse","ExplanationOfBenefit","Task"]+["ClaimResponse"]*30+["Bundle"]*4 and s["maximum"]==1'
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
# Missing executable is a startup failure, without an in-place child retry.
C="${PREFIX}-missing"
docker create --name "${C}" --network none --entrypoint /healthcheck "${IMAGE}" supervise /does-not-exist >/dev/null
OWNED+=("${C}")
docker start "${C}" >/dev/null
await_stopped 1
echo "VALIDATOR LINUX PROCESS VERIFY OK image=${IMAGE} forced_shutdown=${elapsed}s"
