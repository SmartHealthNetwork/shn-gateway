#!/usr/bin/env bash
# Runs inside the pinned build-only compiler; no downloads or runtime overlay.
set -euo pipefail
SRC="$(cd "$(dirname "$0")" && pwd)"
[ "$#" = 5 ] || { echo 'usage: build.sh ORIGINAL_WAR OUTPUT_DIR HAPI_PLATFORM HAPI_CHILD COMPILER_CHILD' >&2; exit 2; }
exec java -Xmx2g "$SRC/BuildBackport.java" build "$SRC" "$1" "$2" "$3" "$4" "$5"
