#!/usr/bin/env bash
#
# gateway/deploy/eval/brpayer/build.sh — build the pinned HL7-DaVinci/br-payer image
# for the payer eval bundle's conformant lane.
#
# br-payer is ONE HAPI FHIR JPA server doing CRD + DTR + PAS in one base, built by a
# single top-level Dockerfile. FHIR resources are served under `/fhir`; CDS Hooks
# services are served at root, e.g. `/cds-services/order-sign-crd`. The image
# exposes container port 8081.
#
# This bundle ships as a snapshot of `gateway/` alone — there is no monorepo checkout to vendor
# br-payer from, so this script clones the upstream MIT source fresh from GitHub and checks
# out the pinned commit before building. Image build only; no cert/UDAP logic (see gencerts.sh).
#
# Pinned to br-payer commit a8bece458cb31f151845db9ea5a892e398deef56 (a8bece4).
#
# Usage:
#   gateway/deploy/eval/brpayer/build.sh build    # clone (if needed) + docker build the pinned commit
set -euo pipefail

COMMIT="a8bece4"
FULL_PIN="a8bece458cb31f151845db9ea5a892e398deef56"
REPO_URL="https://github.com/HL7-DaVinci/br-payer.git"
IMAGE="br-payer:${COMMIT}"
SRC="${BRPAYER_CLONE_DIR:-/tmp/br-payer}"

build() {
  if docker image inspect "${IMAGE}" >/dev/null 2>&1; then
    echo "${IMAGE} already present — skipping build"
    return 0
  fi
  if [ ! -d "${SRC}/.git" ]; then
    git clone "${REPO_URL}" "${SRC}"
  fi
  git -C "${SRC}" fetch --depth 50 origin
  git -C "${SRC}" checkout "${FULL_PIN}"
  docker build -t "${IMAGE}" "${SRC}"
  echo "built ${IMAGE}"
}

case "${1:-build}" in
  build) build ;;
  *) echo "usage: $0 build" >&2; exit 2 ;;
esac
