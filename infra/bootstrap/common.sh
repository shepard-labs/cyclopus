#!/bin/sh
# Shared, POSIX-sh helpers for Cyclopus node bootstrap.
#
# Sourced by the per-role scripts. Every helper is idempotent-friendly and every
# download is checksum-verified, reusing the pattern from
# scripts/integration/deploy_guest.sh.
set -eu

# Pinned versions — keep in lockstep with versions.yaml.
K3S_VERSION='v1.33.13+k3s1'
GVISOR_RELEASE='release-20260727.0'

log() { printf '\n==> bootstrap: %s\n' "$1"; }
warn() { printf 'bootstrap: WARN: %s\n' "$*" >&2; }
fail() { printf 'bootstrap: FAILED: %s\n' "$*" >&2; exit 1; }

retry() {
  _attempts=$1
  shift
  _count=1
  until "$@"; do
    if [ "$_count" -ge "$_attempts" ]; then
      return 1
    fi
    _count=$((_count + 1))
    sleep 3
  done
}

require_root() {
  if [ "$(id -u)" -ne 0 ]; then
    command -v sudo >/dev/null 2>&1 || fail "must run as root or with sudo available"
    sudo -n true 2>/dev/null || fail "passwordless sudo (or root) is required for bootstrap"
    SUDO=sudo
  else
    SUDO=
  fi
}

# Detect and validate the OS profile. Sets DISTRO and CODENAME.
detect_distro() {
  [ -r /etc/os-release ] || fail "/etc/os-release not found"
  # shellcheck disable=SC1091
  . /etc/os-release
  DISTRO=$ID
  CODENAME=${VERSION_CODENAME:-}
  case "$ID:$VERSION_ID" in
    ubuntu:24.04) : ;;
    debian:12) : ;;
    *) fail "unsupported profile: $ID $VERSION_ID (v1 supports ubuntu 24.04 and debian 12)" ;;
  esac
}

arch_go() {
  case "$(uname -m)" in
    aarch64 | arm64) printf 'arm64' ;;
    x86_64 | amd64) printf 'amd64' ;;
    *) fail "unsupported architecture $(uname -m)" ;;
  esac
}

# Download $2 to $1 and verify sha256 == $3. Idempotent: a matching file is kept.
download_verified() {
  _dest=$1
  _url=$2
  _sha=$3
  if [ -f "$_dest" ]; then
    _have=$(sha256sum "$_dest" | cut -d ' ' -f 1)
    [ "$_have" = "$_sha" ] && return 0
    rm -f "$_dest"
  fi
  retry 3 curl -fsSLo "$_dest" "$_url" || fail "download failed: $_url"
  _have=$(sha256sum "$_dest" | cut -d ' ' -f 1)
  [ "$_have" = "$_sha" ] || fail "checksum mismatch for $_url: expected $_sha, observed $_have"
}
