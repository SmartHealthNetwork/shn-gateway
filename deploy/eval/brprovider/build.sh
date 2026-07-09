#!/usr/bin/env bash
#
# gateway/deploy/eval/brprovider/build.sh — build the pinned HL7-DaVinci/br-provider image
# for the provider eval bundle's conformant lane.
#
# br-provider is a multi-part app (HAPI FHIR server + Spring BFF + TanStack React frontend)
# built by a single top-level Dockerfile (5-stage multi-stage build: bun frontend → mkdocs docs
# → maven HAPI server → spring-boot repackage → distroless java17 final stage). The final
# `default` stage exposes port 8080 (FHIR server `/fhir` + BFF `/api/...` + frontend `/`
# combined).
#
# This bundle ships as a snapshot of `gateway/` alone — there is no monorepo checkout to vendor
# br-provider from, so this script clones the upstream MIT source fresh from GitHub and checks
# out the pinned commit before building. Image build only; no cert/UDAP logic (see gencerts.sh).
#
# Pinned to br-provider commit 43a4806a5662863298310374533352d840729cc3 (43a4806).
#
# Usage:
#   gateway/deploy/eval/brprovider/build.sh build    # clone (if needed) + docker build the pinned commit
set -euo pipefail

COMMIT="43a4806"
FULL_PIN="43a4806a5662863298310374533352d840729cc3"
REPO_URL="https://github.com/HL7-DaVinci/br-provider.git"
IMAGE="br-provider:${COMMIT}"
SRC="${BRPROVIDER_CLONE_DIR:-/tmp/br-provider}"

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
