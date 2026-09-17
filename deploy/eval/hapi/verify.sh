#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# Resolve from the shipped script so checkout and standalone export use one context.
exec env PYTHONDONTWRITEBYTECODE=1 python3 "$HERE/verify.py" "$@"
