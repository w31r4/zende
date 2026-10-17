#!/usr/bin/env bash
# User-level releases. Installing changes the entry link, not running services.
set -euo pipefail
umask 077

fail() { printf 'zende install: %s\n' "$*" >&2; exit 1; }
usage() {
  printf '%s\n' \
    'Usage: bash scripts/install.sh [--bin-dir DIR] [--data-dir DIR]' \
    '       bash scripts/install.sh --list [path options]' \
    '       bash scripts/install.sh --rollback RELEASE [path options]'
}

bin_dir="${HOME:?HOME is required}/.local/bin"
data_dir="${XDG_DATA_HOME:-$HOME/.local/share}/zende"
action=install
rollback_id=''
while (($#)); do
  case "$1" in
    --bin-dir|--data-dir|--rollback)
      (($# >= 2)) || fail "$1 requires a value"
      case "$1" in
        --bin-dir) bin_dir=$2 ;;
        --data-dir) data_dir=$2 ;;
        --rollback) [[ $action == install ]] || fail 'choose one action'; action=rollback; rollback_id=$2 ;;
      esac
      shift 2 ;;
    --list) [[ $action == install ]] || fail 'choose one action'; action=list; shift ;;
    --help|-h) usage; exit 0 ;;
    *) fail "unknown argument: $1" ;;
  esac
done
[[ $bin_dir == /* && $data_dir == /* && $bin_dir != / && $data_dir != / ]] || fail 'installation directories must be absolute, non-root paths'
[[ $bin_dir != *$'\n'* && $data_dir != *$'\n'* ]] || fail 'installation paths must not contain newlines'

if [[ $action == list ]]; then
  for entry in "$data_dir"/releases/*/zende; do
    [[ -f $entry && ! -L $entry ]] || continue
    release=${entry%/zende}
    release_id=${release##*/}
    [[ $release_id =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] || continue
    printf '%s\n' "$release_id"
  done
  exit 0
fi

command -v flock >/dev/null || fail 'flock is required (util-linux)'
mkdir -p -- "$bin_dir" "$data_dir/releases"
bin_dir=$(cd -- "$bin_dir" && pwd -P)
releases=$(cd -- "$data_dir/releases" && pwd -P)
destination="$bin_dir/zende"
[[ ! -L $bin_dir/.zende-install.lock && ! -L $releases/.install.lock ]] || fail 'installation locks must not be symbolic links'
exec {entry_lock}>>"$bin_dir/.zende-install.lock"
flock -n "$entry_lock" || fail 'another install or rollback owns this entry directory'
exec {install_lock}>>"$releases/.install.lock"
flock -n "$install_lock" || fail 'another install or rollback owns this release directory'

previous=''
if [[ -L $destination ]]; then
  previous=$(readlink -f -- "$destination") || fail 'existing entry link cannot be resolved'
  case "$previous" in
    "$releases"/*/zende) [[ -f $previous && ! -L $previous ]] || fail 'existing release is unavailable' ;;
    *) fail 'entry link is not managed by this release directory; choose another --bin-dir' ;;
  esac
elif [[ -e $destination ]]; then
  fail 'an unmanaged file occupies the entry; preserve it or choose another --bin-dir'
fi

build_stage=''
link_stage=''
cleanup() {
  # Each directory was made privately by mktemp. Remove only our known files.
  if [[ -n $link_stage ]]; then
    rm -f -- "$link_stage/zende"
    rmdir -- "$link_stage" 2>/dev/null || true
  fi
  if [[ -n $build_stage ]]; then
    rm -f -- "$build_stage/zende"
    rmdir -- "$build_stage" 2>/dev/null || true
  fi
}
trap cleanup EXIT

if [[ $action == rollback ]]; then
  [[ $rollback_id =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] || fail 'invalid release identity'
  release_id=$rollback_id
  [[ -d $releases/$release_id && ! -L $releases/$release_id ]] || fail 'release directory is unavailable'
else
  go_executable=${ZENDE_GO:-go}
  command -v "$go_executable" >/dev/null || fail 'Go 1.26 is required'
  repository=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
  commit=source
  if command -v git >/dev/null && git -C "$repository" rev-parse --verify HEAD >/dev/null 2>&1; then
    commit=$(git -C "$repository" rev-parse --verify HEAD)
    if ! git -C "$repository" diff --quiet || ! git -C "$repository" diff --cached --quiet; then
      commit+=-dirty
    fi
  fi
  build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  build_stage=$(mktemp -d "$releases/.build.XXXXXXXX")
  release_id="$stamp-${commit:0:12}-${build_stage##*.}"
  if [[ $commit == *-dirty ]]; then release_id+=-dirty; fi
  (
    cd -- "$repository"
    "$go_executable" build -trimpath -mod=readonly \
      -ldflags "-X main.version=0.1.0-dev -X main.commit=$commit -X main.buildDate=$build_date" \
      -o "$build_stage/zende" ./cmd/zende
  )
  [[ -f $build_stage/zende && -x $build_stage/zende && ! -L $build_stage/zende ]] || fail 'build did not produce an executable'
  [[ ! -e $releases/$release_id && ! -L $releases/$release_id ]] || fail 'release identity is already occupied'
  mv -T -- "$build_stage" "$releases/$release_id"
  build_stage=''
fi

target="$releases/$release_id/zende"
[[ -f $target && -x $target && ! -L $target ]] || fail 'release executable is unavailable'
# Stage the link beside the entry so activation is a same-filesystem rename.
link_stage=$(mktemp -d "$bin_dir/.zende-install.XXXXXXXX")
ln -s -- "$target" "$link_stage/zende"
if [[ -n $previous ]]; then
  [[ -L $destination && $(readlink -f -- "$destination") == "$previous" ]] || fail 'entry changed during installation; new release retained'
else
  [[ ! -e $destination && ! -L $destination ]] || fail 'entry became occupied; new release retained'
fi
mv -T -- "$link_stage/zende" "$destination"
printf 'Installed: %s\nRelease: %s\n' "$destination" "$release_id"
if [[ -n $previous ]]; then printf 'Previous: %s\n' "$previous"; fi
