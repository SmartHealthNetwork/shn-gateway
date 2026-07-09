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
compose="docker compose -f gateway/deploy/eval/compose.eval.yml"
# Register teardown BEFORE `up` so a failing build/up (which set -e aborts on) still tears
# down any containers that did start — otherwise leaked containers wedge the next run's ports.
trap '$compose down -v' EXIT
$compose up -d --build

# Readiness = retry the first real scenario until it returns well-formed JSON.
# (No GET /healthz exists; a successful uc01 POST proves gateway up + seeded + routing —
# see gateway/engine/originate.go's handleScenario / gateway/engine/gateway.go's route
# table, which mounts ONLY POST /scenario/ucNN.)
ready=0
for i in $(seq 1 90); do
  if curl -fsS -X POST localhost:8080/scenario/uc01 -H 'Content-Type: application/json' \
       -d '{"branch":"covered"}' | jq -e '.covered==true' >/dev/null 2>&1; then ready=1; break; fi
  sleep 10
done
[ "$ready" = 1 ] || { echo "gateway never became ready"; exit 1; }

fail=0
check() { # label  body  jq-predicate
  # Guard the assignment: under set -e a failing curl (a 4xx/5xx from a regressed UC, or a
  # connection error) would otherwise abort the whole script here — skipping the remaining
  # UCs AND the mandatory conformant-lane cert checks below. Record it and keep going.
  if ! out=$(curl -fsS -X POST "localhost:8080/scenario/$1" -H 'Content-Type: application/json' -d "${2:-{}}"); then
    echo "✗ $1: request failed"; fail=1; return
  fi
  if echo "$out" | jq -e "$3" >/dev/null 2>&1; then echo "✓ $1"; else echo "✗ $1: $out"; fail=1; fi
}
# Assertions against the REAL structs (gateway/engine/originate*.go / originate_resume.go),
# mode-a-onboarding.md §4. Branch-body keys verified against the real handlers: handleScenario
# (uc01) switches on req.Branch == "covered"/"notcovered" (originate.go:151-159); handleUC05
# switches on "", "consent", "noconsent" (originate.go:1181-1187). uc02/03/04/06/07/08 don't
# read the body at all, so "{}" is inert.
check uc01 '{"branch":"covered"}'    '.covered==true'
check uc01 '{"branch":"notcovered"}' '.covered==false'
check uc02 '{}'                      '.paRequired==false'
check uc03 '{}'                      '.paRequired==true and (.authNumber|length>0)'
check uc04 '{}'                      '(.authNumber|length>0)'
check uc05 '{}'                      '(.authNumber|length>0)'
check uc05 '{"branch":"noconsent"}'  '.consentDenied==true'
check uc06 '{}'                      '(.authNumber|length>0) and .attested==true'
check uc07 '{}'                      '(.authNumber|length>0) and .attested==true'
check uc08 '{}'                      '.denied==true'

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
