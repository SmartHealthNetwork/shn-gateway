#!/usr/bin/env bash
# Host build prerequisite. Build caches stay outside runtime asset directories.
set -euo pipefail
SRC="$(cd "$(dirname "$0")" && pwd)"
[ "$#" = 1 ] || { echo 'usage: prepare.sh CACHE_ROOT' >&2; exit 2; }
CACHE_ROOT="$1"
platform="${DOCKER_DEFAULT_PLATFORM:-}"
if [ -z "$platform" ]; then
  machine="$(docker info --format '{{.OSType}}/{{.Architecture}}')"
  case "$machine" in
    linux/aarch64|linux/arm64) platform=linux/arm64 ;;
    linux/x86_64|linux/amd64) platform=linux/amd64 ;;
    *) echo "unsupported Docker platform: $machine" >&2; exit 1 ;;
  esac
fi
case "$platform" in linux/arm64|linux/amd64) ;; *) echo "unsupported source platform: $platform" >&2; exit 1 ;; esac
artifact="$(PYTHONDONTWRITEBYTECODE=1 python3 "$SRC/cache.py" ensure "$CACHE_ROOT" "$platform")"
PYTHONDONTWRITEBYTECODE=1 python3 "$SRC/cache.py" verify "$artifact" "$platform" "$platform" >/dev/null
# The selection files contain data only; consumers never evaluate them as shell code.
selection="$(mktemp "$CACHE_ROOT/selected-path.XXXXXX")"
trap 'rm -f "$selection"' EXIT
printf '%s\n' "$artifact" > "$selection"
mv "$selection" "$CACHE_ROOT/selected-path"
printf '%s\n' "$artifact"
