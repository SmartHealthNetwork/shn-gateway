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
# Every file this self-test uses is found from the evaluation bundle's directory, this one's
# parent, so it runs alike from the platform repository and from the published gateway module,
# started from any directory.
eval_dir=$(CDPATH='' cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)

# The provider gateway's /scenario routes admit only the callers its bundle's
# scenario-callers.json names (compose.eval.yml's SCENARIO_CALLERS_FILE). Make this run's caller
# the way the provider README's step 0 does, `shn scenario-key --bundle`, on a copy of your
# provider bundle, so your bundle itself is never written; every request below is signed.
# The copy goes in a work directory under TMPDIR, on the volume SHN_EVAL_KEYS_VOLUME names
# when it is set, or the smoke refuses before copying anything (../workdir.sh).
. "$eval_dir/workdir.sh"
eval_workdir payer-selftest "$SHN_SECRETS_PROVIDER" || exit 1
# The copy holds the bundle's private keys: remove it on every exit from here on.
trap 'rm -rf "$work"' EXIT
mkdir "$work/bundle" && cp -R "$SHN_SECRETS_PROVIDER"/. "$work/bundle"
if [ -n "$eval_sdk" ]; then ( cd "$eval_sdk" && go build -o "$work/shn" ./cmd/shn ); else cp "$(command -v shn)" "$work/shn"; fi
"$work/shn" scenario-key --bundle "$work/bundle" --out "$work"
SHN_SECRETS_PROVIDER="$work/bundle"
key="$work/scenario-caller-send-test.sign"

# With SHN_EVAL_KEYS_VOLUME set, the gateway reads the copy from a ramfs volume this smoke makes
# and fills, never by binding it (../workdir.sh): set the trap that removes it before anything is made.
trap 'eval_bundle_drop; rm -rf "$work"' EXIT
eval_bundle_volume "$SHN_SECRETS_PROVIDER" || exit 1
# An array, so a directory whose path holds a space is one argument.
prov=(docker compose -p evalprov -f "$eval_dir/compose.eval.yml" ${eval_bundle_override[@]+"${eval_bundle_override[@]}"})
# The teardown must carry SHN_SECRETS too: compose.eval.yml interpolates it as a required
# variable on EVERY compose invocation (down included, not just up), so a bare `down -v`
# here would die on interpolation before removing anything — and a silenced trap would
# leave the whole eval stack running after a green run. Same value the `up` below uses. set -e
# holds inside the trap too, so a failed down must not end it before the rest runs.
trap 'SHN_SECRETS="$SHN_SECRETS_PROVIDER" "${prov[@]}" down -v || echo "eval: compose down failed" >&2; eval_bundle_drop; rm -rf "$work"' EXIT

# compose.eval.yml references br-provider:43a4806 as an `image:` (not a build:) service, and that
# reference RI is never published — so `up --build` would try to PULL it and abort unless it's
# already built locally. Build it first (as ../smoke.sh does).
bash "$eval_dir/brprovider/build.sh" build

# Bring up the provider originator, routed at your DEPLOYED payer holder (PAYER_HOLDER_ID → payerdir
# init). br-provider/gencerts still build/boot but are idle for the /scenario provider-data lane; if
# their boot ever slows this run, gate them behind a compose profile in compose.eval.yml (br-provider
# only — the gateway hard-depends on gencerts for INGRESS_CLIENTS_FILE, so gencerts can't be profiled out).
SHN_SECRETS="$SHN_SECRETS_PROVIDER" PAYER_HOLDER_ID="$PAYER_HOLDER_ID" "${prov[@]}" up -d --build

# Every scenario request carries the persona the network's discovery descriptor states for
# its route on the gateway's lane (provider-data) — member, order and claim facts — never a
# value of this script's own (../persona_body.sh). Same descriptor the gateway reads.
disc_url="${SHN_DISCOVERY_URL:-https://accounts.shn-preview.org/discovery}"
desc="$work/discovery.json"
curl -fsS "$disc_url" -o "$desc"

# Readiness: the provider gateway (uc01 POST) — folds in seed + routing.
ready=0
req0=$("$eval_dir/persona_body.sh" "$desc" uc01 provider-data 0 '{"branch":"covered"}')
for _ in $(seq 1 90); do
  # Each request needs its own assertion (one use), so each retry signs again.
  if curl -fsS -X POST localhost:8080/scenario/uc01 -H 'Content-Type: application/json' \
       -H "X-Holder-Assertion: $("$work/shn" scenario-sign --key "$key" --bundle "$SHN_SECRETS_PROVIDER" POST /scenario/uc01 "$req0")" \
       --data-raw "$req0" | jq -e '.covered==true' >/dev/null 2>&1; then ready=1; break; fi
  sleep 10
done
[ "$ready" = 1 ] || { echo "uc01 never succeeded — the provider originator didn't come up, OR your deployed payer holder ($PAYER_HOLDER_ID) isn't reachable/registered"; exit 1; }

# Drive all 8 UCs — every decision comes from YOUR deployed payer's br-payer, via the hosted Hub.
if [ -n "$eval_sdk" ]; then
  # The platform repository, with the SDK's source beside the gateway: build send-test from the
  # sibling sdk/ module (its own Go module, so it must run from inside it — a module-boundary
  # crossing fails from the repository's root).
  ( cd "$eval_sdk" && go run ./cmd/shn send-test --gateway http://127.0.0.1:8080 --discovery "${disc_url%/discovery}" \
      --scenario-key "$key" --bundle "$SHN_SECRETS_PROVIDER" )
else
  # The published gateway module: use the installed shn CLI
  # (go install github.com/SmartHealthNetwork/shn-sdk/cmd/shn@latest).
  shn send-test --gateway http://127.0.0.1:8080 --discovery "${disc_url%/discovery}" \
    --scenario-key "$key" --bundle "$SHN_SECRETS_PROVIDER"
fi
