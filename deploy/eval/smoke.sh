#!/usr/bin/env bash
# gateway/deploy/eval/smoke.sh — the provider evaluation bundle's Definition-of-Done
# validator. Opt-in (Docker + a provisioned `shn register --role
# provider` bundle + reachability to the hosted Hub): boots compose.eval.yml (hapi +
# gencerts + seed + gateway + br-provider), drives all 8 provider-data UCs against the
# hosted conformance-payer through the gateway's real /scenario/ucNN routes, and proves
# the conformant (Da Vinci) lane's GENERATED cert end to end. A broken cert generator or
# a broken UC must fail HERE, on the bundle we ship, not on a partner's first boot.
set -euo pipefail
: "${SHN_KIT_EVAL_LIVE:?set SHN_KIT_EVAL_LIVE=1 to run the live eval smoke}"
: "${SHN_SECRETS:?set SHN_SECRETS to a provisioned provider bundle}"
bash gateway/deploy/eval/brprovider/build.sh build            # image:, must exist before up
# THIS script is SHN's own gate, so it runs the gateway strict: a defect in the bundle we
# ship must fail here rather than be recorded and relayed past us. compose.eval.yml leaves
# the value to its operator precisely so this export can exist without imposing strict on
# the other two things that boot that same file — a partner's `docker compose up` (README)
# and the partner's own payer self-test (payer/smoke.sh) — which take the published default
# of observe and see a partner's defect recorded, not refused. Both halves are pinned by
# the platform's invariants; do not move this export into the compose, and do not add one
# to payer/smoke.sh.
export CONFORMANCE_ENFORCEMENT=strict
# The gateway's /scenario routes admit only the callers its bundle's scenario-callers.json
# names (compose.eval.yml's SCENARIO_CALLERS_FILE). This gate makes its caller the way the
# README's step 0 tells a partner to, `shn scenario-key --bundle`, on a copy of the
# provisioned bundle, so the bundle itself is never written; every request below is signed.
work=$(mktemp -d)
# The copy holds the bundle's private keys: remove it on every exit from here on.
trap 'rm -rf "$work"' EXIT
mkdir "$work/bundle" && cp -R "$SHN_SECRETS"/. "$work/bundle"
if [ -d sdk ]; then ( cd sdk && go build -o "$work/shn" ./cmd/shn ); else cp "$(command -v shn)" "$work/shn"; fi
"$work/shn" scenario-key --bundle "$work/bundle" --out "$work"
export SHN_SECRETS="$work/bundle"
# sign ROUTE BODY prints the assertion for one request; each request needs its own (one use).
sign() { "$work/shn" scenario-sign --key "$work/scenario-caller-send-test.sign" --bundle "$SHN_SECRETS" POST "/scenario/$1" "$2"; }
compose="docker compose -f gateway/deploy/eval/compose.eval.yml"
# Register teardown BEFORE `up` so a failing build/up (which set -e aborts on) still tears
# down any containers that did start — otherwise leaked containers wedge the next run's ports.
trap '$compose down -v; rm -rf "$work"' EXIT
$compose up -d --build

# Every scenario request carries the persona the network's discovery descriptor states for
# its route on this gateway's lane (provider-data): member, order and claim facts
# (persona_body.sh). The smoke states none of them; the descriptor is the one the gateway
# itself reads (compose.eval.yml's SHN_DISCOVERY_URL).
desc=$(mktemp)
curl -fsS "${SHN_DISCOVERY_URL:-https://accounts.shn-preview.org/discovery}" -o "$desc"
body() { gateway/deploy/eval/persona_body.sh "$desc" "$1" provider-data "$2" "${3:-{\}}"; }

# Readiness = retry the first real scenario until it returns well-formed JSON.
# (No GET /healthz exists; a successful uc01 POST proves gateway up + seeded + routing —
# see gateway/engine/module1_originate.go's handleScenario / gateway/engine/module1_originate_routes.go's
# route declarations, which mount ONLY POST /scenario/ucNN.)
ready=0
req0=$(body uc01 0 '{"branch":"covered"}')
for i in $(seq 1 90); do
  if curl -fsS -X POST localhost:8080/scenario/uc01 -H 'Content-Type: application/json' \
       -H "X-Holder-Assertion: $(sign uc01 "$req0")" --data-raw "$req0" | jq -e '.covered==true' >/dev/null 2>&1; then ready=1; break; fi
  sleep 10
done
[ "$ready" = 1 ] || { echo "gateway never became ready"; exit 1; }

fail=0
check() { # route  descriptor-scenario  amend(0|1)  route-keys  jq-predicate
  # Guard the assignment: under set -e a failing curl (a connection error; a 4xx/5xx is an
  # answer, read below) would otherwise abort the whole script here — skipping the remaining
  # UCs AND the mandatory conformant-lane cert checks below. Record it and keep going.
  local req
  if ! req=$(body "$2" "$3" "$4"); then echo "✗ $2: the descriptor states no persona facts"; fail=1; return; fi
  # A payer's pend is its answer; the reference payer pends these families on submit and decides
  # later. ?wait=30 asks the gateway to follow the decision for up to 30 s (its own bound, through
  # Claim/$inquire) before it answers, so the route answers the decision when the payer has made
  # one, and the pend with its continuation when it has not. The signature binds the query too.
  # The status is read separately so a refusal's body, which says why, is printed with it.
  if ! out=$(curl -sS -w '\n%{http_code}' -X POST "localhost:8080/scenario/$1?wait=30" -H 'Content-Type: application/json' \
       -H "X-Holder-Assertion: $(sign "$1?wait=30" "$req")" --data-raw "$req"); then
    echo "✗ $2: request failed (no answer)"; fail=1; return
  fi
  code=${out##*$'\n'}; out=${out%$'\n'*}
  case "$code" in
    2??) ;;
    *) echo "✗ $2: HTTP $code: $out"; fail=1; return ;;
  esac
  if echo "$out" | jq -e "$5" >/dev/null 2>&1; then echo "✓ $2"
  elif cont=$(echo "$out" | jq -er 'select(.decision=="pended") | .continuation' 2>/dev/null); then
    # The evaluation payer decides within the wait; a pend still open after it means something is
    # wrong, so it fails this gate, said as what it is, with the continuation to resume it.
    echo "✗ $2: pended, not decided within 30 s (continuation $cont): $out"; fail=1
  else echo "✗ $2: $out"; fail=1; fi
}
# Assertions against the REAL structs (engine/module1_originate*.go).
# Branch-body keys verified against the real handlers: handleScenario
# (uc01) switches on req.Branch == "covered"/"notcovered" (module1_originate.go:151-159); handleUC05
# switches on "", "consent", "noconsent" (module1_originate.go:1181-1187). uc02/03/04/06/07/08 don't
# read the body at all, so "{}" is inert.
check uc01 uc01            0 '{"branch":"covered"}'    '.covered==true'
check uc01 uc01-notcovered 0 '{"branch":"notcovered"}' '.covered==false'
check uc02 uc02            0 '{}'                      '.paRequired==false'
check uc03 uc03            0 '{}'                      '.paRequired==true and (.authNumber|length>0)'
check uc04 uc04            1 '{}'                      '(.authNumber|length>0)'
check uc05 uc05            1 '{}'                      '(.authNumber|length>0)'
check uc05 uc05-noconsent  1 '{"branch":"noconsent"}'  '.consentDenied==true'
check uc06 uc06            1 '{}'                      '(.authNumber|length>0) and .attested==true'
check uc07 uc07            1 '{}'                      '(.authNumber|length>0) and .attested==true'
check uc08 uc08            0 '{}'                      '.denied==true'

# ── Conformant lane: exercise the GENERATED cert end to end ──
# A broken cert generator must fail HERE, not silently on a partner's first boot.
# br-provider is a slow-booting Java/Spring app that comes up well after the Go gateway; the
# reject test hits the gateway (:8080, already ready), but originate drives br-provider's BFF
# (:8082). Wait for it to actually serve, else the cert check races its boot (curl 000/reset).
brp_bff="${BRPROVIDER_BFF_URL:-http://127.0.0.1:8082}"
brp_ready=0
for _ in $(seq 1 60); do
  brp_code=$(curl -s -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 5 "$brp_bff/" 2>/dev/null || true)
  case "$brp_code" in 2??|3??|4??) brp_ready=1; break;; esac
  sleep 5
done
[ "$brp_ready" = 1 ] || { echo "✗ br-provider BFF never became ready at $brp_bff"; fail=1; }

bash gateway/deploy/eval/brprovider/reject_test.sh || fail=1          # (a) unauth → 401
if [ "$brp_ready" = 1 ] && bash gateway/deploy/eval/brprovider/originate.sh uc02; then  # (b) valid cert → UC completes
  echo "✓ conformant-lane (generated cert)"; else echo "✗ conformant-lane cert"; fail=1; fi

[ "$fail" = 0 ] && echo "ALL EVAL SCENARIOS GREEN" || { echo "EVAL SMOKE FAILED"; exit 1; }
# compose down -v (the trap) wipes the shared `certs` volume — the generated cert is
# per-run, never persisted across runs.
