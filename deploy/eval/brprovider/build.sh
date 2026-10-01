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
# This bundle ships as the gateway module alone — there is no other checkout to vendor
# br-provider from, so this script clones the upstream MIT source from GitHub and builds the
# pinned commit's tree as git stores it: the build context is that tree, not a checkout.
# Image build only; no cert/UDAP logic (see gencerts.sh).
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
# A trailing slash would name the aside directory inside the clone.
SRC="${SRC%/}"

# The clone under /tmp is a cache the system's periodic /tmp cleanup can empty or prune: a
# .git left empty, or objects dropped from it.
#   - A directory with no commit at HEAD, or a clone missing an object of the pinned commit's
#     tree, is moved aside to ${SRC}.broken-<UTC time>-<pid>-<n>, never deleted (delete it once
#     no build is using it), and cloned again, once.
#   - A new clone is made beside ${SRC} and renamed into place, so builds starting together
#     never find one half made there.
#   - A pinned commit the clone lacks is fetched from REPO_URL. When that fetch fails, a clone
#     missing objects its own history names is moved aside and cloned again; a whole one is
#     left as it is and the build stops, since the failure was the network's.
# Git runs with --git-dir, so an emptied .git is never read as a repository enclosing it.
clone_git() { git --git-dir="${SRC}/.git" "$@"; }

clone_usable() { clone_git rev-parse -q --verify 'HEAD^{commit}' >/dev/null 2>&1; }

move_aside() {
  local aside
  aside="${SRC}.broken-$(date -u +%Y%m%dT%H%M%SZ)-$$-${RANDOM}"
  echo "${SRC} $1; moving it to ${aside}" >&2
  mv "${SRC}" "${aside}" || [ ! -e "${SRC}" ]
}

clone_new() {
  local tmp status
  mkdir -p "$(dirname "${SRC}")" || return
  tmp=$(mktemp -d "${SRC}.cloning.XXXXXX") || return
  git clone --no-checkout "${REPO_URL}" "${tmp}" || {
    status=$?
    rm -rf "${tmp}"
    return "${status}"
  }
  if [ -e "${SRC}" ] || ! mv "${tmp}" "${SRC}" 2>/dev/null; then
    rm -rf "${tmp}"
  fi
  # Had ${SRC} appeared between the test and the move, mv put the copy inside it.
  rm -rf "${SRC:?}/${tmp##*/}"
}

ensure_source() {
  local attempt status
  for attempt in 1 2; do
    if [ -e "${SRC}" ] && ! clone_usable; then
      move_aside "is not a usable git clone" || return 1
    fi
    if [ ! -e "${SRC}" ]; then
      clone_new || return
    fi
    if ! clone_git cat-file -e "${FULL_PIN}^{commit}" 2>/dev/null; then
      clone_git fetch "${REPO_URL}" "${FULL_PIN}" || {
        status=$?
        if clone_git rev-list --objects --all >/dev/null 2>&1 || [ "${attempt}" = 2 ]; then
          return "${status}"
        fi
        move_aside "is missing objects its history names" || return 1
        continue
      }
    fi
    if clone_git rev-list --objects "${FULL_PIN}^{tree}" >/dev/null 2>&1; then
      return 0
    fi
    [ "${attempt}" = 1 ] || break
    move_aside "is missing objects of ${FULL_PIN}'s tree" || return 1
  done
  echo "a fresh clone of ${REPO_URL} at ${SRC} still lacks ${FULL_PIN}'s tree" >&2
  return 1
}

build() {
  if docker image inspect "${IMAGE}" >/dev/null 2>&1; then
    echo "${IMAGE} already present — skipping build"
    return 0
  fi
  ensure_source
  # The whole pinned tree is written out before docker reads any of it: docker builds and tags
  # an image from a tar cut short, which a later run would then find present and keep.
  context_tar=$(mktemp "${TMPDIR:-/tmp}/${IMAGE%%:*}-context.XXXXXX")
  trap 'rm -f "${context_tar}"' EXIT
  clone_git archive --format=tar -o "${context_tar}" "${FULL_PIN}"
  docker build -t "${IMAGE}" - < "${context_tar}"
  echo "built ${IMAGE}"
}

case "${1:-build}" in
  build) build ;;
  *) echo "usage: $0 build" >&2; exit 2 ;;
esac
