#!/usr/bin/env sh
# gateway/deploy/eval/payerdir-init.sh — write the provider bundle's payer-directory.json.
#
# The gateway os.ReadFile's PAYER_DIRECTORY (no env/${} interpolation — gateway/engine/
# payerrouter.go:52-62), so the payor→holder mapping must be a concrete file. This init writes it:
#   - PAYER_HOLDER_ID set   → route the personas' payor id 00001 to YOUR payer holder (payer self-test)
#   - PAYER_HOLDER_ID unset → the committed conformance-payer default (provider self-eval)
# PAYER_HOLDER_ID only changes the ROUTE: this gateway stays ROLE=provider, so SHN_SECRETS is
# STILL a provider registration bundle (a payer bundle can't originate — the Authorization
# Framework binds authority to the caller's registered role and denies it, surfacing HTTP 502
# "authorization denied").
# A payer self-test also needs YOUR payer up + registered under a public baseURL — see payer/README.md.
# Runs as an init service BEFORE the gateway; the gateway (uid 65532) reads $OUT, so it is written
# world-readable (named-volume perms are strict for a non-root reader on a named volume).
set -eu
OUT="${OUT:-/config/payer-directory.json}"
SYSTEM="urn:oid:2.16.840.1.113883.6.300"
VALUE="00001"
HOLDER="${PAYER_HOLDER_ID:-conformance-payer}"
mkdir -p "$(dirname "$OUT")"
printf '[{"system":"%s","value":"%s","holderId":"%s"}]\n' "$SYSTEM" "$VALUE" "$HOLDER" > "$OUT"
chmod 0644 "$OUT"
echo "payerdir-init: 00001 -> ${HOLDER}"
