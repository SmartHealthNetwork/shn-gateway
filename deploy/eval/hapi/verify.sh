#!/usr/bin/env bash
set -euo pipefail
IMG="shn-eval-hapi:local"
docker build -t "$IMG" gateway/deploy/eval/hapi
# URL_BASED tenant identification only activates when partitioning is enabled;
# without the partitioning.* vars HAPI parses /fhir/DEFAULT as a resource type
# (HAPI-0302). Mirror the operative env from deploy/compose.multiprocess.yml.
cid=$(docker run -d -p 18081:8080 \
  -e hapi.fhir.tenant_identification_strategy=URL_BASED \
  -e hapi.fhir.partitioning.partitioning_include_in_search_hashes=false \
  -e hapi.fhir.partitioning.allow_references_across_partitions=false \
  -e hapi.fhir.cr.enabled=true "$IMG")
trap 'docker rm -f "$cid" >/dev/null 2>&1 || true' EXIT
for i in $(seq 1 90); do   # first boot generates snapshots — allow minutes
  if curl -fsS http://127.0.0.1:18081/fhir/DEFAULT/metadata >/dev/null 2>&1; then
    echo "HAPI ready"; exit 0
  fi
  sleep 10
done
echo "HAPI did not become ready" >&2; exit 1
