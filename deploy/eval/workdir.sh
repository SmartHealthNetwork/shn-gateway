#!/usr/bin/env bash
# gateway/deploy/eval/workdir.sh — sourced by the evaluation smokes (smoke.sh and payer/smoke.sh),
# never run on its own. It defines eval_workdir, which makes the directory a smoke works in: its
# copy of your registration bundle, private keys included, the scenario caller's key and the
# discovery document all go there. It also defines eval_bundle_volume and eval_bundle_drop (at
# the end), which give the gateway that copy from a ramfs volume when SHN_EVAL_KEYS_VOLUME is set.
#
#   eval_workdir <name> <your bundle directory> || exit 1
#
# It sets `work` to the new directory's absolute path, every symbolic link resolved, and an EXIT
# trap that removes it (a smoke that sets its own EXIT trap later removes it there too). The
# directory is mode 0700 and always under ${TMPDIR:-/tmp}, so you choose the volume: point
# TMPDIR at a RAM disk or an encrypted scratch volume and the copy goes there. (A bare
# `mktemp -d` would not do: on macOS it ignores TMPDIR and creates under /var/folders.)
#
# To require that volume rather than trust it, set SHN_EVAL_KEYS_VOLUME to its mount point (an
# absolute path, such as /Volumes/<your RAM disk>). Then eval_workdir refuses, before the smoke
# copies anything, unless the new directory and your bundle directory both resolve, through any
# symbolic link, to paths inside that mount point, and `df -P` names that mount point for each of
# them: a directory left where a volume was once mounted is not that volume. Anything it cannot
# check is a refusal too. A refusal removes the directory it made, leaves `work` empty and
# returns 1. Unset, nothing is checked and TMPDIR alone decides.
# eval_dir is the evaluation bundle's directory, this file's, every symbolic link in its path
# resolved: the smokes find every file they use from it. eval_sdk is the SDK's source, the sdk/
# directory where a smoke is started, as SHN's repository has beside the gateway when make starts
# a smoke from its root; elsewhere, as in the published module, it is empty and the smokes use the
# installed shn.
eval_dir=$(CDPATH='' cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
eval_sdk=
if [ -d sdk ]; then
  eval_sdk=$(CDPATH='' cd -P -- sdk && pwd -P)
fi

eval_workdir() {
  local name=${1:?eval_workdir: no name} bundle=${2:?eval_workdir: no bundle directory}
  local base=${TMPDIR:-/tmp}
  work=$(mktemp -d "${base%/}/shn-eval-$name.XXXXXX") || {
    work=
    echo "eval: could not make a work directory under $base (TMPDIR)" >&2
    return 1
  }
  trap 'rm -rf "$work"' EXIT
  local made=$work
  if ! chmod 700 "$made" || ! work=$(CDPATH='' cd -P -- "$made" && pwd -P); then
    rmdir "$made"
    work=
    return 1
  fi
  if [ -n "${SHN_EVAL_KEYS_VOLUME+set}" ]; then
    if ! eval_on_volume "$work" "$SHN_EVAL_KEYS_VOLUME" || ! eval_on_volume "$bundle" "$SHN_EVAL_KEYS_VOLUME"; then
      rmdir "$work"
      work=
      echo "eval: refusing to copy the bundle's keys: SHN_EVAL_KEYS_VOLUME is $SHN_EVAL_KEYS_VOLUME, and the work directory or the bundle is not on it" >&2
      return 1
    fi
  fi
}

# eval_on_volume PATH VOLUME: PATH is a directory inside the mount point VOLUME, resolved through
# any symbolic link, and the file system df reports PATH on is mounted at VOLUME exactly.
eval_on_volume() {
  local path=$1 vol=$2 real mount
  case "$vol" in /*) ;; *) echo "eval: SHN_EVAL_KEYS_VOLUME must be an absolute mount point" >&2; return 1 ;; esac
  vol=$(CDPATH='' cd -P -- "$vol" 2>/dev/null && pwd -P) || { echo "eval: SHN_EVAL_KEYS_VOLUME $2 is not a directory" >&2; return 1; }
  real=$(CDPATH='' cd -P -- "$path" 2>/dev/null && pwd -P) || { echo "eval: $path is not a directory" >&2; return 1; }
  case "$real/" in "${vol%/}/"?*) ;; *) echo "eval: $path resolves to $real, outside $vol" >&2; return 1 ;; esac
  # df -P prints a header, then one line whose sixth field onward is the mount point.
  mount=$(df -P "$real" 2>/dev/null | awk 'NR == 2 { sub(/^[^ ]+ +[^ ]+ +[^ ]+ +[^ ]+ +[^ ]+ +/, ""); print }') || mount=
  [ "$mount" = "$vol" ] || { echo "eval: $real is on the file system mounted at '${mount:-unknown}', not $vol" >&2; return 1; }
}

# ── The gateway's bundle, from a ramfs volume ──
#
#   eval_bundle_volume <the smoke's bundle copy> || exit 1    (after the copy is complete)
#   eval_bundle_drop                                          (in the smoke's EXIT trap)
#
# With SHN_EVAL_KEYS_VOLUME set, the bundle stays on that volume, and the gateway may not read it
# from there: Docker Desktop on macOS does not reliably bind-mount a freshly attached RAM disk
# (the container sees its mount point but cannot open anything under it). So the gateway's
# /etc/shn is a ramfs volume in Docker's own VM instead, made and filled here, and
# compose.eval.ramfs.yml (in eval_bundle_override, for the smoke's compose command) puts it in
# place of compose.eval.yml's bind. ramfs, not tmpfs: the VM may swap, and tmpfs pages can be
# swapped to its disk; ramfs pages are not. A ramfs volume is emptied when the last container
# using it stops, so a keeper container (the holder) keeps it mounted until the smoke's teardown.
#
# Unset, nothing here runs: the gateway binds SHN_SECRETS, as compose.eval.yml says. Set, it is
# required: eval_bundle_volume refuses (returns 1) unless the copy is on the volume, no volume or
# container already has the names it would make, the volume is ramfs as mounted, the holder runs
# and mounts it, and the filled volume lists the copy's files. It never falls back to the bind.
# The volume and its containers carry org.shn.owner and org.shn.run labels: SHN_EVAL_OWNER and
# SHN_EVAL_RUN when set, otherwise "eval" and the work directory's name without its shn-eval- prefix.
# Nothing leaves the copy until every check of the volume has passed, and the fill streams it
# through a pipe, so no file of it is written anywhere else. Set the smoke's EXIT trap to call
# eval_bundle_drop before calling eval_bundle_volume: it removes the fill container, the holder,
# then the volume, whatever was made, so a refusal or an interrupted run leaves none behind.
#
# The smokes have no overall timeout, so the holder ends on its own after
# eval_bundle_holder_seconds (4 hours, far past a run's length). A run killed outright, whose trap
# never ran, leaves its gateway's stack running, and the volume keeps the keys while that stack
# or the holder still mounts it: they go when both have stopped, and the empty volume is left,
# labelled to expire an hour after the holder's end.
#
# The holder and the fill run eval_bundle_image, pinned by digest: they handle the bundle's keys,
# so a moved tag must not change what runs them.
eval_bundle_image=alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
eval_bundle_holder_seconds=14400
eval_bundle_vol=
eval_bundle_holder=
eval_bundle_fill=
eval_bundle_override=()

# eval_docker_exists KIND NAME: 0 when docker has a KIND (container or volume) named NAME, 1 when it
# says there is none, 2 when it cannot tell (the daemon unreachable, any other error).
eval_docker_exists() {
  local err
  err=$(docker "$1" inspect "$2" 2>&1 >/dev/null) && return 0
  case "$err" in *"No such"* | *"no such"*) return 1 ;; esac
  return 2
}

# eval_docker_free KIND NAME: no KIND is named NAME yet, or it says why it refuses.
eval_docker_free() {
  local st=0
  eval_docker_exists "$1" "$2" || st=$?
  case $st in
    1) return 0 ;;
    0) echo "eval: a $1 named $2 already exists: refusing to use it" >&2 ;;
    *) echo "eval: could not tell whether a $1 named $2 exists" >&2 ;;
  esac
  return 1
}

eval_bundle_volume() {
  local copy=${1:?eval_bundle_volume: no bundle copy}
  eval_bundle_override=()
  [ -n "${SHN_EVAL_KEYS_VOLUME+set}" ] || return 0
  eval_on_volume "$copy" "$SHN_EVAL_KEYS_VOLUME" || return 1
  if [ -z "$(ls -A "$copy")" ]; then
    echo "eval: the bundle copy $copy is empty" >&2
    return 1
  fi
  # Named for this run's work directory (eval_workdir's name and mktemp suffix), so two runs never
  # share one; a name already taken is refused, never adopted.
  local base=${work##*/}
  base=${base#shn-eval-}
  case "$base" in "" | *[!A-Za-z0-9.-]*) echo "eval: no work directory to name the bundle volume for" >&2; return 1 ;; esac
  local vol=shn-eval-bundle-$base holder=shn-eval-holder-$base fill=shn-eval-fill-$base
  # The run labels: the caller's when set, otherwise "eval" and that same name (provider.<suffix>,
  # payer-selftest.<suffix>).
  local owner=${SHN_EVAL_OWNER:-eval} run=${SHN_EVAL_RUN:-$base}
  eval_docker_free volume "$vol" && eval_docker_free container "$holder" && eval_docker_free container "$fill" || return 1
  eval_bundle_vol=$vol eval_bundle_holder=$holder eval_bundle_fill=$fill
  # The org.shn.expires label: an hour past the holder's own end.
  local exp
  exp=$(( $(date +%s) + eval_bundle_holder_seconds + 3600 ))
  # BSD date takes -r SECONDS; GNU date reads -r as a file and takes -d @SECONDS.
  exp=$(date -u -r "$exp" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "@$exp" +%Y-%m-%dT%H:%M:%SZ) || {
    echo "eval: could not compute the bundle volume's expiry" >&2
    return 1
  }
  local labels=(--label "org.shn.owner=$owner" --label "org.shn.run=$run" --label "org.shn.expires=$exp")
  docker volume create --driver local --opt type=ramfs --opt device=ramfs --opt o=mode=0700 "${labels[@]}" "$eval_bundle_vol" >/dev/null || {
    echo "eval: could not create the ramfs volume $eval_bundle_vol" >&2
    return 1
  }
  docker run -d --rm --name "$eval_bundle_holder" --network none "${labels[@]}" -v "$eval_bundle_vol:/b:ro" \
    "$eval_bundle_image" sleep "$eval_bundle_holder_seconds" >/dev/null || {
    echo "eval: could not start the holder $eval_bundle_holder" >&2
    return 1
  }
  local got
  got=$(docker volume inspect -f '{{index .Options "type"}}' "$eval_bundle_vol" 2>/dev/null) || got=
  [ "$got" = ramfs ] || {
    echo "eval: $eval_bundle_vol is not a ramfs volume" >&2
    return 1
  }
  got=$(docker inspect -f '{{.State.Running}}' "$eval_bundle_holder" 2>/dev/null) || got=
  [ "$got" = true ] || {
    echo "eval: the holder $eval_bundle_holder is not running" >&2
    return 1
  }
  got=$(docker inspect -f '{{range .Mounts}}{{.Name}}{{"\n"}}{{end}}' "$eval_bundle_holder" 2>/dev/null) || got=
  case "
$got
" in *"
$eval_bundle_vol
"*) ;; *)
    echo "eval: the holder $eval_bundle_holder does not mount $eval_bundle_vol" >&2
    return 1
    ;;
  esac
  # What the volume was asked to be is not what was mounted: read the mount itself.
  got=$(docker exec "$eval_bundle_holder" awk '$2 == "/b" { print $3 }' /proc/mounts 2>/dev/null) || got=
  [ "$got" = ramfs ] || {
    echo "eval: $eval_bundle_vol is not mounted as ramfs in the holder" >&2
    return 1
  }
  # macOS's tar (bsdtar) would pack extended attributes and AppleDouble files unless told not to.
  local flags=(--no-xattrs) version
  version=$(tar --version 2>/dev/null) || version=
  case "$version" in *bsdtar*) flags=(--no-mac-metadata --no-xattrs) ;; esac
  COPYFILE_DISABLE=1 tar "${flags[@]}" -C "$copy" -cf - . |
    docker run -i --rm --name "$eval_bundle_fill" --network none "${labels[@]}" -v "$eval_bundle_vol:/b" "$eval_bundle_image" \
      sh -c 'find /b -mindepth 1 -delete && tar -xf - -C /b --no-same-owner && chown -R 65532:65532 /b && chmod -R u=rX,go= /b'
  local piped=("${PIPESTATUS[@]}")
  if [ "${piped[0]}" != 0 ] || [ "${piped[1]}" != 0 ]; then
    echo "eval: filling $eval_bundle_vol failed (tar exit ${piped[0]}, fill exit ${piped[1]})" >&2
    return 1
  fi
  # The regular files' names only, never their content: the volume lists what the copy lists.
  local want
  want=$(cd "$copy" && find . -type f | LC_ALL=C sort) || return 1
  got=$(docker exec "$eval_bundle_holder" sh -c 'cd /b && find . -type f | LC_ALL=C sort' 2>/dev/null) || got=
  [ -n "$got" ] && [ "$got" = "$want" ] || {
    echo "eval: $eval_bundle_vol does not hold the bundle copy's files" >&2
    return 1
  }
  export SHN_EVAL_BUNDLE_VOLUME=$eval_bundle_vol
  eval_bundle_override=(-f "$eval_dir/compose.eval.ramfs.yml")
}

# eval_bundle_drop removes the fill container, the holder, then the volume, each only if it exists;
# it says what it could not remove or could not check, and always returns 0, so the rest of the
# smoke's trap still runs. It runs in that trap, where set -e holds: every status it reads is
# taken with `|| st=$?`, never left to end it.
eval_bundle_drop() {
  local name st try
  for name in "$eval_bundle_fill" "$eval_bundle_holder"; do
    [ -n "$name" ] || continue
    st=0; eval_docker_exists container "$name" || st=$?
    [ "$st" = 1 ] && continue
    docker rm -f "$name" >/dev/null 2>&1 || true
    st=0; eval_docker_exists container "$name" || st=$?
    case "$st" in
      1) ;;
      0) echo "eval: could not remove the container $name" >&2 ;;
      *) echo "eval: could not tell whether the container $name is gone" >&2 ;;
    esac
  done
  [ -n "$eval_bundle_vol" ] || return 0
  st=0; eval_docker_exists volume "$eval_bundle_vol" || st=$?
  [ "$st" = 1 ] && return 0
  for try in 1 2 3 4 5; do
    docker volume rm "$eval_bundle_vol" >/dev/null 2>&1 && break
    sleep 1
  done
  st=0; eval_docker_exists volume "$eval_bundle_vol" || st=$?
  case "$st" in
    1) ;;
    0) echo "eval: could not remove the volume $eval_bundle_vol" >&2 ;;
    *) echo "eval: could not tell whether the volume $eval_bundle_vol is gone" >&2 ;;
  esac
  return 0
}
