#!/usr/bin/env bash
# gateway/deploy/eval/smoke.sh — the provider evaluation bundle's Definition-of-Done
# validator. Opt-in (Docker + a provisioned `shn register --role
# provider` bundle + reachability to the hosted Hub): boots compose.eval.yml (hapi +
# gencerts + seed + gateway + br-provider), drives all 8 provider-data UCs against the
# hosted conformance-payer through the gateway's real /scenario/ucNN routes with `shn
# send-test`, the command the README gives a partner, and proves
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
# (none from v1.0.0, observe before) and never see a partner's defect refused. Both halves are pinned by
# the platform's invariants; do not move this export into the compose, and do not add one
# to payer/smoke.sh.
export CONFORMANCE_ENFORCEMENT=strict
# The payer this gate drives is SHN's hosted conformance payer, a reference payer whose answers
# do not yet conform at every request shape. Strict alone would refuse them, and
# this gate would judge the reference payer instead of the bundle. So the gate tolerates the
# reference payers, as SHN's own provider gateways do: a FHIR profile defect strict would refuse
# in their DTR or PAS answers is recorded as a tolerated finding and the answer carried. Every
# other payer, and every other check, stays strict. It goes once the reference payers conform, with SHN's own lists.
export CONFORMANCE_TOLERATE_PEERS="urn:oid:2.16.840.1.113883.6.300|00001,urn:oid:2.16.840.1.113883.6.300|00300,urn:oid:2.16.840.1.113883.6.300|00301,urn:shn:demo-payer|SHN-BRIDGE-DEMO,urn:shn:demo-payer|SHN-BRIDGE-REFUSE"
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
disc_url="${SHN_DISCOVERY_URL:-https://accounts.shn-preview.org/discovery}"
desc=$(mktemp)
curl -fsS "$disc_url" -o "$desc"
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
# The scenarios: `shn send-test` sends each one the persona the descriptor states for its route,
# and the answer UC-06 and UC-07 state (the gateway supplies none), signed as the caller above.
# The reference payer pends these families on submit and decides later: --follow 30 follows each
# pend to the decision through POST /scenario/pa/inquire. A pend still open after it means
# something is wrong, so it fails this gate (send-test's exit 3) as surely as a failed scenario.
st=0
# send-test reads <accounts>/discovery: pass the descriptor's URL without that last segment.
disc_base=${disc_url%/}; disc_base=${disc_base%/discovery}
"$work/shn" send-test --gateway http://127.0.0.1:8080 --discovery "$disc_base" \
  --scenario-key "$work/scenario-caller-send-test.sign" --bundle "$SHN_SECRETS" --follow 30 || st=$?
case "$st" in
  0) ;;
  3) echo "✗ send-test: a scenario was still pended after 30 s"; fail=1 ;;
  *) echo "✗ send-test: exit $st"; fail=1 ;;
esac

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
