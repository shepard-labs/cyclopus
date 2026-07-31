#!/bin/sh
# Shared helpers + storage_destinations config for Cyclopus backup/DR (§9.1).
#
# WAL-G talks to an S3-compatible storage_destinations target. Credentials come
# from the environment (never hardcoded). The unit gate only syntax-checks these
# scripts; live use requires the env below.
set -eu

WAL_G_VERSION='v3.0.8'
PG_MAJOR="${PG_MAJOR:-18}"

log() { printf '\n==> backup: %s\n' "$1"; }
warn() { printf 'backup: WARN: %s\n' "$*" >&2; }
fail() { printf 'backup: FAILED: %s\n' "$*" >&2; exit 1; }

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
    sudo -n true 2>/dev/null || fail "passwordless sudo (or root) is required"
    SUDO=sudo
  else
    SUDO=
  fi
}

arch_go() {
  case "$(uname -m)" in
    aarch64 | arm64) printf 'aarch64' ;;
    x86_64 | amd64) printf 'amd64' ;;
    *) fail "unsupported architecture $(uname -m)" ;;
  esac
}

# Export WAL-G's S3 configuration from storage_destinations env. Callers set:
#   WALG_S3_ENDPOINT           e.g. https://s3.gra11.io.cloud.ovh.net
#   WALG_S3_BUCKET             storage_destinations bucket
#   WALG_S3_REGION             storage region
#   AWS_ACCESS_KEY_ID          S3 access key
#   AWS_SECRET_ACCESS_KEY      S3 secret key
#   WALG_S3_PREFIX_SUBPATH     optional prefix (defaults to postgres_backup)
walg_env() {
  : "${WALG_S3_ENDPOINT:?WALG_S3_ENDPOINT (storage_destinations S3 endpoint) is required}"
  : "${WALG_S3_BUCKET:?WALG_S3_BUCKET (storage_destinations bucket) is required}"
  : "${AWS_ACCESS_KEY_ID:?AWS_ACCESS_KEY_ID is required}"
  : "${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY is required}"
  _sub=${WALG_S3_PREFIX_SUBPATH:-postgres_backup}
  export WALG_S3_PREFIX="s3://${WALG_S3_BUCKET}/${_sub}"
  # WAL-G v3 reads WALG_S3_PREFIX; keep WALE_S3_PREFIX as an accepted alias for
  # older invocations. Both point at the SAME single source of truth.
  export WALE_S3_PREFIX="$WALG_S3_PREFIX"
  export AWS_ENDPOINT="$WALG_S3_ENDPOINT"
  export AWS_REGION="${WALG_S3_REGION:-us-east-1}"
  export AWS_S3_FORCE_PATH_STYLE=true
  export WALG_COMPRESSION_METHOD=lz4
}

# Materialize /etc/cyclopus/walg.env from the current WAL-G S3 environment so the
# archive_command / restore_command (which source this file) do not depend on
# ambient env. Callers must have run walg_env first. Single source of truth: the
# WALG_S3_* + AWS_* scheme above. Written 0600, root-owned dir 0700.
write_walg_env() {
  _envfile=${1:-/etc/cyclopus/walg.env}
  $SUDO install -d -m 0700 "$(dirname -- "$_envfile")"
  {
    printf 'export WALG_S3_PREFIX=%s\n' "$WALG_S3_PREFIX"
    printf 'export WALE_S3_PREFIX=%s\n' "$WALE_S3_PREFIX"
    printf 'export AWS_ENDPOINT=%s\n' "$AWS_ENDPOINT"
    printf 'export AWS_REGION=%s\n' "$AWS_REGION"
    printf 'export AWS_ACCESS_KEY_ID=%s\n' "$AWS_ACCESS_KEY_ID"
    printf 'export AWS_SECRET_ACCESS_KEY=%s\n' "$AWS_SECRET_ACCESS_KEY"
    printf 'export AWS_S3_FORCE_PATH_STYLE=%s\n' "$AWS_S3_FORCE_PATH_STYLE"
    printf 'export WALG_COMPRESSION_METHOD=%s\n' "$WALG_COMPRESSION_METHOD"
  } | $SUDO tee "$_envfile" >/dev/null
  $SUDO chmod 0600 "$_envfile"
}
