#!/usr/bin/env bash
# gateway/deploy/eval/payer/smoke.sh — payer self-test DoD (deploy-and-test): prove YOUR DEPLOYED
# payer holder adjudicates all 8 prior-auth UCs, decided by its br-payer, through the hosted Hub.
#
# PRECONDITION (a deploy — the smoke can't do it for you): your payer responder bundle
# (compose.eval.payer.yml) is deployed to a PUBLICLY-REACHABLE https endpoint (cloud or a
# tunnel) and registered via `shn register --role payer <public-baseURL>`. PAYER_HOLDER_ID is the
# resulting holder id (see `shn clients`). A 127.0.0.1 payer CANNOT be used — the Hub dials it.
set -euo pipefail
: "${SHN_KIT_EVAL_LIVE:?set SHN_KIT_EVAL_LIVE=1 to run the live payer self-test}"
: "${SHN_SECRETS_PROVIDER:?set SHN_SECRETS_PROVIDER to a provisioned role=provider bundle dir}"
: "${PAYER_HOLDER_ID:?set PAYER_HOLDER_ID to your DEPLOYED+registered payer holder id (shn clients)}"

prov="docker compose -p evalprov -f gateway/deploy/eval/compose.eval.yml"
trap '$prov down -v >/dev/null 2>&1 || true' EXIT

# compose.eval.yml references br-provider:43a4806 as an `image:` (not a build:) service, and that
# reference RI is never published — so `up --build` would try to PULL it and abort unless it's
# already built locally. Build it first (same as gateway/deploy/eval/smoke.sh:12).
bash gateway/deploy/eval/brprovider/build.sh build

# Bring up the provider originator, routed at your DEPLOYED payer holder (PAYER_HOLDER_ID → payerdir
# init). br-provider/gencerts still build/boot but are idle for the /scenario provider-data lane; if
# their boot ever slows this run, gate them behind a compose profile in compose.eval.yml (br-provider
# only — the gateway hard-depends on gencerts for INGRESS_CLIENTS_FILE, so gencerts can't be profiled out).
SHN_SECRETS="$SHN_SECRETS_PROVIDER" PAYER_HOLDER_ID="$PAYER_HOLDER_ID" $prov up -d --build

# Readiness: the provider gateway (uc01 POST) — folds in seed + routing.
ready=0
for _ in $(seq 1 90); do
  if curl -fsS -X POST localhost:8080/scenario/uc01 -H 'Content-Type: application/json' \
       -d '{"branch":"covered"}' | jq -e '.covered==true' >/dev/null 2>&1; then ready=1; break; fi
  sleep 10
done
[ "$ready" = 1 ] || { echo "uc01 never succeeded — the provider originator didn't come up, OR your deployed payer holder ($PAYER_HOLDER_ID) isn't reachable/registered"; exit 1; }

# Drive all 8 UCs — every decision comes from YOUR deployed payer's br-payer, via the hosted Hub.
if [ -d sdk ]; then
  # Monorepo checkout: build send-test from the sibling sdk/ module (its own Go module, so it must
  # run from inside it — a module-boundary crossing fails from the repo root).
  ( cd sdk && go run ./cmd/shn send-test --gateway http://127.0.0.1:8080 )
else
  # Standalone gateway bundle: use the installed shn CLI
  # (go install github.com/SmartHealthNetwork/shn-sdk/cmd/shn@latest).
  shn send-test --gateway http://127.0.0.1:8080
fi
