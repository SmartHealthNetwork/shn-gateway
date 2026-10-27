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
# This bundle ships as the gateway module alone — there is no other checkout to vendor
# br-payer from, so this script clones the upstream Apache-2.0 source from GitHub and builds the
# pinned commit's tree as git stores it: the build context is that tree, not a checkout,
# written out as a directory so that Docker applies upstream's own .dockerignore, as it does
# for upstream's build (a stdin tar context is sent whole: upstream's frontend tests would
# reach its type check).
# The image it tags is the pinned upstream commit's build, plus SHN's trust-store edit (four
# emSign roots removed): ../../validator/truststore/wrap-image.sh adds one layer, the JVM trust
# store without the four eMudhra emSign roots of CVE-2026-114820, and checks it. The upstream
# build alone is kept as <image>-upstream.
# Image build only; no cert/UDAP logic (see gencerts.sh).
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
UPSTREAM_IMAGE="${IMAGE}-upstream" # the pinned commit's own build, before the trust-store edit
# Upstream's Dockerfile, used as git stores it, names its build-stage tools by tags that
# move. Each is pinned here by digest, through a named build context that takes the place of
# the image its FROM names, so a new release of one cannot break the pinned commit's build (the
# docs stage's pip install of mkdocs-material, which names no version, still floats).
# The final stage's runtime image (gcr.io/distroless/java21-debian13:nonroot) is not pinned: each
# build takes its current JRE patches. A pin whose name no FROM line carries fails the build,
# since Docker would ignore it.
BASE_PINS=(
  "oven/bun:1-slim=docker-image://oven/bun:1.4.4-slim@sha256:1846adf22496bd711036aae8be147e57bdebbaea2e5264e3e0b5af1db187d9a4"
  "python:3-alpine=docker-image://python:3.15-alpine3.24@sha256:d44b1254cb92329418d164b22b20d86bbd3869d48e80e09e476531a409a728bb"
  "docker.io/library/maven:3.9.12-eclipse-temurin-17=docker-image://maven:3.9.12-eclipse-temurin-17@sha256:a0603aab698040d9c94259f379ec0487da1678560748d6c7508483034033c53d"
)
SRC="${BRPAYER_CLONE_DIR:-/tmp/br-payer}"
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
  local tmp status stale pid
  mkdir -p "$(dirname "${SRC}")" || return
  # The sibling's name carries this shell's pid. A clone killed part way leaves its sibling
  # behind, so each new clone first removes any whose process is gone; a live one is another
  # caller's clone in progress. Only where ps -p lists both this shell and pid 1: with no ps, one
  # without -p, or one that cannot see other processes, every process would look gone, so none
  # is removed.
  ps -p "$$" >/dev/null 2>&1 && ps -p 1 >/dev/null 2>&1 &&
  for stale in "${SRC}".cloning.*; do
    [ -d "${stale}" ] || continue
    pid=${stale#"${SRC}".cloning.}
    pid=${pid%%.*}
    case ${pid} in '' | *[!0-9]*) continue ;; esac
    ps -p "${pid}" >/dev/null 2>&1 || rm -rf "${stale}" || true
  done
  tmp=$(mktemp -d "${SRC}.cloning.$$.XXXXXX") || return
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
  # Only an image the trust store wrapper built (its label) is kept: one built before it,
  # still trusting the four emSign roots, is built again.
  if [ "$(docker image inspect --format '{{index .Config.Labels "org.shn.truststore"}}' "${IMAGE}" 2>/dev/null)" = emsign-removed ]; then
    echo "${IMAGE} already present — skipping build"
    return 0
  fi
  if ! docker image inspect "${UPSTREAM_IMAGE}" >/dev/null 2>&1; then
    ensure_source
    # The whole pinned tree is written out before docker reads any of it: docker builds and tags
    # an image from a context cut short, which a later run would then find present and keep.
    context_dir=$(mktemp -d "${TMPDIR:-/tmp}/${IMAGE%%:*}-context.XXXXXX")
    trap 'rm -rf "${context_dir}"' EXIT
    clone_git archive --format=tar "${FULL_PIN}" | tar -xf - -C "${context_dir}"
    local pin pin_args=()
    for pin in "${BASE_PINS[@]}"; do
      if ! awk -v name="${pin%%=*}" 'toupper($1) == "FROM" && $2 == name { found = 1 } END { exit !found }' "${context_dir}/Dockerfile" 2>/dev/null; then
        echo "no FROM line in ${FULL_PIN}'s Dockerfile names ${pin%%=*}, which build.sh pins" >&2
        return 1
      fi
      pin_args+=(--build-context "${pin}")
    done
    docker build "${pin_args[@]}" -t "${UPSTREAM_IMAGE}" "${context_dir}"
  fi
  # The pinned upstream commit's build, plus SHN's trust-store edit: the four eMudhra emSign
  # roots (CVE-2026-114820) removed from the JVM trust store, and checked in the built image.
  "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../../validator/truststore/wrap-image.sh" "${UPSTREAM_IMAGE}" "${IMAGE}"
  echo "built ${IMAGE}"
}

case "${1:-build}" in
  build) build ;;
  *) echo "usage: $0 build" >&2; exit 2 ;;
esac
